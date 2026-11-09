//go:build linux

package seaweed

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

// fakeMountSteps answers the two direct-path jobs with fixed results.
func fakeMountSteps(t *testing.T, loadErr error, assessOut string, assessCode int, assessErr error) (func(context.Context, string, int) (tr.ExitInfo, error), *[]string) {
	t.Helper()
	var calls []string
	return func(_ context.Context, label string, _ int) (tr.ExitInfo, error) {
		calls = append(calls, label)
		if label == "mount-soak" {
			return tr.ExitInfo{}, loadErr
		}
		return exitInfo(assessOut, assessCode, time.Now()), assessErr
	}, &calls
}

const validFailVerdict = `{"schema":"seaweed-mount-verdict/1","status":"FAIL","cleanup":"PROVEN"}`

func TestMountCasePublishesAFailVerdictAndReportsIt(t *testing.T) {
	dir := t.TempDir()
	step, _ := fakeMountSteps(t, nil, validFailVerdict, 0, nil)
	rows, err := runMountCase(context.Background(), step, dir, MountSoakCase)
	if err == nil || !strings.Contains(err.Error(), "verdict FAIL") {
		t.Fatalf("err %v", err)
	}
	body, readErr := os.ReadFile(filepath.Join(dir, "mount-verdict.json"))
	if readErr != nil || rows != filepath.Join(dir, "mount-verdict.json") || string(body) != validFailVerdict {
		t.Fatalf("rows %q body %q err %v", rows, body, readErr)
	}
}

func TestMountCasePublishesAVerdictFromANonzeroExitStep(t *testing.T) {
	dir := t.TempDir()
	step, _ := fakeMountSteps(t, nil, validFailVerdict, 1, errors.New("step exited 1"))
	if _, err := runMountCase(context.Background(), step, dir, MountSoakCase); err == nil {
		t.Fatal("a failed step must still be reported")
	}
	if _, err := os.Stat(filepath.Join(dir, "mount-verdict.json")); err != nil {
		t.Fatalf("the completed verdict was dropped: %v", err)
	}
}

func TestMountCaseAssessesEvenWhenTheLoadFailed(t *testing.T) {
	dir := t.TempDir()
	invalid := `{"schema":"seaweed-mount-verdict/1","status":"INVALID","cleanup":"UNKNOWN"}`
	step, calls := fakeMountSteps(t, errors.New("load exited 1"), invalid, 0, nil)
	_, err := runMountCase(context.Background(), step, dir, MountSoakCase)
	if len(*calls) != 2 || err == nil || !strings.Contains(err.Error(), "load exited 1") || !strings.Contains(err.Error(), "verdict INVALID (cleanup UNKNOWN)") {
		t.Fatalf("calls %v err %v", *calls, err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "mount-verdict.json")); statErr != nil {
		t.Fatal(statErr)
	}
}

func TestMountCasePassIsNoError(t *testing.T) {
	step, _ := fakeMountSteps(t, nil, `{"schema":"seaweed-mount-verdict/1","status":"PASS","cleanup":"PROVEN"}`, 0, nil)
	if _, err := runMountCase(context.Background(), step, t.TempDir(), MountSoakCase); err != nil {
		t.Fatal(err)
	}
}

func TestMountSmokeAcceptsAnyVerdictWithProvenCleanup(t *testing.T) {
	for _, status := range []string{"PASS", "FAIL", "INVALID", "ENVIRONMENT"} {
		step, _ := fakeMountSteps(t, nil, `{"schema":"seaweed-mount-verdict/1","status":"`+status+`","cleanup":"PROVEN"}`, 0, nil)
		if _, err := runMountCase(context.Background(), step, t.TempDir(), MountSoakSmokeCase); err != nil {
			t.Fatalf("%s: %v", status, err)
		}
	}
}

func TestMountSmokeFailsOnUnprovenCleanupOrAFailedLoad(t *testing.T) {
	step, _ := fakeMountSteps(t, nil, `{"schema":"seaweed-mount-verdict/1","status":"PASS","cleanup":"UNKNOWN"}`, 0, nil)
	if _, err := runMountCase(context.Background(), step, t.TempDir(), MountSoakSmokeCase); err == nil {
		t.Fatal("unproven cleanup accepted")
	}
	step, _ = fakeMountSteps(t, errors.New("load exited 1"), `{"schema":"seaweed-mount-verdict/1","status":"FAIL","cleanup":"PROVEN"}`, 0, nil)
	if _, err := runMountCase(context.Background(), step, t.TempDir(), MountSoakSmokeCase); err == nil {
		t.Fatal("a failed load accepted")
	}
}

func TestMountCaseRefusesForeignAssessorOutput(t *testing.T) {
	dir := t.TempDir()
	step, _ := fakeMountSteps(t, nil, `not a verdict`, 0, nil)
	if _, err := runMountCase(context.Background(), step, dir, MountSoakCase); err == nil {
		t.Fatal("foreign output accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "mount-verdict.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("foreign output was published")
	}
}
