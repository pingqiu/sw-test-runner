package seaweed

import (
	"encoding/json"
	"fmt"
	"os"
)

// publishFile and publishEvidence are exclusive and atomic (temp + link): a
// reader sees the whole file or none. The Go side of soak_load.publish.
func publishFile(path string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return publishEvidence(path, body)
}

func publishEvidence(path string, body []byte) error {
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	defer os.Remove(tmp)
	return os.Link(tmp, path)
}
