//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReviewerClusterLoadEnvelope(t *testing.T) {
	raw, c, _ := clusterInput(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, e := RunCloudHTTP(ctx, raw, c); e != nil {
		t.Fatal(e)
	}
	files, e := filepath.Glob(filepath.Join(c.Output, "load", "client1", "cloud-step-*.json"))
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, p := range files {
		b, _ := os.ReadFile(p)
		var step map[string]any
		if e := json.Unmarshal(b, &step); e != nil {
			t.Fatal(e)
		}
		command, _ := step["command"].(string)
		if !strings.Contains(command, "cloud_batch.py") {
			continue
		}
		found = true
		t.Logf("actual load command: %s", command)
		if strings.HasPrefix(command, "timeout -k 5 120 ") {
			t.Fatal("32 batches paced at 27 seconds require >=864 seconds, but actual load caller kills at 120 seconds")
		}
	}
	if !found {
		t.Fatal("no actual caller load command captured")
	}
}

func TestActualWorkerPacingFitsDispatchedLoadDeadline(t *testing.T) {
	proof, e := exec.Command("python3", "-I", "cloud_pacing_test.py").CombinedOutput()
	if e != nil || !strings.Contains(string(proof), "'paced_seconds': 864.0") {
		t.Fatalf("actual worker pacing: %s %v", proof, e)
	}
	for _, bound := range []time.Duration{15 * time.Minute, 15 * time.Second} {
		raw, c, _ := clusterInput(t, false)
		ctx, cancel := context.WithTimeout(context.Background(), bound)
		_, e := RunCloudHTTP(ctx, raw, c)
		cancel()
		if e != nil {
			t.Fatal(e)
		}
		files, _ := filepath.Glob(filepath.Join(c.Output, "load", "client1", "cloud-step-*.json"))
		found := false
		for _, p := range files {
			b, _ := os.ReadFile(p)
			var step map[string]any
			_ = json.Unmarshal(b, &step)
			command, _ := step["command"].(string)
			if !strings.Contains(command, "cloud_batch.py") {
				continue
			}
			found = true
			var seconds int
			if _, e = fmt.Sscanf(command, "timeout -k 5 %d", &seconds); e != nil {
				t.Fatal(command)
			}
			if seconds <= 0 || time.Duration(seconds)*time.Second > bound || bound > time.Minute && seconds < 864 {
				t.Fatalf("pacing/deadline mismatch: %s", command)
			}
		}
		if !found {
			t.Fatal("no actual load command")
		}
	}
}

func TestFailureCancelsBeforeOneAllNodeCaptureBudget(t *testing.T) {
	raw, c, _ := clusterInput(t, true, "collect-starvation")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	_, e := RunCloudHTTP(ctx, raw, c)
	files, _ := filepath.Glob(filepath.Join(c.Output, "observed-collect-*"))
	perNode, _ := filepath.Glob(filepath.Join(c.Output, "deploy", "*", "cloud-failure-fetch.json"))
	if e == nil || len(files) != 10 || len(perNode) != 0 || time.Since(start) > 4*time.Second {
		t.Fatalf("failure capture: attempted=%d per-node-budgets=%d elapsed=%s error=%v", len(files), len(perNode), time.Since(start), e)
	}
}

func TestReviewerClusterBeforeBarrier(t *testing.T) {
	for _, variant := range []string{"missing-before-owner", "readonly-before"} {
		t.Run(variant, func(t *testing.T) {
			raw, c, loads := clusterInput(t, false, variant)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, e := RunCloudHTTP(ctx, raw, c)
			if e == nil {
				t.Fatal("invalid before evidence passed final assessor")
			}
			t.Logf("final failure=%v, actual load invocations=%d", e, loads.Load())
			if loads.Load() != 0 {
				t.Fatalf("pre-load owner/writable barrier missing: invoked %d clients", loads.Load())
			}
		})
	}
}

func TestReviewerAllNodeCollectionStartsUnderSharedDeadline(t *testing.T) {
	raw, c, _ := clusterInput(t, false, "collect-starvation")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, e := RunCloudHTTP(ctx, raw, c)
	if e == nil {
		t.Fatal("hanging collection must fail")
	}
	files, err := filepath.Glob(filepath.Join(c.Output, "observed-collect-*"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("collection attempts=%d/10; result=%v", len(files), e)
	if len(files) != 10 {
		t.Fatalf("upload semaphore starved %d node capture attempts", 10-len(files))
	}
}
