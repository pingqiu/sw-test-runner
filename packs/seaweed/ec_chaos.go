package seaweed

import (
	"encoding/hex"
	"errors"
	"math"
)

type EC1aObject struct {
	Key    string `json:"key"`
	FID    string `json:"fid"`
	SHA256 string `json:"sha256"`
	Length int
	ACK    bool
}
type EC1aManifest struct {
	RunID      string       `json:"run_id"`
	Generation string       `json:"generation"`
	Objects    []EC1aObject `json:"objects"`
}
type EC1aRead struct {
	Op             chaosOp
	Route          string
	Status, Length int
	SHA256         string
}
type EC1aResult struct{ Integrity, Exposure string }

// AssessEC1a assesses one route against its frozen ACKed object manifest.
// It has no product reconstruction witness: integrity cannot promote exposure.
func AssessEC1a(m EC1aManifest, reads []EC1aRead, deadline float64) (EC1aResult, error) {
	result := EC1aResult{Integrity: "FAIL", Exposure: "NOT_OBSERVED"}
	refuse := func() (EC1aResult, error) { return result, errors.New("EC1a manifest or exact timed read invalid") }
	finite := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
	if m.RunID == "" || m.Generation == "" || len(m.Objects) == 0 || len(m.Objects) > 1024 || len(reads) != len(m.Objects) || len(reads) > 4096 || !finite(deadline) || deadline <= 0 || deadline > 30 {
		return refuse()
	}
	objects := map[string]EC1aObject{}
	for _, o := range m.Objects {
		digest, e := hex.DecodeString(o.SHA256)
		if o.Key == "" || o.FID == "" || o.Length <= 0 || !o.ACK || e != nil || len(digest) != 32 {
			return refuse()
		}
		if _, ok := objects[o.Key]; ok {
			return refuse()
		}
		objects[o.Key] = o
	}
	seen := map[string]bool{}
	route := reads[0].Route
	if route != "s3" && route != "volume" {
		return refuse()
	}
	for i, r := range reads {
		o, ok := objects[r.Op.Key]
		if !ok || r.Op.Key != m.Objects[i].Key || r.Route != route || r.Op.Op != i || r.Op.Kind != "s3_get" || !r.Op.Exact || r.Op.Error != "" || r.Status != 200 || r.Length != o.Length || r.SHA256 != o.SHA256 || !finite(r.Op.Started) || !finite(r.Op.Ended) || r.Op.Started < 0 || r.Op.Ended < r.Op.Started || r.Op.Ended-r.Op.Started > deadline {
			return refuse()
		}
		seen[o.Key] = true
	}
	if len(seen) != len(objects) {
		return refuse()
	}
	result.Integrity = "PASS"
	return result, nil
}
