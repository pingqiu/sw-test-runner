package seaweed

import (
	"encoding/hex"
	"math"
)

// Bounds are conservative intervals on the MAINTENANCE HOST's boot-relative
// clock. Fault bounds are sampled on that same host before dispatch / after
// confirmed GONE. A raw timestamp from a victim/client is never substituted.
type maintenanceBound struct{ Lower, Upper float64 }
type maintenanceSubject struct {
	Collection string `json:"collection"`
	VID        string `json:"vid"`
	EncodeNS   string `json:"encode_ns"`
	Generation uint32 `json:"generation"`
	Shards     []int  `json:"shards"`
}
type maintenanceFlight struct {
	Subject                              maintenanceSubject `json:"subject"`
	Job, Attempt, Owner, Boot, Operation string
	Begin, End                           maintenanceBound
	Bytes                                int64
	State                                string
}
type maintenanceFault struct {
	Subject      maintenanceSubject
	Owner, Boot  string
	Before, Gone maintenanceBound
	Missing      []int
	Confirmed    bool
}
type maintenanceData struct {
	Key            string `json:"key"`
	ExpectedSHA256 string `json:"expected_sha256,omitempty"`
	ActualSHA256   string `json:"actual_sha256,omitempty"`
	ExpectedLength int64  `json:"expected_length,omitempty"`
	ActualLength   int64  `json:"actual_length,omitempty"`
	Deleted        bool   `json:"deleted,omitempty"`
	Status         int    `json:"status"`
	Observed       bool   `json:"observed"`
}
type maintenanceEvidence struct {
	Task                                          string
	Expected                                      maintenanceSubject
	Job, Attempt, Owner, Boot                     string
	Flights                                       []maintenanceFlight
	Faults                                        []maintenanceFault
	Data                                          []maintenanceData
	ExpectedData                                  []maintenanceData
	Terminal, Refusal                             string
	EffectsKnown, PostOwners, GenerationConverged bool
	Pending                                       int
}
type maintenanceVerdict struct {
	Data, Task, Coverage       string
	Refusal                    string `json:"refusal,omitempty"`
	CoveredFaults, TotalFaults int
}

func validMaintenanceBound(b maintenanceBound) bool {
	return b.Lower > 0 && b.Upper >= b.Lower && !math.IsNaN(b.Lower) && !math.IsNaN(b.Upper) && !math.IsInf(b.Lower, 0) && !math.IsInf(b.Upper, 0)
}
func sameMaintenanceSubject(a, b maintenanceSubject) bool {
	return a.VID != "" && a.Collection != "" && a.VID == b.VID && a.Collection == b.Collection && a.EncodeNS == b.EncodeNS && a.Generation == b.Generation
}

func assessMaintenance(e maintenanceEvidence) maintenanceVerdict {
	v := maintenanceVerdict{Data: "UNKNOWN", Task: "UNKNOWN", Coverage: "NOT_COVERED", TotalFaults: len(e.Faults), Refusal: e.Refusal}
	if len(e.ExpectedData) == 0 {
		return v
	}
	expected := map[string]maintenanceData{}
	for _, d := range e.ExpectedData {
		if d.Key == "" {
			return v
		}
		if _, ok := expected[d.Key]; ok {
			return v
		}
		expected[d.Key] = d
	}
	seen := map[string]bool{}
	unknown := len(e.Data) != len(expected)
	for _, d := range e.Data {
		want, ok := expected[d.Key]
		if !ok || seen[d.Key] {
			unknown = true
			continue
		}
		seen[d.Key] = true
		if !d.Observed {
			unknown = true
			continue
		}
		if want.Deleted {
			if d.Status == 200 {
				v.Data = "FAIL"
				v.Task = "FAIL"
				return v
			}
			if d.Status != 404 && d.Status != 410 {
				unknown = true
			}
		} else {
			if d.Status == 404 || d.Status == 410 || d.Status == 200 && (want.ExpectedLength != d.ActualLength || want.ExpectedSHA256 != d.ActualSHA256) {
				v.Data = "FAIL"
				v.Task = "FAIL"
				return v
			}
			sha, err := hex.DecodeString(want.ExpectedSHA256)
			if d.Status != 200 || want.ExpectedLength <= 0 || err != nil || len(sha) != 32 {
				unknown = true
			}
		}
	}
	if unknown {
		return v
	}
	v.Data = "PASS"
	if !e.EffectsKnown || !e.PostOwners || !e.GenerationConverged || e.Pending != 0 {
		return v
	}
	refusals := map[string]bool{"RPC_UNAVAILABLE": true, "GENERATION_SWITCH_IN_PROGRESS": true, "NO_TARGET_CAPACITY": true, "INSUFFICIENT_SHARDS": true, "MASTER_UNAVAILABLE": true}
	refused := e.Terminal == "refused" && refusals[e.Refusal]
	if refused {
		v.Task = "CLEAN_REFUSAL"
	}
	if (e.Terminal != "succeeded" && !refused) || e.Job == "" || e.Attempt == "" || e.Owner == "" || e.Boot == "" {
		return v
	}
	if e.Task == "ec_repair" || e.Task == "ec_vacuum" {
		if e.Expected.EncodeNS == "" {
			return v
		}
	}
	for _, f := range e.Faults {
		if !f.Confirmed || f.Owner != e.Owner || f.Boot != e.Boot || !sameMaintenanceSubject(f.Subject, e.Expected) || !validMaintenanceBound(f.Before) || !validMaintenanceBound(f.Gone) || f.Gone.Lower < f.Before.Upper {
			continue
		}
		missing := map[int]bool{}
		invalid := false
		for _, shard := range f.Missing {
			if shard < 0 || shard >= 14 {
				invalid = true
			}
			missing[shard] = true
		}
		if invalid || len(missing) > 4 {
			continue
		}
		for _, w := range e.Flights {
			if !sameMaintenanceSubject(w.Subject, e.Expected) || w.Job != e.Job || w.Attempt != e.Attempt || w.Owner != e.Owner || w.Boot != e.Boot || w.Operation == "" || w.State != "in_flight" || w.Bytes <= 0 || !validMaintenanceBound(w.Begin) || !validMaintenanceBound(w.End) {
				continue
			}
			if e.Task == "ec_repair" || e.Task == "ec_vacuum" {
				affected := false
				for _, a := range w.Subject.Shards {
					for _, b := range f.Subject.Shards {
						if a == b && missing[b] {
							affected = true
						}
					}
				}
				if !affected {
					continue
				}
			}
			if w.Begin.Upper <= f.Before.Lower && w.End.Lower >= f.Gone.Upper {
				v.CoveredFaults++
				break
			}
		}
	}
	if v.CoveredFaults > 0 {
		if !refused {
			v.Task = "RECOVERED"
		}
		v.Coverage = "COVERED"
	} else if !refused {
		v.Task = "NOT_PROVEN"
	}
	return v
}
