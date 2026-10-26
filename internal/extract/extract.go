// Package extract unpacks archives: zip, 7z, rar, tar in all its wrappings,
// and single compressed files. The format comes from the file's first bytes.
// Nothing is overwritten, no entry can land outside its folder, and the
// archive itself is kept. Zip entries are unpacked on every core at once;
// gzip reads ahead on spare cores; the stream formats (tar, 7z, rar, xz,
// bz2) are single-stream by nature and run on one core.
package extract

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gett/internal/download"
	"gett/internal/errs"
	"gett/internal/term"

	"github.com/bodgit/sevenzip"
	"github.com/klauspost/compress/zip"
	"github.com/mholt/archives"
	"github.com/nwaples/rardecode/v2"
	"golang.org/x/sync/errgroup"
)

// Run unpacks archive. With dest empty the result lands beside the archive,
// named after it; otherwise the contents land in dest, created if missing. A
// file that is not an archive is reported and left alone, which is not an
// error. Anything else that stops the unpacking is an *errs.Archive, except
// local disk trouble, which is an *errs.Write as everywhere in gett.
func Run(ctx context.Context, archive, dest string) error {
	if dest != "" {
		dest = filepath.Clean(dest)
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	format, _, err := archives.Identify(ctx, archive, f)
	if errors.Is(err, archives.NoMatch) {
		term.Info("%s is not an archive, left as is", archive)
		return nil
	}
	if err != nil {
		return damaged(archive, "", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	format = faster(format)
	if ex, ok := format.(archives.Extractor); ok {
		return unpack(ctx, f, archive, dest, format.Extension(), ex)
	}
	if dc, ok := format.(archives.Decompressor); ok {
		return inflate(ctx, f, archive, dest, format.Extension(), dc)
	}
	return &errs.Archive{Msg: fmt.Sprintf("%s is a %s file, which gett cannot unpack, left as is", archive, format.Extension())}
}

// faster swaps in the multithreaded gzip reader, which the library leaves
// off by default. It reads ahead and checksums on spare cores; gzip itself
// cannot be decoded in parallel. zstd is already concurrent.
func faster(format archives.Format) archives.Format {
	switch f := format.(type) {
	case archives.Gz:
		return archives.Gz{Multithreaded: true}
	case archives.CompressedArchive:
		if _, ok := f.Compression.(archives.Gz); ok {
			f.Compression = archives.Gz{Multithreaded: true}
			return f
		}
	}
	return format
}

// buffers hands out 1 MiB copy buffers: far fewer syscalls than io.Copy's
// 32 KiB on big files, pooled so thousands of small files do not allocate.
var buffers = sync.Pool{New: func() any { b := make([]byte, 1<<20); return &b }}

func copyBuffered(dst io.Writer, src io.Reader) (int64, error) {
	b := buffers.Get().(*[]byte)
	defer buffers.Put(b)
	return io.CopyBuffer(struct{ io.Writer }{dst}, struct{ io.Reader }{src}, *b)
}

// inflate handles a single compressed file: log.gz becomes log.
func inflate(ctx context.Context, f *os.File, archive, dest, ext string, dc archives.Decompressor) error {
	rc, err := dc.OpenReader(f)
	if err != nil {
		return damaged(archive, "", err)
	}
	defer rc.Close()
	src := &reader{ctx: ctx, r: rc}
	prog := term.NewProgress()

	if !term.StdoutTTY() {
		bar := prog.Bar("stdout", -1, false)
		_, err := copyBuffered(bar.Writer(os.Stdout), src)
		if err != nil {
			bar.Abort()
			prog.Shutdown()
			return readFailure(ctx, archive, "", err)
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
	name := stem(filepath.Base(archive), ext)
	final := free(filepath.Join(dir, name), download.Numbered)
	part := final + ".part"
	out, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return &errs.Write{Err: err}
	}
	bar := prog.Bar(filepath.Base(final), -1, false)
	n, err := copyBuffered(bar.Writer(out), src)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		os.Remove(part)
		bar.Abort()
		prog.Shutdown()
		return readFailure(ctx, archive, "", err)
	}
	if err := os.Rename(part, final); err != nil {
		os.Remove(part)
		bar.Abort()
		prog.Shutdown()
		return &errs.Write{Err: err}
	}
	bar.Done()
	prog.Wait()
	term.Info("extracted %s (%s)", final, download.Human(n))
	return nil
}

// unpack handles a multi-file archive. Everything goes into a hidden staging
// folder first and is moved into place in one go at the end, so an archive
// is read once, a failed or interrupted run leaves nothing behind, and the
// result appears complete or not at all.
func unpack(ctx context.Context, f *os.File, archive, dest, ext string, ex archives.Extractor) error {
	if !term.StdoutTTY() {
		return &errs.Usage{Msg: "a folder cannot be piped; run on a terminal to save it"}
	}
	base := filepath.Base(archive)
	name := stem(base, ext)
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
	bar := prog.Bar(base, -1, false)
	u := newUnpacker(ctx, staging, bar, ex)
	defer u.cancel()
	// base, not the group's ctx: a hard link waits the group out and starts a
	// new one, and a spent group ctx would stop the library between entries.
	err := ex.Extract(u.base, f, u.handle)
	if werr := u.wait(); werr != nil {
		err = werr // a failed write is the cause; the library only saw the cancel
	}
	if err == nil {
		err = u.links()
	}
	if err != nil {
		bar.Abort()
		prog.Shutdown()
		os.RemoveAll(staging)
		if errors.As(err, new(*errs.Write)) {
			return err
		}
		return readFailure(ctx, archive, entryName(err), err)
	}
	bar.Done()
	prog.Wait()

	out, err := place(staging, dir, name, dest != "")
	if err != nil {
		return err
	}
	term.Info("extracted %s (%s) to %s%c", files(u.files.Load()), download.Human(u.bytes.Load()), out, filepath.Separator)
	if u.unsafe > 0 {
		term.Info("skipped %d entries that would have landed outside %s; verbose names them", u.unsafe, out)
	}
	return nil
}

// ExtractTar unpacks a tar stream straight into dir, which must exist. An
// entry that would land outside dir is skipped, and so is one whose target
// already exists, so nothing in dir is overwritten; symlinks are created
// last and only when they stay inside dir. Nothing is staged, so what was
// written before a failure stays. bar may be nil. It returns the number of
// entries written, links included, and the bytes the files hold. Disk
// trouble is an *errs.Write, a stream that cannot be read to the end an
// *errs.Archive, and a cancelled ctx comes back as is.
func ExtractTar(ctx context.Context, r io.Reader, dir string, bar *term.Bar) (files, bytes int64, err error) {
	if bar == nil {
		bar = &term.Bar{}
	}
	u := newUnpacker(ctx, dir, bar, archives.Tar{})
	defer u.cancel()
	err = archives.Tar{}.Extract(u.base, r, u.handle)
	if werr := u.wait(); werr != nil {
		err = werr // a failed write is the cause; the library only saw the cancel
	}
	if err == nil {
		err = u.links()
	}
	files, bytes = u.files.Load(), u.bytes.Load()
	switch {
	case err == nil:
	case ctx.Err() != nil:
		err = ctx.Err()
	case errors.As(err, new(*errs.Write)):
	default:
		where := ""
		var ee *entryError
		if errors.As(err, &ee) {
			where, err = " at "+term.Sanitize(ee.name), ee.err
		}
		err = &errs.Archive{Msg: fmt.Sprintf("damaged tar stream%s (%s)", where, term.Sanitize(err.Error()))}
	}
	return files, bytes, err
}

func files(n int64) string {
	if n == 1 {
		return "1 file"
	}
	return fmt.Sprintf("%d files", n)
}

// place moves what was unpacked into its final spot. Without a destination
// the result is a folder named after the archive, or the archive's own single
// top folder when it has one, so src.tar.gz never ends up as src/src/. With a
// destination the contents land there as laid out in the archive. Names
// already taken get " (1)".
func place(staging, dir, name string, intoDest bool) (string, error) {
	entries, err := os.ReadDir(staging)
	if err != nil {
		return "", &errs.Write{Err: err}
	}
	if !intoDest {
		src := staging
		if len(entries) == 1 && entries[0].IsDir() {
			src = filepath.Join(staging, entries[0].Name())
			name = entries[0].Name()
		}
		out := free(filepath.Join(dir, name), folderNumbered)
		if err := os.Rename(src, out); err != nil {
			os.RemoveAll(staging)
			return "", &errs.Write{Err: err}
		}
		os.RemoveAll(staging)
		return out, nil
	}
	for _, e := range entries {
		num := download.Numbered
		if e.IsDir() {
			num = folderNumbered
		}
		target := free(filepath.Join(dir, e.Name()), num)
		if err := os.Rename(filepath.Join(staging, e.Name()), target); err != nil {
			os.RemoveAll(staging)
			return "", &errs.Write{Err: err}
		}
	}
	os.RemoveAll(staging)
	return dir, nil
}

// unpacker writes one archive's entries under root. The library hands
// entries over one at a time; for zip, whose entries are independent and
// safe to open concurrently, the writing is spread over every core. Tar, 7z
// and rar are streams, so for them each entry is written before the next.
type unpacker struct {
	ctx     context.Context
	root    string
	bar     *term.Bar
	seen    map[string]bool
	pending []link // symlinks, created last so none can redirect a later entry
	files   atomic.Int64
	bytes   atomic.Int64
	unsafe  int

	parallel bool
	base     context.Context // outlives the groups; each group's ctx dies with its Wait
	g        *errgroup.Group
	cancel   context.CancelFunc
}

type link struct{ rel, target string }

func newUnpacker(ctx context.Context, root string, bar *term.Bar, ex archives.Extractor) *unpacker {
	u := &unpacker{root: root, bar: bar, seen: map[string]bool{}}
	if _, ok := ex.(archives.Zip); ok {
		u.parallel = true
	}
	u.base, u.cancel = context.WithCancel(ctx)
	u.g, u.ctx = errgroup.WithContext(u.base)
	u.g.SetLimit(workers())
	return u
}

func workers() int {
	n := runtime.GOMAXPROCS(0)
	if n < 2 {
		n = 2
	}
	if n > 32 {
		n = 32
	}
	return n
}

// wait lets every in-flight write finish and reports the first failure.
func (u *unpacker) wait() error { return u.g.Wait() }

// barrier waits for in-flight writes, then accepts new ones. A hard link
// needs its target on disk first.
func (u *unpacker) barrier() error {
	if err := u.g.Wait(); err != nil {
		return err
	}
	u.g, u.ctx = errgroup.WithContext(u.base)
	u.g.SetLimit(workers())
	return nil
}

var errPassword = errors.New("needs a password")

// entryError names the entry an error happened at.
type entryError struct {
	name string
	err  error
}

func (e *entryError) Error() string { return e.name + ": " + e.err.Error() }
func (e *entryError) Unwrap() error { return e.err }

func entryName(err error) string {
	var ee *entryError
	if errors.As(err, &ee) {
		return ee.name
	}
	return ""
}

func (u *unpacker) handle(ctx context.Context, fi archives.FileInfo) error {
	if err := u.ctx.Err(); err != nil {
		return err
	}
	rel, ok := safe(fi.NameInArchive)
	if !ok {
		if rel == "." { // the folder itself, as tar likes to list it
			return nil
		}
		u.skip(fi.NameInArchive, "path leaves the folder")
		return nil
	}
	if u.seen[rel] {
		term.Debug("skipping duplicate entry %s", term.Sanitize(rel))
		return nil
	}
	u.seen[rel] = true
	target := filepath.Join(u.root, filepath.FromSlash(rel))
	mode := fi.Mode()
	switch {
	case fi.IsDir():
		if err := os.MkdirAll(target, 0o755); err != nil {
			return &errs.Write{Err: err}
		}
	case mode&fs.ModeSymlink != 0:
		u.pending = append(u.pending, link{rel, fi.LinkTarget})
	case mode.IsRegular() && fi.LinkTarget != "": // hard link
		src, ok := safe(fi.LinkTarget)
		if !ok || !u.seen[src] {
			u.skip(fi.NameInArchive, "hard link to "+term.Sanitize(fi.LinkTarget))
			return nil
		}
		if err := u.barrier(); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return &errs.Write{Err: err}
		}
		if err := os.Link(filepath.Join(u.root, filepath.FromSlash(src)), target); err != nil {
			return &errs.Write{Err: err}
		}
		u.files.Add(1)
	case mode.IsRegular():
		if encrypted(fi) {
			return &entryError{rel, errPassword}
		}
		if !u.parallel {
			return u.write(rel, fi, target, mode)
		}
		u.g.Go(func() error { return u.write(rel, fi, target, mode) })
	default:
		u.skip(fi.NameInArchive, "not a file, folder or link")
	}
	return nil
}

func (u *unpacker) write(rel string, fi archives.FileInfo, target string, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return &errs.Write{Err: err}
	}
	rc, err := fi.Open()
	if err != nil {
		return &entryError{rel, err}
	}
	defer rc.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644|mode&0o111)
	if err != nil {
		if errors.Is(err, fs.ErrExist) { // a folder entry of the same name came first
			term.Debug("skipping duplicate entry %s", term.Sanitize(rel))
			return nil
		}
		return &errs.Write{Err: err}
	}
	n, err := copyBuffered(u.bar.Writer(out), &reader{ctx: u.ctx, r: rc})
	if cerr := out.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) && pe.Path == target {
			return &errs.Write{Err: err}
		}
		return &entryError{rel, err}
	}
	if t := fi.ModTime(); !t.IsZero() {
		os.Chtimes(target, time.Now(), t)
	}
	u.files.Add(1)
	u.bytes.Add(n)
	return nil
}

// links creates the symlinks, only those that stay inside root.
func (u *unpacker) links() error {
	for _, l := range u.pending {
		if err := u.base.Err(); err != nil { // the group's ctx is spent once waited on
			return err
		}
		if !linkSafe(l.rel, l.target) {
			u.skip(l.rel, "link to "+term.Sanitize(l.target))
			continue
		}
		p := filepath.Join(u.root, filepath.FromSlash(l.rel))
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
		u.files.Add(1)
	}
	return nil
}

func (u *unpacker) skip(name, why string) {
	u.unsafe++
	term.Debug("skipping %s: %s", term.Sanitize(name), why)
}

// safe turns an archive path into a relative slash path that stays inside
// the folder, or reports that it cannot. The folder itself comes back as ".".
func safe(name string) (string, bool) {
	p := strings.TrimLeft(strings.ReplaceAll(name, `\`, "/"), "/")
	p = path.Clean(p)
	if p == "." || p == "" {
		return ".", false
	}
	if !filepath.IsLocal(filepath.FromSlash(p)) {
		return "", false
	}
	return p, true
}

// linkSafe reports whether a symlink at rel pointing to target resolves
// inside the folder. Absolute targets and targets climbing past the top are
// out.
func linkSafe(rel, target string) bool {
	t := strings.ReplaceAll(target, `\`, "/")
	if t == "" || strings.HasPrefix(t, "/") || filepath.VolumeName(target) != "" {
		return false
	}
	joined := path.Join(path.Dir(rel), t)
	return joined != ".." && !strings.HasPrefix(joined, "../")
}

// encrypted reports an entry gett cannot read without a password. The zip
// and rar libraries say so in the header; 7z says so when reading fails.
func encrypted(fi archives.FileInfo) bool {
	switch h := fi.Header.(type) {
	case zip.FileHeader:
		return h.Flags&1 != 0
	case *zip.FileHeader:
		return h.Flags&1 != 0
	case *rardecode.FileHeader:
		return h.Encrypted || h.HeaderEncrypted
	}
	return false
}

func needsPassword(err error) bool {
	var re *sevenzip.ReadError
	return errors.Is(err, errPassword) || errors.Is(err, rardecode.ErrArchiveEncrypted) ||
		errors.Is(err, rardecode.ErrArchivedFileEncrypted) || errors.Is(err, rardecode.ErrBadPassword) ||
		(errors.As(err, &re) && re.Encrypted)
}

// readFailure sorts out why an archive could not be read to the end.
func readFailure(ctx context.Context, archive, entry string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var pe *os.PathError
	if errors.As(err, &pe) && pe.Path != archive {
		return &errs.Write{Err: err}
	}
	return damaged(archive, entry, err)
}

func damaged(archive, entry string, err error) error {
	if needsPassword(err) {
		return &errs.Archive{Msg: archive + " needs a password, left as is"}
	}
	where := archive
	if entry != "" {
		where += " at " + term.Sanitize(entry)
	}
	var ee *entryError
	if errors.As(err, &ee) {
		err = ee.err
	}
	return &errs.Archive{Msg: fmt.Sprintf("%s is damaged (%s), left as is", where, term.Sanitize(err.Error()))}
}

// stem is the archive's name without its extension: src.tar.gz and src.tgz
// both give src. A name without one stays as it is; numbering then keeps the
// result apart from the archive.
func stem(base, ext string) string {
	if ext != "" && len(base) > len(ext) && strings.EqualFold(base[len(base)-len(ext):], ext) {
		return base[:len(base)-len(ext)]
	}
	if e := filepath.Ext(base); e != "" && len(e) < len(base) {
		return base[:len(base)-len(e)]
	}
	return base
}

// free returns p, or the first numbered variant of it that does not exist.
func free(p string, numbered func(string, int) string) string {
	out := p
	for i := 1; download.Exists(out); i++ {
		out = numbered(p, i)
	}
	return out
}

// folderNumbered appends " (n)" without treating dots as an extension, so
// gett-1.0.8 becomes "gett-1.0.8 (1)" and not "gett-1.0 (1).8".
func folderNumbered(name string, n int) string { return fmt.Sprintf("%s (%d)", name, n) }

// reader stops a copy as soon as ctx ends.
type reader struct {
	ctx context.Context
	r   io.Reader
}

func (r *reader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
