package torrentdl

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gett/internal/term"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// seed builds a torrent from the contents of src and serves it from an
// in-process client with no trackers or DHT. It returns the .torrent path
// and a hook that points a downloading torrent at the seeder.
func seed(t *testing.T, src string) (string, func(hash string)) {
	t.Helper()
	info := metainfo.Info{PieceLength: 256 << 10}
	if err := info.BuildFromFilePath(src); err != nil {
		t.Fatal(err)
	}
	mi := &metainfo.MetaInfo{}
	var err error
	if mi.InfoBytes, err = bencode.Marshal(info); err != nil {
		t.Fatal(err)
	}
	torrentFile := filepath.Join(t.TempDir(), "test.torrent")
	f, err := os.Create(torrentFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := mi.Write(f); err != nil {
		t.Fatal(err)
	}
	f.Close()

	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = filepath.Dir(src)
	cfg.Seed = true
	cfg.NoDHT = true
	cfg.DisableTrackers = true
	cfg.DisablePEX = true
	cfg.ListenPort = 0
	cfg.NoDefaultPortForwarding = true
	cfg.Slogger = slog.New(slog.NewTextHandler(io.Discard, nil))
	seeder, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { seeder.Close() })
	st, err := seeder.AddTorrent(mi)
	if err != nil {
		t.Fatal(err)
	}
	<-st.GotInfo()
	if st.BytesCompleted() != st.Length() {
		t.Fatalf("seeder is not complete: %d of %d", st.BytesCompleted(), st.Length())
	}

	// The downloader learns about the seeder through the package test hook.
	connect := func(string) {
		testHook = func(tt *torrent.Torrent) { tt.AddClientPeer(seeder) }
	}
	t.Cleanup(func() { testHook = nil })
	return torrentFile, connect
}

func setup(t *testing.T) string {
	t.Helper()
	tty := true
	term.StdoutIsTTY = &tty
	term.Silent = true
	t.Cleanup(func() { term.StdoutIsTTY = nil })
	dir := t.TempDir()
	old, _ := os.Getwd()
	os.Chdir(dir)
	t.Cleanup(func() { os.Chdir(old) })
	return dir
}

func writeRandom(t *testing.T, path string, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	rand.Read(b)
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return b
}

func hashOf(b []byte) [32]byte { return sha256.Sum256(b) }

func fileHash(t *testing.T, p string) [32]byte {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return hashOf(b)
}

func TestIsTorrent(t *testing.T) {
	yes := []string{"magnet:?xt=urn:btih:abc", "MAGNET:?xt=1", "x.torrent", "https://h/a/b.torrent?x=1", "/tmp/a.TORRENT"}
	no := []string{"https://h/a.torrent.zip", "https://h/file.bin", "movie.mp4", "https://h/?f=a.torrent"}
	for _, s := range yes {
		if !IsTorrent(s) {
			t.Errorf("%q should be a torrent source", s)
		}
	}
	for _, s := range no {
		if IsTorrent(s) {
			t.Errorf("%q should not be a torrent source", s)
		}
	}
}

func TestSingleFile(t *testing.T) {
	setup(t)
	srcDir := t.TempDir()
	data := writeRandom(t, filepath.Join(srcDir, "payload.bin"), 4<<20)
	torrentFile, connect := seed(t, filepath.Join(srcDir, "payload.bin"))
	mi, _ := metainfo.LoadFromFile(torrentFile)
	connect(mi.HashInfoBytes().HexString())

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := Run(ctx, Options{Source: torrentFile}); err != nil {
		t.Fatal(err)
	}
	if fileHash(t, "payload.bin") != hashOf(data) {
		t.Fatal("content mismatch")
	}
	left, _ := filepath.Glob("*.part")
	if len(left) != 0 {
		t.Fatalf("working directory left behind: %v", left)
	}

	// Never overwrite: a second run lands beside the first.
	connect(mi.HashInfoBytes().HexString())
	if _, err := Run(ctx, Options{Source: torrentFile}); err != nil {
		t.Fatal(err)
	}
	if fileHash(t, "payload (1).bin") != hashOf(data) {
		t.Fatal("second copy missing or wrong")
	}

	// Folder target.
	connect(mi.HashInfoBytes().HexString())
	if _, err := Run(ctx, Options{Source: torrentFile, Name: "out/"}); err != nil {
		t.Fatal(err)
	}
	if fileHash(t, "out/payload.bin") != hashOf(data) {
		t.Fatal("folder target failed")
	}
}

func TestMultiFileAndResume(t *testing.T) {
	setup(t)
	srcDir := filepath.Join(t.TempDir(), "album")
	a := writeRandom(t, filepath.Join(srcDir, "one.bin"), 3<<20)
	b := writeRandom(t, filepath.Join(srcDir, "sub", "two.bin"), 2<<20)
	torrentFile, connect := seed(t, srcDir)
	mi, _ := metainfo.LoadFromFile(torrentFile)
	hash := mi.HashInfoBytes().HexString()

	// Interrupt early: cancel as soon as some data is on disk.
	ctx, cancel := context.WithCancel(context.Background())
	connect(hash)
	done := make(chan error, 1)
	go func() { _, err := Run(ctx, Options{Source: torrentFile}); done <- err }()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if size := dirSize(filepath.Join(hash+".part", "album")); size > 256<<10 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err == nil {
		t.Log("download finished before it could be interrupted; resume not exercised")
	} else if !exists(hash + ".part") {
		t.Fatal("interrupted download left no data to resume from")
	}

	// Rerun: completes.
	connect(hash)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel2()
	if _, err := Run(ctx2, Options{Source: torrentFile}); err != nil {
		t.Fatal(err)
	}
	if fileHash(t, "album/one.bin") != hashOf(a) || fileHash(t, "album/sub/two.bin") != hashOf(b) {
		t.Fatal("multi-file content mismatch")
	}
	if exists(hash + ".part") {
		t.Fatal("working directory left behind")
	}
}

func TestPipeSingleFile(t *testing.T) {
	setup(t)
	srcDir := t.TempDir()
	data := writeRandom(t, filepath.Join(srcDir, "stream.bin"), 1<<20)
	torrentFile, connect := seed(t, filepath.Join(srcDir, "stream.bin"))
	mi, _ := metainfo.LoadFromFile(torrentFile)
	connect(mi.HashInfoBytes().HexString())

	tty := false
	term.StdoutIsTTY = &tty
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	got := make(chan []byte)
	go func() { b, _ := io.ReadAll(r); got <- b }()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := Run(ctx, Options{Source: torrentFile})
	w.Close()
	os.Stdout = old
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(<-got, data) {
		t.Fatal("piped content mismatch")
	}
	if exists("stream.bin") {
		t.Fatal("piped download must not write a file")
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func dirSize(root string) int64 {
	var n int64
	filepath.Walk(root, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			n += fi.Size()
		}
		return nil
	})
	return n
}
