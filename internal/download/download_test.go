package download

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gett/internal/errs"
	"gett/internal/httpx"
	"gett/internal/term"
)

// rangeServer serves data with or without range support. When gate is
// non-nil, every response blocks after gateAfter bytes until gate closes.
// The CDN-like knobs (requireReferer, token, rangesAs200, denyChunks) are
// off at their zero values.
type rangeServer struct {
	data           []byte
	ranges         bool
	name           string // Content-Disposition filename
	ctype          string
	gate           chan struct{}
	gateAfter      int
	requireReferer string        // 403 unless the Referer matches
	token          func() string // 403 unless ?access= matches
	rangesAs200    bool          // bounded ranges (chunks) get the whole file
	denyChunks     bool          // bounded ranges (chunks) always get 403
	requests       atomic.Int64
	denied         atomic.Int64
	mu             sync.Mutex
	rangeStarts    []int64
}

func (s *rangeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.requests.Add(1)
	rh := r.Header.Get("Range")
	bounded := rh != "" && !strings.HasSuffix(rh, "-")
	if (s.requireReferer != "" && r.Header.Get("Referer") != s.requireReferer) ||
		(s.token != nil && r.URL.Query().Get("access") != s.token()) ||
		(s.denyChunks && bounded) {
		s.denied.Add(1)
		w.Header().Set("X-Deny", "test")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if bounded && s.rangesAs200 {
		rh = ""
	}
	if s.name != "" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+s.name+`"`)
	}
	if s.ctype != "" {
		w.Header().Set("Content-Type", s.ctype)
	}
	w.Header().Set("ETag", `"abc"`)
	start, end := int64(0), int64(len(s.data))
	status := 200
	if rh != "" && s.ranges {
		w.Header().Set("Accept-Ranges", "bytes")
		spec := strings.TrimPrefix(rh, "bytes=")
		a, b, _ := strings.Cut(spec, "-")
		start, _ = strconv.ParseInt(a, 10, 64)
		if b != "" {
			e, _ := strconv.ParseInt(b, 10, 64)
			end = e + 1
		}
		s.mu.Lock()
		s.rangeStarts = append(s.rangeStarts, start)
		s.mu.Unlock()
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, len(s.data)))
		status = 206
	} else if s.ranges {
		w.Header().Set("Accept-Ranges", "bytes")
	}
	w.Header().Set("Content-Length", strconv.FormatInt(end-start, 10))
	w.WriteHeader(status)
	body := s.data[start:end]
	if s.gate != nil {
		n := s.gateAfter
		if n > len(body) {
			n = len(body)
		}
		w.Write(body[:n])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-s.gate:
		case <-r.Context().Done():
			return
		}
		body = body[n:]
	}
	w.Write(body)
}

func setup(t *testing.T) {
	t.Helper()
	tty := true
	term.StdoutIsTTY = &tty
	term.Silent = true
	t.Cleanup(func() { term.StdoutIsTTY = nil })
	dir := t.TempDir()
	old, _ := os.Getwd()
	os.Chdir(dir)
	t.Cleanup(func() { os.Chdir(old) })
}

func randomData(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func client(t *testing.T) *http.Client {
	c, err := httpx.NewClient(httpx.Config{Retries: true})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func fileHash(t *testing.T, name string) [32]byte {
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

// frontServer plays the download page that redirects to a CDN, handing out
// the current token each time.
type frontServer struct {
	*httptest.Server
	requests atomic.Int64
}

func front(t *testing.T, cdn *httptest.Server, token func() string) *frontServer {
	f := &frontServer{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		target := cdn.URL + "/file.bin"
		if token != nil {
			target += "?access=" + token()
		}
		http.Redirect(w, r, target, http.StatusFound)
	}))
	t.Cleanup(f.Close)
	return f
}

// waitFor polls until cond holds, failing the test after a few seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// interrupt runs a download and cancels it once cond holds.
func interrupt(t *testing.T, o Options, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := Run(ctx, o); done <- err }()
	waitFor(t, "download never started", cond)
	time.Sleep(100 * time.Millisecond)
	cancel()
	if err := <-done; err == nil {
		t.Fatal("expected interrupted download to fail")
	}
}

func TestParallelDownload(t *testing.T) {
	setup(t)
	data := randomData(20 << 20)
	srv := httptest.NewServer(&rangeServer{data: data, ranges: true})
	defer srv.Close()

	_, err := Run(context.Background(), Options{URL: srv.URL + "/big.bin", Client: client(t)})
	if err != nil {
		t.Fatal(err)
	}
	if fileHash(t, "big.bin") != sha256.Sum256(data) {
		t.Fatal("content mismatch")
	}
	if exists("big.bin.part") || exists("big.bin.gett") {
		t.Fatal("temporary files left behind")
	}
}

func TestSingleStreamWhenNoRanges(t *testing.T) {
	setup(t)
	data := randomData(3 << 20)
	srv := httptest.NewServer(&rangeServer{data: data, ranges: false})
	defer srv.Close()
	if _, err := Run(context.Background(), Options{URL: srv.URL + "/plain.bin", Client: client(t)}); err != nil {
		t.Fatal(err)
	}
	if fileHash(t, "plain.bin") != sha256.Sum256(data) {
		t.Fatal("content mismatch")
	}
}

func TestResume(t *testing.T) {
	setup(t)
	data := randomData(8 << 20)
	rs := &rangeServer{data: data, ranges: true, gate: make(chan struct{}), gateAfter: 64 << 10}
	srv := httptest.NewServer(rs)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := Run(ctx, Options{URL: srv.URL + "/file.bin", Client: client(t)}); done <- err }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if st := loadState("file.bin.gett"); st != nil && len(st.Chunks) > 0 && rs.requests.Load() > int64(len(st.Chunks)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("download never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	cancel()
	if err := <-done; err == nil {
		t.Fatal("expected interrupted download to fail")
	}
	st := loadState("file.bin.gett")
	if st == nil || st.done() == 0 {
		t.Fatalf("no progress saved: %+v", st)
	}
	if st.complete() {
		t.Fatal("download should be incomplete")
	}
	saved := st.done()

	close(rs.gate)
	rs.requests.Store(0)
	if _, err := Run(context.Background(), Options{URL: srv.URL + "/file.bin", Client: client(t)}); err != nil {
		t.Fatal(err)
	}
	if fileHash(t, "file.bin") != sha256.Sum256(data) {
		t.Fatal("content mismatch after resume")
	}
	if exists("file.bin.gett") || exists("file.bin.part") {
		t.Fatal("temporary files left behind")
	}
	t.Logf("resumed with %d of %d bytes already saved, %d requests on second run", saved, len(data), rs.requests.Load())
}

func TestNamingAndNoOverwrite(t *testing.T) {
	setup(t)
	data := []byte("%PDF-1.4 hello")
	srv := httptest.NewServer(&rangeServer{data: data, ranges: true, name: "../../evil\u0085name", ctype: "application/pdf"})
	defer srv.Close()
	for i := 0; i < 3; i++ {
		if _, err := Run(context.Background(), Options{URL: srv.URL + "/dl?id=1", Client: client(t)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{"evilname.pdf", "evilname (1).pdf", "evilname (2).pdf"} {
		if !exists(want) {
			entries, _ := filepath.Glob("*")
			t.Fatalf("missing %q, have %v", want, entries)
		}
	}
}

func TestExtensionFromContentType(t *testing.T) {
	setup(t)
	srv := httptest.NewServer(&rangeServer{data: []byte(`{"a":1}`), ranges: false, ctype: "application/json"})
	defer srv.Close()
	if _, err := Run(context.Background(), Options{URL: srv.URL + "/api/data", Client: client(t)}); err != nil {
		t.Fatal(err)
	}
	if !exists("data.json") {
		entries, _ := filepath.Glob("*")
		t.Fatalf("expected data.json, have %v", entries)
	}
}

func TestUserName(t *testing.T) {
	setup(t)
	srv := httptest.NewServer(&rangeServer{data: []byte("x"), ranges: false})
	defer srv.Close()
	if _, err := Run(context.Background(), Options{URL: srv.URL + "/a", Name: "mine.txt", Client: client(t)}); err != nil {
		t.Fatal(err)
	}
	if !exists("mine.txt") {
		t.Fatal("expected mine.txt")
	}
}

func TestHTTPError(t *testing.T) {
	setup(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	_, err := Run(context.Background(), Options{URL: srv.URL + "/missing", Client: client(t)})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected 404 error, got %v", err)
	}
}

func TestClean(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd":  "passwd",
		"a\x1b[31mb":        "a[31mb",
		"..":                "download",
		"":                  "download",
		"dir/sub\\file.txt": "file.txt",
		"  spaced.txt  ":    "spaced.txt",
	}
	for in, want := range cases {
		if got := clean(in); got != want {
			t.Errorf("clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExtensionRules(t *testing.T) {
	setup(t)
	binary := make([]byte, 4096)
	rand.Read(binary)
	cases := []struct {
		path, ctype string
		body        []byte
		want        string
	}{
		{"/fw/PS5UPDATE.PUP", "text/plain", binary, "PS5UPDATE.PUP"},                         // URL extension always wins
		{"/get/archive", "text/plain", []byte("PK\x03\x04" + string(binary)), "archive.zip"}, // bytes beat a wrong Content-Type
		{"/get/blob", "text/plain", binary, "blob"},                                          // unknown binary, text type not trusted
		{"/get/doc", "application/pdf", binary, "doc.pdf"},                                   // specific type used when bytes are unknown
		{"/get/notes", "text/plain", []byte("just some words\n"), "notes.txt"},
		{"/get/page", "", []byte("<!DOCTYPE html><html><body>hi</body></html>"), "page.html"},
	}
	for _, c := range cases {
		srv := httptest.NewServer(&rangeServer{data: c.body, ranges: false, ctype: c.ctype})
		_, err := Run(context.Background(), Options{URL: srv.URL + c.path, Client: client(t)})
		srv.Close()
		if err != nil {
			t.Fatalf("%s: %v", c.path, err)
		}
		if !exists(c.want) {
			entries, _ := filepath.Glob("*")
			t.Errorf("%s with %q: expected %q, have %v", c.path, c.ctype, c.want, entries)
		}
	}
}

func TestFolderTargets(t *testing.T) {
	setup(t)
	srv := httptest.NewServer(&rangeServer{data: []byte("%PDF-1.4 x"), ranges: false, ctype: "application/pdf"})
	defer srv.Close()
	run := func(name string) {
		t.Helper()
		if _, err := Run(context.Background(), Options{URL: srv.URL + "/files/report.pdf", Name: name, Client: client(t)}); err != nil {
			t.Fatalf("%q: %v", name, err)
		}
	}
	run("downloads/")              // trailing slash: new folder, automatic name
	run("downloads/")              // again: never overwrite inside the folder
	run("deep/er/custom.pdf")      // path with a file name, folders created
	os.MkdirAll("existing", 0o755) // existing folder without slash
	run("existing")
	for _, want := range []string{"downloads/report.pdf", "downloads/report (1).pdf", "deep/er/custom.pdf", "existing/report.pdf"} {
		if !exists(want) {
			entries, _ := filepath.Glob("*/*")
			t.Errorf("missing %q, have %v", want, entries)
		}
	}
	if exists("report.pdf") || exists("download") {
		t.Error("file leaked into the current directory")
	}
}

func TestRefererFollowsRedirect(t *testing.T) {
	setup(t)
	data := randomData(20 << 20)
	rs := &rangeServer{data: data, ranges: true}
	cdn := httptest.NewServer(rs)
	defer cdn.Close()
	page := front(t, cdn, nil)
	rs.requireReferer = page.URL + "/download/x"

	if _, err := Run(context.Background(), Options{URL: page.URL + "/download/x", Client: client(t)}); err != nil {
		t.Fatal(err)
	}
	if fileHash(t, "file.bin") != sha256.Sum256(data) {
		t.Fatal("content mismatch")
	}
	if n := rs.denied.Load(); n != 0 {
		t.Fatalf("%d chunk requests were denied: Referer not carried to the CDN", n)
	}

	// A Referer given by the user is sent as is, also on the chunks.
	rs.requireReferer = "https://me/"
	h := http.Header{"Referer": {"https://me/"}}
	if _, err := Run(context.Background(), Options{URL: page.URL + "/download/x", Name: "mine.bin", Headers: h, Client: client(t)}); err != nil {
		t.Fatal(err)
	}
	if fileHash(t, "mine.bin") != sha256.Sum256(data) || rs.denied.Load() != 0 {
		t.Fatal("user Referer not honored")
	}
}

func TestExpiredLinkIsRenewed(t *testing.T) {
	setup(t)
	data := randomData(20 << 20)
	var tok atomic.Int64
	rs := &rangeServer{data: data, ranges: true, token: func() string { return strconv.FormatInt(tok.Load(), 10) }}
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rs.requests.Load() >= 6 {
			tok.CompareAndSwap(0, 1) // the token expires while chunks are in flight
		}
		rs.ServeHTTP(w, r)
	}))
	defer cdn.Close()
	page := front(t, cdn, rs.token)

	if _, err := Run(context.Background(), Options{URL: page.URL + "/download/x", Client: client(t)}); err != nil {
		t.Fatal(err)
	}
	if fileHash(t, "file.bin") != sha256.Sum256(data) {
		t.Fatal("content mismatch")
	}
	if rs.denied.Load() == 0 {
		t.Fatal("test did not exercise an expired link")
	}
	if n := page.requests.Load(); n != 2 {
		t.Fatalf("link was fetched %d times, want 2 (first probe and one renewal)", n)
	}
	if exists("file.bin.part") || exists("file.bin.gett") {
		t.Fatal("temporary files left behind")
	}
}

func TestFallbackToSingleStream(t *testing.T) {
	setup(t)
	data := randomData(6 << 20)
	rs := &rangeServer{data: data, ranges: true, rangesAs200: true}
	srv := httptest.NewServer(rs)
	defer srv.Close()

	if _, err := Run(context.Background(), Options{URL: srv.URL + "/big.bin", Client: client(t)}); err != nil {
		t.Fatal(err)
	}
	if fileHash(t, "big.bin") != sha256.Sum256(data) {
		t.Fatal("content mismatch")
	}
	if exists("big.bin.part") || exists("big.bin.gett") {
		t.Fatal("temporary files left behind")
	}
}

func TestPersistentForbiddenIsNotResumable(t *testing.T) {
	setup(t)
	rs := &rangeServer{data: randomData(4 << 20), ranges: true, denyChunks: true}
	cdn := httptest.NewServer(rs)
	defer cdn.Close()
	page := front(t, cdn, nil)

	_, err := Run(context.Background(), Options{URL: page.URL + "/download/x", Client: client(t)})
	if err == nil {
		t.Fatal("expected failure")
	}
	msg := errs.Message(err)
	if errs.Code(err) != 22 || !strings.Contains(msg, "403") {
		t.Fatalf("want a 403 status error (exit 22), got %d %q", errs.Code(err), msg)
	}
	if strings.Contains(msg, "Run the same command again") {
		t.Fatalf("a refused link must not promise that a rerun helps: %q", msg)
	}
	if n := page.requests.Load(); n != 2 {
		t.Fatalf("link was fetched %d times, want 2 (one renewal before giving up)", n)
	}
}

func TestSingleResumeThroughRedirect(t *testing.T) {
	setup(t)
	data := randomData(768 << 10) // below minChunk: one connection
	rs := &rangeServer{data: data, ranges: true, gate: make(chan struct{}), gateAfter: 64 << 10}
	cdn := httptest.NewServer(rs)
	defer cdn.Close()
	page := front(t, cdn, nil)
	rs.requireReferer = page.URL + "/download/x"
	o := Options{URL: page.URL + "/download/x", Client: client(t)}

	interrupt(t, o, func() bool {
		fi, err := os.Stat("file.bin.part")
		return err == nil && fi.Size() > 0
	})
	close(rs.gate)
	if _, err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if fileHash(t, "file.bin") != sha256.Sum256(data) {
		t.Fatal("content mismatch after resume")
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	resumed := false
	for _, s := range rs.rangeStarts {
		resumed = resumed || s > 0
	}
	if !resumed || rs.denied.Load() != 0 {
		t.Fatalf("resume did not continue from the saved offset through the redirect: starts %v, denied %d", rs.rangeStarts, rs.denied.Load())
	}
}

func TestKeyResumesAcrossURLs(t *testing.T) {
	setup(t)
	data := randomData(8 << 20)
	a := &rangeServer{data: data, ranges: true, gate: make(chan struct{}), gateAfter: 64 << 10}
	b := &rangeServer{data: data, ranges: true}
	srvA, srvB := httptest.NewServer(a), httptest.NewServer(b)
	defer srvA.Close()
	defer srvB.Close()

	interrupt(t, Options{URL: srvA.URL + "/one/pic.bin", Key: "ipfs://cid/pic.bin", Client: client(t)}, func() bool {
		st := loadState("pic.bin.gett")
		return st != nil && a.requests.Load() > int64(len(st.Chunks))
	})
	if st := loadState("pic.bin.gett"); st == nil || st.done() == 0 {
		t.Fatalf("no progress saved: %+v", st)
	}
	if _, err := Run(context.Background(), Options{URL: srvB.URL + "/two/pic.bin", Key: "ipfs://cid/pic.bin", Client: client(t)}); err != nil {
		t.Fatal(err)
	}
	if fileHash(t, "pic.bin") != sha256.Sum256(data) {
		t.Fatal("content mismatch")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.rangeStarts {
		if s > 0 && s%(1<<20) != 0 {
			return // a chunk continued mid-way: the first run's data was kept
		}
	}
	t.Fatalf("second URL started over instead of resuming: range starts %v", b.rangeStarts)
}

func TestForeignSidecarIsKept(t *testing.T) {
	t.Chdir(t.TempDir())
	os.WriteFile("x.gett", []byte(`{"kind":"file","sharer":"laptop","path":"x","done":[[0,1]]}`), 0o644)
	os.WriteFile("x.part", []byte("partial"), 0o644)
	st, name, resumed := pickTarget("x", "http://h/x", "http://h/x", 10, "", "")
	if st != nil || resumed || name != "x (1)" {
		t.Fatalf("pickTarget over a transfer in progress: %v %q %v", st, name, resumed)
	}
	for _, f := range []string{"x.gett", "x.part"} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("%s was removed", f)
		}
	}
}
