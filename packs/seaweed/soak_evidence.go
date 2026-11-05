package seaweed

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
)

// m02EvidenceBound is the most the M02 evidence file may hold: the assessor's object bound for it
// (soak_assess.py OBJECT, 4 MiB).
const m02EvidenceBound = 4 << 20

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// m02Evidence returns the M02 evidence file's bytes for publication (#1002). The evidence job keeps its
// collect row in that file, which grows with every rotation past the 128 KiB a job's stdout may carry,
// and prints only a reference to it: status, file, bytes, sha256 and its clock. fetch reads the named file
// from the M02 run root through the controller's existing step path. The bytes must be exactly the
// referenced ones, within the assessor's bound, and one strict SOAK_COLLECTED value.
func m02Evidence(row []byte, file string, fetch func(name string) (string, error)) ([]byte, error) {
	var ref struct {
		Status string       `json:"status"`
		File   string       `json:"file"`
		Bytes  int          `json:"bytes"`
		SHA256 string       `json:"sha256"`
		Clock  *clockSample `json:"clock"`
	}
	dec := json.NewDecoder(bytes.NewReader(row))
	dec.DisallowUnknownFields()
	if oneStrictValue(row) != nil || dec.Decode(&ref) != nil || ref.Status != "SOAK_COLLECTED" || ref.File != file ||
		ref.Bytes <= 0 || ref.Bytes > m02EvidenceBound || !sha256Hex.MatchString(ref.SHA256) || ref.Clock == nil {
		return nil, errors.New("soak M02 evidence reference failed, truncated or malformed")
	}
	if _, err := clockEvidence(row, "check", 1, 2); err != nil { // the S2 sample's own validity
		return nil, err
	}
	got, err := fetch(file)
	if err != nil {
		return nil, err
	}
	body := []byte(got)
	sum := sha256.Sum256(body)
	if len(body) != ref.Bytes || hex.EncodeToString(sum[:]) != ref.SHA256 {
		return nil, errors.New("soak M02 evidence file differs from its reference")
	}
	var probe struct {
		Status string       `json:"status"`
		Clock  *clockSample `json:"clock"`
	}
	if oneStrictValue(body) != nil || json.Unmarshal(body, &probe) != nil || probe.Status != "SOAK_COLLECTED" || probe.Clock == nil {
		return nil, errors.New("soak M02 evidence file malformed")
	}
	if _, err := clockEvidence(body, "check", 1, 2); err != nil {
		return nil, err
	}
	// The file's clock is the one the S2 sample is built from: the same job, the same boot, the same stamps.
	if *probe.Clock != *ref.Clock {
		return nil, errors.New("soak M02 evidence file clock differs from its reference row")
	}
	return body, nil
}
