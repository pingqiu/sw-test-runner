package seaweed

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path"
	"strings"

	tr "github.com/pingqiu/sw-test-runner"
)

// PayloadManifest describes expanded installed product/native bytes. Its exact
// UTF-8 JSON bytes (inside Plan.Payload) are hashed before provisioning. Node is
// a stable inventory ID, never an address or an observed host identity.
type PayloadManifest struct {
	Version string        `json:"version"`
	Files   []PayloadFile `json:"files"`
}
type PayloadFile struct {
	Node   string `json:"node"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func bindPayload(raw string, in tr.PackInputs) (string, error) {
	fail := func() (string, error) { return "", errors.New("payload manifest/readback file-set mismatch") }
	if len(raw) == 0 || len(raw) > 128<<10 {
		return fail()
	}
	scan := json.NewDecoder(strings.NewReader(raw))
	if err := uniqueJSON(scan, 0); err != nil {
		return fail()
	}
	if _, err := scan.Token(); err != io.EOF {
		return fail()
	}
	var p PayloadManifest
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&p) != nil || p.Version != "seaweed.payload/v1" || len(p.Files) == 0 || len(p.Files) > 4096 {
		return fail()
	}
	expected := 0
	for node, files := range in.PayloadFiles {
		for name, digest := range files {
			if in.Files[node][name] != digest {
				return fail()
			}
			expected++
		}
	}
	if expected != len(p.Files) {
		return fail()
	}
	seen := map[string]bool{}
	for _, f := range p.Files {
		key := f.Node + "\x00" + f.Path
		digest, err := hex.DecodeString(f.SHA256)
		if f.Node == "" || strings.ContainsAny(f.Node, "\x00\r\n") || !path.IsAbs(f.Path) || path.Clean(f.Path) != f.Path || len(f.Path) > 4096 || strings.ContainsAny(f.Path, "\x00\r\n") || seen[key] || err != nil || len(digest) != 32 || strings.ToLower(f.SHA256) != f.SHA256 || in.PayloadFiles[f.Node][f.Path] != f.SHA256 || in.Files[f.Node][f.Path] != f.SHA256 {
			return fail()
		}
		seen[key] = true
	}
	return hash([]byte(raw)), nil
}
