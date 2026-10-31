package seaweed

import (
	"encoding/json"
	"testing"
)

func TestPreKillFailedOperationBoundary(t *testing.T) {
	for _, mode := range []string{"previous-success-before-P", "failed-start-at-P", "straddles-P", "failed-before-P", "close-at-P-plus-60", "close-after-P-plus-60"} {
		t.Run(mode, func(t *testing.T) {
			c := readConfig()
			row := windowBody(t, c)
			row["started"], row["ended"], row["first_exact"], row["last_exact"] = 90., 210., 91., 210.
			times := [][2]float64{{90, 91}, {99.8, 99.9}, {100.1, 100.2}, {100.3, 100.4}, {100.9, 101}, {209, 210}}
			switch mode {
			case "failed-start-at-P":
				times[2][0] = 100
			case "straddles-P":
				times[2] = [2]float64{99.95, 100.05}
			case "failed-before-P":
				times[2] = [2]float64{99.91, 99.95}
			case "close-at-P-plus-60":
				times[4] = [2]float64{159.9, 160}
			case "close-after-P-plus-60":
				times[4] = [2]float64{160, 160.001}
			}
			for i, value := range row["outcomes"].([]any) {
				r := value.(map[string]any)
				r["started"], r["ended"] = times[i][0], times[i][1]
			}
			for i, value := range row["faults"].([]any) {
				r := value.(map[string]any)
				r["started"], r["ended"] = times[i+2][0], times[i+2][1]
			}
			raw, _ := json.Marshal(row)
			_, err := CheckReadTaskOutput(captured(raw), c, "load", "dc", ReadWindow{100})
			pass := mode == "previous-success-before-P" || mode == "failed-start-at-P" || mode == "close-at-P-plus-60"
			if pass && err != nil {
				t.Fatal(err)
			}
			if !pass && err == nil {
				t.Fatal("failed-operation boundary was ignored")
			}
		})
	}
}
