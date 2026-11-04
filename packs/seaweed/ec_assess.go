package seaweed

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
)

// Shared EC payload, byte assessment and frozen-capture validation for direct runs.
type ecSnapshot struct {
	Corrections    []string           `json:"version_corrections"`
	Superblock     string             `json:"superblock"`
	VifVersions    map[string]int     `json:"vif_versions"`
	Layout         []int64            `json:"layout"`
	Intervals      map[string][]int64 `json:"intervals"`
	Manifest       EC1aManifest       `json:"manifest"`
	OriginalAbsent bool               `json:"original_absent"`
	Holders        []struct {
		Job   string
		PID   int
		Start uint64 `json:"starttime"`
		Boot  string `json:"boot_id"`
	} `json:"holders"`
	Shards []struct {
		Volume      string
		Shard       int
		Job, SHA256 string
		Bytes       int64
		Generation  int64
		IndexSHA256 string `json:"index_sha256"`
		VifSHA256   string `json:"vif_sha256"`
	} `json:"shards"`
	Files []struct {
		Job, Name, SHA256 string
		Bytes             int64
	} `json:"files"`
	UnrelatedVolumes []struct {
		Volume, Job, Name, SHA256 string
		Bytes                     int64
	} `json:"unrelated_volumes"`
}

type ecOutcome struct {
	chaosOp
	Route          string
	Status, Length int
	SHA256         string
	Original       *ecOutcome
}

func ecPayload(run, key string) []byte {
	block := sha256.Sum256([]byte(run + "/" + key))
	return bytes.Repeat(block[:], 8192)
}

type ecReadBody struct {
	RunID    string `json:"run_id"`
	Wrong    int
	Control  string
	Outcomes []ecOutcome
}

func ecAssessRead(body ecReadBody, route string, m EC1aManifest, plant bool) error {
	if body.RunID != m.RunID || (!plant && (body.Wrong != 0 || body.Control != "")) || (plant && (body.Wrong != 1 || body.Control != "WRONG_BYTE_REJECTED")) {
		return errors.New("EC read scope/wrong bytes")
	}
	rows := make([]EC1aRead, len(body.Outcomes))
	for i, v := range body.Outcomes {
		if plant && i == 0 {
			expected := ecPayload(m.RunID, v.Key)
			expected[0] ^= 1
			op := v.chaosOp
			op.Exact = true
			if v.Original == nil || v.Exact || v.SHA256 != hash(expected) || v.Status != v.Original.Status || v.Length != v.Original.Length || v.Route != v.Original.Route || v.Original.chaosOp != op || v.Original.Original != nil {
				return errors.New("EC planted mutation/original join invalid")
			}
			v = *v.Original
		} else if v.Original != nil {
			return errors.New("EC unexpected original-control evidence")
		}
		if v.Route != route {
			return errors.New("EC route mismatch")
		}
		rows[i] = EC1aRead{v.chaosOp, v.Route, v.Status, v.Length, v.SHA256}
	}
	_, err := AssessEC1a(m, rows, 30)
	return err
}

func ecValidateSnapshot(seed EC1aManifest, snap ecSnapshot, services []string, boot string) error {
	if !snap.OriginalAbsent || !reflect.DeepEqual(seed.Objects, snap.Manifest.Objects) || snap.Manifest.RunID != seed.RunID || snap.Manifest.Generation == "" || len(snap.Holders) != 14 || len(snap.Shards) != 14 || len(snap.Files) == 0 {
		return errors.New("EC frozen capture invalid")
	}
	holders := map[string]bool{}
	for _, h := range snap.Holders {
		if holders[h.Job] || h.Job == "master" || h.Job == "filer" || h.Job == "s3" || !contains(services, h.Job) || h.PID <= 0 || h.Start == 0 || h.Boot != boot {
			return errors.New("EC holder capture invalid")
		}
		holders[h.Job] = true
	}
	shardSet := map[string]map[int]bool{}
	for _, file := range snap.UnrelatedVolumes {
		if !holders[file.Job] || file.Volume == "" || file.Bytes < 0 || len(file.SHA256) != 64 || filepath.Base(file.Name) != file.Name {
			return errors.New("EC unrelated volume evidence invalid")
		}
		for _, object := range seed.Objects {
			if strings.Split(object.FID, ",")[0] == file.Volume {
				return errors.New("EC target recorded as unrelated volume")
			}
		}
	}
	shardJobs := map[string]bool{}
	generations := map[string]int64{}
	if json.Unmarshal([]byte(snap.Manifest.Generation), &generations) != nil {
		return errors.New("EC generation map malformed")
	}
	for _, s := range snap.Shards {
		if !holders[s.Job] || shardJobs[s.Job] || s.Shard < 0 || s.Shard >= 14 || len(s.SHA256) != 64 || s.Bytes <= 0 || s.Generation <= 0 || s.Generation != generations[s.Volume] || len(s.IndexSHA256) != 64 || len(s.VifSHA256) != 64 {
			return errors.New("EC shard capture invalid")
		}
		shardJobs[s.Job] = true
		if shardSet[s.Volume] == nil {
			shardSet[s.Volume] = map[int]bool{}
		}
		if shardSet[s.Volume][s.Shard] {
			return errors.New("EC stage1a duplicate holder placement unsupported")
		}
		shardSet[s.Volume][s.Shard] = true
	}
	for _, o := range snap.Manifest.Objects {
		if len(shardSet[strings.Split(o.FID, ",")[0]]) != 14 {
			return errors.New("EC full14 shard set missing")
		}
	}
	return nil
}
