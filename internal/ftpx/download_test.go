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

	"gett/internal/download"
	"gett/internal/errs"

	"github.com/spf13/afero"
)

func readLocal(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDownloadFile(t *testing.T) {
	onTerminal(t)
	d, addr := startServer(t)
	files := seed(t, d)
	dir := t.TempDir()

	if _, err := Download(context.Background(), "ftp://u:p@"+addr+"/up/sub/b.bin", dir+"/"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readLocal(t, filepath.Join(dir, "b.bin")), files["sub/b.bin"]) {
		t.Fatal("content differs")
	}
	if download.Exists(filepath.Join(dir, "b.bin.part")) {
		t.Fatal("part file left behind")
	}
	if n := d.retrs.Load(); n != 1 {
		t.Fatalf("file read %d times, want 1", n)
	}

	// A name of the user's choosing.
	if _, err := Download(context.Background(), "ftp://u:p@"+addr+"/up/a.txt", filepath.Join(dir, "other.txt")); err != nil {
		t.Fatal(err)
	}
	if string(readLocal(t, filepath.Join(dir, "other.txt"))) != "hello" {
		t.Fatal("named download failed")
	}
}

func TestDownloadToStdout(t *testing.T) {
	d, addr := startServer(t)
	seed(t, d)
	dir := t.TempDir()
	var err error
	out := capture(t, func() { _, err = Download(context.Background(), "ftp://u:p@"+addr+"/up/a.txt", dir+"/") })
	if err != nil || out != "hello" {
		t.Fatalf("stdout got %q, %v", out, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatal("piped download wrote a file")
	}
	// A folder cannot go to a pipe.
	capture(t, func() { _, err = Download(context.Background(), "ftp://u:p@"+addr+"/up/", dir+"/") })
	if errs.Code(err) != 2 {
		t.Fatalf("piping a folder: want exit 2, got %d (%v)", errs.Code(err), err)
	}
}

func TestDownloadNeverOverwrites(t *testing.T) {
	onTerminal(t)
	d, addr := startServer(t)
	files := seed(t, d)
	dir := t.TempDir()
	for i := 0; i < 2; i++ {
		if _, err := Download(context.Background(), "ftp://u:p@"+addr+"/up/sub/b.bin", dir+"/"); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"b.bin", "b (1).bin"} {
		if !bytes.Equal(readLocal(t, filepath.Join(dir, name)), files["sub/b.bin"]) {
			t.Fatalf("%s missing or wrong", name)
		}
	}
}

func TestDownloadInterruptResumes(t *testing.T) {
	onTerminal(t)
	d, addr := startServer(t)
	data := make([]byte, 16<<20)
	rand.Read(data)
	if err := afero.WriteFile(d.fs, "/up/big.bin", data, 0o644); err != nil {
		t.Fatal(err)
	}
	d.slow.Store(true)
	dir := t.TempDir()
	url := "ftp://u:p@" + addr + "/up/big.bin"
	part := filepath.Join(dir, "big.bin.part")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := Download(ctx, url, dir+"/"); done <- err }()
	waitFor(t, "download never started", func() bool {
		fi, err := os.Stat(part)
		return err == nil && fi.Size() > 0
	})
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || errs.Code(err) != 130 {
			t.Fatalf("want interrupted (exit 130), got %d %v", errs.Code(err), err)
		}
		if msg := errs.Message(err); !strings.Contains(msg, "Run the same command again") {
			t.Fatalf("an interrupted download is resumable, message %q", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("download kept running after Ctrl-C")
	}
	fi, err := os.Stat(part)
	if err != nil || fi.Size() == 0 || fi.Size() >= int64(len(data)) {
		t.Fatalf("expected a partial file to resume from, got %v", fi)
	}

	d.slow.Store(false)
	if _, err := Download(context.Background(), url, dir+"/"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readLocal(t, filepath.Join(dir, "big.bin")), data) {
		t.Fatal("content differs after resume")
	}
	if download.Exists(part) {
		t.Fatal("part file left behind")
	}
	if n := d.retrs.Load(); n != 2 {
		t.Fatalf("file read %d times, want 2 (the interrupted run and one resume)", n)
	}
}

func TestDownloadDropResumes(t *testing.T) {
	onTerminal(t)
	d, addr := startServer(t)
	data := make([]byte, 8<<20)
	rand.Read(data)
	if err := afero.WriteFile(d.fs, "/up/big.bin", data, 0o644); err != nil {
		t.Fatal(err)
	}
	d.failAt.Store(2 << 20) // the server's disk fails once, 2 MiB in
	dir := t.TempDir()

	if _, err := Download(context.Background(), "ftp://u:p@"+addr+"/up/big.bin", dir+"/"); err != nil {
		t.Fatal(err)
	}
	if d.failures.Load() != 1 {
		t.Fatal("test did not break the transfer off")
	}
	if !bytes.Equal(readLocal(t, filepath.Join(dir, "big.bin")), data) {
		t.Fatal("content differs after reconnecting")
	}
	if n := d.retrs.Load(); n != 2 {
		t.Fatalf("file read %d times, want 2 (the broken transfer and one resume)", n)
	}
}

func TestDownloadFolder(t *testing.T) {
	onTerminal(t)
	d, addr := startServer(t)
	files := seed(t, d)
	dir := t.TempDir()
	url := "ftp://u:p@" + addr + "/up/"

	if _, err := Download(context.Background(), url, dir+"/"); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "up")
	for name, want := range files {
		if got := readLocal(t, filepath.Join(root, filepath.FromSlash(name))); !bytes.Equal(got, want) {
			t.Fatalf("%s differs", name)
		}
	}
	if fi, err := os.Stat(filepath.Join(root, "empty")); err != nil || !fi.IsDir() {
		t.Fatal("empty folder was not created")
	}
	first := d.retrs.Load()

	// Running again fetches nothing: everything is there in full.
	if _, err := Download(context.Background(), url, dir+"/"); err != nil {
		t.Fatal(err)
	}
	if n := d.retrs.Load(); n != first {
		t.Fatalf("second run read %d files, want 0", n-first)
	}

	// A local file with the wrong size is kept; the server's copy lands beside it.
	if err := os.WriteFile(filepath.Join(root, "sub", "b.bin"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Download(context.Background(), url, dir+"/"); err != nil {
		t.Fatal(err)
	}
	if string(readLocal(t, filepath.Join(root, "sub", "b.bin"))) != "stale" {
		t.Fatal("existing file was overwritten")
	}
	if !bytes.Equal(readLocal(t, filepath.Join(root, "sub", "b (1).bin")), files["sub/b.bin"]) {
		t.Fatal("server's copy not saved beside the stale one")
	}
}

func TestDownloadFolderWithoutSlash(t *testing.T) {
	for _, noMLST := range []bool{false, true} {
		onTerminal(t)
		d, addr := startServer(t, func(d *driver) { d.noMLST = noMLST })
		files := seed(t, d)
		dir := t.TempDir()
		if _, err := Download(context.Background(), "ftp://u:p@"+addr+"/up", dir+"/"); err != nil {
			t.Fatalf("noMLST=%v: %v", noMLST, err)
		}
		for name, want := range files {
			if got := readLocal(t, filepath.Join(dir, "up", filepath.FromSlash(name))); !bytes.Equal(got, want) {
				t.Fatalf("noMLST=%v: %s differs", noMLST, name)
			}
		}
	}
}

func TestDownloadMissing(t *testing.T) {
	onTerminal(t)
	d, addr := startServer(t)
	seed(t, d)
	_, err := Download(context.Background(), "ftp://u:p@"+addr+"/up/nope.bin", t.TempDir()+"/")
	if errs.Code(err) != 9 {
		t.Fatalf("want exit 9 for a missing file, got %d (%v)", errs.Code(err), err)
	}
	if strings.Contains(errs.Message(err), "Run the same command again") {
		t.Fatalf("a missing file is not resumable: %q", errs.Message(err))
	}
}
