package extract

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sync/atomic"
	"time"

	"gett/internal/download"
	"gett/internal/errs"
	"gett/internal/term"
	"gett/internal/xgett"
)

// runXgett unpacks one of gett's own archives. words are what was typed
// after the file: the folder to unpack into, then the key. An archive that
// opens on its own takes one word at most, the folder. One that does not
// takes its key from the last word.
func runXgett(ctx context.Context, f *os.File, archive string, words []string) error {
	start := time.Now()
	base := filepath.Base(archive)
	a, err := xgett.Open(f, base, "")
	if (errors.Is(err, xgett.ErrNeedsPassword) || errors.Is(err, xgett.ErrNoKey)) && len(words) > 0 {
		last := len(words) - 1
		a, err = xgett.Open(f, base, words[last])
		words = words[:last]
	}
	if err != nil {
		return locked(archive, err)
	}
	defer a.Close()
	if len(words) > 1 {
		return &errs.Usage{Msg: fmt.Sprintf("don't know what %q means", words[1])}
	}
	dest := ""
	if len(words) == 1 {
		dest = filepath.Clean(words[0])
	}
	if len(a.Entries) == 1 && a.Entries[0].Kind == xgett.File {
		return xgettFile(ctx, a, archive, dest, start)
	}
	return xgettTree(ctx, a, archive, dest, start)
}

// locked says why an archive did not open. Passwords are mentioned only for
// an archive that was made with one.
func locked(archive string, err error) error {
	switch {
	case errors.Is(err, xgett.ErrNeedsPassword):
		return &errs.Archive{Msg: archive + " needs a password, left as is"}
	case errors.Is(err, xgett.ErrWrongPassword):
		return &errs.Archive{Msg: "wrong password for " + archive + ", left as is"}
	case errors.Is(err, xgett.ErrNoKey):
		return &errs.Archive{Msg: "unable to extract " + archive + ", left as is"}
	case errors.Is(err, xgett.ErrNewer):
		return &errs.Archive{Msg: archive + " was made by a newer gett, left as is"}
	}
	return xgettDamaged(archive)
}

// xgettDamaged is all gett says about a .xgett it cannot read to the end.
// What exactly was wrong stays in the error inside the program: the words
// for it would describe how the file is built.
func xgettDamaged(archive string) error {
	return &errs.Archive{Msg: archive + " is damaged, left as is"}
}

// xgettFailure sorts out why unpacking stopped: the disk, Ctrl+C, or the
// archive.
func xgettFailure(ctx context.Context, archive string, err error) error {
	if errors.As(err, new(*errs.Write)) {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var pe *os.PathError
	if errors.As(err, &pe) && pe.Path != archive {
		return &errs.Write{Err: err}
	}
	return xgettDamaged(archive)
}

// xgettFile handles an archive that holds one file: it lands beside the
// archive under its own name, or goes to stdout when piped, as a .gz does.
func xgettFile(ctx context.Context, a *xgett.Archive, archive, dest string, start time.Time) error {
	e := &a.Entries[0]
	prog := term.NewProgress()
	if !term.StdoutTTY() {
		bar := prog.Bar("stdout", a.Size, false)
		if err := a.Stream(ctx, bar.Writer(os.Stdout)); err != nil {
			bar.Abort()
			prog.Shutdown()
			return xgettFailure(ctx, archive, err)
		}
		bar.Done()
		prog.Wait()
		return nil
	}

	dir := filepath.Dir(archive)
	if dest != "" {
		dir = dest
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return &errs.Write{Err: err}
		}
	}
	name := stem(filepath.Base(archive), xgett.Ext)
	if rel, ok := safe(e.Path); ok {
		name = path.Base(rel) // its own name, and only the name: nothing here can leave dir
	}
	final := free(filepath.Join(dir, name), download.Numbered)
	part := final + ".part"
	out, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE|os.O_TRUNC, fileMode(e))
	if err != nil {
		return &errs.Write{Err: err}
	}
	bar := prog.Bar(filepath.Base(final), a.Size, false)
	err = a.Each(ctx, func(_ int, off int64, p []byte) error {
		if _, err := out.WriteAt(p, off); err != nil {
			return &errs.Write{Err: err}
		}
		bar.Add(len(p))
		return nil
	})
	if err == nil {
		if serr := out.Sync(); serr != nil {
			err = &errs.Write{Err: serr}
		}
	}
	if cerr := out.Close(); err == nil && cerr != nil {
		err = &errs.Write{Err: cerr}
	}
	if err != nil {
		os.Remove(part)
		bar.Abort()
		prog.Shutdown()
		return xgettFailure(ctx, archive, err)
	}
	if !e.Mtime.IsZero() {
		os.Chtimes(part, time.Now(), e.Mtime)
	}
	if err := os.Rename(part, final); err != nil {
		os.Remove(part)
		bar.Abort()
		prog.Shutdown()
		return &errs.Write{Err: err}
	}
	bar.Done()
	prog.Wait()
	term.Info("extracted %s (%s)%s", final, download.Human(a.Size), xgett.Tail(a.Packed, a.Size, time.Since(start)))
	return nil
}

// xgettTree handles an archive of several entries, by the rules of every
// other archive: a staging folder, moved into place at the end.
func xgettTree(ctx context.Context, a *xgett.Archive, archive, dest string, start time.Time) error {
	if !term.StdoutTTY() {
		return &errs.Usage{Msg: "a folder cannot be piped; run on a terminal to save it"}
	}
	base := filepath.Base(archive)
	name := stem(base, xgett.Ext)
	dir := filepath.Dir(archive)
	if dest != "" {
		dir = dest
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return &errs.Write{Err: err}
		}
	}
	staging := filepath.Join(dir, "."+name+".extracting")
	os.RemoveAll(staging) // from an earlier run that did not finish
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return &errs.Write{Err: err}
	}

	prog := term.NewProgress()
	bar := prog.Bar(base, a.Size, false)
	t := &xtree{root: staging, a: a, bar: bar}
	err := t.prepare(ctx)
	if err == nil {
		err = a.Each(ctx, t.write)
	}
	if err == nil {
		err = t.finish(ctx)
	}
	if err != nil {
		bar.Abort()
		prog.Shutdown()
		os.RemoveAll(staging)
		return xgettFailure(ctx, archive, err)
	}
	bar.Done()
	prog.Wait()

	out, err := place(staging, dir, name, dest != "")
	if err != nil {
		return err
	}
	n := t.bytes.Load()
	term.Info("extracted %s (%s) to %s%c%s", files(t.files.Load()), download.Human(n), out, filepath.Separator, xgett.Tail(a.Packed, n, time.Since(start)))
	if t.unsafe > 0 {
		term.Info("skipped %d entries that would have landed outside %s; verbose names them", t.unsafe, out)
	}
	return nil
}

// xtree writes an archive's entries under root. The entries are sorted out
// one by one before any bytes arrive; the bytes are then written by many
// goroutines at once, each piece into its own file or its own part of one.
type xtree struct {
	root   string
	a      *xgett.Archive
	bar    *term.Bar
	plan   []xfile // one per entry, fixed before the writing starts
	links  []link  // created last so none can redirect an entry
	files  atomic.Int64
	bytes  atomic.Int64
	unsafe int
}

// xfile is where a file entry goes. A file that arrives in one piece is
// created by whoever gets the piece. Any other, one with no bytes or with
// bytes in several blocks, exists before the writing starts, so the pieces
// can land in any order.
type xfile struct {
	path string // "" for an entry that is not written
	made bool
}

func fileMode(e *xgett.Entry) fs.FileMode {
	if e.Exec {
		return 0o755
	}
	return 0o644
}

// create makes a new file, and its folder when the archive listed none. A
// name that is taken comes back as fs.ErrExist.
func create(target string, mode fs.FileMode) (*os.File, error) {
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, err
		}
		f, err = os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	}
	return f, err
}

func (t *xtree) prepare(ctx context.Context) error {
	t.plan = make([]xfile, len(t.a.Entries))
	seen := map[string]bool{}
	for i := range t.a.Entries {
		if i%1024 == 0 && ctx.Err() != nil {
			return ctx.Err()
		}
		e := &t.a.Entries[i]
		rel, ok := safe(e.Path)
		if !ok {
			if rel != "." {
				t.unsafe++
				term.Debug("skipping %s: path leaves the folder", term.Sanitize(e.Path))
			}
			continue
		}
		if seen[rel] {
			term.Debug("skipping duplicate entry %s", term.Sanitize(rel))
			continue
		}
		seen[rel] = true
		target := filepath.Join(t.root, filepath.FromSlash(rel))
		switch e.Kind {
		case xgett.Dir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return &errs.Write{Err: err}
			}
		case xgett.Link:
			t.links = append(t.links, link{rel, e.Target})
		case xgett.File:
			if t.a.Whole(i) {
				t.plan[i].path = target
				continue
			}
			f, err := create(target, fileMode(e))
			if errors.Is(err, fs.ErrExist) { // a folder of that name, or the same name in another case
				term.Debug("skipping duplicate entry %s", term.Sanitize(rel))
				continue
			}
			if err == nil {
				err = f.Close()
			}
			if err != nil {
				return &errs.Write{Err: err}
			}
			t.plan[i] = xfile{path: target, made: true}
		}
	}
	return nil
}

// write puts one piece of file i where it belongs. It runs on many
// goroutines; the plan it reads no longer changes.
func (t *xtree) write(i int, off int64, p []byte) error {
	x := t.plan[i]
	if x.path == "" {
		return nil
	}
	e := &t.a.Entries[i]
	var f *os.File
	var err error
	if x.made {
		f, err = os.OpenFile(x.path, os.O_WRONLY, 0)
	} else {
		f, err = create(x.path, fileMode(e))
		if errors.Is(err, fs.ErrExist) {
			term.Debug("skipping duplicate entry %s", term.Sanitize(e.Path))
			return nil
		}
	}
	if err != nil {
		return &errs.Write{Err: err}
	}
	_, err = f.WriteAt(p, off)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return &errs.Write{Err: err}
	}
	if !x.made {
		if !e.Mtime.IsZero() {
			os.Chtimes(x.path, time.Now(), e.Mtime)
		}
		t.files.Add(1)
	}
	t.bytes.Add(int64(len(p)))
	t.bar.Add(len(p))
	return nil
}

// finish dates the files that were written in pieces, which each piece would
// have dated anew, and creates the links, only those that stay inside root.
func (t *xtree) finish(ctx context.Context) error {
	for i, x := range t.plan {
		if !x.made {
			continue
		}
		if m := t.a.Entries[i].Mtime; !m.IsZero() {
			os.Chtimes(x.path, time.Now(), m)
		}
		t.files.Add(1)
	}
	for _, l := range t.links {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !linkSafe(l.rel, l.target) {
			t.unsafe++
			term.Debug("skipping %s: link to %s", term.Sanitize(l.rel), term.Sanitize(l.target))
			continue
		}
		p := filepath.Join(t.root, filepath.FromSlash(l.rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return &errs.Write{Err: err}
		}
		if err := os.Symlink(filepath.FromSlash(l.target), p); err != nil {
			if errors.Is(err, fs.ErrExist) {
				term.Debug("skipping duplicate entry %s", term.Sanitize(l.rel))
				continue
			}
			return &errs.Write{Err: err}
		}
		t.files.Add(1)
	}
	return nil
}
