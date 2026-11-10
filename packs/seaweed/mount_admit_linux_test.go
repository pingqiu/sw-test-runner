//go:build linux

package seaweed

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The exact remote commands, run here with the host's own python3 and GNU tar, produce a stream admission accepts:
// GNU tar --format=ustar is read as plain USTAR, mnt/ is left out, and the listing binds every byte.
func TestMountRealListingAndTarAreAdmitted(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mountsoak")
	for name, body := range map[string]string{
		"mount-journal.jsonl": `{"t":"end"}` + "\n", "mount-faults.json": "{}", "mount-cohorts.json": "{}",
		"mount-env.json": `{"fuse_ok": true}`, "mount-cleanup.json": `"PROVEN"`, "mount-m0.log": "a log line\n",
	} {
		if e := os.MkdirAll(root, 0700); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); e != nil {
			t.Fatal(e)
		}
	}
	if e := os.MkdirAll(filepath.Join(root, "mnt", "m0"), 0700); e != nil { // unmounted mount points stay behind
		t.Fatal(e)
	}
	run := func(cmd string) []byte {
		out, e := exec.Command("bash", "-c", cmd).Output()
		if e != nil {
			t.Fatalf("%s: %v", cmd, e)
		}
		return out
	}
	l, e := parseMountListing(string(run(mountListCommand(root))))
	if e != nil {
		t.Fatal(e)
	}
	if len(l.Files) != 6 || l.Root != root {
		t.Fatalf("listing: %d files, root %s", len(l.Files), l.Root)
	}
	boot, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	dir := filepath.Join(t.TempDir(), "client1")
	if e = admitMountArchive(bytes.NewReader(run(mountTarCommand(root))), dir, mountAdmitLimits, l); e != nil {
		t.Fatal(e)
	}
	if e = checkMountIdentity(l, strings.TrimSpace(string(boot)), root, dir); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(dir, "mnt")); !os.IsNotExist(e) {
		t.Fatal("mnt/ came back in the stream")
	}
	// A symlink in the run root makes the listing refuse, before any stream.
	if e = os.Symlink("/etc/passwd", filepath.Join(root, "link")); e != nil {
		t.Fatal(e)
	}
	if _, e = parseMountListing(string(run(mountListCommand(root)))); e == nil || !strings.Contains(e.Error(), "non-regular") {
		t.Fatalf("a symlink in the run root was listed: %v", e)
	}
}
