package testrunner

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestS0bRefusesLegacySSHInputsBeforeAuthorityOrJobs(t *testing.T) {
	for _, file := range []string{"scripts/run-block-ci.sh", "scenarios/internal/rdma-rust-volume-rc.yaml"} {
		t.Run(file, func(t *testing.T) {
			body, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			input := S0bExecutionInputs{Scenario: []byte("name: approved")}
			if strings.HasSuffix(file, ".yaml") {
				input.Scenario = body
			} else {
				input.Scripts = map[string][]byte{file: body}
			}
			verified := false
			backend := &fakeOwnedNode{ids: map[string]ProcessIdentity{}, specs: map[string]JobSpec{}}
			scope, err := NewS0bRunScope(t.Context(), testAdmission(), backend, func(context.Context, RunAdmission) error { verified = true; return nil }, input)
			if scope != nil {
				scope.Close()
			}
			if err == nil || verified || len(backend.ids) != 0 || backend.authorized != 0 {
				t.Fatalf("legacy SSH input admitted: err=%v verified=%t jobs=%d", err, verified, backend.authorized)
			}
		})
	}
}

func TestS0bSSHInputGuardDistinguishesOptionsCommandsAndPublicKeys(t *testing.T) {
	for _, body := range []string{
		"option: StrictHostKeyChecking=no",
		"option: stricthostkeychecking = 'No'",
		"exec: ssh host true",
		"exec: $(/usr/bin/ssh -o StrictHostKeyChecking=yes host true)",
		"exec: \"ssh\" host true",
	} {
		t.Run(body, func(t *testing.T) {
			backend := &fakeOwnedNode{ids: map[string]ProcessIdentity{}, specs: map[string]JobSpec{}}
			scope, err := NewS0bRunScope(t.Context(), testAdmission(), backend, func(context.Context, RunAdmission) error { return nil }, S0bExecutionInputs{Scenario: []byte(body)})
			if scope != nil {
				scope.Close()
			}
			if err == nil || backend.authorized != 0 {
				t.Fatal("unsafe SSH content admitted")
			}
		})
	}
	backend := &fakeOwnedNode{ids: map[string]ProcessIdentity{}, specs: map[string]JobSpec{}}
	input := S0bExecutionInputs{Scenario: []byte("host_key: ssh-ed25519 AAAA-approved-pin"), Scripts: map[string][]byte{"worker.sh": []byte("printf 'local work'")}}
	scope, err := NewS0bRunScope(t.Context(), testAdmission(), backend, func(context.Context, RunAdmission) error { return nil }, input)
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	if _, err := scope.Start(JobSpec{Node: "fake", Executable: "/bin/true", SHA256: strings.Repeat("a", 64)}); err != nil {
		t.Fatal(err)
	}
	if backend.authorized != 1 {
		t.Fatal("safe pinned input did not reach ordinary custody")
	}
}
