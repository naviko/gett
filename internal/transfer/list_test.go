package transfer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gett/internal/term"
)

// row is one listing line as printed on a terminal, without trailing spaces.
func row(n, name, size, files string) string {
	return strings.TrimRight(fmt.Sprintf("%-6s %-30s %10s  %s", n, name, size, files), " ") + "\n"
}

// staticFinder answers with fixed peers.
type staticFinder struct{ peers []Peer }

func (f staticFinder) Find(context.Context, string) ([]Peer, error) {
	return dedupe(append([]Peer(nil), f.peers...)), nil
}

// peerOf describes an httptest sharer as the beacon would.
func peerOf(t *testing.T, srv *httptest.Server, name, id string) Peer {
	t.Helper()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	n, _ := strconv.Atoi(port)
	return Peer{ID: id, Name: name, API: api, Addr: host, Port: n}
}

func useFinder(t *testing.T, f Finder) {
	t.Helper()
	old := finder
	finder = f
	t.Cleanup(func() { finder = old })
}

// stderr runs fn with output on and returns what it printed to stderr.
func stderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	term.Silent = false
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	w.Close()
	os.Stderr = old
	term.Silent = true
	return <-done
}

func TestListAndLs(t *testing.T) {
	setup(t)
	a := makeTree(t, map[string][]byte{"photos/a.jpg": []byte("aaa"), "photos/2024/b.jpg": []byte("bb"), "report.pdf": []byte("%PDF")})
	b := makeTree(t, map[string][]byte{"music/x.mp3": []byte("xxxx")})
	srvA, _ := serve(t, "", filepath.Join(a, "photos"), filepath.Join(a, "report.pdf"))
	srvB, _ := serve(t, "tiger", filepath.Join(b, "music"))
	dead := httptest.NewServer(nil)
	dead.Close()
	useFinder(t, staticFinder{[]Peer{
		peerOf(t, srvB, "nas", "idb"),
		peerOf(t, srvA, "laptop", "ida"),
		peerOf(t, dead, "ghost", "idc"),
		{ID: "idd", Name: "future", API: 9, Addr: "127.0.0.1", Port: 1},
	}})

	var out bytes.Buffer
	msgs := stderr(t, func() {
		if err := List(context.Background(), "", &out); err != nil {
			t.Fatal(err)
		}
	})
	want := "shared by: laptop\n" +
		row("1", "photos/", "5 B", "2 files") +
		row("1.1", "2024/", "2 B", "1 file") +
		row("1.2", "a.jpg", "3 B", "") +
		row("2", "report.pdf", "4 B", "")
	if out.String() != want {
		t.Fatalf("listing:\n%s\nwant:\n%s", out.String(), want)
	}
	if !strings.Contains(msgs, "could not connect to ghost (") || !strings.Contains(msgs, "future runs a different gett version; update the older one") {
		t.Fatalf("stderr: %q", msgs)
	}
	if !strings.Contains(msgs, "\nex: gett transfer 1 ") || !strings.Contains(msgs, "\n    gett transfer 1 <folder>/") || !strings.Contains(msgs, "\n    gett transfer ls 1.1 ") {
		t.Fatalf("footer: %q", msgs)
	}
	s := loadLast()
	if s == nil || len(s.Sharers) != 2 || len(s.Items) != 4 || s.Items[2].Path != "photos/a.jpg" || s.Items[2].Sharer != 0 {
		t.Fatalf("saved: %+v", s)
	}

	// The password unlocks nas and is remembered.
	out.Reset()
	msgs = stderr(t, func() {
		if err := List(context.Background(), "tiger", &out); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out.String(), "\nshared by: nas   password:tiger\n"+row("3", "music/", "4 B", "1 file")) {
		t.Fatalf("unlocked listing:\n%s", out.String())
	}
	if strings.Contains(msgs, "no shares found") {
		t.Fatalf("stderr: %q", msgs)
	}
	s = loadLast()
	if len(s.Passwords) != 1 || s.Passwords[0] != (savedPassword{"nas", "tiger"}) {
		t.Fatalf("passwords: %+v", s.Passwords)
	}
	out.Reset()
	List(context.Background(), "", &out)
	if !strings.Contains(out.String(), "password:tiger") {
		t.Fatalf("remembered password not used:\n%s", out.String())
	}

	// A password that unlocks nothing says so and is not kept.
	out.Reset()
	msgs = stderr(t, func() { List(context.Background(), "lion", &out) })
	if !strings.Contains(msgs, "no shares found for that password") {
		t.Fatalf("stderr: %q", msgs)
	}
	if s = loadLast(); len(s.Passwords) != 1 {
		t.Fatalf("passwords after a wrong one: %+v", s.Passwords)
	}

	// Piped: tab-separated, sharer on every line.
	tty := false
	term.StdoutIsTTY = &tty
	out.Reset()
	List(context.Background(), "", &out)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 6 || lines[1] != "1.1\t2024/\t2\t1\tlaptop\t" || lines[2] != "1.2\ta.jpg\t3\t\tlaptop\t" || lines[4] != "3\tmusic/\t4\t1\tnas\ttiger" {
		t.Fatalf("tsv:\n%s", out.String())
	}
	tty = true

	// Ls goes deeper and extends the saved list.
	out.Reset()
	if err := Ls(context.Background(), "1.1", &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "shared by: laptop\n"+row("1.1.1", "b.jpg", "2 B", "") {
		t.Fatalf("ls:\n%q", out.String())
	}
	if s = loadLast(); len(s.Items) != 7 || s.Items[2].N != "1.1.1" || s.Items[2].Path != "photos/2024/b.jpg" {
		t.Fatalf("saved after ls: %+v", s.Items)
	}
	if err := Ls(context.Background(), "1.2", &out); err == nil || !strings.Contains(err.Error(), "is a file") {
		t.Fatalf("ls of a file: %v", err)
	}
	if err := Ls(context.Background(), "9", &out); err == nil || !strings.Contains(err.Error(), "no item 9") {
		t.Fatalf("ls of nothing: %v", err)
	}

	// The sharer dropped the password: it is forgotten.
	srvB.Close()
	srvB2, _ := serve(t, "", filepath.Join(b, "music"))
	useFinder(t, staticFinder{[]Peer{peerOf(t, srvB2, "nas", "idb")}})
	out.Reset()
	List(context.Background(), "", &out)
	if s = loadLast(); len(s.Passwords) != 0 {
		t.Fatalf("stale password kept: %+v", s.Passwords)
	}
}

func TestListEmptyNetwork(t *testing.T) {
	setup(t)
	useFinder(t, staticFinder{nil})
	var out bytes.Buffer
	msgs := stderr(t, func() {
		if err := List(context.Background(), "", &out); err != nil {
			t.Fatal(err)
		}
	})
	if out.Len() != 0 || !strings.Contains(msgs, "no shares found") {
		t.Fatalf("stdout %q, stderr %q", out.String(), msgs)
	}
	msgs = stderr(t, func() { List(context.Background(), "tiger", &out) })
	if !strings.Contains(msgs, "no shares found for that password") {
		t.Fatalf("stderr %q", msgs)
	}
}

func TestLsWithoutList(t *testing.T) {
	setup(t)
	if err := Ls(context.Background(), "1", io.Discard); err == nil || !strings.Contains(err.Error(), "run gett transfer first") {
		t.Fatalf("err %v", err)
	}
}
