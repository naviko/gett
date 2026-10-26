package ipfs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"gett/internal/download"
	"gett/internal/errs"
	"gett/internal/httpx"
	"gett/internal/term"
)

func TestParse(t *testing.T) {
	ok := map[string]addr{
		"ipfs://bafyabc":              {"ipfs", "bafyabc", ""},
		"IPFS://bafyabc/dir/file.pdf": {"ipfs", "bafyabc", "/dir/file.pdf"},
		"ipns://docs.ipfs.tech/x/y":   {"ipns", "docs.ipfs.tech", "/x/y"},
		"/ipfs/QmAbc":                 {"ipfs", "QmAbc", ""},
		"/ipns/example.com/":          {"ipns", "example.com", ""},
		"ipfs:///bafyabc":             {"ipfs", "bafyabc", ""},
		"ipfs://bafyabc/a%20b/c":      {"ipfs", "bafyabc", "/a%20b/c"},
	}
	for in, want := range ok {
		got, err := parse(in)
		if err != nil || got != want {
			t.Errorf("parse(%q) = %+v, %v; want %+v", in, got, err, want)
		}
		if !IsIPFS(in) {
			t.Errorf("IsIPFS(%q) should be true", in)
		}
	}
	for _, bad := range []string{"ipfs://", "ipfs:///", "ipfs://has space", "https://ipfs.io/ipfs/x", "x.torrent", "ipfs"} {
		if _, err := parse(bad); err == nil {
			t.Errorf("parse(%q) should fail", bad)
		}
	}
	if IsIPFS("https://ipfs.io/ipfs/x") {
		t.Error("a plain https URL is not an IPFS address")
	}
}

func TestGatewaysOverride(t *testing.T) {
	t.Setenv("IPFS_GATEWAY", "")
	if len(gateways()) != len(Gateways) {
		t.Fatal("empty override should keep the defaults")
	}
	t.Setenv("IPFS_GATEWAY", " https://a.example/ , b.example,, ")
	got := gateways()
	if len(got) != 2 || got[0] != "https://a.example" || got[1] != "https://b.example" {
		t.Fatalf("override parsed wrong: %v", got)
	}
}

func gw(handler http.HandlerFunc) *httptest.Server { return httptest.NewServer(handler) }

func client(t *testing.T) *http.Client {
	c, err := httpx.NewClient(httpx.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestResolvePicksTheGatewayThatAnswers(t *testing.T) {
	broken := gw(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	defer broken.Close()
	missing := gw(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	defer missing.Close()
	released := make(chan struct{})
	hanging := gw(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-released:
		}
	})
	defer hanging.Close()
	defer close(released)
	good := gw(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=0-0" {
			t.Errorf("probe should ask for one byte, got %q", r.Header.Get("Range"))
		}
		w.Header().Set("Content-Range", "bytes 0-0/5")
		w.WriteHeader(206)
		w.Write([]byte("h"))
	})
	defer good.Close()

	t.Setenv("IPFS_GATEWAY", strings.Join([]string{broken.URL, hanging.URL, missing.URL, good.URL}, ","))
	start := time.Now()
	u, _, err := Resolve(context.Background(), "ipfs://bafytest/hello.txt", client(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if u != good.URL+"/ipfs/bafytest/hello.txt" {
		t.Fatalf("wrong gateway chosen: %s", u)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("resolve waited on the hanging gateway")
	}
}

func TestResolveAllFail(t *testing.T) {
	broken := gw(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(502) })
	defer broken.Close()
	t.Setenv("IPFS_GATEWAY", broken.URL)
	_, _, err := Resolve(context.Background(), "ipfs://bafytest", client(t), nil)
	if err == nil || errs.Code(err) != 6 {
		t.Fatalf("expected gateway error with exit 6, got %v (%d)", err, errs.Code(err))
	}
}

func TestResolveDirectoryBecomesTar(t *testing.T) {
	dir := gw(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Ipfs-Roots", "bafydir")
		w.WriteHeader(200)
		w.Write([]byte("<html>listing</html>"))
	})
	defer dir.Close()
	t.Setenv("IPFS_GATEWAY", dir.URL)
	u, _, err := Resolve(context.Background(), "ipfs://bafydir", client(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(u, "/ipfs/bafydir?format=tar") {
		t.Fatalf("directory should be fetched as tar, got %s", u)
	}
	// An HTML file with an extension is just a file.
	u, _, _ = Resolve(context.Background(), "ipfs://bafydir/index.html", client(t), nil)
	if strings.Contains(u, "format=tar") {
		t.Fatalf("a .html file must not be tarred: %s", u)
	}
}

func TestEndToEndDownload(t *testing.T) {
	tty := true
	term.StdoutIsTTY = &tty
	term.Silent = true
	defer func() { term.StdoutIsTTY = nil }()
	old, _ := os.Getwd()
	os.Chdir(t.TempDir())
	defer os.Chdir(old)

	data := make([]byte, 3<<20)
	rand.Read(data)
	srv := gw(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/ipfs/bafybig/") {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "", time.Time{}, strings.NewReader(string(data)))
	})
	defer srv.Close()
	t.Setenv("IPFS_GATEWAY", srv.URL)

	c := client(t)
	u, key, err := Resolve(context.Background(), "ipfs://bafybig/photos/pic.bin", c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := download.Run(context.Background(), download.Options{URL: u, Key: key, Client: c}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("pic.bin")
	if err != nil {
		t.Fatal("expected pic.bin, named from the IPFS path")
	}
	if fmt.Sprintf("%x", sha256.Sum256(got)) != fmt.Sprintf("%x", sha256.Sum256(data)) {
		t.Fatal("content mismatch")
	}
}
