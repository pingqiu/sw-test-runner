package seaweed

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// publishFile and publishEvidence are exclusive and atomic (temp + link): a
// reader sees the whole file or none. The temp is created O_EXCL under an
// unpredictable name, so a planted file or symlink is never followed or
// truncated; the final link refuses an existing path. The Go side of
// soak_load.publish.
func publishFile(path string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return publishEvidence(path, body)
}

func publishEvidence(path string, body []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Link(f.Name(), path)
}
