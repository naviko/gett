package transfer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"gett/internal/errs"
)

func TestWalk(t *testing.T) {
	setup(t)
	dir := makeTree(t, map[string][]byte{
		"photos/b.jpg":        []byte("bb"),
		"photos/a.jpg":        []byte("a"),
		"photos/.hidden":      []byte("x"),
		"photos/.git/config":  []byte("x"),
		"photos/2024/c.jpg":   []byte("ccc"),
		"photos/2024/d/e.jpg": []byte("eeee"),
	})
	if runtime.GOOS != "windows" {
		os.Symlink("/etc/passwd", filepath.Join(dir, "photos", "passwd"))
		os.Symlink(filepath.Join(dir, "photos", "2024"), filepath.Join(dir, "photos", "link"))
	}
	idx, err := build(context.Background(), []string{filepath.Join(dir, "photos")}, "")
	if err != nil {
		t.Fatal(err)
	}
	sh := idx.shares[0]
	if sh.Name != "photos" || !sh.Tree.Dir || sh.Tree.Rel != "." {
		t.Fatalf("share: %+v", sh)
	}
	var names []string
	for _, k := range sh.Tree.Kids {
		names = append(names, k.Name)
	}
	if want := []string{"2024", "a.jpg", "b.jpg"}; !equal(names, want) {
		t.Fatalf("children %v, want %v", names, want)
	}
	if sh.Tree.Size != 10 || sh.Tree.Files != 4 {
		t.Fatalf("size %d files %d, want 10 and 4", sh.Tree.Size, sh.Tree.Files)
	}
	y := sh.Tree.child("2024")
	if y == nil || y.Size != 7 || y.Files != 2 || y.Rel != "2024" {
		t.Fatalf("2024: %+v", y)
	}

	// lookup follows the walked tree only.
	if s, n := idx.lookup("photos/2024/d/e.jpg"); s != sh || n == nil || n.Rel != "2024/d/e.jpg" || n.Size != 4 {
		t.Fatalf("lookup e.jpg: %v %+v", s, n)
	}
	if _, n := idx.lookup("photos/../photos/a.jpg"); n != nil {
		t.Fatal("dot-dot resolved")
	}
	if _, n := idx.lookup("photos/passwd"); n != nil {
		t.Fatal("symlink resolved")
	}
	if _, n := idx.lookup("photos/.hidden"); n != nil {
		t.Fatal("dotfile resolved")
	}
	if s, _ := idx.lookup("videos"); s != nil {
		t.Fatal("unknown share resolved")
	}

	// each and relTo give a flat view relative to a folder.
	var rels []string
	y.each(func(k *node) { rels = append(rels, y.relTo(k)) })
	if want := []string{"c.jpg", "d", "d/e.jpg"}; !equal(rels, want) {
		t.Fatalf("flat %v, want %v", rels, want)
	}
	if got := sh.Tree.relTo(y.child("d")); got != "2024/d" {
		t.Fatalf("relTo from root: %q", got)
	}
}

func TestBuildFileAndDuplicates(t *testing.T) {
	setup(t)
	dir := makeTree(t, map[string][]byte{"report.pdf": []byte("%PDF"), "a/photos/x": nil, "b/photos/y": nil})
	idx, err := build(context.Background(), []string{filepath.Join(dir, "report.pdf"), filepath.Join(dir, "a", "photos")}, "pw")
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.shares) != 2 || idx.shares[0].Name != "photos" || idx.shares[1].Name != "report.pdf" {
		t.Fatalf("shares out of order: %+v", idx.shares)
	}
	f := idx.shares[1]
	if f.Tree.Dir || f.Tree.Rel != "report.pdf" || f.Tree.Size != 4 || f.Password != "pw" {
		t.Fatalf("file share: %+v", f.Tree)
	}
	if fi, err := f.Root.Stat(f.Tree.Rel); err != nil || fi.Size() != 4 {
		t.Fatalf("file share root: %v", err)
	}
	_, err = build(context.Background(), []string{filepath.Join(dir, "a", "photos"), filepath.Join(dir, "b", "photos")}, "")
	var usage *errs.Usage
	if !errors.As(err, &usage) || usage.Msg != "photos is shared twice" {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := build(context.Background(), []string{filepath.Join(dir, "nope")}, ""); err == nil {
		t.Fatal("missing path accepted")
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
