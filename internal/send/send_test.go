package send

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gett/internal/args"
	"gett/internal/errs"
	"gett/internal/httpx"
	"gett/internal/term"
)

type echo struct {
	Method      string            `json:"method"`
	ContentType string            `json:"content_type"`
	Body        string            `json:"body"`
	Headers     map[string]string `json:"headers"`
	Query       string            `json:"query"`
	Auth        string            `json:"auth"`
}

func echoServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		e := echo{Method: r.Method, ContentType: r.Header.Get("Content-Type"), Body: string(b), Headers: map[string]string{}, Query: r.URL.RawQuery}
		for k := range r.Header {
			e.Headers[k] = r.Header.Get(k)
		}
		if u, p, ok := r.BasicAuth(); ok {
			e.Auth = u + ":" + p
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(e)
	}))
}

// capture runs fn with stdout redirected to a pipe and returns what was written.
func capture(t *testing.T, fn func()) string {
	t.Helper()
	tty := false
	term.StdoutIsTTY = &tty
	defer func() { term.StdoutIsTTY = nil }()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	w.Close()
	os.Stdout = old
	return <-done
}

func doEcho(t *testing.T, r Request) echo {
	t.Helper()
	c, _ := httpx.NewClient(httpx.Config{})
	out := capture(t, func() {
		if err := Do(context.Background(), c, r); err != nil {
			t.Error(err)
		}
	})
	var e echo
	if err := json.Unmarshal([]byte(out), &e); err != nil {
		t.Fatalf("raw body was not the JSON echo: %q", out)
	}
	return e
}

func TestGetDefault(t *testing.T) {
	srv := echoServer()
	defer srv.Close()
	e := doEcho(t, Request{URL: srv.URL + "/x", Headers: http.Header{"X-Test": {"1"}}})
	if e.Method != "GET" || e.Headers["X-Test"] != "1" {
		t.Fatalf("%+v", e)
	}
}

func TestJSONBecomesPost(t *testing.T) {
	srv := echoServer()
	defer srv.Close()
	e := doEcho(t, Request{URL: srv.URL, JSON: `{"a":1}`})
	if e.Method != "POST" || e.ContentType != "application/json" || e.Body != `{"a":1}` {
		t.Fatalf("%+v", e)
	}
}

func TestForm(t *testing.T) {
	srv := echoServer()
	defer srv.Close()
	e := doEcho(t, Request{URL: srv.URL, Method: "PUT", Form: []args.KV{{Key: "a", Value: "1"}, {Key: "b", Value: "x y"}}})
	if e.Method != "PUT" || e.ContentType != "application/x-www-form-urlencoded" || e.Body != "a=1&b=x+y" {
		t.Fatalf("%+v", e)
	}
}

func TestMultipart(t *testing.T) {
	srv := echoServer()
	defer srv.Close()
	p := filepath.Join(t.TempDir(), "hello.txt")
	os.WriteFile(p, []byte("hello world"), 0o644)
	e := doEcho(t, Request{URL: srv.URL, Form: []args.KV{{Key: "name", Value: "v"}}, Files: []args.KV{{Key: "file", Value: p}}})
	if e.Method != "POST" || !strings.HasPrefix(e.ContentType, "multipart/form-data") {
		t.Fatalf("%+v", e)
	}
	if !strings.Contains(e.Body, `filename="hello.txt"`) || !strings.Contains(e.Body, "hello world") || !strings.Contains(e.Body, `name="name"`) {
		t.Fatalf("multipart body: %s", e.Body)
	}
}

func TestRequestFileWithEnv(t *testing.T) {
	srv := echoServer()
	defer srv.Close()
	t.Setenv("TOKEN", "s3cret")
	t.Setenv("BASE", srv.URL)
	p := filepath.Join(t.TempDir(), "req.json")
	os.WriteFile(p, []byte(`[{"url":"${BASE}/one","headers":{"Authorization":"Bearer ${TOKEN}"},"query":{"q":"${MISSING:-dflt}"}},
	  {"method":"delete","url":"${BASE}/two","auth":"u:${TOKEN}"}]`), 0o644)
	reqs, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 2 {
		t.Fatal("expected two requests")
	}
	e := doEcho(t, reqs[0])
	if e.Headers["Authorization"] != "Bearer s3cret" || e.Query != "q=dflt" {
		t.Fatalf("%+v", e)
	}
	e = doEcho(t, reqs[1])
	if e.Method != "DELETE" || e.Auth != "u:s3cret" {
		t.Fatalf("%+v", e)
	}

	os.WriteFile(p, []byte(`{"url":"${BASE}","headers":{"X":"${NOPE}"}}`), 0o644)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "NOPE") {
		t.Fatalf("expected missing variable error, got %v", err)
	}
}

func TestTwoBodies(t *testing.T) {
	srv := echoServer()
	defer srv.Close()
	client, _ := httpx.NewClient(httpx.Config{})
	err := Do(context.Background(), client, Request{URL: srv.URL, JSON: `{"a":1}`, Form: []args.KV{{Key: "a", Value: "1"}}})
	var u *errs.Usage
	if !errors.As(err, &u) {
		t.Fatalf("want usage error, got %v", err)
	}
}

func TestLengthKnown(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.ContentLength != int64(len(b)) || len(r.TransferEncoding) != 0 {
			got = "chunked or wrong length"
		}
	}))
	defer srv.Close()
	p := filepath.Join(t.TempDir(), "hello.txt")
	os.WriteFile(p, []byte("hello world"), 0o644)
	client, _ := httpx.NewClient(httpx.Config{})
	tty := false
	term.StdoutIsTTY = &tty
	defer func() { term.StdoutIsTTY = nil }()
	for _, r := range []Request{
		{URL: srv.URL, Form: []args.KV{{Key: "b", Value: "2"}, {Key: "a", Value: "1"}}, Files: []args.KV{{Key: "f", Value: p}, {Key: "g", Value: p}}},
		{URL: srv.URL, BodyFile: p},
	} {
		if err := Do(context.Background(), client, r); err != nil || got != "" {
			t.Fatalf("%+v: %v %s", r, err, got)
		}
	}
}
