package seaweed

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// A mount client's run root comes back as one gzip'd USTAR stream. Admission never runs tar on those bytes: it reads
// them here, under its own limits, and admits only what the client listed beforehand, byte for byte.

type admitLimits struct {
	Compressed, Expanded, Member int64
	Members                      int
}

var mountAdmitLimits = admitLimits{Compressed: 256 << 20, Expanded: 1 << 30, Member: 512 << 20, Members: 10000}

// mountRequired are the worker's records the assessor rests on.
var mountRequired = []string{"mount-journal.jsonl", "mount-faults.json", "mount-cohorts.json", "mount-env.json", "mount-cleanup.json"}

type listedFile struct {
	Size int64
	SHA  string
}

// mountListing is what the client says its run root holds, before the stream.
type mountListing struct {
	Host, Boot, Root string
	Files            map[string]listedFile
	SHA256           string // of the listing text itself
}

// mountListCommand prints the listing: "identity\t<host>\t<boot>\t<root>", one "file\t<size>\t<sha256>\t<path>" per
// regular file under root except mnt/, "other\t<path>" for anything that is not a regular file or directory, then
// "end\t<count>". Python, so names with any byte are handled; -I, so nothing in the run root is imported.
func mountListCommand(root string) string {
	return "python3 -I -c " + cloudQuote(`import hashlib,os,socket,sys
r=sys.argv[1];os.chdir(r);n=0;o=sys.stdout
o.write('identity\t%s\t%s\t%s\n'%(socket.gethostname(),open('/proc/sys/kernel/random/boot_id').read().strip(),os.getcwd()))
for d,ds,fs in os.walk('.'):
  ds.sort();fs.sort()
  if d=='.' and 'mnt' in ds: ds.remove('mnt')
  for f in fs:
    p=os.path.join(d,f)[2:];st=os.lstat(p)
    if not __import__('stat').S_ISREG(st.st_mode): o.write('other\t%s\n'%p);continue
    h=hashlib.sha256()
    with open(p,'rb') as fh:
      for b in iter(lambda:fh.read(1<<20),b''): h.update(b)
    o.write('file\t%d\t%s\t%s\n'%(st.st_size,h.hexdigest(),p));n+=1
o.write('end\t%d\n'%n)`) + " " + cloudQuote(root)
}

// mountTarCommand streams the run root minus mnt/ as gzip'd USTAR; tar's own errors fail the stream, not its bytes.
func mountTarCommand(root string) string {
	return "cd " + cloudQuote(root) + " && tar --format=ustar --exclude=./mnt -czf - . 2>/dev/null"
}

func parseMountListing(out string) (mountListing, error) {
	sum := sha256.Sum256([]byte(out))
	l := mountListing{Files: map[string]listedFile{}, SHA256: hex.EncodeToString(sum[:])}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) < 2 {
		return l, errors.New("listing too short")
	}
	id := strings.Split(lines[0], "\t")
	if len(id) != 4 || id[0] != "identity" || id[1] == "" || id[2] == "" || id[3] == "" {
		return l, errors.New("listing has no identity line")
	}
	l.Host, l.Boot, l.Root = id[1], id[2], id[3]
	end := strings.Split(lines[len(lines)-1], "\t")
	for _, ln := range lines[1 : len(lines)-1] {
		f := strings.SplitN(ln, "\t", 4)
		switch {
		case len(f) == 2 && f[0] == "other":
			return l, fmt.Errorf("run root holds a non-regular file %q", f[1])
		case len(f) == 4 && f[0] == "file":
			size, e := strconv.ParseInt(f[1], 10, 64)
			if e != nil || size < 0 || len(f[2]) != 64 || !mountName(f[3]) {
				return l, fmt.Errorf("bad listing line %q", ln)
			}
			if _, dup := l.Files[f[3]]; dup {
				return l, fmt.Errorf("listing names %q twice", f[3])
			}
			l.Files[f[3]] = listedFile{Size: size, SHA: f[2]}
		default:
			return l, fmt.Errorf("bad listing line %q", ln)
		}
	}
	if len(end) != 2 || end[0] != "end" || end[1] != strconv.Itoa(len(l.Files)) {
		return l, errors.New("listing has no matching end line")
	}
	return l, nil
}

// mountName: relative, already clean, no .., no backslash, no NUL.
func mountName(n string) bool {
	if n == "" || n == "." || strings.HasPrefix(n, "/") || strings.ContainsAny(n, "\\\x00") || path.Clean(n) != n {
		return false
	}
	for _, e := range strings.Split(n, "/") {
		if e == ".." {
			return false
		}
	}
	return true
}

type limitedReader struct {
	r    io.Reader
	left int64
	what string
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.left <= 0 {
		return 0, fmt.Errorf("%s exceeds its limit", l.what)
	}
	if int64(len(p)) > l.left {
		p = p[:l.left+1] // one byte past the limit is enough to know
	}
	n, e := l.r.Read(p)
	l.left -= int64(n)
	if l.left < 0 {
		return n, fmt.Errorf("%s exceeds its limit", l.what)
	}
	return n, e
}

// admitMountArchive reads the stream into dir (fresh, must not exist), admitting only USTAR regular files and
// directories with safe unique names, within the limits, and returns once every admitted file matches the listing.
func admitMountArchive(r io.Reader, dir string, lim admitLimits, want mountListing) error {
	if e := os.Mkdir(dir, 0700); e != nil {
		return e
	}
	z, e := gzip.NewReader(&limitedReader{r: r, left: lim.Compressed, what: "compressed stream"})
	if e != nil {
		return fmt.Errorf("not a gzip stream: %w", e)
	}
	tr := tar.NewReader(z)
	expanded := &limitedReader{r: tr, left: lim.Expanded, what: "expanded archive"}
	seen, folded, got := map[string]bool{}, map[string]bool{}, map[string]listedFile{}
	for members := 0; ; members++ {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return fmt.Errorf("archive read: %w", e)
		}
		if members >= lim.Members {
			return fmt.Errorf("more than %d members", lim.Members)
		}
		if h.Format != tar.FormatUSTAR || len(h.PAXRecords) != 0 {
			return fmt.Errorf("member %q is not plain USTAR", h.Name)
		}
		name := strings.TrimPrefix(h.Name, "./")
		if h.Typeflag == tar.TypeDir {
			name = strings.TrimSuffix(name, "/")
			if name == "" || name == "." {
				continue // the root entry itself
			}
		}
		if !mountName(name) {
			return fmt.Errorf("unsafe member name %q", h.Name)
		}
		if seen[name] || folded[strings.ToLower(name)] {
			return fmt.Errorf("member %q appears twice", name)
		}
		seen[name], folded[strings.ToLower(name)] = true, true
		target := filepath.Join(dir, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if e = os.MkdirAll(target, 0700); e != nil {
				return e
			}
		case tar.TypeReg:
			if h.Size > lim.Member {
				return fmt.Errorf("member %q is larger than %d bytes", name, lim.Member)
			}
			if e = os.MkdirAll(filepath.Dir(target), 0700); e != nil {
				return e
			}
			f, e := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if e != nil {
				return e
			}
			hash := sha256.New()
			n, e := io.Copy(io.MultiWriter(f, hash), io.LimitReader(expanded, h.Size))
			if c := f.Close(); e == nil {
				e = c
			}
			if e != nil {
				return fmt.Errorf("member %q: %w", name, e)
			}
			if n != h.Size {
				return fmt.Errorf("member %q is truncated", name)
			}
			got[name] = listedFile{Size: n, SHA: hex.EncodeToString(hash.Sum(nil))}
		default:
			return fmt.Errorf("member %q has type %q, not a regular file or directory", name, h.Typeflag)
		}
	}
	for name, f := range want.Files {
		if g, ok := got[name]; !ok || g != f {
			return fmt.Errorf("listed file %q is missing or differs", name)
		}
	}
	for name := range got {
		if _, ok := want.Files[name]; !ok {
			return fmt.Errorf("member %q was not listed", name)
		}
	}
	return nil
}

// checkMountIdentity: the listing came from the expected client boot and root, and the worker's records are there.
func checkMountIdentity(l mountListing, boot, root, dir string) error {
	if l.Boot != boot {
		return fmt.Errorf("boot id changed: %s, recorded %s", l.Boot, boot)
	}
	if l.Root != root {
		return fmt.Errorf("run root %s, expected %s", l.Root, root)
	}
	for _, name := range mountRequired {
		if _, ok := l.Files[name]; !ok {
			return fmt.Errorf("required record %s is missing", name)
		}
	}
	for _, name := range []string{"mount-env.json", "mount-cleanup.json"} {
		b, e := os.ReadFile(filepath.Join(dir, name))
		var v any
		if e != nil || json.Unmarshal(b, &v) != nil {
			return fmt.Errorf("%s does not parse", name)
		}
		if _, isMap := v.(map[string]any); name == "mount-env.json" && !isMap {
			return errors.New("mount-env.json is not an object")
		}
		if s, isStr := v.(string); name == "mount-cleanup.json" && (!isStr || s != "PROVEN" && s != "UNKNOWN") {
			return errors.New("mount-cleanup.json is not PROVEN or UNKNOWN")
		}
	}
	return nil
}
