package testrunner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

type FilePin struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type ApprovedHost struct {
	Node    string `json:"node"`
	Host    string `json:"host"`
	User    string `json:"user"`
	HostKey string `json:"host_key"`
	Root    string `json:"root"`
}

// RunBinding is compared as a complete object against independent GO data.
// The request does not select a verifier, adapter, command or key file.
type RunBinding struct {
	RequestID        string         `json:"request_id"`
	RunID            string         `json:"run_id"`
	Workload         string         `json:"workload"`
	Transport        string         `json:"transport"`
	Inventory        FilePin        `json:"inventory"`
	Scenario         FilePin        `json:"scenario"`
	Policy           FilePin        `json:"policy"`
	Artifacts        FilePin        `json:"artifacts"`
	Hosts            []ApprovedHost `json:"hosts"`
	LeaseID          string         `json:"lease_id"`
	LeaseOwner       string         `json:"lease_owner"`
	LeaseResource    string         `json:"lease_resource"`
	OutputDir        string         `json:"output_dir"`
	WorkDeadline     time.Time      `json:"work_deadline"`
	HardDeadline     time.Time      `json:"hard_deadline"`
	CleanupReserveMS int64          `json:"cleanup_reserve_ms"`
}

type ClaimedRequest struct {
	Version string     `json:"version"`
	Binding RunBinding `json:"binding"`
	Bytes   []byte     `json:"-"`
	SHA256  string     `json:"-"`
}

// ReadClaimedRequest never sources the existing queue's .env file. Its versioned
// S0b body is strict JSON. It returns a stable byte snapshot, not authority.
func ReadClaimedRequest(name, runningDir string, owner uint32) (ClaimedRequest, error) {
	var request ClaimedRequest
	if !filepath.IsAbs(name) || filepath.Clean(name) != name || !filepath.IsAbs(runningDir) || filepath.Clean(runningDir) != runningDir || filepath.Dir(name) != runningDir {
		return request, errors.New("request is not in the exact claimed directory")
	}
	raw, err := snapshotRecord(name, &owner, false)
	if err != nil {
		return request, err
	}
	if err = decodeStrictJSON(raw, &request); err != nil {
		return request, err
	}
	if request.Version != "s0b-request-v1" || request.Binding.RequestID == "" || filepath.Base(name) != request.Binding.RequestID+".env" {
		return request, errors.New("claimed request identity/version mismatch")
	}
	request.Bytes = append([]byte(nil), raw...)
	sum := sha256.Sum256(raw)
	request.SHA256 = hex.EncodeToString(sum[:])
	return request, nil
}

func snapshotRecord(name string, owner *uint32, protected bool) ([]byte, error) {
	if !filepath.IsAbs(name) || filepath.Clean(name) != name || strings.ContainsRune(name, 0) {
		return nil, errors.New("noncanonical record path")
	}
	for dir := filepath.Dir(name); ; dir = filepath.Dir(dir) {
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("record ancestor refused: %s", dir)
		}
		if protected && info.Mode().Perm()&0022 != 0 {
			return nil, errors.New("trusted record ancestor is writable")
		}
		if protected && owner != nil {
			uid, err := recordUID(info)
			if err != nil || (uid != 0 && uid != *owner) {
				return nil, errors.New("foreign trusted-record ancestor")
			}
		}
		if dir == filepath.Dir(dir) {
			break
		}
	}
	before, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > 1<<20 || before.Size() < 0 {
		return nil, errors.New("record is not a bounded regular file")
	}
	if protected && before.Mode().Perm()&0022 != 0 {
		return nil, errors.New("trusted record is writable")
	}
	if owner != nil {
		uid, err := recordUID(before)
		if err != nil || (uid != *owner && !(protected && uid == 0)) {
			return nil, errors.New("record owner mismatch")
		}
	}
	file, err := openRecordFile(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, errors.New("record replaced while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	after, err := os.Lstat(name)
	if err != nil || len(raw) > 1<<20 || !sameRecord(before, after) {
		return nil, errors.New("record changed during capture")
	}
	return raw, nil
}

func decodeStrictJSON(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	if err := uniqueJSON(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	if err := exactJSONFields(raw, reflect.TypeOf(out).Elem()); err != nil {
		return err
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(out)
}

func uniqueJSON(d *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("JSON nesting exceeds bound")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	if delim, ok := token.(json.Delim); ok {
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("duplicate or malformed JSON member")
				}
				seen[name] = true
				if err := uniqueJSON(d, depth+1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := uniqueJSON(d, depth+1); err != nil {
					return err
				}
			}
		default:
			return errors.New("unexpected JSON delimiter")
		}
		_, err = d.Token()
		return err
	}
	return nil
}

// encoding/json accepts case-insensitive aliases. Authority records do not:
// Version/version must not become a second last-key-wins spelling.
func exactJSONFields(raw []byte, typ reflect.Type) error {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == reflect.TypeFor[time.Time]() {
		return nil
	}
	switch typ.Kind() {
	case reflect.Struct:
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return err
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			tag := strings.Split(f.Tag.Get("json"), ",")[0]
			if tag == "-" {
				continue
			}
			if tag == "" {
				tag = f.Name
			}
			fields[tag] = f.Type
		}
		for name, value := range object {
			field, ok := fields[name]
			if !ok {
				return fmt.Errorf("unknown JSON member %q", name)
			}
			if err := exactJSONFields(value, field); err != nil {
				return err
			}
		}
	case reflect.Slice:
		var array []json.RawMessage
		if err := json.Unmarshal(raw, &array); err != nil {
			return err
		}
		for _, v := range array {
			if err := exactJSONFields(v, typ.Elem()); err != nil {
				return err
			}
		}
	case reflect.Map:
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return err
		}
		for _, v := range object {
			if err := exactJSONFields(v, typ.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}
