package seaweed

import (
	"encoding/json"
	"math"
	"testing"
)

func windowBody(t *testing.T, c SmokeConfig) map[string]any {
	t.Helper()
	var r map[string]any
	if err := json.Unmarshal(pythonReadOutput(t, "recovery", "dc"), &r); err != nil {
		t.Fatal(err)
	}
	start := 1.
	r["started"] = start
	r["run_id"], r["client"], r["root"] = c.RunID, c.Client, c.Root
	r["stage"], r["elapsed"], r["ended"], r["exact"], r["attempts"] = "load", 120., start+120, 4, 6
	r["first_exact"], r["last_exact"] = 2., 120.
	ids := []int{0, 1, 2, 3, 4, 5}
	times := [][2]float64{{1, 2}, {31, 32}, {33, 34}, {35, 36}, {37, 38}, {119, 120}}
	outcomes := []any{}
	faults := []any{}
	for i, id := range ids {
		exact := i != 2 && i != 3
		key := c.Client + "-" + string(rune('0'+id%c.Keys))
		outcomes = append(outcomes, map[string]any{"operation_id": id, "key": key, "started": times[i][0], "ended": times[i][1], "exact": exact})
		if !exact {
			faults = append(faults, map[string]any{"operation_id": id, "key": key, "action": "get", "target": c.RDMAAddress, "transport": "dc", "started": times[i][0], "ended": times[i][1], "ok": false, "results": nil})
		}
	}
	r["faults"], r["outcomes"] = faults, outcomes
	before := r["before"].(map[string]any)["loader"].(map[string]any)
	after := r["after"].(map[string]any)["loader"].(map[string]any)
	for k := range before {
		if _, ok := before[k].(float64); ok {
			before[k] = 0.
		}
	}
	after["read_objects"], after["direct_dc_reads"] = 4, 4
	after["read_bytes"], after["direct_dc_bytes"] = 4*c.Length, 4*c.Length
	r["after"].(map[string]any)["read_errors"] = 2
	return r
}

func TestRestartWindowActualAssessor(t *testing.T) {
	for _, mode := range []string{"positive", "missing-outcomes", "second-window", "before-prekill", "overlong", "missing-close", "wrong-key", "missing-id", "wrong-outcome", "reordered", "no-prefault-hit", "outside-load", "exact-close-budget", "late-close-budget"} {
		t.Run(mode, func(t *testing.T) {
			c := readConfig()
			row := windowBody(t, c)
			w := ReadWindow{30}
			o := row["outcomes"].([]any)
			switch mode {
			case "missing-outcomes":
				delete(row, "outcomes")
			case "second-window":
				// Keep failure counts and ownership consistent; split errors with success.
				o[3].(map[string]any)["exact"] = true
				o[4].(map[string]any)["exact"] = false
				f := row["faults"].([]any)[1].(map[string]any)
				f["operation_id"], f["key"], f["started"], f["ended"] = 4, "m01-0", 37., 38.
			case "exact-close-budget":
				o[4].(map[string]any)["started"], o[4].(map[string]any)["ended"] = 89., 90.
			case "late-close-budget":
				o[4].(map[string]any)["started"], o[4].(map[string]any)["ended"] = 89., 90.001
			case "before-prekill":
				w.PreKill = 33.001
			case "overlong":
				o[4].(map[string]any)["started"], o[4].(map[string]any)["ended"] = 95., 96.
			case "missing-close":
				for _, i := range []int{4, 5} {
					o[i].(map[string]any)["exact"] = false
				}
			case "wrong-key":
				o[1].(map[string]any)["key"] = "m01-0"
			case "missing-id":
				delete(o[0].(map[string]any), "operation_id")
			case "wrong-outcome":
				o[2].(map[string]any)["exact"] = true
			case "reordered":
				o[1].(map[string]any)["started"] = 1.
			case "outside-load":
				o[5].(map[string]any)["ended"] = 500.
			case "no-prefault-hit":
				w.PreKill = 1
			}
			raw, _ := json.Marshal(row)
			_, err := CheckReadTaskOutput(captured(raw), c, "load", "dc", w)
			if (mode == "positive" || mode == "exact-close-budget") && err != nil {
				t.Fatal(err)
			}
			if mode != "positive" && mode != "exact-close-budget" && err == nil {
				t.Fatal("unproven window accepted")
			}
		})
	}
}

func TestReadWindowRejectsNonfinitePreKill(t *testing.T) {
	c := readConfig()
	raw, err := json.Marshal(windowBody(t, c))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		value float64
	}{
		{"NaN", math.NaN()}, {"positive-infinity", math.Inf(1)}, {"negative-infinity", math.Inf(-1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := CheckReadTaskOutput(captured(raw), c, "load", "dc", ReadWindow{tc.value}); err == nil {
				t.Fatal("nonfinite pre-Kill clock accepted")
			}
		})
	}
	if _, err := CheckReadTaskOutput(captured(raw), c, "load", "dc", ReadWindow{30}); err != nil {
		t.Fatalf("finite valid pre-Kill clock refused: %v", err)
	}
}
