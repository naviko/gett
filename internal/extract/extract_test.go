package extract

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gett/internal/errs"
	"gett/internal/term"
)

func setup(t *testing.T) string {
	t.Helper()
	tty := true
	term.StdoutIsTTY = &tty
	term.Silent = true
	t.Cleanup(func() { term.StdoutIsTTY = nil; term.Silent = false })
	return t.TempDir()
}

type entry struct {
	name, body, link string
	dir              bool
}

func writeZip(t *testing.T, p string, entries []entry) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate, Modified: time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)}
		switch {
		case e.dir:
			h.SetMode(0o755 | os.ModeDir)
		case e.link != "":
			h.SetMode(0o777 | os.ModeSymlink)
		default:
			h.SetMode(0o644)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if e.link != "" {
			w.Write([]byte(e.link))
		} else {
			w.Write([]byte(e.body))
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeTarGz(t *testing.T, p string, entries []entry) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), ModTime: time.Now()}
		switch {
		case e.dir:
			h.Typeflag, h.Mode, h.Size = tar.TypeDir, 0o755, 0
		case e.link != "":
			h.Typeflag, h.Linkname, h.Size = tar.TypeSymlink, e.link, 0
		default:
			h.Typeflag = tar.TypeReg
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	gz.Close()
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return string(b)
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestZipSingleTopFolderFlattens(t *testing.T) {
	dir := setup(t)
	z := filepath.Join(dir, "src-1.2.zip")
	writeZip(t, z, []entry{
		{name: "src/", dir: true},
		{name: "src/main.go", body: "package main"},
		{name: "src/lib/", dir: true},
		{name: "src/lib/x.go", body: "package lib"},
	})
	if err := Run(context.Background(), z, ""); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(dir, "src", "lib", "x.go")) != "package lib" {
		t.Fatal("content wrong")
	}
	if exists(filepath.Join(dir, "src", "src")) || exists(filepath.Join(dir, "src-1.2")) {
		t.Fatal("single top folder should become the result itself")
	}
	if exists(filepath.Join(dir, ".src-1.2.extracting")) {
		t.Fatal("staging folder left behind")
	}
	if !exists(z) {
		t.Fatal("archive was deleted")
	}
	fi, _ := os.Stat(filepath.Join(dir, "src", "main.go"))
	if fi.ModTime().Year() != 2020 {
		t.Fatalf("mtime not kept: %v", fi.ModTime())
	}

	// Again: the result is numbered, nothing overwritten.
	if err := Run(context.Background(), z, ""); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(dir, "src (1)", "main.go")) {
		t.Fatal("second run should land in 'src (1)'")
	}
}

func TestTarGzLooseFilesNamedAfterArchive(t *testing.T) {
	dir := setup(t)
	tgz := filepath.Join(dir, "gett-1.0.8.tar.gz")
	writeTarGz(t, tgz, []entry{
		{name: "README", body: "hi"},
		{name: "bin/", dir: true},
		{name: "bin/run", body: "#!/bin/sh"},
	})
	if err := Run(context.Background(), tgz, ""); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(dir, "gett-1.0.8", "bin", "run")) != "#!/bin/sh" {
		t.Fatal("content wrong")
	}
	if err := Run(context.Background(), tgz, ""); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(dir, "gett-1.0.8 (1)", "README")) {
		t.Fatal("folder numbering must not treat .8 as an extension")
	}
}

func TestIntoDestKeepsLayoutAndNumbersCollisions(t *testing.T) {
	dir := setup(t)
	z := filepath.Join(dir, "game.zip")
	writeZip(t, z, []entry{{name: "game/", dir: true}, {name: "game/a.txt", body: "a"}, {name: "notes.txt", body: "n"}})
	dest := filepath.Join(dir, "games")
	os.MkdirAll(dest, 0o755)
	os.WriteFile(filepath.Join(dest, "notes.txt"), []byte("mine"), 0o644)

	if err := Run(context.Background(), z, dest); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(dest, "game", "a.txt")) != "a" {
		t.Fatal("layout not kept")
	}
	if read(t, filepath.Join(dest, "notes.txt")) != "mine" {
		t.Fatal("existing file overwritten")
	}
	if read(t, filepath.Join(dest, "notes (1).txt")) != "n" {
		t.Fatal("colliding file not numbered")
	}
}

func TestUnsafeEntriesSkipped(t *testing.T) {
	dir := setup(t)
	z := filepath.Join(dir, "evil.zip")
	writeZip(t, z, []entry{
		{name: "ok.txt", body: "fine"},
		{name: "../escape.txt", body: "bad"},
		{name: "/etc/passwd", body: "bad"},
		{name: "C:\\Windows\\x", body: "bad"},
		{name: "root", link: "/"},
		{name: "up", link: "../../x"},
		{name: "sub/", dir: true},
		{name: "sub/inside", link: "../ok.txt"},
	})
	if err := Run(context.Background(), z, ""); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "evil")
	if read(t, filepath.Join(out, "ok.txt")) != "fine" {
		t.Fatal("good entry missing")
	}
	for _, p := range []string{filepath.Join(dir, "escape.txt"), filepath.Join(out, "root"), filepath.Join(out, "up")} {
		if exists(p) {
			t.Fatalf("%s should not exist", p)
		}
	}
	// An absolute path loses its leading slash and lands inside, as tar does.
	if read(t, filepath.Join(out, "etc", "passwd")) != "bad" {
		t.Fatal("absolute entry should be unpacked inside the folder")
	}
	if target, err := os.Readlink(filepath.Join(out, "sub", "inside")); err != nil || target != filepath.FromSlash("../ok.txt") {
		t.Fatalf("a link that stays inside should be created: %q %v", target, err)
	}
}

func TestNotAnArchiveIsNotAnError(t *testing.T) {
	dir := setup(t)
	p := filepath.Join(dir, "README.md")
	os.WriteFile(p, []byte("# hello\n"), 0o644)
	if err := Run(context.Background(), p, ""); err != nil {
		t.Fatalf("plain file: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("nothing should be created: %v", entries)
	}
}

func TestDamagedArchive(t *testing.T) {
	dir := setup(t)
	tgz := filepath.Join(dir, "broken.tar.gz")
	writeTarGz(t, tgz, []entry{{name: "a.txt", body: strings.Repeat("x", 100000)}, {name: "b.txt", body: "b"}})
	b, _ := os.ReadFile(tgz)
	os.WriteFile(tgz, b[:len(b)/2], 0o644)
	err := Run(context.Background(), tgz, "")
	if errs.Code(err) != 2 || !errors.As(err, new(*errs.Archive)) || !strings.Contains(err.Error(), "left as is") {
		t.Fatalf("want *errs.Archive, got %v", err)
	}
	if exists(filepath.Join(dir, "broken")) || exists(filepath.Join(dir, ".broken.extracting")) {
		t.Fatal("a damaged archive must leave nothing behind")
	}
}

func TestGzipSingleFile(t *testing.T) {
	dir := setup(t)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write([]byte("log line\n"))
	gz.Close()
	p := filepath.Join(dir, "app.log.gz")
	os.WriteFile(p, buf.Bytes(), 0o644)
	if err := Run(context.Background(), p, ""); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(dir, "app.log")) != "log line\n" {
		t.Fatal("inflated content wrong")
	}
	if err := Run(context.Background(), p, ""); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(dir, "app (1).log")) {
		t.Fatal("second run should be numbered")
	}
}

func TestCancelLeavesNothing(t *testing.T) {
	dir := setup(t)
	z := filepath.Join(dir, "big.zip")
	var entries []entry
	for i := 0; i < 50; i++ {
		entries = append(entries, entry{name: "f" + string(rune('a'+i%26)) + string(rune('a'+i/26)), body: strings.Repeat("y", 200000)})
	}
	writeZip(t, z, entries)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, z, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled, got %v", err)
	}
	if exists(filepath.Join(dir, "big")) || exists(filepath.Join(dir, ".big.extracting")) {
		t.Fatal("interrupted run left files behind")
	}
}

func TestZipManyFilesParallel(t *testing.T) {
	dir := setup(t)
	z := filepath.Join(dir, "many.zip")
	var entries []entry
	want := map[string]string{}
	for i := 0; i < 600; i++ {
		name := fmt.Sprintf("d%d/f%d.txt", i%40, i)
		body := strings.Repeat(fmt.Sprintf("line %d of file %d\n", i%7, i), 50)
		entries = append(entries, entry{name: name, body: body})
		want[name] = body
	}
	for i := 0; i < 2; i++ {
		name := fmt.Sprintf("big%d.bin", i)
		body := strings.Repeat(string(rune('A'+i)), 3<<20)
		entries = append(entries, entry{name: name, body: body})
		want[name] = body
	}
	writeZip(t, z, entries)
	if err := Run(context.Background(), z, ""); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "many")
	for name, body := range want {
		if read(t, filepath.Join(out, filepath.FromSlash(name))) != body {
			t.Fatalf("%s differs", name)
		}
	}
	if exists(filepath.Join(dir, ".many.extracting")) {
		t.Fatal("staging folder left behind")
	}
}

func TestDamagedZipEntry(t *testing.T) {
	dir := setup(t)
	z := filepath.Join(dir, "bad.zip")
	var entries []entry
	seed := uint32(7)
	for i := 0; i < 20; i++ {
		body := make([]byte, 100000) // hard to compress, so f0's data is long
		for j := range body {
			seed = seed*1664525 + 1013904223
			body[j] = byte(seed >> 24)
		}
		entries = append(entries, entry{name: fmt.Sprintf("f%d.txt", i), body: string(body)})
	}
	writeZip(t, z, entries)
	// Corrupt the compressed body of the first entry (right after its 30-byte
	// local header and the name) without touching the central directory, so
	// the archive still opens and the read of that entry fails its checksum.
	b, _ := os.ReadFile(z)
	for i := 100; i < 5000; i++ {
		b[i] ^= 0xff
	}
	os.WriteFile(z, b, 0o644)
	err := Run(context.Background(), z, "")
	if !errors.As(err, new(*errs.Archive)) || !strings.Contains(err.Error(), "f0.txt") {
		t.Fatalf("want *errs.Archive naming f0.txt, got %v", err)
	}
	if exists(filepath.Join(dir, "bad")) || exists(filepath.Join(dir, ".bad.extracting")) {
		t.Fatal("a damaged archive must leave nothing behind")
	}
}

func TestTarHardLink(t *testing.T) {
	dir := setup(t)
	tgz := filepath.Join(dir, "links.tar.gz")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "a.txt", Mode: 0o644, Size: 5, Typeflag: tar.TypeReg})
	tw.Write([]byte("hello"))
	tw.WriteHeader(&tar.Header{Name: "b.txt", Mode: 0o644, Typeflag: tar.TypeLink, Linkname: "a.txt"})
	tw.WriteHeader(&tar.Header{Name: "c.txt", Mode: 0o644, Typeflag: tar.TypeLink, Linkname: "../outside"})
	tw.Close()
	gz.Close()
	os.WriteFile(tgz, buf.Bytes(), 0o644)
	if err := Run(context.Background(), tgz, ""); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "links")
	if read(t, filepath.Join(out, "b.txt")) != "hello" {
		t.Fatal("hard link not created")
	}
	if exists(filepath.Join(out, "c.txt")) {
		t.Fatal("hard link outside the folder should be skipped")
	}
}
