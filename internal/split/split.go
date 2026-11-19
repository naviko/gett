// Package split cuts a file into parts and joins parts into a file. A part
// is a plain piece of the file, with nothing added: the first holds the
// first bytes, the second the bytes after those, and so on. Every common
// splitter writes parts like that, which is why the parts gett makes can be
// joined by other tools and gett can join theirs.
//
// Parts do not depend on each other, so several are copied at once, on as
// many goroutines as gett uses elsewhere for heavy work.
package split

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gett/internal/download"
	"gett/internal/errs"
	"gett/internal/term"
	"gett/internal/xgett"

	"golang.org/x/sync/errgroup"
)

const (
	// DefaultSize is the size of a part when none is given, and DefaultText
	// how that size is written.
	DefaultSize = 1_000_000_000
	DefaultText = "1GB"

	// maxParts stops a mistyped size, 1KB for 1GB, from filling a folder
	// with millions of files.
	maxParts = 9999

	bufSize = 4 << 20
)

var buffers = sync.Pool{New: func() any { b := make([]byte, bufSize); return &b }}

// transfer copies n bytes of src, from off on, into dst from at on. name is
// what src is called in an error.
func transfer(ctx context.Context, dst io.WriterAt, at int64, src io.ReaderAt, off, n int64, name string, bar *term.Bar) error {
	bp := buffers.Get().(*[]byte)
	defer buffers.Put(bp)
	buf := *bp
	for n > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		m := int(min(int64(len(buf)), n))
		if k, err := src.ReadAt(buf[:m], off); k < m {
			if err == nil || err == io.EOF {
				return fmt.Errorf("%s changed while it was being read", name)
			}
			return err
		}
		if _, err := dst.WriteAt(buf[:m], at); err != nil {
			return &errs.Write{Err: err}
		}
		bar.Add(m)
		off, at, n = off+int64(m), at+int64(m), n-int64(m)
	}
	return nil
}

// each runs do for every i below n, as many at once as there are workers,
// and stops at the first error. It returns when all that started are done.
func each(parent context.Context, n int, do func(ctx context.Context, i int) error) error {
	g, ctx := errgroup.WithContext(parent)
	g.SetLimit(xgett.Workers())
	for i := 0; i < n && ctx.Err() == nil; i++ {
		g.Go(func() error { return do(ctx, i) })
	}
	if err := g.Wait(); err != nil {
		return err
	}
	return parent.Err() // interrupted before every part was started
}

// Split cuts file into parts of size bytes, the last one smaller, named
// file.001, file.002 and so on. They are written beside the file, or into
// dir. sizeText is the size as the user typed it, for the messages. The file
// itself is kept, nothing is overwritten, and a run that fails leaves no
// parts behind.
func Split(ctx context.Context, file string, size int64, sizeText, dir string) error {
	start := time.Now()
	fi, err := os.Stat(file)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return &errs.Usage{Msg: file + " is not a file; split takes one file"}
	}
	total := fi.Size()
	if total <= size {
		term.Info("%s is not larger than %s, nothing to split", file, sizeText)
		return nil
	}
	count := (total + size - 1) / size
	if count > maxParts {
		return &errs.Usage{Msg: fmt.Sprintf("%s parts would make %d files; use a larger size", sizeText, count)}
	}

	into := filepath.Dir(file)
	if dir != "" {
		into = filepath.Clean(dir)
		if err := os.MkdirAll(into, 0o755); err != nil {
			return &errs.Write{Err: err}
		}
	}
	// The parts take the file's name. If parts of that name are there
	// already, from this file or another, the new ones get a numbered name.
	base := filepath.Base(file)
	name := base
	for i := 1; taken(into, name); i++ {
		name = download.Numbered(base, i)
	}
	parts := make([]string, count)
	for i := range parts {
		parts[i] = filepath.Join(into, fmt.Sprintf("%s.%03d", name, i+1))
	}

	src, err := os.Open(file)
	if err != nil {
		return err
	}
	defer src.Close()
	prog := term.NewProgress()
	bar := prog.Bar(base, total, false)

	// Each part reads its own stretch of the file and writes its own file.
	err = each(ctx, len(parts), func(ctx context.Context, i int) error {
		off := int64(i) * size
		out, err := os.OpenFile(parts[i]+".part", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return &errs.Write{Err: err}
		}
		err = transfer(ctx, out, 0, src, off, min(size, total-off), file, bar)
		if cerr := out.Close(); err == nil && cerr != nil {
			err = &errs.Write{Err: cerr}
		}
		return err
	})
	// Every part is complete before the first gets its name, so a set never
	// looks whole when it is not.
	for i := 0; err == nil && i < len(parts); i++ {
		if rerr := os.Rename(parts[i]+".part", parts[i]); rerr != nil {
			err = &errs.Write{Err: rerr}
		}
	}
	if err != nil {
		for _, p := range parts {
			os.Remove(p + ".part")
			os.Remove(p)
		}
		bar.Abort()
		prog.Shutdown()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	bar.Done()
	prog.Wait()
	term.Info("split %s (%s) into %d parts of %s, in %s", file, download.Human(total), count, sizeText, term.Took(time.Since(start)))
	if dir != "" || name != base {
		term.Info("parts %s to %s", parts[0], filepath.Base(parts[len(parts)-1]))
	}
	return nil
}

// taken reports whether dir already holds parts of name, finished or not.
func taken(dir, name string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		s, ok := strings.CutPrefix(e.Name(), name+".")
		if !ok {
			continue
		}
		s = strings.TrimSuffix(s, ".part")
		if len(s) >= 3 && strings.Trim(s, digits) == "" {
			return true
		}
	}
	return false
}

// Join puts the parts of a split file back together. part is any one of
// them, or the name they were made from. The file lands beside the parts,
// or in dir, under the name without the counter; when stdout is piped it is
// written there instead and nothing is saved. Before anything is written
// the parts are checked: none may be missing, and all but the last must be
// of one size, unless force says to join them as they are.
func Join(ctx context.Context, part, dir string, force bool) error {
	start := time.Now()
	s, err := find(part)
	if err != nil {
		return err
	}
	sizes := make([]int64, len(s.parts))
	starts := make([]int64, len(s.parts)) // where each part begins in the file
	var total int64
	for i, p := range s.parts {
		fi, err := os.Stat(p)
		if err != nil {
			return err
		}
		sizes[i], starts[i] = fi.Size(), total
		total += sizes[i]
		last := i == len(s.parts)-1
		if force || i == 0 || (!last && sizes[i] == sizes[0]) || (last && sizes[i] <= sizes[0] && sizes[i] > 0) {
			continue
		}
		return &errs.Archive{Msg: p + " is not the same size as the other parts, nothing joined; add force to join anyway"}
	}
	prog := term.NewProgress()
	if !term.StdoutTTY() {
		return stream(ctx, s.parts, sizes, total, prog)
	}

	into := filepath.Dir(s.parts[0])
	if dir != "" {
		into = filepath.Clean(dir)
		if err := os.MkdirAll(into, 0o755); err != nil {
			return &errs.Write{Err: err}
		}
	}
	final := filepath.Join(into, s.name)
	for i := 1; download.Exists(final); i++ {
		final = filepath.Join(into, download.Numbered(s.name, i))
	}
	tmp := final + ".part"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return &errs.Write{Err: err}
	}
	bar := prog.Bar(filepath.Base(final), total, false)
	// The file gets its full length first, so every part can be written to
	// its own place at once, and a disk that cannot hold a file this large
	// says so before any copying.
	err = out.Truncate(total)
	if err != nil {
		err = &errs.Write{Err: err}
	} else {
		err = each(ctx, len(s.parts), func(ctx context.Context, i int) error {
			src, err := os.Open(s.parts[i])
			if err != nil {
				return err
			}
			defer src.Close()
			return transfer(ctx, out, starts[i], src, 0, sizes[i], s.parts[i], bar)
		})
	}
	if cerr := out.Close(); err == nil && cerr != nil {
		err = &errs.Write{Err: cerr}
	}
	if err == nil {
		if rerr := os.Rename(tmp, final); rerr != nil {
			err = &errs.Write{Err: rerr}
		}
	}
	if err != nil {
		os.Remove(tmp)
		bar.Abort()
		prog.Shutdown()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	bar.Done()
	prog.Wait()
	term.Info("joined %d parts into %s (%s), in %s", len(s.parts), final, download.Human(total), term.Took(time.Since(start)))
	return nil
}

// stream writes the parts to stdout one after the other. A pipe takes bytes
// in order only, so this is the one place where nothing runs side by side.
func stream(ctx context.Context, parts []string, sizes []int64, total int64, prog *term.Progress) error {
	bar := prog.Bar("stdout", total, false)
	bp := buffers.Get().(*[]byte)
	defer buffers.Put(bp)
	fail := func(err error) error {
		bar.Abort()
		prog.Shutdown()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	for i, name := range parts {
		f, err := os.Open(name)
		if err != nil {
			return fail(err)
		}
		left := sizes[i]
		for left > 0 {
			if ctx.Err() != nil {
				f.Close()
				return fail(ctx.Err())
			}
			n := int(min(int64(len(*bp)), left))
			if _, err := io.ReadFull(f, (*bp)[:n]); err != nil {
				f.Close()
				if err == io.EOF || err == io.ErrUnexpectedEOF {
					err = fmt.Errorf("%s changed while it was being read", name)
				}
				return fail(err)
			}
			if _, err := os.Stdout.Write((*bp)[:n]); err != nil {
				f.Close()
				return fail(&errs.Write{Err: err})
			}
			bar.Add(n)
			left -= int64(n)
		}
		f.Close()
	}
	bar.Done()
	prog.Wait()
	return nil
}
