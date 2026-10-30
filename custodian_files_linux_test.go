//go:build linux

package testrunner

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestCustodySnapshotProvesImmutableExecutableBytes(t *testing.T) {
	dir := t.TempDir()
	filename := filepath.Join(dir, "executable")
	data := []byte("#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(filename, data, 0700); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(data)
	file, err := snapshotExecutable(filename, hex.EncodeToString(h[:]))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := os.WriteFile(filename, []byte("replacement"), 0700); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(file)
	if err != nil || string(got) != string(data) {
		t.Fatalf("snapshot changed: %q %v", got, err)
	}
	if _, err := file.WriteAt([]byte("x"), 0); err == nil {
		t.Fatal("snapshot was not sealed")
	}
	if _, err := snapshotExecutable(filename, hex.EncodeToString(h[:])); err == nil {
		t.Fatal("wrong bytes were accepted as the requested digest")
	}
}

func TestCustodyLogDescriptorsRejectSymlinkAncestorsAndStayInsideOwnedRoot(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	root := filepath.Join(dir, "run")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if f, err := custodyLogRoot(link); err == nil {
		f.Close()
		t.Fatal("symlink output ancestor accepted")
	}
	f, err := custodyLogRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if log, err := custodyLog(f, root, root+"/escape/out"); err == nil {
		log.Close()
		t.Fatal("log ancestor escaped")
	}
	if err := os.Rename(root, root+"-pinned"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	log, err := custodyLog(f, root, root+"/out")
	if err != nil {
		t.Fatal(err)
	}
	log.Close()
	if _, err := os.Stat(filepath.Join(outside, "out")); !os.IsNotExist(err) {
		t.Fatal("replacement pathname received the log")
	}
	if _, err := os.Stat(root + "-pinned/out"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outside, 0755); err != nil {
		t.Fatal(err)
	}
	if f, err := custodyLogRoot(outside); err == nil {
		f.Close()
		t.Fatal("nonprivate root accepted")
	}
}
