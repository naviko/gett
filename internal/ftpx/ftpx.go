// Package ftpx moves files to and from FTP/FTPS servers: upload, download,
// list and delete, over several connections, resuming and skipping what is
// already there.
package ftpx

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gett/internal/download"
	"gett/internal/errs"
	"gett/internal/term"

	"github.com/jlaffaye/ftp"
	"golang.org/x/sync/errgroup"
)

const (
	maxConns    = 8
	maxAttempts = 8 // reconnections per file before giving up
)

// stallTimeout ends a connection that moves nothing for this long; the
// transfer is then retried. A variable so tests can shorten it.
var stallTimeout = 30 * time.Second

type job struct {
	local, remote string
	size          int64 // -1 when unknown
	mtime         time.Time
}

type server struct {
	addr, host, user, pass string
	implicitTLS            bool
}

type stats struct {
	done, skipped, resumed, bytes atomic.Int64
}

// parse splits an FTP URL into the server and the path, which is relative to
// the login directory unless the URL has "//". folder reports a trailing
// slash or no path at all: the user means a directory.
func parse(rawURL string) (srv *server, p string, folder bool, err error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "ftp" && u.Scheme != "ftps") || u.Hostname() == "" {
		return nil, "", false, &errs.BadURL{URL: rawURL}
	}
	s := &server{host: u.Hostname(), user: "anonymous", pass: "anonymous@", implicitTLS: u.Scheme == "ftps"}
	port := u.Port()
	if port == "" {
		port = "21"
		if s.implicitTLS {
			port = "990"
		}
	}
	s.addr = net.JoinHostPort(s.host, port)
	if u.User != nil {
		s.user = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			s.pass = pw
		}
	}
	folder = u.Path == "" || strings.HasSuffix(u.Path, "/")
	// "ftp://host/pub" is relative to the login directory; "ftp://host//pub" is absolute.
	dir := strings.TrimPrefix(u.Path, "/")
	if dir == "" {
		dir = "."
	}
	return s, path.Clean(dir), folder, nil
}

// tlsConfig is how a server's certificate is checked; tests swap in their own roots.
var tlsConfig = func(host string) *tls.Config {
	return &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
}

func (s *server) dial(ctx context.Context) (*ftp.ServerConn, error) {
	cfg := tlsConfig(s.host)
	var c *ftp.ServerConn
	var err error
	if s.implicitTLS {
		c, err = ftp.Dial(s.addr, ftp.DialWithTLS(cfg), ftp.DialWithDialFunc(dialer(ctx, cfg, true)))
	} else {
		c, err = ftp.Dial(s.addr, ftp.DialWithExplicitTLS(cfg), ftp.DialWithDialFunc(dialer(ctx, cfg, false)))
		var ne net.Error
		if err != nil && (ctx.Err() != nil || errors.As(err, &ne)) {
			return nil, err // the server is not answering at all; plain FTP would not help
		}
		if err != nil {
			term.Debug("TLS not available on %s (%s), using plain FTP", s.addr, term.Sanitize(err.Error()))
			c, err = ftp.Dial(s.addr, ftp.DialWithDialFunc(dialer(ctx, nil, false)))
		}
	}
	if err != nil {
		return nil, err
	}
	if err := c.Login(s.user, s.pass); err != nil {
		c.Quit()
		if code(err) == ftp.StatusNotLoggedIn {
			return nil, &errs.Login{Host: s.host}
		}
		return nil, err
	}
	return c, nil
}

// dialer opens guarded connections, wrapped in TLS where the server expects
// it: everywhere for implicit TLS, on data connections for explicit TLS (the
// library upgrades the control connection itself after AUTH TLS). The
// library would do this wrapping only when it dials, and it must not: its
// connections have no deadlines, so a server that stops talking, or never
// greets, would hang gett, and Ctrl-C would not stop it.
func dialer(ctx context.Context, cfg *tls.Config, implicit bool) func(network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 30 * time.Second}
	var dials atomic.Int32
	return func(network, addr string) (net.Conn, error) {
		c, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		g := &guarded{Conn: c, ctx: ctx}
		g.stop = context.AfterFunc(ctx, func() { c.SetDeadline(time.Now()) })
		if cfg != nil && (implicit || dials.Add(1) > 1) {
			return tls.Client(g, cfg), nil
		}
		return g, nil
	}
}

// guarded is a connection that gives up when the server stops talking for
// stallTimeout or when ctx ends.
type guarded struct {
	net.Conn
	ctx  context.Context
	stop func() bool
}

func (g *guarded) Read(p []byte) (int, error) {
	if err := g.ctx.Err(); err != nil {
		return 0, err
	}
	g.Conn.SetReadDeadline(time.Now().Add(stallTimeout))
	n, err := g.Conn.Read(p)
	if err != nil && g.ctx.Err() != nil {
		err = g.ctx.Err()
	}
	return n, err
}

func (g *guarded) Write(p []byte) (int, error) {
	if err := g.ctx.Err(); err != nil {
		return 0, err
	}
	g.Conn.SetWriteDeadline(time.Now().Add(stallTimeout))
	n, err := g.Conn.Write(p)
	if err != nil && g.ctx.Err() != nil {
		err = g.ctx.Err()
	}
	return n, err
}

func (g *guarded) Close() error {
	g.stop()
	return g.Conn.Close()
}

// code is the server's reply code behind err, or 0.
func code(err error) int {
	var tp *textproto.Error
	if errors.As(err, &tp) {
		return tp.Code
	}
	return 0
}

// stat tells a file from a folder. MLST is authoritative. Without it, SIZE
// succeeding means a file and SIZE failing with 550 is settled by trying to
// list the path. A server without MLST that answers SIZE for directories
// would pass one off as a file; RETR then fails and its message is shown.
func stat(c *ftp.ServerConn, p string) (*ftp.Entry, error) {
	if c.IsTimePreciseInList() {
		e, err := c.GetEntry(p)
		if err == nil {
			e.Name = path.Base(p)
			return e, nil
		}
		if code(err) == ftp.StatusFileUnavailable {
			return nil, err
		}
	}
	n, err := c.FileSize(p)
	if err == nil {
		return &ftp.Entry{Name: path.Base(p), Type: ftp.EntryTypeFile, Size: uint64(n)}, nil
	}
	if code(err) == ftp.StatusFileUnavailable {
		if _, lerr := c.List(p); lerr == nil {
			return &ftp.Entry{Name: path.Base(p), Type: ftp.EntryTypeFolder}, nil
		}
	}
	return nil, err
}

// children is a listing of dir without what the server says about dir
// itself: ".", "..", and MLSD lines naming the directory. Names that come
// as full paths are reduced to their last element.
func children(dir string, entries []*ftp.Entry) []*ftp.Entry {
	out := make([]*ftp.Entry, 0, len(entries))
	for _, e := range entries {
		name := e.Name
		if strings.Contains(name, "/") {
			if path.Clean(name) == path.Clean(dir) {
				continue
			}
			name = path.Base(name)
		}
		if name == "" || name == "." || name == ".." {
			continue
		}
		if e.Type == ftp.EntryTypeFolder && name == dir {
			continue
		}
		c := *e
		c.Name = name
		out = append(out, &c)
	}
	return out
}

// walk visits every entry under dir, depth first. Folders are entered,
// links are reported but never followed.
func walk(ctx context.Context, c *ftp.ServerConn, dir string, fn func(p string, e *ftp.Entry) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := c.List(dir)
	if err != nil {
		return err
	}
	for _, e := range children(dir, entries) {
		p := path.Join(dir, e.Name)
		if err := fn(p, e); err != nil {
			return err
		}
		if e.Type == ftp.EntryTypeFolder {
			if err := walk(ctx, c, p, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// worker owns one connection. Connections are not shared: the protocol
// allows one transfer at a time per connection.
type worker struct {
	srv     *server
	conn    *ftp.ServerConn
	created *sync.Map
	st      *stats
	prog    *term.Progress
	overall *term.Bar // nil when there is a single file
	// credited is how much of the current job the overall bar has been given.
	// A job may take several attempts, each starting from wherever the
	// destination now ends; the bar must not be given that start twice.
	credited int64
}

// at tells the overall bar where the current job stands: at the bytes the
// destination already holds, no matter how many attempts it took to get there.
func (w *worker) at(n int64) {
	w.moved(int(n - w.credited))
}

// moved adds n bytes that just crossed the wire for the current job.
func (w *worker) moved(n int) {
	if w.overall != nil {
		w.overall.Add(n)
	}
	w.credited += int64(n)
}

// tally wraps a transfer so the overall bar follows it byte for byte.
type tally struct {
	w *worker
	r io.Reader
	d io.Writer
}

func (t *tally) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	t.w.moved(n)
	return n, err
}

func (t *tally) Write(p []byte) (int, error) {
	n, err := t.d.Write(p)
	t.w.moved(n)
	return n, err
}

// op moves one file over the worker's connection.
type op func(ctx context.Context, w *worker, j job) error

func (w *worker) drop() {
	if w.conn != nil {
		w.conn.Quit()
		w.conn = nil
	}
}

// run performs do for one file. A transfer that breaks off is picked up again
// on a new connection from where it stopped, so a bad link or an interrupted
// earlier run costs only the bytes that were in flight.
func (w *worker) run(ctx context.Context, j job, do op) error {
	var last error
	w.credited = 0
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			term.Debug("connection lost (%s), reconnecting", term.Sanitize(last.Error()))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if w.conn == nil {
			c, err := w.srv.dial(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if errors.As(err, new(*errs.Login)) {
					return err
				}
				last = err
				continue
			}
			w.conn = c
		}
		err := do(ctx, w, j)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil { // interrupted: the transfer was cut on purpose
			return ctx.Err()
		}
		if permanent(err) {
			return err
		}
		w.drop()
		last = err
	}
	return fmt.Errorf("connection to %s keeps dropping (%s)", w.srv.host, term.Sanitize(last.Error()))
}

// permanent reports an error that another attempt cannot fix: a server
// refusal, a local file problem, or an upload the server will not keep.
func permanent(err error) bool {
	if c := code(err); c != 0 {
		return c >= 500
	}
	return errors.As(err, new(*errs.Upload)) || errors.As(err, new(*errs.Write)) ||
		errors.As(err, new(*errs.Usage)) || errors.As(err, new(*os.PathError))
}

// refused is a definite no from the server before any data moved, as opposed
// to a connection that failed or a transient condition.
func refused(err error) bool { return code(err) >= 500 }

// transferError is a transfer that broke off after data had moved. It hides
// whatever the server said at that point, so it is never taken for a refusal.
type transferError struct {
	err error
	n   int64
}

func (e *transferError) Error() string {
	return fmt.Sprintf("transfer broke off after %s: %s", download.Human(e.n), e.err.Error())
}

// pool performs do on every job over up to maxConns connections; first is an
// open connection that one worker takes over. It draws the overall bar and
// marks a failure resumable when running the command again would finish it.
func pool(ctx context.Context, srv *server, first *ftp.ServerConn, jobs []job, total int64, do op) (*stats, error) {
	n := len(jobs)
	if n > maxConns {
		n = maxConns
	}
	if n < 1 {
		n = 1
	}
	term.Debug("%d files, %s, %d connections", len(jobs), download.Human(total), n)

	prog := term.NewProgress()
	overall := prog.Bar("total", total, false)
	var created sync.Map
	st := &stats{}
	ch := make(chan job)
	g, gctx := errgroup.WithContext(ctx)
	for i := 0; i < n; i++ {
		conn := first
		if i > 0 {
			conn = nil
		}
		g.Go(func() error {
			w := &worker{srv: srv, conn: conn, created: &created, st: st, prog: prog, overall: overall}
			defer w.drop()
			for j := range ch {
				if err := w.run(gctx, j, do); err != nil {
					return err
				}
			}
			return nil
		})
	}
feed:
	for _, j := range jobs {
		select {
		case ch <- j:
		case <-gctx.Done():
			break feed
		}
	}
	close(ch)
	if err := g.Wait(); err != nil {
		overall.Abort()
		prog.Shutdown()
		if ctx.Err() != nil || errs.CanResume(err) { // a rerun skips what is there and completes the rest
			return st, &errs.Resumable{Err: err}
		}
		return st, err
	}
	overall.Done()
	prog.Wait()
	return st, nil
}
