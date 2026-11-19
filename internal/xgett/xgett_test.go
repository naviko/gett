package xgett

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"gett/internal/term"
)

func setup(t testing.TB) string {
	t.Helper()
	tty := true
	term.StdoutIsTTY = &tty
	term.Silent = true
	t.Cleanup(func() { term.StdoutIsTTY = nil; term.Silent = false })
	return t.TempDir()
}

// smallBlocks makes new archives use 4 KiB blocks, so a few kilobytes of
// test data lie across many of them.
func smallBlocks(t testing.TB) {
	t.Helper()
	old := blockLog
	blockLog = minBlockLog
	t.Cleanup(func() { blockLog = old })
}

func put(t testing.TB, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// text is n bytes that pack well, noise n bytes that do not pack at all.
func text(seed, n int) string {
	return strings.Repeat(fmt.Sprintf("line %d of the file\n", seed), n/10+1)[:n]
}

func noise(seed int64, n int) string {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return string(b)
}

// unpacked opens an archive and reads everything in it: files by path,
// folders as "path/", links as "-> target".
func unpacked(t testing.TB, path, key string) (map[string]string, *Archive, error) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	a, err := Open(f, filepath.Base(path), key)
	if err != nil {
		return nil, nil, err
	}
	defer a.Close()
	bodies := make([][]byte, len(a.Entries))
	for i, e := range a.Entries {
		if e.Kind == File {
			bodies[i] = make([]byte, e.Size)
		}
	}
	// Pieces never overlap, so the goroutines need no lock between them.
	err = a.Each(context.Background(), func(i int, off int64, p []byte) error {
		copy(bodies[i][off:], p)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	got := map[string]string{}
	for i, e := range a.Entries {
		switch e.Kind {
		case File:
			got[e.Path] = string(bodies[i])
		case Dir:
			got[e.Path+"/"] = ""
		case Link:
			got[e.Path] = "-> " + e.Target
		}
	}
	return got, a, nil
}

func same(t testing.TB, got, want map[string]string) {
	t.Helper()
	for name, body := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("%s is missing", name)
		} else if g != body {
			t.Errorf("%s: got %d bytes, want %d, or the bytes differ", name, len(g), len(body))
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("%s should not be there", name)
		}
	}
}

func TestLabels(t *testing.T) {
	for file, want := range map[string]string{
		"My Report 2024.xgett": "MYREPORT2024",
		"report.pdf.xgett":     "REPORTPDF",
		"photos (1).xgett":     "PHOTOS1",
		"photos.bin":           "PHOTOS",
		"---.xgett":            "XGETT",
		".xgett":               "XGETT",
		"写真.xgett":             "写真",
		"café.xgett":           "CAFÉ",
		"café.xgett":     "CAFÉ", // the same text in another Unicode form
	} {
		if got := label(file); got != want {
			t.Errorf("label(%q) = %q, want %q", file, got, want)
		}
	}
	for file, want := range map[string][]string{
		"photos (1).xgett": {"PHOTOS1", "PHOTOS"},
		"a (2) (1).xgett":  {"A21", "A2", "A"},
		"photos.xgett":     {"PHOTOS"},
	} {
		if got := labels(file); !reflect.DeepEqual(got, want) {
			t.Errorf("labels(%q) = %q, want %q", file, got, want)
		}
	}
	for word, want := range map[string][]string{
		"report.pdf": {"REPORT", "REPORTPDF"},
		"PHOTOS":     {"PHOTOS"},
	} {
		if got := wordLabels(word); !reflect.DeepEqual(got, want) {
			t.Errorf("wordLabels(%q) = %q, want %q", word, got, want)
		}
	}
}

func TestIndexRoundTrip(t *testing.T) {
	when := time.Date(2024, 5, 6, 7, 8, 9, 123456789, time.UTC)
	in := []Entry{
		{Kind: Dir, Path: "top"},
		{Kind: File, Path: "top/a.txt", Size: 5, Mtime: when},
		{Kind: File, Path: "top/run", Size: 1 << 40, Exec: true},
		{Kind: File, Path: "top/empty"},
		{Kind: Link, Path: "top/link", Target: "a.txt"},
	}
	b := encodeIndex(in)
	out, err := decodeIndex(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(in) {
		t.Fatalf("got %d entries, want %d", len(out), len(in))
	}
	for i := range in {
		g, w := out[i], in[i]
		if g.Kind != w.Kind || g.Path != w.Path || g.Size != w.Size || g.Exec != w.Exec || g.Target != w.Target || !g.Mtime.Equal(w.Mtime) {
			t.Errorf("entry %d: got %+v, want %+v", i, g, w)
		}
	}
	for n := 0; n < len(b); n++ {
		if _, err := decodeIndex(b[:n]); err == nil {
			t.Fatalf("an index cut to %d of %d bytes was accepted", n, len(b))
		}
	}
	if _, err := decodeIndex(append(b, 0)); err == nil {
		t.Fatal("an index with a byte too many was accepted")
	}
}

func TestRoundTrip(t *testing.T) {
	dir := setup(t)
	smallBlocks(t)
	in := filepath.Join(dir, "in")
	want := map[string]string{
		"top/empty":          "",
		"top/small.txt":      "hello",
		"top/block":          text(1, 1<<minBlockLog), // exactly one block long
		"top/big.log":        text(2, 50_000),
		"top/random.bin":     noise(3, 20_000),
		"top/sub/deep/x.txt": "deep",
		"top/写真 é.txt":       "unicode",
		"top/run.sh":         "#!/bin/sh\n",
		"report.pdf":         text(4, 9_000),
	}
	for name, body := range want {
		put(t, filepath.Join(in, filepath.FromSlash(name)), body)
	}
	top := filepath.Join(in, "top")
	if err := os.Mkdir(filepath.Join(top, "void"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"top/", "top/sub/", "top/sub/deep/", "top/void/"} {
		want[d] = ""
	}
	if os.Symlink("small.txt", filepath.Join(top, "link")) == nil {
		want["top/link"] = "-> small.txt"
	}
	os.Chmod(filepath.Join(top, "run.sh"), 0o755)
	when := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	os.Chtimes(filepath.Join(top, "small.txt"), when, when)

	out := filepath.Join(dir, "out.xgett")
	made, err := Pack(context.Background(), []string{top, filepath.Join(in, "report.pdf")}, out, "")
	if err != nil {
		t.Fatal(err)
	}
	if made != out || !exists(out) || exists(out+".part") {
		t.Fatalf("made %q; the archive should be %q and no .part should remain", made, out)
	}
	got, a, err := unpacked(t, out, "")
	if err != nil {
		t.Fatal(err)
	}
	same(t, got, want)

	var size int64
	for i, e := range a.Entries {
		size += e.Size
		switch e.Path {
		case "top/run.sh":
			if runtime.GOOS != "windows" && !e.Exec {
				t.Error("the execute bit was lost")
			}
		case "top/small.txt":
			if e.Exec || !e.Mtime.Equal(when) {
				t.Errorf("small.txt: exec %v, mtime %v", e.Exec, e.Mtime)
			}
			if !a.Whole(i) {
				t.Error("a small file should arrive in one piece")
			}
		case "top/big.log", "top/empty":
			if a.Whole(i) {
				t.Errorf("%s does not arrive in one piece", e.Path)
			}
		}
	}
	if a.Size != size {
		t.Errorf("Size is %d, the files hold %d", a.Size, size)
	}
	if fi, _ := os.Stat(out); a.Packed != fi.Size() {
		t.Errorf("Packed is %d, the file is %d", a.Packed, fi.Size())
	}
}

func TestNothingToPack(t *testing.T) {
	dir := setup(t)
	if err := os.Mkdir(filepath.Join(dir, "void"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "void.xgett")
	if _, err := Pack(context.Background(), []string{filepath.Join(dir, "void")}, out, ""); err != nil {
		t.Fatal(err)
	}
	got, _, err := unpacked(t, out, "")
	if err != nil {
		t.Fatal(err)
	}
	same(t, got, map[string]string{"void/": ""})

	if _, err := Pack(context.Background(), []string{filepath.Join(dir, "missing")}, filepath.Join(dir, "m.xgett"), ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing input: %v", err)
	}
	if exists(filepath.Join(dir, "m.xgett")) || exists(filepath.Join(dir, "m.xgett.part")) {
		t.Fatal("a failed run left a file behind")
	}
}

func copyFile(t testing.TB, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	put(t, to, string(b))
}

func TestNoPassword(t *testing.T) {
	dir := setup(t)
	smallBlocks(t)
	put(t, filepath.Join(dir, "in", "a.txt"), text(1, 20_000))
	want := map[string]string{"in/": "", "in/a.txt": text(1, 20_000)}
	in := []string{filepath.Join(dir, "in")}
	first := filepath.Join(dir, "photos.xgett")
	if _, err := Pack(context.Background(), in, first, ""); err != nil {
		t.Fatal(err)
	}
	second, err := Pack(context.Background(), in, first, "")
	if err != nil {
		t.Fatal(err)
	}
	if second != filepath.Join(dir, "photos (1).xgett") {
		t.Fatalf("the second archive is %q", second)
	}
	copyFile(t, first, filepath.Join(dir, "a", "photos (1).xgett"))
	copyFile(t, first, filepath.Join(dir, "a", "photos.bin"))
	copyFile(t, first, filepath.Join(dir, "a", "holiday.xgett"))
	copyFile(t, second, filepath.Join(dir, "b", "photos.xgett"))

	for _, c := range []struct {
		file, key string
		want      error
	}{
		{first, "", nil},
		{second, "", nil},
		{filepath.Join(dir, "a", "photos (1).xgett"), "", nil},
		{filepath.Join(dir, "a", "photos.bin"), "", nil},
		{filepath.Join(dir, "b", "photos.xgett"), "", nil},
		{filepath.Join(dir, "a", "holiday.xgett"), "", ErrNoKey},
		{filepath.Join(dir, "a", "holiday.xgett"), "beach", ErrNoKey},
		{filepath.Join(dir, "a", "holiday.xgett"), "photos", nil},
		{filepath.Join(dir, "a", "holiday.xgett"), "PHOTOS", nil},
		{filepath.Join(dir, "a", "holiday.xgett"), "photos.xgett", nil},
		{filepath.Join(dir, "a", "holiday.xgett"), "Photos!", nil},
	} {
		got, _, err := unpacked(t, c.file, c.key)
		if !errors.Is(err, c.want) {
			t.Fatalf("%s with key %q: got %v, want %v", c.file, c.key, err, c.want)
		}
		if err == nil {
			same(t, got, want)
		}
	}
}

func TestPassword(t *testing.T) {
	dir := setup(t)
	put(t, filepath.Join(dir, "in", "a.txt"), "secret")
	out := filepath.Join(dir, "safe.xgett")
	if _, err := Pack(context.Background(), []string{filepath.Join(dir, "in")}, out, "tiger"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := unpacked(t, out, ""); !errors.Is(err, ErrNeedsPassword) {
		t.Fatalf("no password: %v", err)
	}
	if _, _, err := unpacked(t, out, "lion"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, _, err := unpacked(t, out, "SAFE"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("another word: %v", err)
	}
	other := filepath.Join(dir, "elsewhere", "other.xgett")
	copyFile(t, out, other)
	for _, p := range []string{out, other} {
		got, _, err := unpacked(t, p, "tiger")
		if err != nil {
			t.Fatalf("%s with its password: %v", p, err)
		}
		same(t, got, map[string]string{"in/": "", "in/a.txt": "secret"})
	}
}

func TestDamage(t *testing.T) {
	dir := setup(t)
	smallBlocks(t)
	put(t, filepath.Join(dir, "in", "a.bin"), noise(1, 60_000))
	out := filepath.Join(dir, "out.xgett")
	if _, err := Pack(context.Background(), []string{filepath.Join(dir, "in")}, out, ""); err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	change := func(f func(b []byte) []byte) error {
		p := filepath.Join(t.TempDir(), "out.xgett")
		put(t, p, string(f(bytes.Clone(good))))
		_, _, err := unpacked(t, p, "")
		return err
	}
	if err := change(func(b []byte) []byte { return b }); err != nil {
		t.Fatalf("an untouched copy: %v", err)
	}
	for name, f := range map[string]func(b []byte) []byte{
		"a flipped bit in the last block":  func(b []byte) []byte { b[len(b)-100] ^= 1; return b },
		"a flipped bit in the first block": func(b []byte) []byte { b[headerLen+30] ^= 1; return b },
		"a flipped bit in the salt":        func(b []byte) []byte { b[30] ^= 1; return b },
		"cut short":                        func(b []byte) []byte { return b[:len(b)-10] },
		"cut in half":                      func(b []byte) []byte { return b[:len(b)/2] },
		"only a header":                    func(b []byte) []byte { return b[:headerLen] },
		"less than a header":               func(b []byte) []byte { return b[:30] },
		"a byte too many":                  func(b []byte) []byte { return append(b, 0) },
		"an impossible block size":         func(b []byte) []byte { b[8] = 40; return b },
		"a smaller block size":             func(b []byte) []byte { b[8]--; return b },
		"a huge block count":               func(b []byte) []byte { binary.LittleEndian.PutUint64(b[16:], 1<<40); return b },
		"one block fewer":                  func(b []byte) []byte { binary.LittleEndian.PutUint64(b[16:], binary.LittleEndian.Uint64(b[16:])-1); return b },
		"two blocks swapped": func(b []byte) []byte {
			// Blocks of noise are stored as they are, so all but the last have one length.
			n := 4 + frameExtra + 1<<minBlockLog
			second, third := headerLen+n, headerLen+2*n
			tmp := bytes.Clone(b[second:third])
			copy(b[second:third], b[third:third+n])
			copy(b[third:third+n], tmp)
			return b
		},
	} {
		if err := change(f); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, f := range map[string]func(b []byte) []byte{
		"a later version": func(b []byte) []byte { b[6] = 9; return b },
		"an unknown flag": func(b []byte) []byte { b[7] |= 0x80; return b },
		"a reserved byte": func(b []byte) []byte { b[12] = 1; return b },
	} {
		if err := change(f); !errors.Is(err, ErrNewer) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestChangedInput(t *testing.T) {
	dir := setup(t)
	smallBlocks(t)
	p := filepath.Join(dir, "in", "a.log")
	put(t, p, text(1, 30_000))
	walked = func() { os.Truncate(p, 10) }
	t.Cleanup(func() { walked = func() {} })
	out := filepath.Join(dir, "out.xgett")
	_, err := Pack(context.Background(), []string{filepath.Join(dir, "in")}, out, "")
	if err == nil || !strings.Contains(err.Error(), "changed while") {
		t.Fatalf("a file that shrank: %v", err)
	}
	if exists(out) || exists(out+".part") {
		t.Fatal("a failed run left a file behind")
	}

	// A file that grew is packed as it was listed.
	put(t, p, text(1, 30_000))
	walked = func() {
		f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
		f.WriteString("more")
		f.Close()
	}
	if _, err := Pack(context.Background(), []string{filepath.Join(dir, "in")}, out, ""); err != nil {
		t.Fatal(err)
	}
	got, _, err := unpacked(t, out, "")
	if err != nil {
		t.Fatal(err)
	}
	same(t, got, map[string]string{"in/": "", "in/a.log": text(1, 30_000)})
}

func TestCancel(t *testing.T) {
	dir := setup(t)
	smallBlocks(t)
	put(t, filepath.Join(dir, "in", "a.log"), text(1, 200_000))
	in := []string{filepath.Join(dir, "in")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := filepath.Join(dir, "out.xgett")
	if _, err := Pack(ctx, in, out, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled, got %v", err)
	}
	if exists(out) || exists(out+".part") {
		t.Fatal("an interrupted run left a file behind")
	}

	if _, err := Pack(context.Background(), in, out, ""); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	a, err := Open(f, "out.xgett", "")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.Each(ctx, func(int, int64, []byte) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled, got %v", err)
	}
}

func TestStream(t *testing.T) {
	dir := setup(t)
	smallBlocks(t)
	body := text(1, 30_000) + noise(2, 30_000)
	put(t, filepath.Join(dir, "one.bin"), body)
	out := filepath.Join(dir, "one.xgett")
	if _, err := Pack(context.Background(), []string{filepath.Join(dir, "one.bin")}, out, ""); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	a, err := Open(f, "one.xgett", "")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	var buf bytes.Buffer
	if err := a.Stream(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != body {
		t.Fatalf("streamed %d bytes, want %d, or the bytes differ", buf.Len(), len(body))
	}
}

func TestFinishLine(t *testing.T) {
	for _, c := range []struct {
		from, to int64
		want     string
	}{
		{100, 48, "52% smaller"},
		{100, 209, "109% larger"},
		{100, 101, "1% larger"},
		{7, 7, "same size"},
		{0, 56, ""},
	} {
		if got := Change(c.from, c.to); got != c.want {
			t.Errorf("Change(%d, %d) = %q, want %q", c.from, c.to, got, c.want)
		}
	}
	for d, want := range map[time.Duration]string{
		3210 * time.Millisecond:  "3.2s",
		65300 * time.Millisecond: "1m5.3s",
		45 * time.Millisecond:    "45ms",
		400 * time.Microsecond:   "0s",
	} {
		if got := term.Took(d); got != want {
			t.Errorf("Took(%v) = %q, want %q", d, got, want)
		}
	}
	if got := Tail(100, 48, 3210*time.Millisecond); got != ", 52% smaller, in 3.2s" {
		t.Errorf("Tail = %q", got)
	}
	if got := Tail(0, 56, 45*time.Millisecond); got != ", in 45ms" {
		t.Errorf("Tail with nothing to compare = %q", got)
	}
}
