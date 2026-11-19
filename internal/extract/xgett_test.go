package extract

import (
	"context"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gett/internal/errs"
	"gett/internal/term"
	"gett/internal/xgett"
)

var xgettWhen = time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)

// xgettSample makes a folder "photos" under dir/src and packs it into
// dir/out/photos.xgett. The big file does not pack and is longer than two
// blocks, so it reaches the disk in pieces, from several goroutines.
func xgettSample(t *testing.T, dir, password string) (archive string, want map[string]string) {
	t.Helper()
	big := make([]byte, 9<<20)
	rand.New(rand.NewSource(1)).Read(big)
	want = map[string]string{
		"empty":           "",
		"cover.jpg":       "jpeg",
		"2024/a.txt":      strings.Repeat("a line of text\n", 1000),
		"2024/deep/b.txt": "b",
		"big.bin":         string(big),
	}
	src := filepath.Join(dir, "src", "photos")
	for name, body := range want {
		p := filepath.Join(src, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(src, "void"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.Chmod(filepath.Join(src, "cover.jpg"), 0o755)
	os.Chtimes(filepath.Join(src, "2024", "a.txt"), xgettWhen, xgettWhen)
	os.Chtimes(filepath.Join(src, "big.bin"), xgettWhen, xgettWhen)
	os.Symlink("cover.jpg", filepath.Join(src, "link"))

	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	archive, err := xgett.Pack(context.Background(), []string{src}, filepath.Join(out, "photos.xgett"), password)
	if err != nil {
		t.Fatal(err)
	}
	return archive, want
}

func checkTree(t *testing.T, root string, want map[string]string) {
	t.Helper()
	for name, body := range want {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			t.Errorf("%s: %v", name, err)
		} else if string(b) != body {
			t.Errorf("%s: got %d bytes, want %d, or the bytes differ", name, len(b), len(body))
		}
	}
}

func copyTo(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// refused checks that err is the one-line "left as is" kind, exit 2, and
// names the reason.
func refused(t *testing.T, err error, reason string) {
	t.Helper()
	if errs.Code(err) != 2 || !errors.As(err, new(*errs.Archive)) || !strings.Contains(err.Error(), reason) || !strings.Contains(err.Error(), "left as is") {
		t.Fatalf("want an *errs.Archive saying %q, got %v", reason, err)
	}
}

func TestXgettRoundTrip(t *testing.T) {
	dir := setup(t)
	archive, want := xgettSample(t, dir, "")
	if err := Run(context.Background(), archive); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out", "photos")
	checkTree(t, out, want)
	if fi, err := os.Stat(filepath.Join(out, "void")); err != nil || !fi.IsDir() {
		t.Fatalf("the empty folder: %v", err)
	}
	if exists(filepath.Join(dir, "out", ".photos.extracting")) {
		t.Fatal("staging folder left behind")
	}
	if !exists(archive) {
		t.Fatal("archive was deleted")
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(filepath.Join(out, "cover.jpg")); fi.Mode()&0o111 == 0 {
			t.Error("the execute bit was lost")
		}
		if fi, _ := os.Stat(filepath.Join(out, "2024", "a.txt")); fi.Mode()&0o111 != 0 {
			t.Error("an execute bit appeared")
		}
		if target, err := os.Readlink(filepath.Join(out, "link")); err != nil || target != "cover.jpg" {
			t.Errorf("the link: %q %v", target, err)
		}
	}
	for _, name := range []string{"2024/a.txt", "big.bin"} { // one written whole, one in pieces
		if fi, _ := os.Stat(filepath.Join(out, filepath.FromSlash(name))); !fi.ModTime().Equal(xgettWhen) {
			t.Errorf("%s is dated %v, want %v", name, fi.ModTime(), xgettWhen)
		}
	}

	if err := Run(context.Background(), archive); err != nil {
		t.Fatal(err)
	}
	checkTree(t, filepath.Join(dir, "out", "photos (1)"), want)
}

func TestXgettSingleFile(t *testing.T) {
	dir := setup(t)
	body := strings.Repeat("pdf ", 5<<20/4) // longer than one block
	src := filepath.Join(dir, "report.pdf")
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(src, xgettWhen, xgettWhen)
	out := filepath.Join(dir, "out")
	archive, err := xgett.Pack(context.Background(), []string{src}, filepath.Join(out, "papers.xgett"), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), archive); err != nil {
		t.Fatal(err)
	}
	// One file comes out as itself, under its own name, not in a folder.
	if read(t, filepath.Join(out, "report.pdf")) != body {
		t.Fatal("content wrong")
	}
	if fi, _ := os.Stat(filepath.Join(out, "report.pdf")); !fi.ModTime().Equal(xgettWhen) {
		t.Errorf("dated %v, want %v", fi.ModTime(), xgettWhen)
	}
	if exists(filepath.Join(out, "report.pdf.part")) || exists(filepath.Join(out, "papers")) {
		t.Fatal("something besides the file was left")
	}
	if err := Run(context.Background(), archive); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(out, "report (1).pdf")) != body {
		t.Fatal("second run should be numbered")
	}
	if err := Run(context.Background(), archive, filepath.Join(dir, "elsewhere")); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(dir, "elsewhere", "report.pdf")) != body {
		t.Fatal("content wrong in the given folder")
	}
}

func TestXgettPiped(t *testing.T) {
	dir := setup(t)
	piped := false
	term.StdoutIsTTY = &piped

	body := strings.Repeat("log line\n", 5<<20/9)
	src := filepath.Join(dir, "app.log")
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	archive, err := xgett.Pack(context.Background(), []string{src}, filepath.Join(out, "app.xgett"), "")
	if err != nil {
		t.Fatal(err)
	}
	capture, err := os.Create(filepath.Join(dir, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = capture
	err = Run(context.Background(), archive)
	os.Stdout = stdout
	capture.Close()
	if err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(dir, "stdout")) != body {
		t.Fatal("the file should stream to stdout, in order")
	}
	if entries, _ := os.ReadDir(out); len(entries) != 1 {
		t.Fatalf("piping should save nothing: %v", entries)
	}

	folder, _ := xgettSample(t, t.TempDir(), "")
	err = Run(context.Background(), folder)
	if errs.Code(err) != 2 || !errors.As(err, new(*errs.Usage)) || !strings.Contains(err.Error(), "cannot be piped") {
		t.Fatalf("a folder cannot be piped: %v", err)
	}
}

func TestXgettWords(t *testing.T) {
	dir := setup(t)
	ctx := context.Background()

	// An archive that opens on its own takes one word, the folder.
	open, want := xgettSample(t, dir, "")
	if err := Run(ctx, open, filepath.Join(dir, "dest")); err != nil {
		t.Fatal(err)
	}
	checkTree(t, filepath.Join(dir, "dest", "photos"), want)
	err := Run(ctx, open, filepath.Join(dir, "dest2"), "tiger")
	if errs.Code(err) != 2 || !errors.As(err, new(*errs.Usage)) || !strings.Contains(err.Error(), `"tiger"`) {
		t.Fatalf("a second word for an archive that needs none: %v", err)
	}
	if exists(filepath.Join(dir, "dest2")) {
		t.Fatal("a refused run created the folder")
	}

	// One with a password takes it from the last word.
	ld := t.TempDir()
	locked, want := xgettSample(t, ld, "tiger")
	refused(t, Run(ctx, locked), "needs a password")
	refused(t, Run(ctx, locked, "lion"), "wrong password")
	refused(t, Run(ctx, locked, "tiger", filepath.Join(ld, "d")), "wrong password")
	if exists(filepath.Join(ld, "out", "photos")) || exists(filepath.Join(ld, "d")) {
		t.Fatal("a refused run unpacked something")
	}
	if err := Run(ctx, locked, "tiger"); err != nil {
		t.Fatalf("a lone word is the password: %v", err)
	}
	checkTree(t, filepath.Join(ld, "out", "photos"), want)
	if err := Run(ctx, locked, filepath.Join(ld, "d"), "tiger"); err != nil {
		t.Fatalf("folder, then password: %v", err)
	}
	checkTree(t, filepath.Join(ld, "d", "photos"), want)

	moved := filepath.Join(ld, "out", "other.xgett")
	if err := os.Rename(locked, moved); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, moved, filepath.Join(ld, "e"), "tiger"); err != nil {
		t.Fatalf("%s with its password: %v", moved, err)
	}
	checkTree(t, filepath.Join(ld, "e", "photos"), want)
}

func TestXgettCopies(t *testing.T) {
	dir := setup(t)
	ctx := context.Background()
	archive, want := xgettSample(t, dir, "")
	out := filepath.Dir(archive)

	second := filepath.Join(out, "photos (1).xgett")
	copyTo(t, archive, second)
	if err := Run(ctx, second, filepath.Join(dir, "one")); err != nil {
		t.Fatalf("%s: %v", second, err)
	}
	checkTree(t, filepath.Join(dir, "one", "photos"), want)

	bin := filepath.Join(out, "photos.bin")
	copyTo(t, archive, bin)
	if err := Run(ctx, bin, filepath.Join(dir, "two")); err != nil {
		t.Fatalf("%s: %v", bin, err)
	}
	checkTree(t, filepath.Join(dir, "two", "photos"), want)

	// An archive that does not open gets one fixed sentence and no more.
	other := filepath.Join(out, "holiday.xgett")
	copyTo(t, archive, other)
	for _, err := range []error{
		Run(ctx, other),
		Run(ctx, other, "beach"),
		Run(ctx, other, "photos", filepath.Join(dir, "three")),
	} {
		refused(t, err, "unable to extract")
		if err.Error() != "unable to extract "+other+", left as is" {
			t.Fatalf("the message says more than it should: %q", err)
		}
	}
	if exists(filepath.Join(dir, "three")) || exists(filepath.Join(out, ".holiday.extracting")) {
		t.Fatal("a refused run left something behind")
	}
	if err := Run(ctx, other, filepath.Join(dir, "three"), "photos"); err != nil {
		t.Fatalf("folder, then key: %v", err)
	}
	checkTree(t, filepath.Join(dir, "three", "photos"), want)
	if err := Run(ctx, other, "photos.xgett"); err != nil {
		t.Fatalf("key as the only word: %v", err)
	}
}

func TestXgettDamaged(t *testing.T) {
	dir := setup(t)
	archive, _ := xgettSample(t, dir, "")
	out := filepath.Dir(archive)
	good, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	clean := func(what string) {
		t.Helper()
		if exists(filepath.Join(out, "photos")) || exists(filepath.Join(out, ".photos.extracting")) {
			t.Fatalf("%s: a damaged archive must leave nothing behind", what)
		}
	}

	// Whatever is wrong, gett says the same few words: the details would
	// describe how the file is built.
	isDamaged := func(what string) {
		t.Helper()
		err := Run(context.Background(), archive)
		refused(t, err, "is damaged")
		if err.Error() != archive+" is damaged, left as is" {
			t.Fatalf("%s: the message says more than it should: %q", what, err)
		}
		clean(what)
	}

	// A flipped bit near the end is found after most of it is written.
	bad := append([]byte(nil), good...)
	bad[len(bad)-1000] ^= 1
	os.WriteFile(archive, bad, 0o644)
	isDamaged("flipped bit")

	bad = append([]byte(nil), good...)
	bad[100] ^= 1
	os.WriteFile(archive, bad, 0o644)
	isDamaged("flipped bit at the start")

	os.WriteFile(archive, good[:len(good)/2], 0o644)
	isDamaged("cut in half")

	os.WriteFile(archive, append(append([]byte(nil), good...), 0), 0o644)
	isDamaged("a byte too many")

	newer := append([]byte(nil), good...)
	newer[6] = 9
	os.WriteFile(archive, newer, 0o644)
	refused(t, Run(context.Background(), archive), "newer gett")
	clean("a later version")
}

func TestXgettCancel(t *testing.T) {
	dir := setup(t)
	archive, _ := xgettSample(t, dir, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, archive); !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled, got %v", err)
	}
	out := filepath.Dir(archive)
	if exists(filepath.Join(out, "photos")) || exists(filepath.Join(out, ".photos.extracting")) {
		t.Fatal("interrupted run left files behind")
	}
}

func TestXgettStaysInside(t *testing.T) {
	dir := setup(t)
	in := filepath.Join(dir, "in")
	if err := os.Mkdir(in, 0o755); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(in, "forged.xgett")
	bodies := []string{"evil", "abs", "fine", "", "", "twice", "again"}
	entries := []xgett.Entry{
		{Kind: xgett.File, Path: "../evil.txt"},
		{Kind: xgett.File, Path: "/abs.txt"},
		{Kind: xgett.File, Path: "ok/fine.txt"}, // no entry for the folder ok
		{Kind: xgett.Link, Path: "ok/out", Target: "../../evil.txt"},
		{Kind: xgett.Link, Path: "ok/in", Target: "fine.txt"},
		{Kind: xgett.File, Path: "twice.txt"},
		{Kind: xgett.File, Path: "twice.txt"},
	}
	if err := xgett.Forge(archive, "", entries, func(i int) []byte { return []byte(bodies[i]) }); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), archive); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(in, "forged")
	if exists(filepath.Join(in, "evil.txt")) || exists(filepath.Join(dir, "evil.txt")) {
		t.Fatal("an entry escaped the folder")
	}
	if read(t, filepath.Join(out, "abs.txt")) != "abs" {
		t.Fatal("absolute entry should be unpacked inside the folder")
	}
	if read(t, filepath.Join(out, "ok", "fine.txt")) != "fine" {
		t.Fatal("a file whose folder was not listed")
	}
	if exists(filepath.Join(out, "ok", "out")) {
		t.Fatal("a link that leaves the folder was created")
	}
	if target, err := os.Readlink(filepath.Join(out, "ok", "in")); err != nil || target != "fine.txt" {
		t.Fatalf("a link that stays inside should be created: %q %v", target, err)
	}
	if read(t, filepath.Join(out, "twice.txt")) != "twice" {
		t.Fatal("of two entries with one name the first one counts")
	}
}
