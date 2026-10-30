package testrunner

import (
	"strings"
	"testing"
	"time"
)

func TestCapturedOutputRefusesAlteredOrIncompleteReceipt(t *testing.T) {
	now := time.Now()
	valid := CapturedOutput{Complete: true, StdoutBase64: "e30K", StdoutSHA256: "ca3d163bab055381827226140568f3bef7eaac187cebd76878e0b63e9e442356", StderrSHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", ReadStart: now, ReadEnd: now}
	// Literal byte pin for "{}\n", shared with the actual strict-entry fixture.
	if _, _, err := valid.Decode(); err != nil {
		t.Fatal(err)
	}
	for _, which := range []string{"hash", "base64", "partial", "oversize", "bracket"} {
		t.Run(which, func(t *testing.T) {
			row := valid
			switch which {
			case "hash":
				row.StdoutSHA256 = strings.Repeat("0", 64)
			case "base64":
				row.StdoutBase64 = "!!!"
			case "partial":
				row.Complete = false
			case "oversize":
				row.StdoutBase64 = strings.Repeat("A", 180000)
			case "bracket":
				row.ReadEnd = now.Add(-time.Second)
			}
			a, b, err := row.Decode()
			if err == nil || a != nil || b != nil {
				t.Fatal("unqualified bytes escaped")
			}
		})
	}
}
