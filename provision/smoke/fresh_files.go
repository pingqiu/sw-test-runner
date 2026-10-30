package smoke

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func protectedFreshDir(name string) error {
	if !filepath.IsAbs(name) || filepath.Clean(name) != name {
		return errors.New("noncanonical protected path")
	}
	for {
		i, e := os.Lstat(name)
		if e != nil || !i.IsDir() || i.Mode()&os.ModeSymlink != 0 || i.Mode().Perm()&0022 != 0 || !freshOwner(i) {
			return errors.New("unprotected ancestor")
		}
		if filepath.Dir(name) == name {
			return nil
		}
		name = filepath.Dir(name)
	}
}
func protectedFresh(name string) ([]byte, error) {
	if !filepath.IsAbs(name) || filepath.Clean(name) != name || strings.ContainsRune(name, 0) {
		return nil, errors.New("noncanonical protected input")
	}
	if e := protectedFreshDir(filepath.Dir(name)); e != nil {
		return nil, e
	}
	i, e := os.Lstat(name)
	if e != nil || !i.Mode().IsRegular() || i.Mode().Perm()&0022 != 0 || !freshOwner(i) || i.Size() < 0 || i.Size() > 512<<20 {
		return nil, errors.New("unprotected input")
	}
	f, e := openFreshFile(name)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	j, e := f.Stat()
	if e != nil || !sameFreshRecord(i, j) {
		return nil, errors.New("input replaced")
	}
	b, e := io.ReadAll(io.LimitReader(f, 512<<20+1))
	if e != nil {
		return nil, e
	}
	j, e = f.Stat()
	if e != nil || !sameFreshRecord(i, j) || int64(len(b)) != i.Size() {
		return nil, errors.New("input changed")
	}
	terminal, e := os.Lstat(name)
	if e != nil || !sameFreshRecord(i, terminal) {
		return nil, errors.New("input pathname changed")
	}
	if e = protectedFreshDir(filepath.Dir(name)); e != nil {
		return nil, e
	}
	return b, nil
}
func publishFresh(name string, b []byte) error {
	if !filepath.IsAbs(name) || filepath.Clean(name) != name {
		return errors.New("noncanonical publication")
	}
	if e := protectedFreshDir(filepath.Dir(name)); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(name), ".fresh-publication-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	_, e = f.Write(b)
	if e == nil {
		e = f.Sync()
	}
	e = errors.Join(e, f.Close())
	if e != nil {
		return e
	}
	// Link is an atomic no-replace publication in this same protected directory.
	if e = os.Link(f.Name(), name); e != nil {
		return e
	}
	d, e := os.Open(filepath.Dir(name))
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
