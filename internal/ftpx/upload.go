package ftpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/textproto"
	"os"
	"path"
	"path/filepath"

	"gett/internal/download"
	"gett/internal/errs"
	"gett/internal/term"

	"github.com/jlaffaye/ftp"
)

// Upload sends local (a file or a directory) into the directory named by rawURL.
func Upload(ctx context.Context, rawURL, local string) error {
	srv, remoteDir, _, err := parse(rawURL)
	if err != nil {
		return err
	}
	jobs, err := collect(local, remoteDir)
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		return &errs.Usage{Msg: "nothing to upload in " + local}
	}
	var total int64
	for _, j := range jobs {
		total += j.size
	}
	first, err := srv.dial(ctx)
	if err != nil {
		return err
	}
	st, err := pool(ctx, srv, first, jobs, total, upload)
	if err != nil {
		return err
	}
	term.Info("uploaded %d files (%s), skipped %d, resumed %d",
		st.done.Load(), download.Human(st.bytes.Load()), st.skipped.Load(), st.resumed.Load())
	return nil
}

func collect(local, remoteDir string) ([]job, error) {
	fi, err := os.Stat(local)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return []job{{local: local, remote: path.Join(remoteDir, filepath.Base(local)), size: fi.Size()}}, nil
	}
	var jobs []job
	err = filepath.WalkDir(local, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(local, p)
		jobs = append(jobs, job{local: p, remote: path.Join(remoteDir, filepath.ToSlash(rel)), size: info.Size()})
		return nil
	})
	return jobs, err
}

func upload(ctx context.Context, w *worker, j job) error {
	w.ensureDir(path.Dir(j.remote))
	remoteSize, sizeErr := w.conn.FileSize(j.remote)
	if sizeErr == nil && remoteSize == j.size {
		w.st.skipped.Add(1)
		w.at(j.size)
		term.Debug("skip %s (already there)", j.remote)
		return nil
	}
	f, err := os.Open(j.local)
	if err != nil {
		return err
	}
	defer f.Close()
	bar := w.prog.Bar(filepath.Base(j.local), j.size, true)
	defer bar.Done()

	resumed := false
	if sizeErr == nil && remoteSize > 0 && remoteSize < j.size {
		if _, err := f.Seek(remoteSize, io.SeekStart); err != nil {
			return err
		}
		bar.SetCurrent(remoteSize)
		w.at(remoteSize)
		src := &transfer{ctx: ctx, r: f}
		err := w.conn.StorFrom(j.remote, bar.Reader(&tally{w: w, r: src}), uint64(remoteSize))
		switch {
		case err == nil:
			resumed = true
		case ctx.Err() != nil:
			return ctx.Err()
		case src.n > 0 || !refused(err):
			// The transfer broke off: the caller reconnects and resumes
			// from what the server now has. Starting over would wipe it.
			return &transferError{err, src.n}
		default:
			term.Debug("resume refused for %s (%s), uploading whole file", j.remote, term.Sanitize(err.Error()))
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return err
			}
			bar.SetCurrent(0)
			w.at(0)
		}
	}
	if !resumed {
		src := &transfer{ctx: ctx, r: f}
		if err := w.conn.Stor(j.remote, bar.Reader(&tally{w: w, r: src})); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if src.n > 0 {
				return &transferError{err, src.n}
			}
			return err
		}
	}
	if err := w.verify(ctx, j, f, bar); err != nil {
		return err
	}
	w.st.done.Add(1)
	w.st.bytes.Add(j.size)
	if resumed {
		w.st.resumed.Add(1)
	}
	return nil
}

// verify checks the server's size; on mismatch it re-uploads once.
func (w *worker) verify(ctx context.Context, j job, f *os.File, bar *term.Bar) error {
	got, err := w.conn.FileSize(j.remote)
	if err != nil {
		var tp *textproto.Error
		if errors.As(err, &tp) && tp.Code != ftp.StatusFileUnavailable {
			term.Debug("server cannot report sizes, %s not verified", j.remote)
			return nil
		}
		return &errs.Upload{Msg: j.remote + " is missing after upload"}
	}
	if got == j.size {
		return nil
	}
	term.Debug("%s: server has %d bytes, local has %d, retrying", j.remote, got, j.size)
	w.conn.Delete(j.remote)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	bar.SetCurrent(0)
	w.at(0)
	src := &transfer{ctx: ctx, r: f}
	if err := w.conn.Stor(j.remote, bar.Reader(&tally{w: w, r: src})); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if src.n > 0 {
			return &transferError{err, src.n}
		}
		return err
	}
	if got, err = w.conn.FileSize(j.remote); err != nil || got != j.size {
		return &errs.Upload{Msg: fmt.Sprintf("%s: server has %d bytes, local has %d", j.remote, got, j.size)}
	}
	return nil
}

// transfer feeds a file to the server, counts what was sent, and ends the
// transfer as soon as ctx is done. The FTP library only honors the context
// while connecting; once data flows nothing else would stop it before the
// end of the file.
type transfer struct {
	ctx context.Context
	r   io.Reader
	n   int64
}

func (t *transfer) Read(p []byte) (int, error) {
	if err := t.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := t.r.Read(p)
	t.n += int64(n)
	return n, err
}

func (w *worker) ensureDir(dir string) {
	if dir == "." || dir == "/" || dir == "" {
		return
	}
	if _, ok := w.created.Load(dir); ok {
		return
	}
	w.ensureDir(path.Dir(dir))
	if err := w.conn.MakeDir(dir); err != nil {
		term.Debug("mkdir %s: %s", dir, term.Sanitize(err.Error()))
	}
	w.created.Store(dir, true)
}
