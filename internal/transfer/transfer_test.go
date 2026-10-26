package transfer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"gett/internal/term"
)

// setup silences output, pretends stdout is a terminal, moves Home to a
// temp folder and shrinks the timings.
func setup(t *testing.T) {
	t.Helper()
	tty := true
	term.StdoutIsTTY = &tty
	term.Silent = true
	home := t.TempDir()
	Home = func() string { return home }
	wrongDelay, discoverWait, stallTimeout = 50*time.Millisecond, 200*time.Millisecond, 300*time.Millisecond
	smallMax, minUnit, maxUnit, firstUnit = 64<<10, 16<<10, 64<<10, 32<<10
	t.Cleanup(func() {
		term.StdoutIsTTY = nil
		term.Silent = false
		wrongDelay, discoverWait, stallTimeout = time.Second, time.Second, 2*time.Second
		smallMax, minUnit, maxUnit, firstUnit = 16<<20, 4<<20, 64<<20, 8<<20
	})
}

// makeTree writes files under a new temp folder and returns it.
func makeTree(t *testing.T, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	for name, data := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// recorder collects the sharer's start and done events.
type recorder struct {
	mu     sync.Mutex
	events []event
}

func (r *recorder) record(ev event) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

func (r *recorder) all() []event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]event(nil), r.events...)
}

// serveIndex serves an index that was built by hand.
func serveIndex(t *testing.T, idx *index) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(newHandler(idx, func(event) {}))
	t.Cleanup(srv.Close)
	return srv
}

// serve runs the real handler for the given paths on loopback.
func serve(t *testing.T, password string, paths ...string) (*httptest.Server, *recorder) {
	t.Helper()
	idx, err := build(context.Background(), paths, password)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	srv := httptest.NewServer(newHandler(idx, rec.record))
	t.Cleanup(srv.Close)
	return srv, rec
}

// call sends one API request with the headers every receiver sends.
func call(t *testing.T, srv *httptest.Server, method, path string, query map[string]string, headers map[string]string, body any) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	q := req.URL.Query()
	for k, v := range query {
		q.Set(k, v)
	}
	req.URL.RawQuery = q.Encode()
	req.Header.Set("Gett-Api", strconv.Itoa(api))
	req.Header.Set("Gett-Name", "desk")
	for k, v := range headers {
		if k == "Gett-Password" {
			req.Header.Add(k, v)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

func listing(t *testing.T, srv *httptest.Server, path string, headers map[string]string) []item {
	t.Helper()
	var out struct{ Items []item }
	decode(t, call(t, srv, "GET", "/v1/ls", map[string]string{"path": path}, headers, nil), &out)
	return out.Items
}
