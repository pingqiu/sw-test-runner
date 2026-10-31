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

	"golang.org/x/crypto/ssh"
)

func reviewerJournalShell(command string, ch ssh.Channel) uint32 {
	if root := os.Getenv("REVIEW_CAPTURE_ROOT"); root != "" {
		command = strings.ReplaceAll(command, "/data/seaweed-runs/pack-cpu", root)
	}
	cmd := exec.Command("sh", "-c", command)
	cmd.Env = append(os.Environ(), "PATH="+os.Getenv("REVIEW_CAPTURE_BIN")+":/usr/bin:/bin")
	cmd.Stdout = ch
	cmd.Stderr = ch.Stderr()
	if e := cmd.Run(); e != nil {
		if exit, ok := e.(*exec.ExitError); ok {
			return uint32(exit.ExitCode())
		}
		return 99
	}
	return 0
}

func TestApplicableTailFailureStillCollectsRemainingEvidence(t *testing.T) {
	bin, root := t.TempDir(), t.TempDir()
	t.Setenv("REVIEW_CAPTURE_BIN", bin)
	t.Setenv("REVIEW_CAPTURE_ROOT", root)
	if e := os.Mkdir(filepath.Join(root, "worker"), 0700); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"kv_smoke.py", "soak_oracle.py"} {
		if e := os.WriteFile(filepath.Join(root, "worker", name), []byte("fixture only"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	for name, body := range map[string]string{"sudo": "#!/bin/sh\nexit 0\n", "tail": "#!/bin/sh\ncase \"$*\" in *kv_smoke.py*) printf 'tail failed 17\\n' >&2; exit 17;; *) printf 'remaining-tail-collected\\n';; esac\n"} {
		if e := os.WriteFile(filepath.Join(bin, name), []byte(body), 0700); e != nil {
			t.Fatal(e)
		}
	}
	raw, c, _ := clusterInput(t, false, "reviewer-journal-shell")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, e := RunCloudHTTP(ctx, raw, c)
	if e == nil {
		t.Fatal("applicable tail failure passed")
	}
	b, re := os.ReadFile(filepath.Join(c.Output, "collect/meta/cloud-failure-fetch.json"))
	if re != nil {
		t.Fatal(re)
	}
	var step struct {
		RC     int    `json:"exit_code"`
		Error  string `json:"error"`
		Stdout string `json:"stdout"`
		Stderr string `json:"stderr"`
	}
	if json.Unmarshal(b, &step) != nil || step.RC != 17 || step.Error == "" || !strings.Contains(step.Stdout, "remaining-tail-collected") || !strings.Contains(step.Stdout, "MISSING") || !strings.Contains(step.Stderr, "tail failed 17") {
		t.Fatal(string(b))
	}
}

func TestReviewerJournalCaptureFailureMustFailCluster(t *testing.T) {
	for _, code := range []int{0, 23} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			bin := t.TempDir()
			t.Setenv("REVIEW_CAPTURE_BIN", bin)
			sudo := fmt.Sprintf("#!/bin/sh\nprintf 'reviewer journal exit=%d\\n' >&2\nexit %d\n", code, code)
			if e := os.WriteFile(filepath.Join(bin, "sudo"), []byte(sudo), 0700); e != nil {
				t.Fatal(e)
			}
			// Never read real local log contents when the unmodified compound command
			// probes its fixed paths; the only real boundary here is shell exit status.
			if e := os.WriteFile(filepath.Join(bin, "tail"), []byte("#!/bin/sh\nprintf 'fixture tail\\n'\n"), 0700); e != nil {
				t.Fatal(e)
			}
			raw, c, _ := clusterInput(t, false, "reviewer-journal-shell")
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			bundle, e := RunCloudHTTP(ctx, raw, c)
			b, readErr := os.ReadFile(filepath.Join(c.Output, "collect", "meta", "cloud-failure-fetch.json"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			t.Logf("journal subcommand=%d bundle=%s pack_error=%v capture=%s", code, bundle, e, b)
			if code == 0 && e != nil {
				t.Fatal("positive shell capture refused", e)
			}
			if code != 0 && e == nil {
				t.Fatal("cluster falsely passed after journalctl failed; shell loop masked the required capture error")
			}
		})
	}
}

func TestReviewerActualLoadTimeoutAndKillGrace(t *testing.T) {
	for _, bound := range []time.Duration{15 * time.Minute, 15 * time.Second} {
		t.Run(bound.String(), func(t *testing.T) {
			raw, c, _ := clusterInput(t, false)
			ctx, cancel := context.WithTimeout(context.Background(), bound)
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
				b, e := os.ReadFile(p)
				if e != nil {
					t.Fatal(e)
				}
				var step map[string]any
				if e = json.Unmarshal(b, &step); e != nil {
					t.Fatal(e)
				}
				command, _ := step["command"].(string)
				if !strings.Contains(command, "cloud_batch.py") {
					continue
				}
				found = true
				var seconds, grace int
				if _, e = fmt.Sscanf(command, "timeout -k %d %d", &grace, &seconds); e != nil {
					t.Fatal(command)
				}
				t.Logf("work_bound=%s load_timeout=%ds kill_grace=%ds", bound, seconds, grace)
				if grace != 5 || seconds <= 0 || time.Duration(seconds+grace)*time.Second > bound {
					t.Fatal("hard envelope exceeded", command)
				}
				if bound == 15*time.Minute && seconds != 894 {
					t.Fatal("paced budget mismatch", command)
				}
			}
			if !found {
				t.Fatal("actual load command absent")
			}
		})
	}
}
