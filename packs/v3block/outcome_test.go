package v3block

import (
	"context"
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
		{"an outcome outside the three, exit 0", `{"outcome":"maybe","volume":"v001","layout_id":"` + id + `"}`, 0, "unknown"},
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
		if got, _ := chunkOutcome(chunkDetach, c.out, c.code); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

// One row per (operation, code): refused only where that operation's path
// answers the code before any change. The unknown rows are the red controls:
// the same code from an operation, or a path, that can answer it after a
// change stays unknown.
func TestChunkOutcomePerOperation(t *testing.T) {
	body := func(code, msg string) string {
		return `{"code":"` + code + `","message":"` + msg + `","retryable":false}`
	}
	// The product's two not_admitted answers to a volume delete (messages as
	// the CLI prints err.Error()): the reservation refused before persisting,
	// and the HEAD update refused after the reservation committed.
	const beforeReservation = "layout: delete reservation: chunkstore: volume delete not admitted: rpc error: code = FailedPrecondition desc = reserve volume deletion: workspace: deletion refused before persistence: workspace delete: live until 2026-11-26T10:00:00Z: lease live"
	const afterReservation = "layout: a holder is acting on the volume: chunkstore: volume delete not admitted: rpc error: code = FailedPrecondition desc = workspace delete: live until 2026-11-26T10:00:00Z: lease live"
	all := []string{chunkDetach, chunkSnapshot, chunkSnapDelHolder, chunkSnapDelStore, chunkRestore, chunkRelease, chunkVolumeDelete}
	refused := map[string]map[string]string{ // code -> op -> message, for the refused cells
		"layout_refused":          {chunkSnapDelHolder: "layout: a snapshot has dependents"},
		"snapshot_has_dependents": {chunkSnapDelStore: "layout: a snapshot has dependents"},
		"volume_deleting":         {chunkSnapDelStore: "layout: volume is being deleted"},
		"conflict":                {chunkSnapDelStore: "layout: HEAD generation moved"},
		"target_exists":           {chunkRestore: "create volume: t is bound to another restore request"},
		"target_live":             {chunkRelease: "layout: the dependent's target is live"},
		"not_admitted":            {chunkVolumeDelete: beforeReservation},
		"snapshot_exists":         {},
		"snapshot_deleting":       {},
		"internal":                {},
	}
	for code, cells := range refused {
		for _, op := range all {
			msg, want := cells[op], tr.OutcomeRefused
			if msg == "" {
				msg, want = "x", tr.OutcomeUnknown
			}
			if got, _ := chunkOutcome(op, body(code, msg), 1); got != want {
				t.Errorf("%s from %s: %s, want %s", code, op, got, want)
			}
		}
	}
	// Red controls named by the side effect they guard against.
	for _, c := range []struct{ name, op, out, want string }{
		{"delete refused after its reservation committed", chunkVolumeDelete, body("not_admitted", afterReservation), tr.OutcomeUnknown},
		{"delete: dependents found after the reservation", chunkVolumeDelete, body("snapshot_has_dependents", "layout: a snapshot has dependents"), tr.OutcomeUnknown},
		{"delete: HEAD update lost after the reservation", chunkVolumeDelete, body("conflict", "layout: HEAD generation moved"), tr.OutcomeUnknown},
		{"restore not admitted: the target name may stay bound", chunkRestore, body("not_admitted", "restore target: layout: snapshot names another volume's chunks"), tr.OutcomeUnknown},
		{"release lost its update after the tombstone", chunkRelease, body("conflict", "layout: HEAD generation moved"), tr.OutcomeUnknown},
		{"eleventh snapshot: the cut committed", chunkSnapshot, body("internal", "layout: at most 10 per volume"), tr.OutcomeUnknown},
		{"snapshot answered layout_refused: the cut may have committed", chunkSnapshot, body("layout_refused", "x"), tr.OutcomeUnknown},
		{"an unnamed operation gets the common codes only", "", body("target_exists", "x"), tr.OutcomeUnknown},
		{"the sentinel under another code", chunkVolumeDelete, body("internal", beforeReservation), tr.OutcomeUnknown},
		{"the sentinel from another operation", chunkRestore, body("not_admitted", beforeReservation), tr.OutcomeUnknown},
		{"a refused code with exit 0 is ok", chunkRestore, body("target_exists", "x"), tr.OutcomeOK},
	} {
		code := 1
		if c.want == tr.OutcomeOK {
			code = 0
		}
		if got, _ := chunkOutcome(c.op, c.out, code); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

// A negative delete (expect=refused) is one call; any other delete reruns
// while the answer is not_admitted.
func TestDeleteRetriesNotAdmittedUnlessNegative(t *testing.T) {
	defer func(d time.Duration) { deleteRetryEvery = d }(deleteRetryEvery)
	deleteRetryEvery = time.Millisecond
	live := `{"code":"not_admitted","message":"layout: delete reservation: chunkstore: volume delete not admitted: rpc error: code = FailedPrecondition desc = reserve volume deletion: workspace: deletion refused before persistence: workspace delete: live until T","retryable":false}`
	horizon := `{"code":"not_admitted","message":"layout: a holder is acting on the volume: workspace delete: live until T","retryable":false}`
	deleted := `{"volume":"v1","state":"deleted"}`
	count := func(f *fakeNode) int {
		n := 0
		for _, c := range f.calls {
			if strings.Contains(c, "volume-delete") {
				n++
			}
		}
		return n
	}
	v, _, e := testVolume()

	neg := e
	neg.expect = tr.OutcomeRefused
	f := (&fakeNode{}).on("volume-delete", fakeAnswer{out: live, code: 1}, ok(deleted))
	if out, res := neg.deleteVolume(context.Background(), f, v, "delete-v1", time.Minute); out != tr.OutcomeRefused || count(f) != 1 {
		t.Fatalf("negative delete: %s after %d calls %v; want refused after 1", out, count(f), res)
	}
	// A negative whose answer is the post-reservation not_admitted is still
	// one call, and unknown.
	f = (&fakeNode{}).on("volume-delete", fakeAnswer{out: horizon, code: 1}, ok(deleted))
	if out, _ := neg.deleteVolume(context.Background(), f, v, "delete-v1", time.Minute); out != tr.OutcomeUnknown || count(f) != 1 {
		t.Fatalf("negative delete after the reservation: %s after %d calls; want unknown after 1", out, count(f))
	}

	// Red control: an ordinary delete still reruns through not_admitted,
	// whichever of the two it is, and ends with the last answer.
	f = (&fakeNode{}).on("volume-delete", fakeAnswer{out: live, code: 1}, fakeAnswer{out: horizon, code: 1}, ok(deleted))
	out, res := e.deleteVolume(context.Background(), f, v, "delete-v1", time.Minute)
	if out != tr.OutcomeOK || count(f) != 3 || len(res["attempts"].([]string)) != 3 {
		t.Fatalf("ordinary delete: %s after %d calls %v; want ok after 3", out, count(f), res)
	}
	// And stops at the bound with the last answer.
	f = (&fakeNode{}).on("volume-delete", fakeAnswer{out: horizon, code: 1})
	if out, _ := e.deleteVolume(context.Background(), f, v, "delete-v1", 20*time.Millisecond); out != tr.OutcomeUnknown || count(f) < 2 {
		t.Fatalf("ordinary delete at its bound: %s after %d calls; want unknown after at least 2", out, count(f))
	}
}

// Per runner operation. Red control: the CLI's "nothing done" exits 1, 2 and
// 5 stay unknown for stop and move, which sealed HEAD before the command.
func TestWsOutcome(t *testing.T) {
	want := map[int]string{0: "ok", 1: "unknown", 2: "unknown", 3: "unknown", 4: "unknown", 5: "unknown", 9: "unknown", -1: "unknown"}
	for _, op := range []string{wsStop, wsMove, "", "other"} {
		for code, w := range want {
			if got := wsOutcome(op, code); got != w {
				t.Errorf("%q exit %d: %s, want %s", op, code, got, w)
			}
		}
	}
}

// The delete sentinel fails safe: any change to the product's text makes the
// answer unknown, never refused.
func TestDeleteSentinelFailsSafe(t *testing.T) {
	const prefix = "layout: delete reservation: chunkstore: volume delete not admitted: rpc error: code = FailedPrecondition desc = reserve volume deletion: "
	body := func(msg string) string { return `{"code":"not_admitted","message":"` + msg + `","retryable":false}` }
	if got, _ := chunkOutcome(chunkVolumeDelete, body(prefix+refusedBeforeChange+": live until T"), 1); got != tr.OutcomeRefused {
		t.Fatalf("the sentinel as the product writes it: %s, want refused", got)
	}
	for _, changed := range []string{
		"workspace: deletion refused before persisting",
		"workspace: Deletion refused before persistence",
		"workspace:  deletion refused before persistence",
		"workspace: deletion was refused before persistence",
		"deletion refused before persistence", // without its "workspace: " prefix
		"",
	} {
		if got, _ := chunkOutcome(chunkVolumeDelete, body(prefix+changed+": live until T"), 1); got != tr.OutcomeUnknown {
			t.Errorf("changed text %q: %s, want unknown", changed, got)
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
