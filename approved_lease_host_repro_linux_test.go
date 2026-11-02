//go:build linux

package testrunner

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestHostLedgerRenewRace reproduces the r3/r4 lease-read race on a real filesystem (run it on the lab's CIFS ledger
// mount). It is skipped unless SW_LEDGER_REPRO_DIR names an absolute directory the caller owns. A writer renews a
// throwaway lease exactly as the worker does (temp file, sync, rename, settleWorkerRecord) every
// SW_LEDGER_REPRO_RENEW_S seconds while SW_LEDGER_REPRO_READERS goroutines loop ReadLiveLease for
// SW_LEDGER_REPRO_SECONDS. Any reader error fails the test. Only files named after the throwaway lease are touched.
func TestHostLedgerRenewRace(t *testing.T) {
	dir := os.Getenv("SW_LEDGER_REPRO_DIR")
	if dir == "" {
		t.Skip("set SW_LEDGER_REPRO_DIR to run the host reproduction")
	}
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		t.Fatal("SW_LEDGER_REPRO_DIR must be a clean absolute path")
	}
	num := func(name string, def int) int {
		if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
			return v
		}
		return def
	}
	duration := time.Duration(num("SW_LEDGER_REPRO_SECONDS", 180)) * time.Second
	every := time.Duration(num("SW_LEDGER_REPRO_RENEW_S", 3)) * time.Second
	readers := num("SW_LEDGER_REPRO_READERS", 2)
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	id, owner := hex.EncodeToString(b), "ledger-repro"
	path := func(s string) string { return filepath.Join(dir, id+"-"+s+".json") }
	defer func() {
		for _, s := range []string{"acquire", "renew"} {
			os.Remove(path(s))
		}
		matches, _ := filepath.Glob(filepath.Join(dir, id+"-renew-*.tmp"))
		for _, m := range matches {
			os.Remove(m)
		}
	}()
	stamp := func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	acquire, _ := json.Marshal(map[string]any{"lease_id": id, "agent": owner, "started": stamp()})
	if err := os.WriteFile(path("acquire"), append(acquire, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if err := settleWorkerRecord(path("acquire"), append(acquire, '\n'), time.Now().Add(time.Minute)); err != nil {
		t.Fatal("acquire settle: ", err)
	}
	stop := make(chan struct{})
	var mu sync.Mutex
	renewals, reads, failures := 0, 0, map[string]int{}
	var first string
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // the worker's renew(): temp file + sync + rename, then the writer-side settle
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(every):
			}
			raw, _ := json.Marshal(map[string]any{"lease_id": id, "agent": owner, "renewed": stamp()})
			raw = append(raw, '\n')
			f, err := os.CreateTemp(dir, id+"-renew-*.tmp")
			if err == nil {
				_, w := f.Write(raw)
				s, c := f.Sync(), f.Close()
				if err = errors.Join(w, s, c); err == nil {
					err = os.Rename(f.Name(), path("renew"))
				}
			}
			if err == nil {
				err = settleWorkerRecord(path("renew"), raw, time.Now().Add(time.Minute))
			}
			mu.Lock()
			if err != nil {
				failures["writer: "+err.Error()]++
			} else {
				renewals++
			}
			mu.Unlock()
		}
	}()
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, err := ReadLiveLease(dir, id, owner)
				mu.Lock()
				reads++
				if err != nil {
					failures["reader: "+err.Error()]++
					if first == "" {
						first = time.Now().UTC().Format("15:04:05.000") + " " + err.Error()
					}
				}
				mu.Unlock()
				time.Sleep(5 * time.Millisecond)
			}
		}()
	}
	time.Sleep(duration)
	close(stop)
	wg.Wait()
	keys := make([]string, 0, len(failures))
	for k := range failures {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	report := fmt.Sprintf("LEDGER_REPRO renewals=%d reads=%d readers=%d every=%s duration=%s", renewals, reads, readers, every, duration)
	for _, k := range keys {
		report += fmt.Sprintf("\n  %6d  %s", failures[k], k)
	}
	if first != "" {
		report += "\n  first reader failure: " + first
	}
	t.Log(report)
	if len(failures) > 0 || renewals == 0 {
		t.Fatal("LEDGER_REPRO RED")
	}
	t.Log("LEDGER_REPRO GREEN")
}
