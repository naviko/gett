package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"gett/internal/download"
	"gett/internal/errs"
	"gett/internal/term"
)

// sidecar is what gett knows about an item in progress, kept beside
// name.part as name.gett. A file records which byte spans are verified; a
// folder's contents are their own record.
type sidecar struct {
	Kind   string    `json:"kind"` // "file" or "folder"
	Sharer string    `json:"sharer"`
	Path   string    `json:"path"`
	Size   int64     `json:"size,omitempty"`
	MTime  time.Time `json:"mtime,omitempty"`
	Done   []span    `json:"done,omitempty"`
}

func (s span) MarshalJSON() ([]byte, error) { return json.Marshal([2]int64{s.Start, s.End}) }

func (s *span) UnmarshalJSON(b []byte) error {
	var a [2]int64
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	s.Start, s.End = a[0], a[1]
	return nil
}

// loadSidecar returns nil when there is none or it cannot be read.
func loadSidecar(p string) *sidecar {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var s sidecar
	if json.Unmarshal(b, &s) != nil || s.Kind == "" {
		return nil
	}
	return &s
}

func (s *sidecar) save(p string) error {
	b, _ := json.Marshal(s)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// matches reports whether the sidecar describes this item from this sharer.
func (s *sidecar) matches(sharer, path string, dir bool, size int64, mtime time.Time) bool {
	if s.Sharer != sharer || s.Path != path {
		return false
	}
	if dir {
		return s.Kind == "folder"
	}
	return s.Kind == "file" && s.Size == size && s.MTime.Equal(mtime)
}

var (
	errStall   = errors.New("no data")
	errShort   = errors.New("short unit")
	errCRC     = errors.New("checksum mismatch")
	errChanged = errors.New("changed")
)

// engine pulls one big file: work units of a few seconds each handed to as
// many connections as the link rewards, every unit checked against the
// sharer's CRC, verified spans recorded in the sidecar once a second.
type engine struct {
	ctx   context.Context
	p     *peer
	path  string // wire path
	stamp string
	f     *os.File
	meta  string
	side  *sidecar
	q     *queue
	bar   *term.Bar
	busy  *atomic.Int32 // connections another lane holds

	unit   atomic.Int64
	want   atomic.Int32
	active atomic.Int32
	moved  atomic.Int64 // bytes since the last window
	mu     sync.Mutex   // sidecar saves
}

// run moves every missing byte of the file into f and returns when the file
// is complete or something stopped it. The sidecar is current either way.
func (e *engine) run() error {
	e.unit.Store(firstUnit)
	ctl := newControl()
	if e.busy != nil {
		ctl.setMax(maxConns - int(e.busy.Load()))
	}
	g, gctx := errgroup.WithContext(e.ctx)
	spawn := func(n int) {
		for i := 0; i < n; i++ {
			e.active.Add(1)
			g.Go(func() error { return e.worker(gctx) })
		}
	}
	e.want.Store(int32(ctl.conns))
	spawn(ctl.conns)
	finished := make(chan error, 1)
	go func() { finished <- g.Wait() }()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case err := <-finished:
			e.save()
			if err != nil {
				return err
			}
			if !e.q.complete() {
				return errShort
			}
			return nil
		case <-tick.C:
			rate := float64(e.moved.Swap(0))
			if e.busy != nil {
				ctl.setMax(maxConns - int(e.busy.Load()))
			}
			conns, unit := ctl.observe(rate)
			e.unit.Store(unit)
			if more := conns - int(e.active.Load()); more > 0 && e.q.remaining() > 0 {
				spawn(more)
			}
			e.want.Store(int32(conns))
			e.save()
			term.Debug("%d connections, %s units, %s/s", conns, download.Human(unit), download.Human(int64(rate)))
		}
	}
}

func (e *engine) save() {
	e.mu.Lock()
	e.side.Done = e.q.snapshot()
	if err := e.side.save(e.meta); err != nil {
		term.Debug("sidecar: %v", err)
	}
	e.mu.Unlock()
}

// worker takes units until there are none, it is one connection too many,
// or something is wrong that another attempt will not fix.
func (e *engine) worker(ctx context.Context) error {
	defer e.active.Add(-1)
	attempts, bad := 0, 0
	for {
		for a := e.active.Load(); a > e.want.Load(); a = e.active.Load() {
			if e.active.CompareAndSwap(a, a-1) {
				e.active.Add(1) // the deferred Add(-1) does the real count
				return nil
			}
		}
		sp, ok := e.q.next(e.unit.Load())
		if !ok {
			return nil
		}
		err := e.fetch(ctx, sp)
		switch {
		case err == nil:
			e.q.finish(sp)
			attempts, bad = 0, 0
			continue
		case ctx.Err() != nil:
			e.q.fail(sp)
			return ctx.Err()
		case errors.Is(err, errStall), errors.Is(err, errShort):
			e.q.fail(sp)
			term.Debug("unit %d-%d: %v, asking again", sp.Start, sp.End, err)
		case errors.Is(err, errCRC):
			e.q.fail(sp)
			if bad++; bad >= 3 {
				return fmt.Errorf("data from %s arrived damaged", e.p.Name)
			}
			term.Debug("unit %d-%d: %v, asking again", sp.Start, sp.End, err)
		case status(err) == http.StatusPreconditionFailed:
			e.q.fail(sp)
			return errChanged
		case status(err) != 0, errors.As(err, new(*errs.Write)):
			e.q.fail(sp)
			return err
		default:
			e.q.fail(sp)
			if attempts++; attempts >= 8 {
				return err
			}
			term.Debug("unit %d-%d: %v, retrying", sp.Start, sp.End, err)
			select {
			case <-time.After(time.Duration(attempts) * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// fetch asks for one span, writes it in place and checks the CRC trailer.
func (e *engine) fetch(ctx context.Context, sp span) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	hdr := http.Header{}
	hdr.Set("Range", fmt.Sprintf("bytes=%d-%d", sp.Start, sp.End-1))
	hdr.Set("Gett-Stamp", e.stamp)
	resp, err := e.p.do(ctx, http.MethodGet, "/v1/file", url.Values{"path": {e.path}}, nil, hdr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return errShort
	}
	n := sp.End - sp.Start
	crc := crc32.New(castagnoli)
	sr := newStallReader(resp.Body, cancel)
	defer sr.stop()
	w := &unitWriter{e: e, off: sp.Start, crc: crc}
	buf := buffers.Get().(*[]byte)
	defer buffers.Put(buf)
	got, err := io.CopyBuffer(w, io.LimitReader(sr, n), *buf)
	switch {
	case err != nil && sr.stalled.Load():
		return errStall
	case err != nil:
		return err
	case got != n:
		return errShort
	}
	io.Copy(io.Discard, resp.Body) // reach the end so the trailer is in
	if want := resp.Trailer.Get("Gett-Crc32"); want != "" && want != fmt.Sprintf("%08x", crc.Sum32()) {
		return errCRC
	}
	return nil
}

// unitWriter puts bytes in place and counts them.
type unitWriter struct {
	e   *engine
	off int64
	crc io.Writer
}

func (w *unitWriter) Write(p []byte) (int, error) {
	n, err := w.e.f.WriteAt(p, w.off)
	w.off += int64(n)
	w.crc.Write(p[:n])
	w.e.moved.Add(int64(n))
	w.e.bar.Add(n)
	if err != nil {
		return n, &errs.Write{Err: err}
	}
	return n, nil
}

// stallReader cancels its request when no data arrives for stallTimeout.
type stallReader struct {
	r       io.Reader
	timer   *time.Timer
	stalled atomic.Bool
}

func newStallReader(r io.Reader, cancel context.CancelFunc) *stallReader {
	s := &stallReader{r: r}
	s.timer = time.AfterFunc(stallTimeout, func() {
		s.stalled.Store(true)
		cancel()
	})
	return s
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.timer.Reset(stallTimeout)
	}
	return n, err
}

func (s *stallReader) stop() { s.timer.Stop() }
