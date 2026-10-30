package testrunner

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// JobPin authorizes exact bytes and argv. It does not authorize an executable
// name with arbitrary arguments. Generated log paths remain scope-owned.
type JobPin struct {
	Node       string   `json:"node"`
	Executable string   `json:"executable"`
	SHA256     string   `json:"sha256"`
	Argv       []string `json:"argv"`
}

type CleanupGrant struct {
	Kind     string            `json:"kind"`
	Node     string            `json:"node"`
	Resource string            `json:"resource"`
	Before   map[string]string `json:"before"`
	During   map[string]string `json:"during"`
	Apply    JobPin            `json:"apply"`
	Restore  JobPin            `json:"restore"`
	Deadline time.Time         `json:"deadline"`
}

type ApprovedGO struct {
	Bytes         []byte         `json:"-"`
	Version       string         `json:"version"`
	SourceID      string         `json:"source_id"`
	RequestSHA256 string         `json:"request_sha256"`
	Binding       RunBinding     `json:"binding"`
	Cleanup       []CleanupGrant `json:"cleanup"`
	AllowMutating bool           `json:"allow_mutating"`
}

// ReadApprovedGO requires the service's independent pin and protected local
// approval export. A Huly ID or a request-provided manifest is not authority.
// OpenApprovedRun subsequently binds every run input and the existing lease.
func ReadApprovedGO(pin FilePin, cancelFile string, owner uint32) (ApprovedGO, error) {
	var approval ApprovedGO
	absent := func() error {
		if !filepath.IsAbs(cancelFile) || filepath.Clean(cancelFile) != cancelFile {
			return errors.New("explicit canonical cancel path required")
		}
		_, err := os.Lstat(cancelFile)
		if !os.IsNotExist(err) {
			return errors.New("work cancelled or cancel absence unprovable")
		}
		return nil
	}
	if err := absent(); err != nil {
		return approval, err
	}
	raw, err := snapshotRecord(pin.Path, &owner, true)
	if err != nil {
		return approval, err
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != pin.SHA256 {
		return approval, errors.New("approved GO bytes differ from independent pin")
	}
	if err = decodeStrictJSON(raw, &approval); err != nil {
		return approval, err
	}
	id, err := hex.DecodeString(approval.SourceID)
	if err != nil || len(id) != 12 || approval.SourceID != strings.ToLower(approval.SourceID) || approval.Version != "s0b-go-v1" || !validSHA256(approval.RequestSHA256) {
		return approval, errors.New("incomplete approved GO export")
	}
	approval.Bytes = append([]byte(nil), raw...)
	return approval, absent()
}
