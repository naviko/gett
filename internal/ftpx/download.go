package ftpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"gett/internal/download"
	"gett/internal/errs"
	"gett/internal/term"

	"github.com/jlaffaye/ftp"
)

// Download saves the file or folder at rawURL and returns where it landed. A
// folder is told by a trailing slash, or by asking the server. Piped, a file
// streams to stdout and the path is empty.
func Download(ctx context.Context, rawURL, dest string) (saved string, err error) {
	srv, remote, folder, err := parse(rawURL)
	if err != nil {
		return "", err
	}
	conn, err := srv.dial(ctx)
	if err != nil {
		return "", err
	}
	var e *ftp.Entry
	if !folder {
		if e, err = stat(conn, remote); err != nil {
			conn.Quit()
			return "", err
		}
		folder = e.Type == ftp.EntryTypeFolder
	}
	if folder {
		return downloadFolder(ctx, srv, conn, remote, dest)
	}
	size := int64(e.Size)
	if size == 0 { // the listing may not carry sizes
		if n, err := conn.FileSize(remote); err == nil {
			size = n
		}
	}
	if !term.StdoutTTY() {
		defer conn.Quit()
		return "", stream(ctx, conn, remote, size)
	}
	return downloadFile(ctx, srv, conn, remote, size, e.Time, dest)
}

// stream copies a file to stdout in one go: a pipe cannot resume.
func stream(ctx context.Context, c *ftp.ServerConn, remote string, size int64) error {
	resp, err := c.Retr(remote)
	if err != nil {
		return err
	}
	prog := term.NewProgress()
	bar := prog.Bar("stdout", size, false)
	stop := context.AfterFunc(ctx, func() { resp.SetDeadline(time.Now()) })
	b := &body{ctx: ctx, resp: resp}
	_, err = io.CopyBuffer(bar.Writer(os.Stdout), b, make([]byte, 1<<20))
	stop()
	if err != nil {
		bar.Abort()
		prog.Shutdown()
		c.Quit() // Close would wait for the server's final reply on a dead transfer
		resp.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	resp.Close()
	bar.Done()
	prog.Wait()
	return nil
}

func downloadFile(ctx context.Context, srv *server, conn *ftp.ServerConn, remote string, size int64, mtime time.Time, dest string) (string, error) {
	dir, name, err := download.Target(dest)
	if err != nil {
		conn.Quit()
		return "", &errs.Write{Err: err}
	}
	if name == "" {
		name = download.Clean(path.Base(remote))
	}
	final, _ := place(filepath.Join(dir, name), size, false)
	if download.Exists(final + ".part") {
		term.Info("resuming %s", final)
	}
	j := job{local: final, remote: remote, size: size, mtime: mtime}

	prog := term.NewProgress()
	bar := prog.Bar(filepath.Base(final), size, false)
	w := &worker{srv: srv, conn: conn, st: &stats{}, prog: prog}
	defer w.drop()
	err = w.run(ctx, j, func(ctx context.Context, w *worker, j job) error {
		_, err := fetch(ctx, w, j, bar)
		return err
	})
	if err != nil {
		bar.Abort()
		prog.Shutdown()
		if ctx.Err() != nil || errs.CanResume(err) {
			return "", &errs.Resumable{Err: err}
		}
		return "", err
	}
	bar.Done()
	prog.Wait()
	term.Info("saved %s (%s)", final, download.Human(size))
	return final, nil
}

// downloadFolder mirrors the remote folder into a local folder of the same
// name, skipping files already there in full and resuming partial ones.
func downloadFolder(ctx context.Context, srv *server, first *ftp.ServerConn, remote, dest string) (string, error) {
	if !term.StdoutTTY() {
		first.Quit()
		return "", &errs.Usage{Msg: "a folder cannot be piped; give a file, or run on a terminal to save it"}
	}
	dir, name, err := download.Target(dest)
	if err != nil {
		first.Quit()
		return "", &errs.Write{Err: err}
	}
	if name == "" {
		if remote == "." || remote == "/" {
			name = download.Clean(srv.host)
		} else {
			name = download.Clean(path.Base(remote))
		}
	}
	root := filepath.Join(dir, name)

	var dirs []string
	var jobs []job
	var total int64
	err = walk(ctx, first, remote, func(p string, e *ftp.Entry) error {
		local := localPath(root, remote, p)
		switch e.Type {
		case ftp.EntryTypeFolder:
			dirs = append(dirs, local)
		case ftp.EntryTypeLink:
			term.Debug("skipping link %s -> %s", p, term.Sanitize(e.Target))
		default:
			jobs = append(jobs, job{local: local, remote: p, size: int64(e.Size), mtime: e.Time})
			total += int64(e.Size)
		}
		return nil
	})
	if err != nil {
		first.Quit()
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", err
	}
	for _, d := range append([]string{root}, dirs...) {
		if err := os.MkdirAll(d, 0o755); err != nil {
			first.Quit()
			return "", &errs.Write{Err: err}
		}
	}

	st, err := pool(ctx, srv, first, jobs, total, func(ctx context.Context, w *worker, j job) error {
		final, skip := place(j.local, j.size, true)
		if skip {
			w.st.skipped.Add(1)
			w.at(j.size)
			term.Debug("skip %s (already there)", final)
			return nil
		}
		j.local = final
		bar := w.prog.Bar(filepath.Base(final), j.size, true)
		defer bar.Done()
		resumed, err := fetch(ctx, w, j, bar)
		if err != nil {
			return err
		}
		w.st.done.Add(1)
		w.st.bytes.Add(j.size)
		if resumed {
			w.st.resumed.Add(1)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	term.Info("saved %d files (%s) in %s, skipped %d, resumed %d",
		st.done.Load(), download.Human(st.bytes.Load()), root, st.skipped.Load(), st.resumed.Load())
	return root, nil
}

// localPath maps a remote path below remote onto root. Every name is cleaned
// on its own, so a server cannot place a file outside root.
func localPath(root, remote, p string) string {
	rel := p
	if remote != "." {
		rel = strings.TrimPrefix(strings.TrimPrefix(p, remote), "/")
	}
	segs := []string{root}
	for _, s := range strings.Split(rel, "/") {
		if s != "" {
			segs = append(segs, download.Clean(s))
		}
	}
	return filepath.Join(segs...)
}

// place picks the local name for remote content of size bytes: a matching
// .part is resumed, a complete copy is skipped when skipSame, and anything
// else gets the first free "name (n).ext". A .gett sidecar marks an HTTP
// download in progress and is left alone.
func place(name string, size int64, skipSame bool) (final string, skip bool) {
	for i := 0; ; i++ {
		cand := name
		if i > 0 {
			cand = download.Numbered(name, i)
		}
		if fi, err := os.Lstat(cand); err == nil {
			if skipSame && !fi.IsDir() && fi.Size() == size {
				return cand, true
			}
			continue
		}
		if download.Exists(cand + ".gett") {
			continue
		}
		return cand, false
	}
}

// fetch brings j.remote into j.local+".part" from where that file ends, then
// renames it into place. Every attempt starts from the part's current size.
func fetch(ctx context.Context, w *worker, j job, bar *term.Bar) (resumed bool, err error) {
	part := j.local + ".part"
	f, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return false, &errs.Write{Err: err}
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return false, &errs.Write{Err: err}
	}
	off := fi.Size()
	if j.size >= 0 && off > j.size {
		term.Debug("%s is larger than the server's copy, starting over", part)
		if err := f.Truncate(0); err != nil {
			return false, &errs.Write{Err: err}
		}
		off = 0
	}
	resumed = off > 0
	if j.size < 0 || off < j.size {
		if err := w.receive(ctx, j, f, off, bar); err != nil {
			return resumed, err
		}
	}
	if fi, err = f.Stat(); err != nil {
		return resumed, &errs.Write{Err: err}
	}
	got := fi.Size()
	if j.size >= 0 && got != j.size {
		if got > j.size {
			os.Remove(part)
			return resumed, fmt.Errorf("%s: the server sent %s but reported %s", j.remote, download.Human(got), download.Human(j.size))
		}
		return resumed, &transferError{io.ErrUnexpectedEOF, got - off}
	}
	if err := f.Sync(); err != nil {
		return resumed, &errs.Write{Err: err}
	}
	f.Close()
	if err := os.Rename(part, j.local); err != nil {
		return resumed, &errs.Write{Err: err}
	}
	if !j.mtime.IsZero() {
		os.Chtimes(j.local, time.Now(), j.mtime)
	}
	return resumed, nil
}

// receive appends the rest of j.remote to f, which already holds off bytes.
func (w *worker) receive(ctx context.Context, j job, f *os.File, off int64, bar *term.Bar) error {
	resp, err := w.conn.RetrFrom(j.remote, uint64(off))
	if err != nil && off > 0 && refused(err) {
		term.Debug("resume refused for %s (%s), downloading whole file", j.remote, term.Sanitize(err.Error()))
		if err := f.Truncate(0); err != nil {
			return &errs.Write{Err: err}
		}
		off = 0
		resp, err = w.conn.RetrFrom(j.remote, 0)
	}
	if err != nil {
		return err
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		resp.Close()
		return &errs.Write{Err: err}
	}
	bar.SetCurrent(off)
	w.at(off)
	dst := bar.Writer(&tally{w: w, d: f})
	stop := context.AfterFunc(ctx, func() { resp.SetDeadline(time.Now()) })
	b := &body{ctx: ctx, resp: resp}
	_, err = io.CopyBuffer(dst, b, make([]byte, 1<<20))
	stop()
	if err != nil {
		w.drop() // Close would wait for the server's final reply on a dead transfer
		resp.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var pe *os.PathError
		if errors.As(err, &pe) {
			return &errs.Write{Err: err}
		}
		return &transferError{err, b.n}
	}
	if err := resp.Close(); err != nil {
		// The size check decides whether the transfer was complete.
		term.Debug("%s: server said %s after the transfer", j.remote, term.Sanitize(err.Error()))
	}
	return nil
}

// body reads one RETR data connection. Every read gets a fresh deadline, so
// a server that stops sending ends the transfer after stallTimeout, and the
// caller's context.AfterFunc moves the deadline to now to end it at once.
// Nothing here closes the Response: Close reads the server's final reply
// and must run in the owning goroutine only.
type body struct {
	ctx  context.Context
	resp *ftp.Response
	n    int64
}

func (b *body) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	b.resp.SetDeadline(time.Now().Add(stallTimeout))
	n, err := b.resp.Read(p)
	b.n += int64(n)
	if err != nil && b.ctx.Err() != nil {
		err = b.ctx.Err()
	}
	return n, err
}
