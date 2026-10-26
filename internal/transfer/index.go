package transfer

import (
	"context"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"gett/internal/errs"
	"gett/internal/term"
)

// node is one file or folder of a share, as walked at start. Rel is the
// slash-separated path under the share's root, "." for the root itself.
type node struct {
	Name  string
	Rel   string
	Dir   bool
	Size  int64 // bytes, recursive for a folder
	MTime time.Time
	Files int     // files below a folder, 1 for a file
	Kids  []*node // sorted by name
}

// share is one shared path: its root on disk and its tree. A shared file
// has its parent folder as root and a single file node as tree.
type share struct {
	Name     string // base name, the first segment of every wire path
	Root     *os.Root
	Tree     *node
	Password string
}

// index is everything a sharer serves, built once at start.
type index struct {
	shares []*share // sorted by name
}

// build opens every path and walks it. Two paths with the same base name
// cannot both be shared, because the name is how receivers ask for them.
func build(ctx context.Context, paths []string, password string) (*index, error) {
	idx := &index{}
	for _, p := range paths {
		abs, err := filepath.Abs(filepath.Clean(p))
		if err != nil {
			return nil, err
		}
		name := filepath.Base(abs)
		if idx.share(name) != nil {
			return nil, &errs.Usage{Msg: name + " is shared twice"}
		}
		fi, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		sh := &share{Name: name, Password: password}
		switch {
		case fi.IsDir():
			if sh.Root, err = os.OpenRoot(abs); err != nil {
				return nil, err
			}
			if sh.Tree, err = walk(ctx, sh.Root.FS(), ".", name); err != nil {
				return nil, err
			}
		case fi.Mode().IsRegular():
			if sh.Root, err = os.OpenRoot(filepath.Dir(abs)); err != nil {
				return nil, err
			}
			sh.Tree = &node{Name: name, Rel: name, Size: fi.Size(), MTime: fi.ModTime(), Files: 1}
		default:
			return nil, &errs.Usage{Msg: name + " is not a file or folder"}
		}
		idx.shares = append(idx.shares, sh)
	}
	slices.SortFunc(idx.shares, func(a, b *share) int { return strings.Compare(a.Name, b.Name) })
	return idx, nil
}

// walk reads a folder and everything below it. Names starting with a dot,
// symlinks, devices and names that are not UTF-8 are left out; an
// unreadable subfolder is skipped with a debug line.
func walk(ctx context.Context, fsys fs.FS, rel, name string) (*node, error) {
	n := &node{Name: name, Rel: rel, Dir: true}
	entries, err := fs.ReadDir(fsys, rel)
	if err != nil {
		return nil, err
	}
	for i, e := range entries {
		if i%1000 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		en := e.Name()
		if strings.HasPrefix(en, ".") || !utf8.ValidString(en) {
			continue
		}
		child := path.Join(rel, en)
		var kid *node
		switch {
		case e.IsDir():
			if kid, err = walk(ctx, fsys, child, en); err != nil {
				if ctx.Err() != nil {
					return nil, err
				}
				term.Debug("skipping %s: %v", child, err)
				continue
			}
		case e.Type().IsRegular():
			fi, err := e.Info()
			if err != nil {
				term.Debug("skipping %s: %v", child, err)
				continue
			}
			kid = &node{Name: en, Rel: child, Size: fi.Size(), MTime: fi.ModTime(), Files: 1}
		default:
			continue
		}
		n.Size += kid.Size
		n.Files += kid.Files
		n.Kids = append(n.Kids, kid)
	}
	slices.SortFunc(n.Kids, func(a, b *node) int { return strings.Compare(a.Name, b.Name) })
	return n, nil
}

func (idx *index) share(name string) *share {
	for _, sh := range idx.shares {
		if sh.Name == name {
			return sh
		}
	}
	return nil
}

// lookup resolves a wire path like "photos/2024/a.jpg": the first segment
// is a share, the rest descend its tree. Only names that were walked
// resolve, so ".." and friends find nothing.
func (idx *index) lookup(p string) (*share, *node) {
	first, rest, _ := strings.Cut(p, "/")
	sh := idx.share(first)
	if sh == nil {
		return nil, nil
	}
	n := sh.Tree
	for rest != "" {
		var seg string
		seg, rest, _ = strings.Cut(rest, "/")
		if n = n.child(seg); n == nil {
			return sh, nil
		}
	}
	return sh, n
}

func (n *node) child(name string) *node {
	i, ok := slices.BinarySearchFunc(n.Kids, name, func(k *node, s string) int { return strings.Compare(k.Name, s) })
	if !ok {
		return nil
	}
	return n.Kids[i]
}

// each visits every node below n, parents before children.
func (n *node) each(fn func(*node)) {
	for _, k := range n.Kids {
		fn(k)
		k.each(fn)
	}
}

// relTo is a descendant's path relative to folder n.
func (n *node) relTo(k *node) string {
	if n.Rel == "." {
		return k.Rel
	}
	return strings.TrimPrefix(k.Rel, n.Rel+"/")
}

// stamp identifies a file's content cheaply: size and mtime. A receiver
// sends it with every request so a changed file is noticed at once.
func stamp(fi fs.FileInfo) string {
	return strconvItoa(fi.Size()) + "-" + strconvItoa(fi.ModTime().UnixNano())
}
