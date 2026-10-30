package testrunner

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type LeaseObservation struct {
	LeaseID, Owner     string
	Started, Stamp     float64
	ReadStart, ReadEnd time.Time
}

// ReadLiveLease observes the existing broker's ledger; it acquires or renews
// nothing. The broker host is deliberately not interpreted as permitted hosts.
// Release is tested before and after capture, as in the adopted lease_guard.
func ReadLiveLease(dir, id, owner string) (r LeaseObservation, err error) {
	r = LeaseObservation{LeaseID: id, Owner: owner, ReadStart: time.Now()}
	defer func() { r.ReadEnd = time.Now() }()
	decoded, e := hex.DecodeString(id)
	if e != nil || len(decoded) != 16 || id != strings.ToLower(id) || owner == "" || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || dir == "/" {
		return r, errors.New("invalid explicit lease reference")
	}
	release := filepath.Join(dir, id+"-release.json")
	absent := func() error {
		_, err := os.Lstat(release)
		if !os.IsNotExist(err) {
			return errors.New("lease released or release absence unprovable")
		}
		return nil
	}
	if err = absent(); err != nil {
		return r, err
	}
	read := func(suffix string) (map[string]any, error) {
		raw, err := snapshotRecord(filepath.Join(dir, id+suffix+".json"), nil, false)
		if err != nil {
			return nil, err
		}
		var record map[string]any
		if err = decodeStrictJSON(raw, &record); err != nil {
			return nil, err
		}
		if record["lease_id"] != id || record["agent"] != owner {
			return nil, errors.New("foreign lease record")
		}
		return record, nil
	}
	acquire, err := read("-acquire")
	if err != nil {
		return r, fmt.Errorf("acquire: %w", err)
	}
	r.Started, _ = acquire["started"].(float64)
	stamp := acquire["started"]
	_, err = os.Lstat(filepath.Join(dir, id+"-renew.json"))
	if err == nil {
		renew, err := read("-renew")
		if err != nil {
			return r, fmt.Errorf("renewal: %w", err)
		}
		stamp = renew["renewed"]
	} else if !os.IsNotExist(err) {
		return r, errors.New("renewal absence unprovable")
	}
	now := float64(time.Now().UnixNano()) / 1e9
	r.Stamp, _ = stamp.(float64)
	finite := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
	if _, ok := acquire["started"].(float64); !ok || !finite(r.Started) || r.Started <= 0 || r.Started > now {
		return r, errors.New("invalid/future acquire clock")
	}
	if _, ok := stamp.(float64); !ok || !finite(r.Stamp) || r.Stamp > now {
		return r, errors.New("invalid/future lease clock")
	}
	if now-r.Stamp >= 1800 {
		return r, errors.New("lease TTL expired")
	}
	if now-r.Stamp > 600 {
		return r, errors.New("lease renewal cadence missed")
	}
	return r, absent()
}
