package seaweed

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type member struct {
	name string
	body []byte
	typ  byte
	link string
}

func records() []member {
	return []member{{name: "./", typ: tar.TypeDir},
		{name: "./mount-journal.jsonl", body: []byte(`{"t":"end"}` + "\n")}, {name: "./mount-faults.json", body: []byte(`{}`)},
		{name: "./mount-cohorts.json", body: []byte(`{}`)}, {name: "./mount-env.json", body: []byte(`{"fuse_ok":true}`)},
		{name: "./mount-cleanup.json", body: []byte(`"PROVEN"`)}, {name: "./logs/", typ: tar.TypeDir},
		{name: "./logs/mount-m0.log", body: []byte("mounted\n")}}
}

func archive(t *testing.T, ms []member, format tar.Format) []byte {
	t.Helper()
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	w := tar.NewWriter(z)
	for _, m := range ms {
		typ := m.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: m.name, Typeflag: typ, Mode: 0644, Size: int64(len(m.body)), Linkname: m.link, Format: format}
		if typ != tar.TypeReg {
			h.Size = 0
		}
		if e := w.WriteHeader(h); e != nil {
			t.Fatal(e)
		}
		if typ == tar.TypeReg {
			w.Write(m.body)
		}
	}
	w.Close()
	z.Close()
	return b.Bytes()
}

// paxArchive writes the members with the given PAX records on the last regular file.
func paxArchive(t *testing.T, ms []member, pax map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	w := tar.NewWriter(z)
	for i, m := range ms {
		h := &tar.Header{Name: m.name, Typeflag: tar.TypeDir, Mode: 0755}
		if m.typ == 0 {
			h = &tar.Header{Name: m.name, Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(m.body))}
			if i == len(ms)-1 {
				h.PAXRecords, h.Format = pax, tar.FormatPAX
			}
		}
		if e := w.WriteHeader(h); e != nil {
			t.Fatal(e)
		}
		w.Write(m.body)
	}
	w.Close()
	z.Close()
	return b.Bytes()
}

// listingFor is the listing a client would print for these members.
func listingFor(ms []member, boot, root string) string {
	var lines []string
	for _, m := range ms {
		if m.typ == 0 || m.typ == tar.TypeReg {
			s := sha256.Sum256(m.body)
			lines = append(lines, fmt.Sprintf("file\t%d\t%s\t%s", len(m.body), hex.EncodeToString(s[:]), strings.TrimPrefix(m.name, "./")))
		}
	}
	sort.Strings(lines)
	return "identity\tclient1\t" + boot + "\t" + root + "\n" + strings.Join(lines, "\n") + fmt.Sprintf("\nend\t%d\n", len(lines))
}

const testBoot, testRoot = "11111111-2222-3333-4444-555555555555", "/data/seaweed-runs/r/mountsoak"

func admit(t *testing.T, raw []byte, listing string, lim admitLimits) (string, error) {
	t.Helper()
	l, e := parseMountListing(listing)
	if e != nil {
		return "", e
	}
	dir := filepath.Join(t.TempDir(), "client1")
	if e = admitMountArchive(bytes.NewReader(raw), dir, lim, l); e != nil {
		return dir, e
	}
	return dir, checkMountIdentity(l, testBoot, testRoot, dir)
}

func TestMountArchiveAdmitsAListedRunRoot(t *testing.T) {
	ms := records()
	dir, e := admit(t, archive(t, ms, tar.FormatUSTAR), listingFor(ms, testBoot, testRoot), mountAdmitLimits)
	if e != nil {
		t.Fatal(e)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "logs", "mount-m0.log")); string(b) != "mounted\n" {
		t.Fatalf("content not admitted: %q", b)
	}
}

func TestMountArchiveRefusesEveryUnsafeOrUnboundStream(t *testing.T) {
	small := admitLimits{Compressed: 1 << 20, Expanded: 1 << 20, Member: 1 << 19, Members: 20}
	zeros := func(n int) []byte { return make([]byte, n) }
	base := records()
	with := func(extra ...member) []member { return append(append([]member{}, base...), extra...) }
	good := archive(t, base, tar.FormatUSTAR)
	bomb := with(member{name: "./a", body: zeros(400 << 10)}, member{name: "./b", body: zeros(400 << 10)}, member{name: "./c", body: zeros(400 << 10)})
	cases := []struct {
		name    string
		raw     []byte
		listing string
		lim     admitLimits
		want    string
	}{
		{"small compressed, huge expanded", archive(t, bomb, tar.FormatUSTAR), listingFor(bomb, testBoot, testRoot), small, "expanded archive exceeds"},
		{"one member over its cap", archive(t, with(member{name: "./big", body: zeros(600 << 10)}), tar.FormatUSTAR),
			listingFor(with(member{name: "./big", body: zeros(600 << 10)}), testBoot, testRoot), small, "larger than"},
		{"too many members", good, listingFor(base, testBoot, testRoot), admitLimits{Compressed: 1 << 20, Expanded: 1 << 20, Member: 1 << 19, Members: 3}, "more than 3 members"},
		{"compressed over its cap", good, listingFor(base, testBoot, testRoot), admitLimits{Compressed: 64, Expanded: 1 << 20, Member: 1 << 19, Members: 20}, "compressed stream exceeds"},
		{"symlink", archive(t, with(member{name: "./l", typ: tar.TypeSymlink, link: "/etc/passwd"}), tar.FormatUSTAR), listingFor(base, testBoot, testRoot), mountAdmitLimits, "not a regular file"},
		{"hard link", archive(t, with(member{name: "./h", typ: tar.TypeLink, link: "mount-env.json"}), tar.FormatUSTAR), listingFor(base, testBoot, testRoot), mountAdmitLimits, "not a regular file"},
		{"device", archive(t, with(member{name: "./d", typ: tar.TypeChar}), tar.FormatUSTAR), listingFor(base, testBoot, testRoot), mountAdmitLimits, "not a regular file"},
		{"dot-dot", archive(t, with(member{name: "../x", body: []byte("x")}), tar.FormatUSTAR), listingFor(base, testBoot, testRoot), mountAdmitLimits, "unsafe member name"},
		{"absolute", archive(t, with(member{name: "/x", body: []byte("x")}), tar.FormatUSTAR), listingFor(base, testBoot, testRoot), mountAdmitLimits, "unsafe member name"},
		{"unclean", archive(t, with(member{name: "./a//b", body: []byte("x")}), tar.FormatUSTAR), listingFor(base, testBoot, testRoot), mountAdmitLimits, "unsafe member name"},
		{"backslash", archive(t, with(member{name: "./a\\b", body: []byte("x")}), tar.FormatUSTAR), listingFor(base, testBoot, testRoot), mountAdmitLimits, "unsafe member name"},
		{"duplicate", archive(t, with(member{name: "./mount-env.json", body: []byte(`{}`)}), tar.FormatUSTAR), listingFor(base, testBoot, testRoot), mountAdmitLimits, "appears twice"},
		{"case-fold duplicate", archive(t, with(member{name: "./MOUNT-ENV.JSON", body: []byte(`{}`)}), tar.FormatUSTAR), listingFor(base, testBoot, testRoot), mountAdmitLimits, "appears twice"},
		{"PAX record", paxArchive(t, base, map[string]string{"comment": "x"}), listingFor(base, testBoot, testRoot), mountAdmitLimits, "not plain USTAR"},
		{"name needing a PAX extension", archive(t, with(member{name: "./" + strings.Repeat("a", 120), body: []byte("x")}), tar.FormatUnknown), listingFor(with(member{name: "./" + strings.Repeat("a", 120), body: []byte("x")}), testBoot, testRoot), mountAdmitLimits, "not plain USTAR"},
		{"GNU header", archive(t, base, tar.FormatGNU), listingFor(base, testBoot, testRoot), mountAdmitLimits, "not plain USTAR"},
		{"member not listed", archive(t, with(member{name: "./extra", body: []byte("x")}), tar.FormatUSTAR), listingFor(base, testBoot, testRoot), mountAdmitLimits, "was not listed"},
		{"listed file absent", good, listingFor(with(member{name: "./gone", body: []byte("x")}), testBoot, testRoot), mountAdmitLimits, "missing or differs"},
		{"truncated stream", good[:len(good)/2], listingFor(base, testBoot, testRoot), mountAdmitLimits, "archive read"},
		{"not gzip", []byte("plain text"), listingFor(base, testBoot, testRoot), mountAdmitLimits, "not a gzip stream"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, e := admit(t, c.raw, c.listing, c.lim)
			if e == nil || c.want != "" && !strings.Contains(e.Error(), c.want) {
				t.Fatalf("want refusal %q, got %v", c.want, e)
			}
		})
	}
}

// After tar's end only zero padding may remain in the gzip member: listed-looking content there is refused.
func TestMountArchiveRefusesNonZeroBytesAfterTheEnd(t *testing.T) {
	ms := records()
	g, e := gzip.NewReader(bytes.NewReader(archive(t, ms, tar.FormatUSTAR)))
	if e != nil {
		t.Fatal(e)
	}
	plain, _ := io.ReadAll(g)
	for name, tail := range map[string][]byte{"text": []byte("UNLISTED-NONZERO-DATA"), "one byte": {1}, "zeros": make([]byte, 512)} {
		t.Run(name, func(t *testing.T) {
			var b bytes.Buffer
			z := gzip.NewWriter(&b)
			z.Write(append(append([]byte{}, plain...), tail...))
			z.Close()
			_, e := admit(t, b.Bytes(), listingFor(ms, testBoot, testRoot), mountAdmitLimits)
			if name == "zeros" && e != nil || name != "zeros" && (e == nil || !strings.Contains(e.Error(), "non-zero data")) {
				t.Fatalf("%s: %v", name, e)
			}
		})
	}
}

func TestMountArchiveHashAndSizeAreBound(t *testing.T) {
	ms := records()
	raw := archive(t, ms, tar.FormatUSTAR)
	for _, mutate := range []func(string) string{
		func(s string) string { // the listed hash of one file is wrong
			s2 := sha256.Sum256([]byte(`"PROVEN"`))
			return strings.Replace(s, hex.EncodeToString(s2[:]), strings.Repeat("0", 64), 1)
		},
		func(s string) string { return strings.Replace(s, "file\t8\t", "file\t9\t", 1) }, // a listed size is wrong
	} {
		if _, e := admit(t, raw, mutate(listingFor(ms, testBoot, testRoot)), mountAdmitLimits); e == nil || !strings.Contains(e.Error(), "missing or differs") {
			t.Fatalf("a wrong listed hash or size was admitted: %v", e)
		}
	}
}

func TestMountIdentityAndRecordsAreRequired(t *testing.T) {
	ms := records()
	cases := map[string]struct {
		ms      []member
		boot    string
		root    string
		listing func([]member) string
		want    string
	}{
		"boot changed":   {ms, testBoot, testRoot, func(m []member) string { return listingFor(m, "99999999-0000-0000-0000-000000000000", testRoot) }, "boot id changed"},
		"wrong root":     {ms, testBoot, testRoot, func(m []member) string { return listingFor(m, testBoot, "/tmp/elsewhere") }, "run root"},
		"record missing": {ms[:len(ms)-3], testBoot, testRoot, func(m []member) string { return listingFor(m, testBoot, testRoot) }, "required record"},
		"env not an object": {append(append([]member{}, ms[:4]...), member{name: "./mount-env.json", body: []byte(`[1]`)}, ms[5], ms[6], ms[7]), testBoot, testRoot,
			func(m []member) string { return listingFor(m, testBoot, testRoot) }, "mount-env.json is not an object"},
		"cleanup not a state": {append(append([]member{}, ms[:5]...), member{name: "./mount-cleanup.json", body: []byte(`"MAYBE"`)}, ms[6], ms[7]), testBoot, testRoot,
			func(m []member) string { return listingFor(m, testBoot, testRoot) }, "not PROVEN or UNKNOWN"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, e := admit(t, archive(t, c.ms, tar.FormatUSTAR), c.listing(c.ms), mountAdmitLimits); e == nil || !strings.Contains(e.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, e)
			}
		})
	}
}

func TestMountListingRefusesNonRegularAndMalformed(t *testing.T) {
	for _, bad := range []string{
		"",
		"identity\th\tb\t/r\nother\tlink\nend\t0\n",
		"identity\th\tb\t/r\nfile\t1\tshort\tx\nend\t1\n",
		"identity\th\tb\t/r\nfile\t1\t" + strings.Repeat("a", 64) + "\t../x\nend\t1\n",
		"identity\th\tb\t/r\nfile\t1\t" + strings.Repeat("a", 64) + "\tx\nend\t2\n",
		"identity\th\tb\t/r\nfile\t1\t" + strings.Repeat("a", 64) + "\tx\nfile\t1\t" + strings.Repeat("a", 64) + "\tx\nend\t2\n",
		"nobody\nend\t0\n",
	} {
		if _, e := parseMountListing(bad); e == nil {
			t.Fatalf("listing admitted: %q", bad)
		}
	}
}

func TestMountArchiveNeverOverwritesAnExistingDirectory(t *testing.T) {
	ms := records()
	l, _ := parseMountListing(listingFor(ms, testBoot, testRoot))
	dir := t.TempDir() // exists already
	if e := admitMountArchive(bytes.NewReader(archive(t, ms, tar.FormatUSTAR)), dir, mountAdmitLimits, l); e == nil {
		t.Fatal("admitted into an existing directory")
	}
}

var _ = io.EOF
