//go:build linux

package testrunner

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"time"
)

// Read only the log descriptors created for this owned child, after settlement.
// No submitted pathname is reopened. No partial/truncated export is qualified.
func captureOwnedOutput(stdout, stderr *os.File, deadline time.Time) (r CapturedOutput) {
	r.ReadStart = time.Now()
	defer func() { r.ReadEnd = time.Now() }()
	read := func(f *os.File) (string, string, error) {
		if !deadline.After(time.Now()) {
			return "", "", errors.New("output capture deadline")
		}
		before, err := f.Stat()
		if err != nil {
			return "", "", err
		}
		if !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > MaxCapturedOutputBytes {
			return "", "", errors.New("owned output exceeds capture bound or is not regular")
		}
		data := make([]byte, int(before.Size()))
		if _, err = f.ReadAt(data, 0); err != nil {
			return "", "", err
		}
		after, err := f.Stat()
		if err != nil || !sameRecord(before, after) || !deadline.After(time.Now()) {
			return "", "", errors.New("owned output changed or capture expired")
		}
		return base64.StdEncoding.EncodeToString(data), fmt.Sprintf("%x", sha256.Sum256(data)), nil
	}
	var err error
	r.StdoutBase64, r.StdoutSHA256, err = read(stdout)
	if err == nil {
		r.StderrBase64, r.StderrSHA256, err = read(stderr)
	}
	if err != nil {
		r.StdoutBase64, r.StdoutSHA256, r.StderrBase64, r.StderrSHA256 = "", "", "", ""
		r.Error = err.Error()
		return r
	}
	r.Complete = true
	return r
}
