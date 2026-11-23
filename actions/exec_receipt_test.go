package actions

import (
	"encoding/json"
	"errors"
	tr "github.com/pingqiu/sw-test-runner"
	"os"
	"path/filepath"
	"testing"
)

func TestExecReceiptNeverTurnsNoAnswerIntoClean(t *testing.T) {
	path := filepath.Join(t.TempDir(), "closeout.json")
	act := tr.Action{Node: "m01", Params: map[string]string{"receipt_path": path}}
	f, err := beginReceipt(act, "closeout")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	read := func() commandReceipt {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var r commandReceipt
		if err = json.Unmarshal(b, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	if read().Complete {
		t.Fatal("pre-dispatch intent treated as answer")
	}
	if err = writeReceipt(f, "m01", "closeout", "CLOSEOUT CLEAN m01", "", 0, errors.New("SSH answer lost")); err != nil {
		t.Fatal(err)
	}
	if read().Complete {
		t.Fatal("partial CLEAN plus transport error treated as proof")
	}
	if err = writeReceipt(f, "m01", "closeout", "CLOSEOUT CLEAN m01", "", 0, nil); err != nil {
		t.Fatal(err)
	}
	if !read().Complete || read().Code != 0 || read().CommandSHA256 == "" {
		t.Fatal("confirmed answer missing binding")
	}
	if other, err := beginReceipt(act, "different command"); err == nil {
		other.Close()
		t.Fatal("reused receipt path accepted")
	}
}
