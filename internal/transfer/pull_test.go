package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gett/internal/errs"
	"gett/internal/term"
)

// faults sits in front of the real handler and misbehaves on demand.
type faults struct {
	next        http.Handler
	stallOnce   atomic.Bool  // one unit stops after its first write
	corruptOnce atomic.Bool  // one unit has a byte flipped
	changedOnce atomic.Bool  // one unit is answered 412
	delay       atomic.Int64 // nanoseconds before each unit

	mu       sync.Mutex
	tars     []tarRequest
	tarsDone int
	files    atomic.Int64
}

func (f *faults) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/tar":
		b, _ := io.ReadAll(r.Body)
		var req tarRequest
		json.Unmarshal(b, &req)
		f.mu.Lock()
		f.tars = append(f.tars, req)
		f.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(b))
		f.next.ServeHTTP(w, r)
		f.mu.Lock()
		f.tarsDone++
		f.mu.Unlock()
		return
	case "/v1/file":
		f.files.Add(1)
		time.Sleep(time.Duration(f.delay.Load()))
		switch {
		case f.changedOnce.CompareAndSwap(true, false):
			http.Error(w, "changed", http.StatusPreconditionFailed)
			return
		case f.stallOnce.CompareAndSwap(true, false):
			f.next.ServeHTTP(&stallWriter{ResponseWriter: w, ctx: r.Context()}, r)
			return
		case f.corruptOnce.CompareAndSwap(true, false):
			f.next.ServeHTTP(&corruptWriter{ResponseWriter: w}, r)
			return
		}
	}
	f.next.ServeHTTP(w, r)
}

// stallWriter lets the first write through and then hangs until the
// receiver gives up.
type stallWriter struct {
	http.ResponseWriter
	ctx   context.Context
	wrote bool
}

func (s *stallWriter) Write(p []byte) (int, error) {
	if s.wrote {
		<-s.ctx.Done()
		return 0, s.ctx.Err()
	}
	s.wrote = true
	n, err := s.ResponseWriter.Write(p)
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	return n, err
}

// corruptWriter flips the first byte it sees.
type corruptWriter struct {
	http.ResponseWriter
	done bool
}

func (c *corruptWriter) Write(p []byte) (int, error) {
	if !c.done && len(p) > 0 {
		c.done = true
		q := bytes.Clone(p)
		q[0] ^= 0xff
		return c.ResponseWriter.Write(q)
	}
	return c.ResponseWriter.Write(p)
}

// sharer runs the real handler behind faults and registers it as the only
// sharer on the network.
func sharer(t *testing.T, name, password string, paths ...string) (*faults, *recorder) {
	t.Helper()
	idx, err := build(context.Background(), paths, password)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	f := &faults{next: newHandler(idx, rec.record)}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	useFinder(t, staticFinder{[]Peer{peerOf(t, srv, name, "id-"+name)}})
	return f, rec
}

func random(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	rand.New(rand.NewSource(int64(n))).Read(b)
	return b
}

func sum(t *testing.T, p string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

func list(t *testing.T) {
	t.Helper()
	if err := List(context.Background(), "", io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestPullFile(t *testing.T) {
	setup(t)
	data := random(t, 300<<10)
	src := makeTree(t, map[string][]byte{"big.bin": data})
	_, rec := sharer(t, "laptop", "", filepath.Join(src, "big.bin"))
	list(t)
	dest := t.TempDir()
	msgs := stderr(t, func() {
		saved, err := Pull(context.Background(), []string{"1"}, dest, "")
		if err != nil || len(saved) != 1 || saved[0] != filepath.Join(dest, "big.bin") {
			t.Fatalf("saved %v, err %v", saved, err)
		}
	})
	if sum(t, filepath.Join(dest, "big.bin")) != sha256.Sum256(data) {
		t.Fatal("content differs")
	}
	if entries, _ := os.ReadDir(dest); len(entries) != 1 {
		t.Fatalf("leftovers: %v", entries)
	}
	if !strings.Contains(msgs, "saved "+filepath.Join(dest, "big.bin")+" (300.0 KiB)") {
		t.Fatalf("stderr: %q", msgs)
	}
	evs := rec.all()
	if len(evs) != 2 || evs[0].Path != "big.bin" || evs[0].Done || !evs[1].Done || !evs[1].OK {
		t.Fatalf("sharer events: %+v", evs)
	}

	// Again: identical, so nothing happens.
	msgs = stderr(t, func() {
		if _, err := Pull(context.Background(), []string{"1"}, dest, ""); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(msgs, "big.bin already there") {
		t.Fatalf("stderr: %q", msgs)
	}
	// A different file with that name is left alone.
	os.WriteFile(filepath.Join(dest, "big.bin"), []byte("other"), 0o644)
	saved, err := Pull(context.Background(), []string{"1"}, dest, "")
	if err != nil || len(saved) != 1 || saved[0] != filepath.Join(dest, "big (1).bin") {
		t.Fatalf("numbered: %v %v", saved, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "big.bin")); string(b) != "other" {
		t.Fatal("overwritten")
	}

	// Sidecar junk is not deleted; an orphan sidecar is reused.
	os.WriteFile(filepath.Join(dest, "big (1).bin"), []byte("other too"), 0o644)
	os.WriteFile(filepath.Join(dest, "big (2).bin.gett"), []byte("{junk"), 0o644)
	os.WriteFile(filepath.Join(dest, "big (2).bin.part"), []byte("x"), 0o644)
	orphan := &sidecar{Kind: "file", Sharer: "laptop", Path: "big.bin", Size: int64(len(data)), MTime: loadLast().Items[0].MTime}
	orphan.save(filepath.Join(dest, "big (3).bin.gett"))
	saved, err = Pull(context.Background(), []string{"1"}, dest, "")
	if err != nil || len(saved) != 1 || saved[0] != filepath.Join(dest, "big (3).bin") {
		t.Fatalf("orphan: %v %v", saved, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "big (2).bin.gett")); string(b) != "{junk" {
		t.Fatal("junk sidecar touched")
	}
	if _, err := os.Stat(filepath.Join(dest, "big (3).bin.gett")); err == nil {
		t.Fatal("sidecar left after completion")
	}

	// Into the current folder when no destination is given.
	cwd := t.TempDir()
	t.Chdir(cwd)
	if saved, err = Pull(context.Background(), []string{"1"}, "", ""); err != nil || saved[0] != "big.bin" {
		t.Fatalf("cwd: %v %v", saved, err)
	}
}

func TestPullFolderAndResume(t *testing.T) {
	setup(t)
	big := random(t, 1<<20)
	tree := map[string][]byte{
		"photos/a.txt":      []byte("aaa"),
		"photos/2024/b.txt": bytes.Repeat([]byte("b"), 40<<10),
		"photos/2024/c/d":   []byte("d"),
		"photos/big.bin":    big,
		"photos/.hidden":    []byte("no"),
	}
	src := makeTree(t, tree)
	f, rec := sharer(t, "laptop", "", filepath.Join(src, "photos"))
	f.delay.Store(int64(20 * time.Millisecond))
	list(t)
	dest := t.TempDir()

	// Interrupt once the small files are in and the big one is under way.
	ctx, cancel := context.WithCancel(context.Background())
	var perr error
	done := make(chan struct{})
	go func() {
		_, perr = Pull(ctx, []string{"1"}, dest, "")
		close(done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		f.mu.Lock()
		ready := f.tarsDone == 1 && f.files.Load() >= 8
		f.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pull did not get going")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if errs.Code(perr) != 130 || !strings.HasSuffix(errs.Message(perr), "Run the same command again to resume") {
		t.Fatalf("interrupted: %v (%d)", perr, errs.Code(perr))
	}
	part := filepath.Join(dest, "photos.part")
	if _, err := os.Stat(part); err != nil {
		t.Fatal("no photos.part after interruption")
	}
	side := loadSidecar(filepath.Join(dest, "photos.gett"))
	if side == nil || side.Kind != "folder" || side.Sharer != "laptop" || side.Path != "photos" {
		t.Fatalf("folder sidecar: %+v", side)
	}
	bigSide := loadSidecar(filepath.Join(part, "big.bin.gett"))
	if bigSide == nil || len(bigSide.Done) == 0 {
		t.Fatalf("big file sidecar: %+v", bigSide)
	}
	if evs := rec.all(); len(evs) != 2 || !evs[1].Done || evs[1].OK {
		t.Fatalf("sharer should have seen a stop: %+v", evs)
	}

	// Run again: the small files already there are not asked for, the one
	// that went missing is, and the big one resumes.
	f.delay.Store(0)
	os.Remove(filepath.Join(part, "2024", "c", "d"))
	msgs := stderr(t, func() {
		if _, err := Pull(context.Background(), []string{"1"}, dest, ""); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(msgs, "resuming "+filepath.Join(dest, "photos")) || !strings.Contains(msgs, "saved "+filepath.Join(dest, "photos")+" (1.0 MiB, 4 files)") {
		t.Fatalf("stderr: %q", msgs)
	}
	f.mu.Lock()
	tars := f.tars
	f.mu.Unlock()
	if len(tars) != 2 || len(tars[0].Have) != 0 || len(tars[1].Have) != 2 {
		t.Fatalf("tar requests: %+v", tars)
	}
	for name, want := range tree {
		p := filepath.Join(dest, filepath.FromSlash(name))
		if strings.Contains(name, ".hidden") {
			if _, err := os.Stat(p); err == nil {
				t.Fatal("dotfile came along")
			}
			continue
		}
		got, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s: %v, %d bytes", name, err, len(got))
		}
		src, _ := os.Stat(filepath.Join(src, filepath.FromSlash(name)))
		fi, _ := os.Stat(p)
		if !fi.ModTime().Truncate(time.Second).Equal(src.ModTime().Truncate(time.Second)) {
			t.Fatalf("%s: mtime %v, want %v", name, fi.ModTime(), src.ModTime())
		}
	}
	if _, err := os.Stat(part); err == nil {
		t.Fatal("photos.part left behind")
	}
	if _, err := os.Stat(filepath.Join(dest, "photos", "big.bin.gett")); err == nil {
		t.Fatal("inner sidecar left behind")
	}

	// Again: identical folder, nothing to do, nothing asked.
	msgs = stderr(t, func() { Pull(context.Background(), []string{"1"}, dest, "") })
	if !strings.Contains(msgs, "already there") {
		t.Fatalf("stderr: %q", msgs)
	}
	f.mu.Lock()
	if len(f.tars) != 2 {
		t.Fatalf("a tar request for an identical folder: %+v", f.tars)
	}
	f.mu.Unlock()
	// Selecting the folder and something inside it pulls the folder once.
	other := t.TempDir()
	saved, err := Pull(context.Background(), []string{"1.2", "1", "1"}, other, "")
	if err != nil || len(saved) != 1 || saved[0] != filepath.Join(other, "photos") {
		t.Fatalf("nested selection: %v %v", saved, err)
	}
	if entries, _ := os.ReadDir(other); len(entries) != 1 {
		t.Fatalf("nested selection left: %v", entries)
	}
}

func TestPullFaults(t *testing.T) {
	setup(t)
	data := random(t, 400<<10)
	src := makeTree(t, map[string][]byte{"big.bin": data})
	f, _ := sharer(t, "laptop", "", filepath.Join(src, "big.bin"))
	list(t)

	f.stallOnce.Store(true)
	f.corruptOnce.Store(true)
	dest := t.TempDir()
	if _, err := Pull(context.Background(), []string{"1"}, dest, ""); err != nil {
		t.Fatal(err)
	}
	if sum(t, filepath.Join(dest, "big.bin")) != sha256.Sum256(data) {
		t.Fatal("content differs after a stall and a corrupt unit")
	}
	units := int64(len(data)) / firstUnit
	if n := f.files.Load(); n < units+2 {
		t.Fatalf("%d unit requests for %d units: the bad ones were not asked again", n, units)
	}

	f.changedOnce.Store(true)
	dest = t.TempDir()
	msgs := stderr(t, func() {
		if _, err := Pull(context.Background(), []string{"1"}, dest, ""); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(msgs, "big.bin changed on laptop, starting over") {
		t.Fatalf("stderr: %q", msgs)
	}
	if sum(t, filepath.Join(dest, "big.bin")) != sha256.Sum256(data) {
		t.Fatal("content differs after a restart")
	}
}

func TestPullStaleListAndErrors(t *testing.T) {
	setup(t)
	src := makeTree(t, map[string][]byte{"photos/a.txt": []byte("aaa"), "photos/b.txt": []byte("b")})
	sharer(t, "laptop", "", filepath.Join(src, "photos"))
	list(t)
	dest := t.TempDir()

	if _, err := Pull(context.Background(), []string{"7"}, dest, ""); err == nil || !strings.Contains(err.Error(), "no item 7") {
		t.Fatalf("unknown number: %v", err)
	}
	os.WriteFile(filepath.Join(src, "photos", "a.txt"), []byte("longer"), 0o644)
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	_, err := Pull(context.Background(), []string{"1.1"}, dest, "")
	w.Close()
	os.Stdout = old
	fresh, _ := io.ReadAll(r)
	if errs.Code(err) != 33 || !strings.Contains(err.Error(), "photos/a.txt on laptop changed; run again with the numbers above") {
		t.Fatalf("changed item: %v", err)
	}
	if !strings.Contains(string(fresh), "a.txt") || !strings.Contains(string(fresh), "6 B") {
		t.Fatalf("fresh list not printed: %q", fresh)
	}

	dead := httptest.NewServer(nil)
	dead.Close()
	useFinder(t, staticFinder{[]Peer{peerOf(t, dead, "laptop", "id-laptop")}})
	_, err = Pull(context.Background(), []string{"1"}, dest, "")
	var peerErr *errs.Peer
	if errs.Code(err) != 7 || !errors.As(err, &peerErr) || !strings.HasPrefix(errs.Message(err), "could not connect to laptop (127.0.0.1:") || !strings.HasSuffix(errs.Message(err), "Run the same command again to resume") {
		t.Fatalf("dead sharer: %v", err)
	}

	useFinder(t, staticFinder{nil})
	os.Remove(lastFile())
	if _, err := Pull(context.Background(), []string{"1"}, dest, ""); err == nil || !strings.Contains(err.Error(), "run gett transfer first") {
		t.Fatalf("no list: %v", err)
	}
}

func TestPullPiped(t *testing.T) {
	setup(t)
	data := random(t, 100<<10)
	src := makeTree(t, map[string][]byte{"photos/big.bin": data, "photos/x": []byte("x")})
	sharer(t, "laptop", "", filepath.Join(src, "photos"))
	list(t)
	tty := false
	term.StdoutIsTTY = &tty

	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	var got []byte
	done := make(chan struct{})
	go func() {
		got, _ = io.ReadAll(r)
		close(done)
	}()
	saved, err := Pull(context.Background(), []string{"1.1"}, "", "")
	w.Close()
	os.Stdout = old
	<-done
	if err != nil || len(saved) != 0 || !bytes.Equal(got, data) {
		t.Fatalf("piped: err %v, saved %v, %d bytes", err, saved, len(got))
	}
	if _, err := Pull(context.Background(), []string{"1"}, "", ""); err == nil || !strings.Contains(err.Error(), "cannot be piped") {
		t.Fatalf("piped folder: %v", err)
	}
	if _, err := Pull(context.Background(), []string{"1.1", "1.2"}, "", ""); err == nil || !strings.Contains(err.Error(), "one file at a time") {
		t.Fatalf("piped two: %v", err)
	}
}
