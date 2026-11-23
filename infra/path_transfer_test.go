package infra

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"golang.org/x/crypto/ssh"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPathTransferCanceledBeforeDispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	node := commandSSHFixture(t)
	src := filepath.Join(t.TempDir(), "packet.tgz")
	if err := os.WriteFile(src, []byte("packet"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := node.UploadContext(ctx, src, "/unused/packet.tgz"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "ledger")
	if err := node.DownloadContext(ctx, "/unused/ledger", dst); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("canceled download created destination: %v", err)
	}
}

// The SSH fixture echoes bytes instead of returning the protocol receipt: a
// transport exchange alone must never claim upload or evidence completion.
func TestPathTransferRejectsUnframedBytes(t *testing.T) {
	node := commandSSHFixture(t)
	src := filepath.Join(t.TempDir(), "packet.tgz")
	if err := os.WriteFile(src, []byte("packet"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := node.UploadContext(ctx, src, "/unused/packet.tgz"); err == nil {
		t.Fatal("echo accepted as upload proof")
	}
}

func TestPathTransferStreamsLargeEvidence(t *testing.T) {
	data := bytes.Repeat([]byte("ledger-row\n"), 300000)
	sum := fmt.Sprintf("%x", sha256.Sum256(data))
	for _, upload := range []bool{true, false} {
		t.Run(fmt.Sprint(upload), func(t *testing.T) {
			node := commandSSHFixtureResponder(t, nil, nil, func(_ string, ch ssh.Channel) {
				if upload {
					got, err := io.ReadAll(ch)
					if err != nil || !bytes.Equal(got, data) {
						t.Errorf("upload bytes=%d err=%v", len(got), err)
					}
					fmt.Fprintf(ch, "UPLOADED %s %d\n", sum, len(data))
				} else {
					fmt.Fprintf(ch, "%d\n", len(data))
					ch.Write(data)
					fmt.Fprintf(ch, "SHA256 %s\n", sum)
				}
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			local := filepath.Join(t.TempDir(), "ledger.tsv")
			if upload {
				os.WriteFile(local, data, 0600)
				if err := node.UploadContext(ctx, local, "/run/ledger.tsv"); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := node.DownloadContext(ctx, "/run/ledger.tsv", local); err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(local)
				if err != nil || !bytes.Equal(got, data) {
					t.Fatalf("download bytes=%d err=%v", len(got), err)
				}
			}
		})
	}
}
func TestDownloadDoesNotPublishPartialEvidence(t *testing.T) {
	node := commandSSHFixtureResponder(t, nil, nil, func(_ string, ch ssh.Channel) { fmt.Fprint(ch, "100\npartial") })
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	dst := filepath.Join(t.TempDir(), "ledger.tsv")
	if err := node.DownloadContext(ctx, "/run/ledger.tsv", dst); err == nil {
		t.Fatal("partial evidence accepted")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("partial destination published: %v", err)
	}
}

func TestUploadCancellationInterruptsInflightSSH(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	node := commandSSHFixtureResponder(t, nil, nil, func(_ string, ch ssh.Channel) {
		first := make([]byte, 1)
		if _, err := io.ReadFull(ch, first); err != nil {
			t.Errorf("first byte: %v", err)
			return
		}
		cancel()
		io.Copy(io.Discard, ch)
	})
	src := filepath.Join(t.TempDir(), "packet.tgz")
	if err := os.WriteFile(src, bytes.Repeat([]byte("x"), 8<<20), 0600); err != nil {
		t.Fatal(err)
	}
	if err := node.UploadContext(ctx, src, "/run/packet.tgz"); !errors.Is(err, context.Canceled) {
		t.Fatalf("want caller cancellation, got %v", err)
	}
}
