package v3block

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

// Every function that decides an outcome, one row per cell.

func TestParseV3ctl(t *testing.T) {
	id := strings.Repeat("ab", 16)
	for _, c := range []struct {
		name, out string
		code      int
		want      string
	}{
		{"ok", `{"outcome":"ok","volume":"v001","layout_id":"` + id + `","call_ns":5}`, 0, "ok"},
		{"refused", `{"outcome":"refused","volume":"v001","code":"AlreadyExists"}`, 1, "refused"},
		{"unknown", `{"outcome":"unknown","volume":"v001"}`, 3, "unknown"},
		{"ok answer, nonzero exit", `{"outcome":"ok","volume":"v001","layout_id":"` + id + `"}`, 1, "unknown"},
		{"refused answer, exit 0", `{"outcome":"refused","volume":"v001"}`, 0, "unknown"},
		{"ok without a layout id", `{"outcome":"ok","volume":"v001"}`, 0, "unknown"},
		{"another volume", `{"outcome":"ok","volume":"v002","layout_id":"` + id + `"}`, 0, "unknown"},
		{"no answer", ``, 0, "unknown"},
		{"no outcome", `{"volume":"v001"}`, 0, "unknown"},
	} {
		if _, got := parseV3ctl(c.out, c.code, "v001"); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

func TestChunkOutcome(t *testing.T) {
	for _, c := range []struct {
		name, out string
		code      int
		want      string
	}{
		{"ok", `{"name":"s1"}`, 0, "ok"},
		// Refused only where the product states nothing was applied.
		{"detach aborted, retryable, applied nothing", `{"code":"detach_aborted","message":"x","retryable":true}`, 1, "refused"},
		{"usage, before the request", `{"code":"bad_request","message":"x","retryable":false}`, 2, "refused"},
		// The product's own unknown stays unknown whatever its flag says.
		{"outcome unknown, retryable", `{"code":"outcome_unknown","message":"x","retryable":true}`, 1, "unknown"},
		{"outcome unknown, not retryable", `{"code":"outcome_unknown","message":"x","retryable":false}`, 1, "unknown"},
		// No stated "not applied": unknown, a read of the state decides.
		{"non-retryable without a guarantee", `{"code":"layout_refused","message":"x","retryable":false}`, 1, "unknown"},
		{"retryable", `{"code":"remote_unavailable","message":"x","retryable":true}`, 1, "unknown"},
		{"no body", ``, 1, "unknown"},
		{"body without a code", `{"message":"x"}`, 1, "unknown"},
	} {
		if got, _ := chunkOutcome(c.out, c.code); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

func TestWsOutcome(t *testing.T) {
	want := map[int]string{0: "ok", 1: "refused", 2: "refused", 3: "unknown", 4: "unknown", 5: "refused", 9: "unknown", -1: "unknown"}
	for code, w := range want {
		if got := wsOutcome(code); got != w {
			t.Errorf("exit %d: %s, want %s", code, got, w)
		}
	}
}

func TestParseDetach(t *testing.T) {
	good := `{"volumeID":"v001","headGen":7,"headEpoch":2,"cutLSN":3,"lastWrittenLSN":3}`
	if g, err := parseDetach(good, "sealed HEAD generation: 7\n", "v001", 2); err != nil || g != 7 {
		t.Fatalf("good detach: %d %v", g, err)
	}
	for name, c := range map[string][2]string{
		"stderr names another generation": {good, "sealed HEAD generation: 8"},
		"stderr has more":                 {good, "sealed HEAD generation: 7\nwarning"},
		"another volume":                  {strings.Replace(good, "v001", "v002", 1), "sealed HEAD generation: 7"},
		"another epoch":                   {strings.Replace(good, `"headEpoch":2`, `"headEpoch":3`, 1), "sealed HEAD generation: 7"},
		"no generation":                   {strings.Replace(good, `"headGen":7`, `"headGen":0`, 1), "sealed HEAD generation: 0"},
		"not JSON":                        {"sealed", "sealed HEAD generation: 7"},
	} {
		if _, err := parseDetach(c[0], c[1], "v001", 2); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestShowsAck(t *testing.T) {
	out := "volume v001\nrecorded ACK: r1@2 HEAD generation 7\nlease: none\n"
	if !showsAck(out, 2, 7) {
		t.Fatal("the recorded ACK was not seen")
	}
	for _, c := range [][2]uint64{{2, 8}, {3, 7}} {
		if showsAck(out, c[0], c[1]) {
			t.Errorf("ACK r1@%d gen %d accepted from %q", c[0], c[1], out)
		}
	}
	if showsAck("recorded ACK: none\n", 2, 7) || showsAck("x recorded ACK: r1@2 HEAD generation 7", 2, 7) {
		t.Error("an ACK line that is not the line was accepted")
	}
}

func TestParseMoved(t *testing.T) {
	line := "clean move issued: volume v001 request v001-move-1: r1@2 -> r1@3 on b-v001 (sealed HEAD gen 7, remote LSN 4)\nnext: start volume v001 on b-v001\n"
	if e, err := parseMoved(line, "v001", "v001-move-1", 2, 7, 7, "b-v001"); err != nil || e != 3 {
		t.Fatalf("good move: %d %v", e, err)
	}
	// An operator write after the stop moved HEAD from 7 to 8: the move names
	// HEAD's generation at the move and is still a clean move.
	afterOpWrite := strings.Replace(line, "gen 7", "gen 8", 1)
	if e, err := parseMoved(afterOpWrite, "v001", "v001-move-1", 2, 7, 8, "b-v001"); err != nil || e != 3 {
		t.Fatalf("move after an operator write: %d %v", e, err)
	}
	for name, args := range map[string]struct {
		vol, req, target    string
		epoch, sealed, head uint64
	}{
		"another volume":             {"v002", "v001-move-1", "b-v001", 2, 7, 7},
		"another request":            {"v001", "x", "b-v001", 2, 7, 7},
		"another target":             {"v001", "v001-move-1", "a-v001", 2, 7, 7},
		"another epoch":              {"v001", "v001-move-1", "b-v001", 1, 7, 7},
		"below the sealed gen":       {"v001", "v001-move-1", "b-v001", 2, 8, 7},
		"not HEAD's gen at the move": {"v001", "v001-move-1", "b-v001", 2, 7, 8},
	} {
		if _, err := parseMoved(line, args.vol, args.req, args.epoch, args.sealed, args.head, args.target); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := parseMoved(strings.Replace(line, "r1@3", "r1@2", 1), "v001", "v001-move-1", 2, 7, 7, "b-v001"); err == nil {
		t.Error("a move to the same epoch was accepted")
	}
}

// Every shell-special character is quoted; plain words stay bare.
func TestShellQuote(t *testing.T) {
	for _, s := range []string{"v001|r1|0|", "a;b", "a&b", "a>b", "a<b", "(a)", "a*", "a?", "[a]", "~a", "#a", "a!b", "a b", "it's", "$x", "`x`", "a\nb"} {
		q := shellQuote(s)
		if !strings.HasPrefix(q, "'") {
			t.Errorf("%q left bare as %q", s, q)
		}
	}
	for _, s := range []string{"v001", "127.0.0.1:20013", "/root/x-y_z", "nqn.2026-11.io.seaweedfs:vs-v001", "a=b,c@d%e+f"} {
		if q := shellQuote(s); q != s {
			t.Errorf("%q quoted as %q", s, q)
		}
	}
	if q := shellQuote("it's"); q != `'it'\''s'` {
		t.Errorf("embedded quote: %q", q)
	}
}

// A write whose outcome is unknown leaves the block's earlier values or the
// attempted one, nothing else; an acknowledged write leaves only its value.
func TestUnknownWriteKeepsTheSet(t *testing.T) {
	a, b := blockSHA("v001|r1|0|"), blockSHA("v001|r2|0|")
	if got := unknownWrite(nil, a); len(got) != 2 || got[0] != zeroBlockSHA || got[1] != a {
		t.Fatalf("first write unknown: %v", got)
	}
	got := unknownWrite([]string{a}, b)
	if !contains(got, a) || !contains(got, b) || contains(got, zeroBlockSHA) || len(got) != 2 {
		t.Fatalf("after an acknowledged write, an unknown one: %v", got)
	}
	if contains(got, blockSHA("garbage")) {
		t.Fatal("another content is allowed")
	}
	if z := blockSHA(strings.Repeat("\x00", 8)); z != zeroBlockSHA {
		t.Fatalf("zeroBlockSHA %s, a zero block hashes to %s", zeroBlockSHA, z)
	}
}

func TestServing(t *testing.T) {
	ok := vsStatus{VolumeID: "v001", Epoch: 2, Healthy: true, AuthorityRole: "primary", FrontendPrimaryReady: true}
	if !ok.serving("v001") {
		t.Fatal("a serving primary was not serving")
	}
	for name, st := range map[string]vsStatus{
		"other volume": {VolumeID: "v002", Epoch: 2, Healthy: true, AuthorityRole: "primary", FrontendPrimaryReady: true},
		"unhealthy":    {VolumeID: "v001", Epoch: 2, AuthorityRole: "primary", FrontendPrimaryReady: true},
		"not primary":  {VolumeID: "v001", Epoch: 2, Healthy: true, AuthorityRole: "replica", FrontendPrimaryReady: true},
		"no frontend":  {VolumeID: "v001", Epoch: 2, Healthy: true, AuthorityRole: "primary"},
		"epoch zero":   {VolumeID: "v001", Healthy: true, AuthorityRole: "primary", FrontendPrimaryReady: true},
	} {
		if st.serving("v001") {
			t.Errorf("%s: serving", name)
		}
	}
}

func TestLatencyNearestRank(t *testing.T) {
	var d []time.Duration
	for i := 1; i <= 200; i++ {
		d = append(d, time.Duration(i)*time.Millisecond)
	}
	l := latency(d)
	if l["p50_ms"] != "100.0" || l["p99_ms"] != "198.0" || l["max_ms"] != "200.0" || l["n"] != "200" {
		t.Fatalf("latency of 1..200 ms: %v", l)
	}
	if l := latency(d[:10]); l["p99_ms"] != l["max_ms"] {
		t.Fatalf("with 10 samples p99 is the max: %v", l)
	}
	if l := latency(nil); l["n"] != "0" || l["p99_ms"] != "" {
		t.Fatalf("no samples: %v", l)
	}
}

// A volume that is not ok fails the action and is named; refused and
// unknown are never counted as ok.
func TestReportFailsOnAnyVolume(t *testing.T) {
	actx := &tr.ActionContext{Vars: map[string]string{}, Log: func(string, ...interface{}) {}}
	act := tr.Action{SaveAs: "c", Params: map[string]string{}}
	outs := []outcome{{Volume: "v001", Result: "ok", Dur: time.Millisecond}, {Volume: "v002", Result: "unknown", Detail: "x"}, {Volume: "v003", Result: "refused", Detail: "y"}}
	res, err := report(actx, act, "op", outs)
	if err == nil || !strings.Contains(err.Error(), "v002 unknown") || !strings.Contains(err.Error(), "v003 refused") {
		t.Fatalf("error %v", err)
	}
	if res["value"] != "1" || res["failed"] != "2" || actx.Vars["c_n"] != "1" {
		t.Fatalf("result %v vars %v", res, actx.Vars)
	}
	if _, err := report(actx, act, "op", outs[:1]); err != nil {
		t.Fatalf("all ok: %v", err)
	}
}

// The spec places each volume on its first host and gives no size: the
// volume is created by the timed call, not by the spec import.
func TestClusterSpecHoldsPlacementsOnly(t *testing.T) {
	s := &vsSet{}
	for i := 1; i <= 2; i++ {
		name := []string{"", "v001", "v002"}[i]
		s.Volumes = append(s.Volumes, vsVolume{Name: name, Hosts: vsSpecHosts(20000, i, name)})
	}
	spec := vsClusterSpec(s)
	for _, want := range []string{"server_id: a-v001", "server_id: b-v001", "- id: v002", "      - server_id: a-v002"} {
		if !strings.Contains(spec, want) {
			t.Errorf("spec lacks %q:\n%s", want, spec)
		}
	}
	if strings.Contains(spec, "size_bytes") {
		t.Errorf("spec creates volumes:\n%s", spec)
	}
	if h := vsSpecHosts(20000, 2, "v002"); h[0].NVMe != "127.0.0.1:20023" || h[1].Status != "127.0.0.1:20026" {
		t.Errorf("ports %+v", h)
	}
}

func TestHostSamplerKeepsMaximaAndSampleCount(t *testing.T) {
	py, err := exec.LookPath("python")
	if err != nil {
		py, err = exec.LookPath("python3")
	}
	if err != nil {
		t.Skip("Python unavailable")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "samples.json")
	harness := `import builtins,io,os,sys,time
realopen=builtins.open; reallist=os.listdir; realexists=os.path.exists
step=0
cpurows=iter(["cpu 50 0 0 50 0", "cpu 140 0 0 60 0", "cpu 160 0 0 140 0"])
def fakeopen(path,*args,**kw):
    if path=="/proc/stat": return io.StringIO(next(cpurows))
    if path=="/proc/1/cmdline": return io.BytesIO(b"/owned/blockvolume\0")
    if path=="/proc/1/status": return io.StringIO("VmRSS: %d kB\n"%(2048 if step==1 else 1024))
    return realopen(path,*args,**kw)
def listing(path):
    if path=="/proc": return ["1"]
    if path=="/proc/1/fd": return list(range(10 if step==1 else 4))
    return reallist(path)
def sleep(n):
    global step
    assert n==1
    step+=1
builtins.open=fakeopen; os.listdir=listing; time.sleep=sleep
os.path.exists=lambda p: step>=2 if p==sys.argv[3] else realexists(p)
`
	path := filepath.Join(dir, "sampler.py")
	if err := os.WriteFile(path, []byte(harness+hostSampler), 0600); err != nil {
		t.Fatal(err)
	}
	if b, err := exec.Command(py, path, "/owned/blockvolume", out, out+".stop").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]float64
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["samples"] != 2 || got["rss_mib"] != 2 || got["open_fds"] != 10 || got["cpu_busy_pct"] != 90 {
		t.Fatalf("maxima/count: %s", b)
	}
	if _, err := os.Stat(out + ".done"); err != nil {
		t.Fatal("sampler did not settle", err)
	}
}
