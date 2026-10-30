package testrunner

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

// Decode verifies completeness and byte pins without changing settlement state.
func (o CapturedOutput) Decode() (stdout, stderr []byte, err error) {
	if !o.Complete || o.Error != "" || o.ReadStart.IsZero() || o.ReadEnd.Before(o.ReadStart) {
		return nil, nil, errors.New("owned output missing, incomplete or invalid capture bracket: " + o.Error)
	}
	decode := func(encoded, digest string) ([]byte, error) {
		if len(encoded) > base64.StdEncoding.EncodedLen(MaxCapturedOutputBytes) {
			return nil, errors.New("owned output exceeds capture bound")
		}
		data, err := base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil || len(data) > MaxCapturedOutputBytes || fmt.Sprintf("%x", sha256.Sum256(data)) != digest {
			return nil, errors.New("owned output byte pin mismatch")
		}
		return data, nil
	}
	stdout, err = decode(o.StdoutBase64, o.StdoutSHA256)
	if err == nil {
		stderr, err = decode(o.StderrBase64, o.StderrSHA256)
	}
	if err != nil {
		return nil, nil, err
	}
	return stdout, stderr, nil
}
