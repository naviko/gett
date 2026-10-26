package ftpx

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gett/internal/errs"

	"github.com/spf13/afero"
)

func TestUploadSkipResume(t *testing.T) {
	d, addr := startServer(t)
	dir, files := makeTree(t)
	url := "ftp://u:p@" + addr + "/up"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := Upload(ctx, url, dir); err != nil {
		t.Fatal(err)
	}
	for name, want := range files {
		got, err := afero.ReadFile(d.fs, "/up/"+name)
		if err != nil {
			t.Fatalf("%s missing on server: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s differs on server", name)
		}
	}
	first := d.stores.Load()
	if first < int64(len(files)) {
		t.Fatalf("expected at least %d stores, got %d", len(files), first)
	}

	// Second run: everything already there, nothing stored again.
	if err := Upload(ctx, url, dir); err != nil {
		t.Fatal(err)
	}
	if d.stores.Load() != first {
		t.Fatalf("second run stored %d files, expected 0", d.stores.Load()-first)
	}

	// Truncate a remote file: the upload must complete it.
	half := files["sub/b.bin"][:1<<20]
	if err := afero.WriteFile(d.fs, "/up/sub/b.bin", half, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Upload(ctx, url, dir); err != nil {
		t.Fatal(err)
	}
	got, _ := afero.ReadFile(d.fs, "/up/sub/b.bin")
	if !bytes.Equal(got, files["sub/b.bin"]) {
		t.Fatal("truncated remote file was not completed correctly")
	}

	// Single file into the directory.
	if err := Upload(ctx, url+"/single", filepath.Join(dir, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if got, _ := afero.ReadFile(d.fs, "/up/single/a.txt"); string(got) != "hello" {
		t.Fatal("single file upload failed")
	}
}

func TestLoginDenied(t *testing.T) {
	_, addr := startServer(t)
	dir, _ := makeTree(t)
	err := Upload(context.Background(), "ftp://u:wrong@"+addr+"/up", dir)
	if errs.Code(err) != 67 {
		t.Fatalf("expected exit 67, got %d (%v)", errs.Code(err), err)
	}
}

func bigFile(t *testing.T, dir string, n int) []byte {
	t.Helper()
	data := make([]byte, n)
	rand.Read(data)
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestInterruptStopsUpload(t *testing.T) {
	d, addr := startServer(t)
	d.slow.Store(true)
	dir := t.TempDir()
	data := bigFile(t, dir, 16<<20)
	url := "ftp://u:p@" + addr + "/up"

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Upload(ctx, url, dir) }()
	waitFor(t, "upload never started", func() bool {
		fi, err := d.fs.Stat("/up/big.bin")
		return err == nil && fi.Size() > 0
	})
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || errs.Code(err) != 130 {
			t.Fatalf("want interrupted (exit 130), got %d %v", errs.Code(err), err)
		}
		if msg := errs.Message(err); !strings.Contains(msg, "Run the same command again") {
			t.Fatalf("an interrupted upload is resumable, message %q", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("upload kept running after Ctrl-C")
	}
	if fi, err := d.fs.Stat("/up/big.bin"); err == nil && fi.Size() >= int64(len(data)) {
		t.Fatal("upload ran to completion despite being interrupted")
	}

	// Running again completes the file.
	d.slow.Store(false)
	if err := Upload(context.Background(), url, dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := afero.ReadFile(d.fs, "/up/big.bin"); !bytes.Equal(got, data) {
		t.Fatal("content differs after resume")
	}
}

func TestInterruptDuringResumeKeepsRemote(t *testing.T) {
	d, addr := startServer(t)
	d.slow.Store(true)
	dir := t.TempDir()
	data := bigFile(t, dir, 16<<20)
	// An earlier run left the first MiB on the server.
	if err := afero.WriteFile(d.fs, "/up/big.bin", data[:1<<20], 0o644); err != nil {
		t.Fatal(err)
	}
	url := "ftp://u:p@" + addr + "/up"

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Upload(ctx, url, dir) }()
	waitFor(t, "upload never resumed", func() bool {
		fi, err := d.fs.Stat("/up/big.bin")
		return err == nil && fi.Size() > 3<<20
	})
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want interrupted, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("upload kept running after Ctrl-C")
	}
	fi, err := d.fs.Stat("/up/big.bin")
	if err != nil || fi.Size() < 3<<20 {
		t.Fatalf("Ctrl-C during a resumed transfer wiped the server's copy: %v", fi)
	}

	d.slow.Store(false)
	if err := Upload(context.Background(), url, dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := afero.ReadFile(d.fs, "/up/big.bin"); !bytes.Equal(got, data) {
		t.Fatal("content differs after resume")
	}
}

func TestDropDuringTransferResumes(t *testing.T) {
	d, addr := startServer(t)
	dir := t.TempDir()
	data := bigFile(t, dir, 8<<20)
	// The server's disk fails once, 2 MiB into a resumed transfer: the first
	// MiB is already there, so a naive client would start the file over.
	if err := afero.WriteFile(d.fs, "/up/big.bin", data[:1<<20], 0o644); err != nil {
		t.Fatal(err)
	}
	d.failAt.Store(2 << 20)
	url := "ftp://u:p@" + addr + "/up"

	if err := Upload(context.Background(), url, dir); err != nil {
		t.Fatal(err)
	}
	if d.failures.Load() != 1 {
		t.Fatal("test did not break the transfer off")
	}
	if got, _ := afero.ReadFile(d.fs, "/up/big.bin"); !bytes.Equal(got, data) {
		t.Fatal("content differs after reconnecting")
	}
	if n := d.stores.Load(); n != 2 {
		t.Fatalf("file was opened for writing %d times, want 2 (the broken transfer and one resume)", n)
	}
}
