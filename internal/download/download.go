// Package download fetches a URL to a file over parallel connections and
// resumes interrupted downloads.
package download

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gett/internal/errs"
	"gett/internal/httpx"
	"gett/internal/term"

	"golang.org/x/sync/errgroup"
)

const (
	minChunk     = 1 << 20 // never split finer than 1 MiB
	maxConns     = 16
	headBytes    = 32 << 10
	maxForward   = 5
	copyBuf      = 1 << 20
	stallTimeout = 30 * time.Second // a connection that sends nothing for this long is retried
)

type Options struct {
	URL     string
	Name    string // output name chosen by the user, "" = automatic
	Key     string // identity for resume across different URLs, "" = URL
	Headers http.Header
	Client  *http.Client
}

// errNoRanges means the server answered a range request with the whole file:
// the parallel download gives way to a single stream.
var errNoRanges = errors.New("server stopped honoring range requests")

// state is the sidecar that makes resume possible.
type state struct {
	URL     string  `json:"url"`
	Key     string  `json:"key,omitempty"`
	Size    int64   `json:"size"`
	ETag    string  `json:"etag,omitempty"`
	LastMod string  `json:"last_modified,omitempty"`
	Chunks  []chunk `json:"chunks,omitempty"` // empty = single stream
}

type chunk struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"` // exclusive
	Done  int64 `json:"done"`
}

func (s *state) done() int64 {
	var n int64
	for _, c := range s.Chunks {
		n += c.Done
	}
	return n
}

func (s *state) complete() bool {
	for _, c := range s.Chunks {
		if c.Done < c.End-c.Start {
			return false
		}
	}
	return true
}

// prefix is how many bytes from the start of the file are known good:
// the chunks are contiguous, so it ends at the first unfinished one.
func (s *state) prefix() int64 {
	var n int64
	for _, c := range s.Chunks {
		n += c.Done
		if c.Done < c.End-c.Start {
			break
		}
	}
	return n
}

// Run downloads o.URL and returns the path it was saved under. When stdout is
// not a terminal the data streams there and the path is empty.
func Run(ctx context.Context, o Options) (saved string, err error) {
	if _, err := httpx.ParseURL(o.URL); err != nil {
		return "", err
	}
	resp, head, err := probe(ctx, o, 0)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	size, ranges := rangeInfo(resp)
	prog := term.NewProgress()

	if !term.StdoutTTY() {
		bar := prog.Bar("stdout", size, false)
		err := copyAll(bar.Writer(os.Stdout), head, resp.Body)
		bar.Done()
		prog.Wait()
		return "", err
	}

	dir, name, err := Target(o.Name)
	if err != nil {
		return "", &errs.Write{Err: err}
	}
	if name == "" {
		name = deriveName(resp, head)
	}
	name = filepath.Join(dir, name)
	key := o.Key
	if key == "" {
		key = o.URL
	}
	st, name, resumed := pickTarget(name, o.URL, key, size, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"))
	part := name + ".part"
	meta := name + ".gett"

	var f *os.File
	if resumed {
		f, err = os.OpenFile(part, os.O_RDWR, 0o644)
	} else {
		f, err = os.OpenFile(part, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	}
	if err != nil {
		return "", &errs.Write{Err: err}
	}
	defer f.Close()

	chunked := ranges && size > minChunk
	if !resumed {
		st = &state{URL: o.URL, Key: key, Size: size, ETag: resp.Header.Get("ETag"), LastMod: resp.Header.Get("Last-Modified")}
		if chunked {
			st.Chunks = split(size, chunkCount(size))
			if err := f.Truncate(size); err != nil {
				return "", &errs.Write{Err: err}
			}
		}
		if err := saveState(meta, st); err != nil {
			return "", &errs.Write{Err: err}
		}
	}
	if resumed {
		term.Info("resuming %s", name)
	}

	bar := prog.Bar(name, size, false)
	if len(st.Chunks) > 0 {
		src := newSource(o, resp, size)
		resp.Body.Close() // the chunks open their own connections
		err = parallel(ctx, o, src, f, st, meta, head, resumed, bar)
		if errors.Is(err, errNoRanges) {
			p := st.prefix()
			term.Debug("ranges stopped working, continuing with one connection from %s", human(p))
			st.Chunks = nil
			if serr := saveState(meta, st); serr != nil {
				err = &errs.Write{Err: serr}
			} else {
				err = stream(ctx, o, f, size, p, bar)
			}
		}
	} else {
		err = single(ctx, o, f, resp, head, size, ranges, resumed, bar)
	}
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

	if err := f.Sync(); err != nil {
		return "", &errs.Write{Err: err}
	}
	f.Close()
	if err := os.Rename(part, name); err != nil {
		return "", &errs.Write{Err: err}
	}
	os.Remove(meta)
	if t, err := http.ParseTime(st.LastMod); err == nil {
		os.Chtimes(name, time.Now(), t)
	}
	if fi, err := os.Stat(name); err == nil {
		term.Info("saved %s (%s)", name, human(fi.Size()))
	}
	return name, nil
}

// rangeInfo reads the file size and whether the server honors ranges from
// the answer to a "bytes=N-" request.
func rangeInfo(resp *http.Response) (size int64, ranges bool) {
	if resp.StatusCode == http.StatusPartialContent {
		if size = httpx.ContentRangeTotal(resp.Header.Get("Content-Range")); size < 0 {
			size = resp.ContentLength
		}
		return size, true
	}
	return resp.ContentLength, strings.Contains(resp.Header.Get("Accept-Ranges"), "bytes")
}

// probe issues the first GET for bytes from the given offset, follows HTML
// forwarders, and returns the response with the first bytes of the body
// already read.
func probe(ctx context.Context, o Options, from int64) (*http.Response, []byte, error) {
	u, referer := o.URL, ""
	for hop := 0; hop < maxForward; hop++ {
		req, err := httpx.NewRequest(ctx, http.MethodGet, u, nil, o.Headers)
		if err != nil {
			return nil, nil, err
		}
		if referer != "" && req.Header.Get("Referer") == "" {
			req.Header.Set("Referer", referer)
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", from))
		resp, err := o.Client.Do(req)
		if err != nil {
			return nil, nil, err
		}
		if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
			resp.Body.Close()
			req.Header.Del("Range")
			if resp, err = o.Client.Do(req); err != nil {
				return nil, nil, err
			}
		}
		if resp.StatusCode >= 400 {
			resp.Body.Close()
			return nil, nil, &errs.HTTPStatus{Code: resp.StatusCode, Status: term.Sanitize(resp.Status), URL: resp.Request.URL.Redacted()}
		}
		head, err := io.ReadAll(io.LimitReader(resp.Body, headBytes))
		if err != nil {
			resp.Body.Close()
			return nil, nil, err
		}
		// A forwarder is a whole small page; a partial answer is file data.
		if resp.StatusCode != http.StatusPartialContent {
			if fwd := httpx.Forwarder(resp, head); fwd != "" && fwd != u {
				resp.Body.Close()
				term.Debug("-> forwards to %s", term.Sanitize(fwd))
				referer = refererFor(resp.Request.URL, fwd)
				u = fwd
				continue
			}
		}
		return resp, head, nil
	}
	return nil, nil, &errs.Redirect{Msg: fmt.Sprintf("more than %d forwards", maxForward)}
}

// refererFor is the Referer a browser sends when page from leads to the URL
// to: none from a secure page to a plain one, and never with credentials.
func refererFor(from *url.URL, to string) string {
	if t, err := url.Parse(to); err != nil || (from.Scheme == "https" && t.Scheme == "http") {
		return ""
	}
	c := *from
	c.User = nil
	return c.String()
}

// source is where the chunk requests go: the URL after redirects together
// with the Referer and headers a browser would send to it. When a link stops
// working (expired token, lost Referer) refresh asks o.URL again; gen tells
// a worker whether another one already did.
type source struct {
	mu      sync.Mutex
	url     string
	referer string
	headers http.Header
	gen     int
	size    int64
	etag    string
}

func newSource(o Options, resp *http.Response, size int64) *source {
	s := &source{size: size, etag: resp.Header.Get("ETag")}
	s.set(o, resp)
	return s
}

func (s *source) set(o Options, resp *http.Response) {
	s.url = resp.Request.URL.String()
	s.referer = resp.Request.Header.Get("Referer")
	s.headers = hopHeaders(o, resp.Request.URL)
}

// request builds a GET for the range rng against the current source.
func (s *source) request(ctx context.Context, rng string) (*http.Request, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	req, err := httpx.NewRequest(ctx, http.MethodGet, s.url, nil, s.headers)
	if err != nil {
		return nil, 0, err
	}
	if s.referer != "" && req.Header.Get("Referer") == "" {
		req.Header.Set("Referer", s.referer)
	}
	req.Header.Set("Range", rng)
	return req, s.gen, nil
}

// refresh renews the source unless another worker did since generation seen.
// It returns the current generation.
func (s *source) refresh(ctx context.Context, o Options, seen int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gen != seen {
		return s.gen, nil
	}
	resp, _, err := probe(ctx, o, 0)
	if err != nil {
		return s.gen, err
	}
	resp.Body.Close()
	size, ranges := rangeInfo(resp)
	if !ranges {
		return s.gen, errNoRanges
	}
	if size != s.size || (s.etag != "" && resp.Header.Get("ETag") != s.etag) {
		return s.gen, &errs.Range{Msg: "the file on the server changed; run again to start over"}
	}
	s.set(o, resp)
	s.gen++
	term.Debug("link renewed")
	return s.gen, nil
}

// hopHeaders drops the headers a browser does not carry to another site,
// the way net/http does when it follows a redirect there.
func hopHeaders(o Options, final *url.URL) http.Header {
	first, err := httpx.ParseURL(o.URL)
	if err != nil || sameSite(final.Hostname(), first.Hostname()) {
		return o.Headers
	}
	h := o.Headers.Clone()
	for _, k := range []string{"Authorization", "Cookie", "Cookie2", "Www-Authenticate"} {
		h.Del(k)
	}
	return h
}

func sameSite(sub, parent string) bool {
	sub, parent = strings.ToLower(sub), strings.ToLower(parent)
	if sub == parent {
		return true
	}
	if strings.ContainsAny(sub, ":%") { // IP addresses
		return false
	}
	return strings.HasSuffix(sub, "."+parent)
}

// pickTarget decides the output name: resume a matching partial download,
// otherwise the first name that does not exist yet.
func pickTarget(name, source, key string, size int64, etag, lastMod string) (*state, string, bool) {
	cand := name
	for i := 1; ; i++ {
		if st := loadState(cand + ".gett"); st != nil {
			// Validators differ between mirrors of the same content, so they
			// only count when the key is the URL itself.
			same := st.Key == key && st.Size == size && (key != source || (st.ETag == etag && st.LastMod == lastMod))
			if same && exists(cand+".part") && !exists(cand) {
				st.URL = source
				return st, cand, true
			}
			term.Debug("stale partial download %s, starting over", cand)
			os.Remove(cand + ".gett")
			os.Remove(cand + ".part")
		}
		if !exists(cand) && !exists(cand+".part") {
			return nil, cand, false
		}
		cand = numbered(name, i)
	}
}

func chunkCount(size int64) int {
	n := int(size / minChunk)
	if n > maxConns {
		n = maxConns
	}
	if n < 1 {
		n = 1
	}
	return n
}

func split(size int64, n int) []chunk {
	cs := make([]chunk, n)
	per := size / int64(n)
	for i := range cs {
		cs[i].Start = int64(i) * per
		cs[i].End = cs[i].Start + per
	}
	cs[n-1].End = size
	return cs
}

func loadState(p string) *state {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var st state
	if json.Unmarshal(b, &st) != nil || st.URL == "" {
		return nil // not ours: a transfer in progress keeps its sidecar
	}
	if st.Key == "" {
		st.Key = st.URL
	}
	return &st
}

func saveState(p string, st *state) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// parallel downloads every unfinished chunk at once.
func parallel(ctx context.Context, o Options, src *source, f *os.File, st *state, meta string, head []byte, resumed bool, bar *term.Bar) error {
	var mu sync.Mutex
	if !resumed && len(head) > 0 {
		n := int64(len(head))
		if n > st.Chunks[0].End {
			n = st.Chunks[0].End
		}
		if _, err := f.WriteAt(head[:n], 0); err != nil {
			return &errs.Write{Err: err}
		}
		st.Chunks[0].Done = n
	}
	bar.SetCurrent(st.done())
	term.Debug("%d connections", len(st.Chunks))

	stop := make(chan struct{})
	var saver sync.WaitGroup
	saver.Add(1)
	go func() {
		defer saver.Done()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				mu.Lock()
				saveState(meta, st)
				mu.Unlock()
			case <-stop:
				return
			}
		}
	}()

	g, gctx := errgroup.WithContext(ctx)
	for i := range st.Chunks {
		c := &st.Chunks[i]
		if c.Done >= c.End-c.Start {
			continue
		}
		g.Go(func() error { return fetchChunk(gctx, o, src, f, c, &mu, bar) })
	}
	err := g.Wait()
	close(stop)
	saver.Wait()
	mu.Lock()
	saveErr := saveState(meta, st)
	mu.Unlock()
	if err != nil {
		return err
	}
	if saveErr != nil {
		return &errs.Write{Err: saveErr}
	}
	if !st.complete() {
		return fmt.Errorf("download ended before all data arrived: %w", io.ErrUnexpectedEOF)
	}
	return nil
}

func fetchChunk(ctx context.Context, o Options, src *source, f *os.File, c *chunk, mu *sync.Mutex, bar *term.Bar) error {
	var last error
	fresh := -1 // generation of a renewed link this worker has not received data from yet
	for attempt := 0; attempt < 8; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		mu.Lock()
		off := c.Start + c.Done
		mu.Unlock()
		if off >= c.End {
			return nil
		}
		reqCtx, cancel := context.WithCancel(ctx)
		req, gen, err := src.request(reqCtx, fmt.Sprintf("bytes=%d-%d", off, c.End-1))
		if err != nil {
			cancel()
			return err
		}
		resp, err := o.Client.Do(req)
		if err != nil {
			cancel()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			last = err
			continue
		}
		switch {
		case resp.StatusCode == http.StatusPartialContent:
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			resp.Body.Close()
			cancel()
			wait := retryAfter(resp, time.Duration(attempt+1)*3*time.Second)
			term.Debug("server said %s, waiting %s", term.Sanitize(resp.Status), wait)
			last = &errs.HTTPStatus{Code: resp.StatusCode, Status: term.Sanitize(resp.Status), URL: resp.Request.URL.Redacted()}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		case resp.StatusCode == http.StatusOK:
			resp.Body.Close()
			cancel()
			return errNoRanges
		case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
			resp.Body.Close()
			cancel()
			return &errs.Range{Msg: fmt.Sprintf("server has a different file now (%s)", term.Sanitize(resp.Status))}
		default:
			// 401, 403, 404...: the link may have expired or lost what the
			// server wants to see. Ask the original URL for a fresh one; the
			// same answer from a fresh link is final.
			status := &errs.HTTPStatus{Code: resp.StatusCode, Status: term.Sanitize(resp.Status), URL: resp.Request.URL.Redacted()}
			resp.Body.Close()
			cancel()
			if gen == fresh {
				return status
			}
			term.Debug("server said %s, renewing the link", status.Status)
			g, err := src.refresh(ctx, o, gen)
			switch {
			case err == nil:
				fresh = g
				last = status
				continue
			case ctx.Err() != nil:
				return ctx.Err()
			case errors.Is(err, errNoRanges), !errs.CanResume(err), errors.As(err, new(*errs.HTTPStatus)):
				return err
			default: // network trouble while renewing: an ordinary retry
				last = err
				continue
			}
		}
		fresh = -1
		w := &chunkWriter{f: f, off: off, c: c, mu: mu, bar: bar}
		body := newStallReader(io.LimitReader(resp.Body, c.End-off), cancel)
		_, err = io.CopyBuffer(w, body, make([]byte, copyBuf))
		body.stop()
		resp.Body.Close()
		cancel()
		if err == nil {
			mu.Lock()
			short := c.Done < c.End-c.Start
			mu.Unlock()
			if !short {
				return nil
			}
			term.Debug("connection ended early, retrying range from %d", c.Start+c.Done)
			last = io.ErrUnexpectedEOF
			continue
		}
		if body.stalled.Load() {
			term.Debug("connection stalled, retrying range from %d", c.Start+c.Done)
			last = fmt.Errorf("connection stalled")
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.As(err, new(*errs.Write)) {
			return err
		}
		last = err
	}
	return last
}

type chunkWriter struct {
	f   *os.File
	off int64
	c   *chunk
	mu  *sync.Mutex
	bar *term.Bar
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	n, err := w.f.WriteAt(p, w.off)
	w.off += int64(n)
	w.mu.Lock()
	w.c.Done += int64(n)
	w.mu.Unlock()
	w.bar.Add(n)
	if err != nil {
		return n, &errs.Write{Err: err}
	}
	return n, nil
}

// single streams the body with one connection, resuming from the partial
// file's size when the server allows ranges.
func single(ctx context.Context, o Options, f *os.File, resp *http.Response, head []byte, size int64, ranges, resumed bool, bar *term.Bar) error {
	if !resumed {
		return copyAll(bar.Writer(f), head, resp.Body)
	}
	fi, err := f.Stat()
	if err != nil {
		return &errs.Write{Err: err}
	}
	if off := fi.Size(); off > 0 && ranges {
		resp.Body.Close()
		return stream(ctx, o, f, size, off, bar)
	}
	if err := f.Truncate(0); err != nil {
		return &errs.Write{Err: err}
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return &errs.Write{Err: err}
	}
	return copyAll(bar.Writer(f), head, resp.Body)
}

// stream continues a single-stream download at off. It asks o.URL again, so
// redirects and the Referer a browser would send are honored, writes from
// off when the server answers 206, and starts over otherwise.
func stream(ctx context.Context, o Options, f *os.File, size, off int64, bar *term.Bar) error {
	resp, head, err := probe(ctx, o, off)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if off > 0 {
		if resp.StatusCode != http.StatusPartialContent {
			term.Debug("server ignored resume, starting over")
			off = 0
		} else if t := httpx.ContentRangeTotal(resp.Header.Get("Content-Range")); size > 0 && t >= 0 && t != size {
			return &errs.Range{Msg: "the file on the server changed; run again to start over"}
		}
	}
	if err := f.Truncate(off); err != nil {
		return &errs.Write{Err: err}
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return &errs.Write{Err: err}
	}
	bar.SetCurrent(off)
	return copyAll(bar.Writer(f), head, resp.Body)
}

func copyAll(w io.Writer, head []byte, body io.Reader) error {
	if len(head) > 0 {
		if _, err := w.Write(head); err != nil {
			return &errs.Write{Err: err}
		}
	}
	_, err := io.CopyBuffer(struct{ io.Writer }{w}, body, make([]byte, copyBuf))
	var pe *os.PathError
	if errors.As(err, &pe) {
		return &errs.Write{Err: err}
	}
	return err
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

// retryAfter honors a Retry-After header (seconds or date), else returns def.
func retryAfter(resp *http.Response, def time.Duration) time.Duration {
	h := resp.Header.Get("Retry-After")
	if h == "" {
		return def
	}
	if secs, err := strconv.Atoi(h); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return def
}
