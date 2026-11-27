package v3block

// The proposal pin: a fixed seed must keep producing the same run. Two runs
// are pinned against golden files in testdata/, record by record (kind, verb,
// volume, outcome, args and result; not seq or the clock): a 300-operation
// run (seed 1, default negative share and weights) and the same run with a
// fault schedule. The gen_step records in them are the proposal list. A
// change to either file changes every recorded run of the same seed, so it
// must come with a new generator version.
//
// To rewrite the golden files after an intended change:
//
//	go test ./packs/v3block/ -run TestGenProposalPin -update-pin

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
)

var updatePin = flag.Bool("update-pin", false, "rewrite the proposal pin's golden files")

// pinLines renders a history as one line per record, without seq or clock.
func pinLines(t *testing.T, p string) string {
	t.Helper()
	var b strings.Builder
	for _, r := range records(t, p) {
		args, _ := json.Marshal(r.Args)
		res, _ := json.Marshal(r.Result)
		fmt.Fprintf(&b, "%s %s %s %s %s %s %s\n", r.Kind, r.Client, r.Verb, r.Volume, r.Outcome, args, res)
	}
	return b.String()
}

func checkPin(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *updatePin {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if w := strings.ReplaceAll(string(want), "\r\n", "\n"); w != got {
		wl, gl := strings.Split(w, "\n"), strings.Split(got, "\n")
		for i := 0; i < len(wl) && i < len(gl); i++ {
			if wl[i] != gl[i] {
				t.Fatalf("%s: line %d differs:\nwant %s\ngot  %s", name, i+1, wl[i], gl[i])
			}
		}
		t.Fatalf("%s: %d lines, want %d", name, len(gl), len(wl))
	}
}

func TestGenProposalPin(t *testing.T) {
	t.Run("m1", func(t *testing.T) {
		f := &fakeProduct{}
		f.install(t)
		actx, p := genRunCtx(t)
		act := genAct("300")
		act.Params["seed"] = "1"
		if _, err := v3GenRun(context.Background(), actx, act); err != nil {
			t.Fatal(err)
		}
		_ = actx.History.Close()
		got := pinLines(t, p)
		if n := strings.Count(got, " gen_step "); n != 300 {
			t.Fatalf("%d proposals, want 300", n)
		}
		checkPin(t, "pin-m1-seed1-300.txt", got)
	})
	t.Run("faults", func(t *testing.T) {
		f := &faultFake{}
		f.install(t)
		actx, p := genRunCtx(t)
		act := faultAct("300", "0.05")
		act.Params["seed"] = "1"
		if _, err := v3GenRun(context.Background(), actx, act); err != nil {
			t.Fatal(err)
		}
		_ = actx.History.Close()
		got := pinLines(t, p)
		if n := strings.Count(got, " gen_step "); n != 300 || !strings.Contains(got, tr.HistoryInvoke+" ") {
			t.Fatalf("%d proposals, want 300", n)
		}
		if !strings.Contains(got, " kill ") {
			t.Fatal("the pinned fault run has no kill")
		}
		checkPin(t, "pin-m2-faults-seed1-300.txt", got)
	})
	t.Run("master-outage", func(t *testing.T) {
		f := &faultFake{}
		f.install(t)
		actx, p := genRunCtx(t)
		act := outageAct("300")
		act.Params["seed"] = "1"
		if _, err := v3GenRun(context.Background(), actx, act); err != nil {
			t.Fatal(err)
		}
		_ = actx.History.Close()
		got := pinLines(t, p)
		if n := strings.Count(got, " gen_step "); n != 300 || !strings.Contains(got, " master_outage ") {
			t.Fatalf("%d proposals, or no master outage", n)
		}

		checkPin(t, "pin-m2-outage-seed1-300.txt", got)
	})
}
