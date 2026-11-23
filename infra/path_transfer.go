package infra

import (
	"bufio"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// UploadContext uploads one regular file/archive to a fresh destination.
// The dedicated command connection is closed on cancellation, including I/O.
func (n *Node) UploadContext(ctx context.Context, local, remote string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok := ctx.Deadline(); !ok {
		return fmt.Errorf("transfer: deadline required")
	}
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("upload: regular file required")
	}
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return err
	}
	digest := fmt.Sprintf("%x", h.Sum(nil))
	if _, err = f.Seek(0, 0); err != nil {
		return err
	}
	// Exclusive create; failed/partial files remain in the run-owned staging
	// root for host cleanup. A retry must use a new path, never overwrite it.
	script := `import sys,hashlib
p,want,size=sys.argv[1:]; h=hashlib.sha256(); count=0
with open(p,'xb') as f:
 while True:
  b=sys.stdin.buffer.read(1048576)
  if not b: break
  count+=len(b); f.write(b); h.update(b)
 f.flush()
assert count==int(size) and h.hexdigest()==want
print('UPLOADED '+want+' '+str(count))`
	pipe, err := n.OpenCommand(ctx, "python3", "-I", "-c", script, remote, digest, fmt.Sprint(st.Size()))
	if err != nil {
		return err
	}
	defer pipe.Close()
	stop := context.AfterFunc(ctx, func() { pipe.Close() })
	defer stop()
	_, err = io.Copy(pipe, f)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	closeErr := pipe.WriteCloser.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("upload bytes: %w", err)
	}
	receipt, err := io.ReadAll(io.LimitReader(pipe.Reader, 4096))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(receipt)) != fmt.Sprintf("UPLOADED %s %d", digest, st.Size()) {
		return fmt.Errorf("upload: missing hash/size receipt")
	}
	return nil
}

// DownloadContext streams a regular file with a size header and digest trailer.
// Only a complete, verified transfer renames the local temporary file.
func (n *Node) DownloadContext(ctx context.Context, remote, local string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok := ctx.Deadline(); !ok {
		return fmt.Errorf("transfer: deadline required")
	}
	script := `import sys,os,stat,hashlib
with open(sys.argv[1],'rb') as f:
 s=os.fstat(f.fileno()); assert stat.S_ISREG(s.st_mode)
 out=sys.stdout.buffer; out.write((str(s.st_size)+'\n').encode()); out.flush(); h=hashlib.sha256()
 while True:
  b=f.read(1048576)
  if not b: break
  out.write(b); h.update(b)
 out.write(('SHA256 '+h.hexdigest()+'\n').encode()); out.flush()`
	pipe, err := n.OpenCommand(ctx, "python3", "-I", "-c", script, remote)
	if err != nil {
		return err
	}
	defer pipe.Close()
	stop := context.AfterFunc(ctx, func() { pipe.Close() })
	defer stop()
	pipe.WriteCloser.Close()
	reader := bufio.NewReaderSize(pipe.Reader, 32)
	lineBytes, err := reader.ReadSlice('\n')
	line := string(lineBytes)
	if err != nil || len(line) > 32 {
		return fmt.Errorf("download: missing size header")
	}
	var size int64
	if _, err = fmt.Sscanf(line, "%d\n", &size); err != nil || size < 0 {
		return fmt.Errorf("download: invalid size")
	}
	f, err := os.CreateTemp(filepath.Dir(local), ".transfer-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	h := sha256.New()
	if _, err = io.CopyN(io.MultiWriter(f, h), reader, size); err != nil {
		return fmt.Errorf("download bytes: %w", err)
	}
	trailer, err := io.ReadAll(io.LimitReader(reader, 4096))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(trailer)) != "SHA256 "+fmt.Sprintf("%x", h.Sum(nil)) {
		return fmt.Errorf("download: missing digest trailer")
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), local)
}
