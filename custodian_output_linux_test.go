//go:build linux

package testrunner

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"testing"
)

func TestCustodySettledReceiptExportsExactOwnedOutput(t *testing.T) {
	s, _, spec := nativeCustodyScope(t)
	spec.Argv = []string{"s0a-profile-output"}
	spec.CaptureOutput = true
	h, err := s.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	x, err := s.Wait(t.Context(), h)
	if err != nil || !x.Settled || x.Code != 0 {
		t.Fatalf("settlement: %+v %v", x, err)
	}
	if !x.Output.Complete || x.Output.Error != "" {
		t.Fatalf("owned output not exported: %+v", x.Output)
	}
	for _, v := range []struct{ encoded, hash, want string }{
		{x.Output.StdoutBase64, x.Output.StdoutSHA256, "profile-record\n"},
		{x.Output.StderrBase64, x.Output.StderrSHA256, "diagnostic\n"},
	} {
		raw, err := base64.StdEncoding.DecodeString(v.encoded)
		if err != nil || string(raw) != v.want || v.hash != fmt.Sprintf("%x", sha256.Sum256([]byte(v.want))) {
			t.Fatalf("wrong owned bytes: %+v %v", v, err)
		}
	}
}

func TestCustodyOversizedExportRetainsSettlementButNoPartialEvidence(t *testing.T) {
	s, _, spec := nativeCustodyScope(t)
	spec.Argv, spec.CaptureOutput = []string{"s0a-profile-large-output"}, true
	h, err := s.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	x, err := s.Wait(t.Context(), h)
	if err != nil || !x.Settled || x.Code != 0 {
		t.Fatalf("lost proven settlement: %+v %v", x, err)
	}
	if x.Output.Complete || x.Output.Error == "" || x.Output.StdoutBase64 != "" {
		t.Fatalf("partial/oversized bytes qualified: %+v", x.Output)
	}
	if _, _, err := x.Output.Decode(); err == nil {
		t.Fatal("oversized evidence decoded as complete")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("output loss fabricated unknown custody: %v", err)
	}
}

func TestCustodyLegacyJobDoesNotCaptureOutput(t *testing.T) {
	s, _, spec := nativeCustodyScope(t)
	spec.Argv = []string{"s0a-profile-output"}
	h, err := s.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	x, err := s.Wait(t.Context(), h)
	if err != nil || !x.Settled || x.Output != (CapturedOutput{}) {
		t.Fatalf("legacy export behavior changed: %+v %v", x, err)
	}
}
