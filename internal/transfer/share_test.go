package transfer

import (
	"archive/tar"
	"bytes"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestListingAndPasswords(t *testing.T) {
	setup(t)
	pub := makeTree(t, map[string][]byte{"photos/a.jpg": []byte("aaa"), "photos/2024/b.jpg": []byte("bb")})
	priv := makeTree(t, map[string][]byte{"secret/s.txt": []byte("s")})
	idx, err := build(t.Context(), []string{filepath.Join(pub, "photos")}, "")
	if err != nil {
		t.Fatal(err)
	}
	more, err := build(t.Context(), []string{filepath.Join(priv, "secret")}, "tiger")
	if err != nil {
		t.Fatal(err)
	}
	idx.shares = append(idx.shares, more.shares...)
	srv := serveIndex(t, idx)

	items := listing(t, srv, "", nil)
	if len(items) != 1 || items[0].Name != "photos" || !items[0].Dir || items[0].Size != 5 || items[0].Files != 2 || items[0].Password != "" {
		t.Fatalf("public listing: %+v", items)
	}
	items = listing(t, srv, "", map[string]string{"Gett-Password": "tiger"})
	if len(items) != 2 || items[1].Name != "secret" || items[1].Password != "tiger" || items[0].Password != "" {
		t.Fatalf("unlocked listing: %+v", items)
	}
	if resp := call(t, srv, "GET", "/v1/ls", map[string]string{"path": "secret"}, nil, nil); resp.StatusCode != 404 {
		t.Fatalf("locked share without password: %d", resp.StatusCode)
	}
	items = listing(t, srv, "photos", nil)
	if len(items) != 2 || items[0].Name != "2024" || !items[0].Dir || items[1].Name != "a.jpg" || items[1].Size != 3 {
		t.Fatalf("folder listing: %+v", items)
	}

	// A grown file is listed with its current size.
	os.WriteFile(filepath.Join(pub, "photos", "a.jpg"), []byte("aaaaaa"), 0o644)
	if items = listing(t, srv, "photos", nil); items[1].Size != 6 {
		t.Fatalf("stale size %d", items[1].Size)
	}

	// Wrong passwords are answered one at a time, each after a pause.
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := call(t, srv, "GET", "/v1/ls", nil, map[string]string{"Gett-Password": "wrong" + strconv.Itoa(i)}, nil)
			resp.Body.Close()
		}()
	}
	wg.Wait()
	if took := time.Since(start); took < 3*wrongDelay {
		t.Fatalf("three wrong passwords answered in %v", took)
	}

	if resp := call(t, srv, "GET", "/v1/ls", nil, map[string]string{"Gett-Api": "99"}, nil); resp.StatusCode != 426 {
		t.Fatalf("api mismatch: %d", resp.StatusCode)
	}
}

func TestFileRangesAndTrailer(t *testing.T) {
	setup(t)
	data := bytes.Repeat([]byte("0123456789"), 100)
	dir := makeTree(t, map[string][]byte{"photos/big.bin": data, "photos/empty": nil})
	srv, _ := serve(t, "", filepath.Join(dir, "photos"))

	resp := call(t, srv, "GET", "/v1/file", map[string]string{"path": "photos/big.bin"}, map[string]string{"Range": "bytes=10-29"}, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || string(body) != string(data[10:30]) || resp.Header.Get("Content-Range") != "bytes 10-29/1000" {
		t.Fatalf("range: %d %q %q", resp.StatusCode, body, resp.Header.Get("Content-Range"))
	}
	want := fmt.Sprintf("%08x", crc32.Checksum(data[10:30], castagnoli))
	if got := resp.Trailer.Get("Gett-Crc32"); got != want {
		t.Fatalf("trailer %q, want %q", got, want)
	}

	resp = call(t, srv, "GET", "/v1/file", map[string]string{"path": "photos/big.bin"}, map[string]string{"Range": "bytes=990-"}, nil)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || len(body) != 10 {
		t.Fatalf("open end: %d %d bytes", resp.StatusCode, len(body))
	}

	resp = call(t, srv, "GET", "/v1/file", map[string]string{"path": "photos/big.bin"}, nil, nil)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(body, data) || resp.Trailer.Get("Gett-Crc32") != fmt.Sprintf("%08x", crc32.Checksum(data, castagnoli)) {
		t.Fatalf("whole file: %d %d bytes trailer %q", resp.StatusCode, len(body), resp.Trailer.Get("Gett-Crc32"))
	}

	resp = call(t, srv, "GET", "/v1/file", map[string]string{"path": "photos/empty"}, nil, nil)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(body) != 0 {
		t.Fatalf("empty file: %d %d", resp.StatusCode, len(body))
	}

	for _, rng := range []string{"bytes=1000-", "bytes=5-2", "bytes=0-1,5-6", "bytes=x-y"} {
		resp = call(t, srv, "GET", "/v1/file", map[string]string{"path": "photos/big.bin"}, map[string]string{"Range": rng}, nil)
		resp.Body.Close()
		if resp.StatusCode != 416 {
			t.Fatalf("%s: %d, want 416", rng, resp.StatusCode)
		}
	}

	fi, _ := os.Stat(filepath.Join(dir, "photos", "big.bin"))
	resp = call(t, srv, "GET", "/v1/file", map[string]string{"path": "photos/big.bin"}, map[string]string{"Gett-Stamp": stamp(fi)}, nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("right stamp: %d", resp.StatusCode)
	}
	resp = call(t, srv, "GET", "/v1/file", map[string]string{"path": "photos/big.bin"}, map[string]string{"Gett-Stamp": "1-2"}, nil)
	resp.Body.Close()
	if resp.StatusCode != 412 {
		t.Fatalf("stale stamp: %d", resp.StatusCode)
	}

	if resp = call(t, srv, "GET", "/v1/file", map[string]string{"path": "photos"}, nil, nil); resp.StatusCode != 400 {
		t.Fatalf("folder as file: %d", resp.StatusCode)
	}
	resp.Body.Close()
	if resp = call(t, srv, "GET", "/v1/file", map[string]string{"path": "photos/nope"}, nil, nil); resp.StatusCode != 404 {
		t.Fatalf("unknown file: %d", resp.StatusCode)
	}
	resp.Body.Close()

	os.Remove(filepath.Join(dir, "photos", "big.bin"))
	if resp = call(t, srv, "GET", "/v1/file", map[string]string{"path": "photos/big.bin"}, nil, nil); resp.StatusCode != 404 {
		t.Fatalf("gone file: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestFileUnreadable(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("needs file permissions")
	}
	setup(t)
	dir := makeTree(t, map[string][]byte{"photos/a": []byte("a")})
	srv, _ := serve(t, "", filepath.Join(dir, "photos"))
	os.Chmod(filepath.Join(dir, "photos", "a"), 0)
	t.Cleanup(func() { os.Chmod(filepath.Join(dir, "photos", "a"), 0o644) })
	resp := call(t, srv, "GET", "/v1/file", map[string]string{"path": "photos/a"}, nil, nil)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("unreadable: %d", resp.StatusCode)
	}
}

func TestSymlinkInsideShare(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	setup(t)
	dir := makeTree(t, map[string][]byte{"photos/a": []byte("a")})
	os.Symlink("/etc/passwd", filepath.Join(dir, "photos", "passwd"))
	srv, _ := serve(t, "", filepath.Join(dir, "photos"))
	items := listing(t, srv, "photos", nil)
	if len(items) != 1 || items[0].Name != "a" {
		t.Fatalf("symlink listed: %+v", items)
	}
	resp := call(t, srv, "GET", "/v1/file", map[string]string{"path": "photos/passwd"}, nil, nil)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("symlink served: %d", resp.StatusCode)
	}
}

func TestFilesAndTar(t *testing.T) {
	setup(t)
	big := bytes.Repeat([]byte("x"), 200)
	dir := makeTree(t, map[string][]byte{
		"photos/a.txt":      []byte("aaa"),
		"photos/2024/b.txt": []byte("bb"),
		"photos/2024/c.txt": []byte("c"),
		"photos/big.bin":    big,
	})
	srv, _ := serve(t, "", filepath.Join(dir, "photos"))

	var flat struct {
		Files []fileEntry
		Dirs  []string
	}
	decode(t, call(t, srv, "GET", "/v1/files", map[string]string{"path": "photos"}, nil, nil), &flat)
	if len(flat.Files) != 4 || flat.Files[0].Path != "2024/b.txt" || flat.Files[3].Path != "big.bin" || len(flat.Dirs) != 1 || flat.Dirs[0] != "2024" {
		t.Fatalf("files: %+v dirs %v", flat.Files, flat.Dirs)
	}

	body := map[string]any{"max": 100, "have": []map[string]any{{"path": "2024/b.txt", "size": 2}, {"path": "a.txt", "size": 99}}}
	resp := call(t, srv, "POST", "/v1/tar", map[string]string{"path": "photos"}, nil, body)
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/x-tar" {
		t.Fatalf("tar: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	got := map[string]string{}
	tr := tar.NewReader(resp.Body)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		got[h.Name] = string(b)
	}
	// b.txt is already there with the right size, big.bin is over max, a.txt
	// is there with the wrong size and comes again.
	if len(got) != 2 || got["a.txt"] != "aaa" || got["2024/c.txt"] != "c" {
		t.Fatalf("tar entries: %v", got)
	}
}

func TestStartAndDone(t *testing.T) {
	setup(t)
	dir := makeTree(t, map[string][]byte{"photos/a": []byte("abc")})
	srv, rec := serve(t, "", filepath.Join(dir, "photos"))
	resp := call(t, srv, "POST", "/v1/start", nil, nil, map[string]any{"path": "photos"})
	if resp.StatusCode != 204 {
		t.Fatalf("start: %d", resp.StatusCode)
	}
	time.Sleep(20 * time.Millisecond)
	resp = call(t, srv, "POST", "/v1/done", nil, nil, map[string]any{"path": "photos", "ok": true})
	if resp.StatusCode != 204 {
		t.Fatalf("done: %d", resp.StatusCode)
	}
	resp = call(t, srv, "POST", "/v1/start", nil, nil, map[string]any{"path": "photos/a"})
	resp = call(t, srv, "POST", "/v1/done", nil, nil, map[string]any{"path": "photos/a", "ok": false})
	resp = call(t, srv, "POST", "/v1/start", nil, nil, map[string]any{"path": "nope"})
	if resp.StatusCode != 404 {
		t.Fatalf("start on unknown path: %d", resp.StatusCode)
	}
	evs := rec.all()
	if len(evs) != 4 {
		t.Fatalf("%d events: %+v", len(evs), evs)
	}
	if e := evs[0]; e.Receiver != "desk" || e.Path != "photos" || !e.Dir || e.Size != 3 || e.Done {
		t.Fatalf("start event: %+v", e)
	}
	if e := evs[1]; !e.Done || !e.OK || e.Took < 20*time.Millisecond {
		t.Fatalf("done event: %+v", e)
	}
	if e := evs[3]; !e.Done || e.OK || e.Dir || e.Path != "photos/a" {
		t.Fatalf("stopped event: %+v", e)
	}
	if resp = call(t, srv, "GET", "/v1/start", nil, nil, nil); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET start: %d", resp.StatusCode)
	}
}
