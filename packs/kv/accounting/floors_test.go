package accounting_test

import (
	"fmt"
	"github.com/pingqiu/sw-test-runner/packs/kv/accounting"
	"testing"
)

// Literal accepted thresholds, independent of the implementation's policy table.
// Each row is aggregate count/bytes and native count/bytes for one window.
var acceptedFloors = [2][4]uint64{
	{1500, 1635779410, 1120, 1205863080},
	{1200, 1321206440, 900, 1006633470},
}

func floorWrites(totals [2][4]uint64) []accounting.Write {
	var writes []accounting.Write
	for window, total := range totals {
		at := []float64{310, 610}[window]
		// Preserve each byte total when reducing a count: count and byte negatives
		// therefore cannot accidentally fail another floor.
		for group, counts := range [][2]uint64{{total[2], total[3]}, {total[0] - total[2], total[1] - total[3]}} {
			for n := uint64(0); n < counts[0]; n++ {
				size := counts[1] / counts[0]
				if n < counts[1]%counts[0] {
					size++
				}
				writer := 0
				if group == 1 {
					writer = 3
				}
				writes = append(writes, accounting.Write{
					ID: fmt.Sprintf("%d/%d/%d", window, group, n), Writer: writer, Bytes: size,
					Admitted: point(at - .004), Submitted: point(at - .003), Completed: point(at - .002),
					Received: point(at - .001), ACKed: point(at),
				})
			}
		}
	}
	return writes
}

func TestEachFrozenFloorIndependently(t *testing.T) {
	_, _, reads := fixture()
	names := [4]string{"aggregate_count", "aggregate_bytes", "native_count", "native_bytes"}
	for window := 0; window < 2; window++ {
		for floor, name := range names {
			for _, below := range []bool{false, true} {
				t.Run(fmt.Sprintf("window_%d/%s/below_%t", window, name, below), func(t *testing.T) {
					totals := acceptedFloors
					if below {
						totals[window][floor]--
					}
					got, err := accounting.Evaluate(0, nil, floorWrites(totals), reads)
					if err != nil {
						t.Fatal(err)
					}
					wantResult := "PASS"
					if below {
						wantResult = "NOT_COVERED"
					}
					if got.Coverage != wantResult {
						t.Fatalf("coverage %s, want %s", got.Coverage, wantResult)
					}
					for i, row := range got.Windows {
						want := "PASS"
						if below && i == window {
							want = "NOT_COVERED"
						}
						measured := [4]uint64{row.ACKed.Count, row.ACKed.Bytes, row.RCACKed.Count, row.RCACKed.Bytes}
						if measured != totals[i] || row.Coverage != want || row.Telemetry != "COMPLETE" {
							t.Fatalf("window %d: measured=%v want=%v coverage=%s telemetry=%s", i, measured, totals[i], row.Coverage, row.Telemetry)
						}
						// Verify the successful read floors independently of the write totals.
						wantReads := [2][4]uint64{{300, 300, 300, 150}, {240, 240, 240, 120}}
						if row.Reads != wantReads[i] {
							t.Fatalf("read floors: %v", row.Reads)
						}
					}
				})
			}
		}
	}
}
