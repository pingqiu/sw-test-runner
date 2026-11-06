package accounting_test

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/pingqiu/sw-test-runner/packs/kv/accounting"
	"io"
	"math"
	"os"
	"testing"
)

func TestRetainedV3AStarvationMatchesFrozenPython(t *testing.T) {
	raw, e := os.ReadFile("testdata/provenance.json")
	if e != nil {
		t.Fatal(e)
	}
	var provenance struct {
		SHA256 string `json:"fixture_sha256"`
	}
	if e = json.Unmarshal(raw, &provenance); e != nil {
		t.Fatal(e)
	}
	raw, e = os.ReadFile("testdata/v3a-load.json.gz")
	if e != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != provenance.SHA256 {
		t.Fatal("fixture bytes changed")
	}
	f, e := os.Open("testdata/v3a-load.json.gz")
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	gz, e := gzip.NewReader(f)
	if e != nil {
		t.Fatal(e)
	}
	defer gz.Close()
	var input struct {
		Begin  float64
		Offers []accounting.Offer
		Writes []accounting.Write
		Reads  []accounting.Read
	}
	if e = json.NewDecoder(io.LimitReader(gz, 3<<20)).Decode(&input); e != nil {
		t.Fatal(e)
	}
	got, e := accounting.Evaluate(input.Begin, input.Offers, input.Writes, input.Reads)
	if e != nil {
		t.Fatal(e)
	}
	raw, e = os.ReadFile("testdata/legacy-expected.json")
	if e != nil {
		t.Fatal(e)
	}
	var want struct {
		Status  string
		Windows []struct {
			Bounds     [2]float64 `json:"window"`
			ACKRate    float64    `json:"aggregate_ack_s"`
			ByteRate   float64    `json:"aggregate_bytes_s"`
			RCRate     float64    `json:"rc_ack_s"`
			RCByteRate float64    `json:"rc_bytes_s"`
			Reads      [4]uint64
			Status     string
		}
	}
	if e = json.Unmarshal(raw, &want); e != nil {
		t.Fatal(e)
	}
	raw, e = os.ReadFile("testdata/phase-expected.json")
	if e != nil {
		t.Fatal(e)
	}
	var phases struct{ Windows []map[string]float64 }
	if e = json.Unmarshal(raw, &phases); e != nil {
		t.Fatal(e)
	}
	if got.Coverage != want.Status || len(got.Windows) != len(want.Windows) {
		t.Fatal(got)
	}
	offered := [2]uint64{5860, 4657}
	skipped := [2]uint64{5775, 4559}
	acks := [2]uint64{84, 97}
	for i, w := range want.Windows {
		g := got.Windows[i]
		if g.Bounds != w.Bounds || g.Coverage != w.Status || g.Reads != w.Reads || g.Offered != offered[i] || g.Skipped != skipped[i] || g.ACKed.Count != acks[i] || g.Admitted.Count != acks[i] {
			t.Fatal(g, w)
		}
		for _, v := range [][2]float64{{g.ACKed.PerSecond, w.ACKRate}, {g.ACKed.BytesPerSecond, w.ByteRate}, {g.RCACKed.PerSecond, w.RCRate}, {g.RCACKed.BytesPerSecond, w.RCByteRate}} {
			if math.Abs(v[0]-v[1]) > 1e-9 {
				t.Fatal(v)
			}
		}
		if g.Telemetry != "COMPLETE" || g.SET.Samples != acks[i] {
			t.Fatal("retained phase timing missing", g)
		}
		for name, got := range map[string]accounting.Timing{"SET": g.SET, "QueueAndPreparation": g.QueueAndPreparation, "NativeWait": g.NativeWait, "ReceiptAndChecks": g.ReceiptAndChecks, "Journal": g.Journal} {
			if got.Samples != acks[i] || math.Abs(got.MeanSeconds-phases.Windows[i][name]) > 1e-6 {
				t.Fatal("retained phase differs", name, got, phases.Windows[i][name])
			}
		}
	}
}

func point(v float64) *float64 { return &v }
func fixture() ([]accounting.Offer, []accounting.Write, []accounting.Read) {
	var offers []accounting.Offer
	var writes []accounting.Write
	var reads []accounting.Read
	for _, w := range [][2]float64{{300, 600}, {600, 840}} {
		for n := 0; n < int((w[1]-w[0])*20); n++ {
			offers = append(offers, accounting.Offer{Epoch: w[0] + float64(n)/20})
		}
		for n := 0; n < int((w[1]-w[0])*8); n++ {
			at := w[0] + .01 + float64(n)/8
			writes = append(writes, accounting.Write{ID: fmt.Sprint(w[0], "/", n), Writer: n % 4, Bytes: 1048576, Admitted: point(at - .004), Submitted: point(at - .003), Completed: point(at - .002), Received: point(at - .001), ACKed: point(at)})
		}
		for i := 0; i < 4; i++ {
			rate := 1.
			if i == 3 {
				rate = .5
			}
			for n := 0; n < int((w[1]-w[0])*rate); n++ {
				reads = append(reads, accounting.Read{Epoch: w[0] + float64(n)/rate, Worker: i, Succeeded: true})
			}
		}
	}
	return offers, writes, reads
}

func TestOffersCannotSubstituteForACKOrNativeWork(t *testing.T) {
	for _, kind := range []string{"slow-ACK", "HTTP-only", "read-starved"} {
		t.Run(kind, func(t *testing.T) {
			a, w, r := fixture()
			switch kind {
			case "slow-ACK":
				var reduced []accounting.Write
				for i, x := range w {
					if i%10 == 0 {
						reduced = append(reduced, x)
					}
				}
				w = reduced
			case "HTTP-only":
				for i := range w {
					w[i].Writer = 3
				}
			case "read-starved":
				for i := range r {
					if r[i].Worker == 0 {
						r[i].Succeeded = false
					}
				}
			}
			got, e := accounting.Evaluate(0, a, w, r)
			if e != nil {
				t.Fatal(e)
			}
			if got.Coverage != "NOT_COVERED" {
				t.Fatal("offered/configured load substituted", got)
			}
		})
	}
}
func TestLateACKIsNotBackcharged(t *testing.T) {
	w := accounting.Write{ID: "crossing", Writer: 0, Bytes: 100, Admitted: point(599.999), Submitted: point(599.9991), Completed: point(600), Received: point(600.0005), ACKed: point(600.001)}
	got, e := accounting.Evaluate(0, nil, []accounting.Write{w}, nil)
	if e != nil {
		t.Fatal(e)
	}
	if got.Windows[0].Admitted.Count != 1 || got.Windows[0].ACKed.Count != 0 || got.Windows[1].ACKed.Count != 1 {
		t.Fatal(got)
	}
	w.ACKed = point(840)
	w.Received = point(840)
	got, e = accounting.Evaluate(0, nil, []accounting.Write{w}, nil)
	if e != nil {
		t.Fatal(e)
	}
	if got.Windows[1].ACKed.Count != 0 {
		t.Fatal("upper bound included")
	}
}
func TestMissingStageIsNotZeroTimeProof(t *testing.T) {
	a, w, r := fixture()
	w[0].Submitted = nil
	got, e := accounting.Evaluate(0, a, w, r)
	if e != nil {
		t.Fatal(e)
	}
	if got.Coverage != "NOT_PROVEN" || got.Windows[0].Telemetry != "NOT_PROVEN" || got.Windows[0].Submitted.Count != 2399 {
		t.Fatal(got.Windows[0])
	}
}

func TestStageTimesAreMeasuredAndNamedWithoutStoreClaim(t *testing.T) {
	a, w, r := fixture()
	got, e := accounting.Evaluate(0, a, w, r)
	if e != nil {
		t.Fatal(e)
	}
	first := got.Windows[0]
	if first.SET.Samples != 2400 || math.Abs(first.SET.MeanSeconds-.004) > 1e-10 {
		t.Fatal(first.SET)
	}
	for _, phase := range []accounting.Timing{first.QueueAndPreparation, first.NativeWait, first.ReceiptAndChecks, first.Journal} {
		if phase.Samples != 2400 || math.Abs(phase.MeanSeconds-.001) > 1e-10 {
			t.Fatal(phase)
		}
	}
	w[0].Submitted = nil
	got, e = accounting.Evaluate(0, a, w, r)
	if e != nil {
		t.Fatal(e)
	}
	if got.Windows[0].NativeWait.Samples != 2399 || got.Windows[0].Telemetry != "NOT_PROVEN" {
		t.Fatal("unknown timing filled with zero")
	}
}
func TestInvalidEvidenceRefused(t *testing.T) {
	for _, kind := range []string{"begin", "offer", "writer", "read-worker", "duplicate", "backwards", "overflow", "timing-overflow"} {
		t.Run(kind, func(t *testing.T) {
			a, w, r := fixture()
			begin := 0.
			switch kind {
			case "begin":
				begin = math.NaN()
			case "offer":
				a[0].Epoch = math.Inf(1)
			case "writer":
				w[0].Writer = -1
			case "read-worker":
				r[0].Worker = 4
			case "duplicate":
				w[1].ID = w[0].ID
			case "backwards":
				w[0].Completed = point(0)
			case "overflow":
				w[0].Bytes = ^uint64(0)
			case "timing-overflow":
				for i := 0; i < 2; i++ {
					w[i].Admitted = point(-1e308)
					w[i].Submitted = point(-1e308)
				}
			}
			if _, e := accounting.Evaluate(begin, a, w, r); e == nil {
				t.Fatal("invalid evidence accepted", kind)
			}
		})
	}
}
func TestFrozenWindowsCountMeasuredACKs(t *testing.T) {
	a, w, r := fixture()
	got, e := accounting.Evaluate(0, a, w, r)
	if e != nil {
		t.Fatal(e)
	}
	if got.Coverage != "PASS" || len(got.Windows) != 2 {
		t.Fatal(got)
	}
	first := got.Windows[0]
	if first.Offered != 6000 || first.ACKed.Count != 2400 || first.RCACKed.Count != 1800 || first.ACKed.PerSecond != 8 || first.Telemetry != "COMPLETE" {
		t.Fatal(first)
	}
}
