//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"time"
)

func readBound(path string, v any) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return errors.New("soak: not a regular file: " + filepath.Base(path))
	}
	body, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return err
	}
	if len(body) > 4096 {
		return errors.New("soak: file over 4096 bytes: " + filepath.Base(path))
	}
	return json.Unmarshal(body, v)
}

func pollBound(ctx context.Context, path string, v any) error {
	for {
		err := readBound(path, v)
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
