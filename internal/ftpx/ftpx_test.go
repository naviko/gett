package ftpx

import (
	"crypto/rand"
	"crypto/tls"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gett/internal/term"

	ftpserver "github.com/fclairamb/ftpserverlib"
	"github.com/spf13/afero"
)

// driver is the test server's disk with knobs: slow transfers so one can be
// interrupted while running, and one failing read or write to break a
// transfer off.
type driver struct {
	fs       afero.Fs
	noMLST   bool         // serve LIST only, like an old server
	stores   atomic.Int64 // files opened for writing (STOR/APPE)
	retrs    atomic.Int64 // files opened for reading (RETR)
	slow     atomic.Bool  // every server-side read and write takes a moment
	failAt   atomic.Int64 // first read or write past this many bytes of a file fails, once
	failures atomic.Int64
}

func (d *driver) GetSettings() (*ftpserver.Settings, error) {
	return &ftpserver.Settings{ListenAddr: "127.0.0.1:0", DisableMLSD: d.noMLST, DisableMLST: d.noMLST}, nil
}
func (d *driver) ClientConnected(ftpserver.ClientContext) (string, error) { return "test", nil }
func (d *driver) ClientDisconnected(ftpserver.ClientContext)              {}
func (d *driver) GetTLSConfig() (*tls.Config, error)                      { return nil, errors.New("no TLS") }
func (d *driver) AuthUser(_ ftpserver.ClientContext, user, pass string) (ftpserver.ClientDriver, error) {
	if user == "u" && pass == "p" {
		return &faultFs{d.fs, d}, nil
	}
	return nil, errors.New("bad credentials")
}

type faultFs struct {
	afero.Fs
	d *driver
}

func (s *faultFs) OpenFile(name string, flag int, perm os.FileMode) (afero.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR) != 0 {
		s.d.stores.Add(1)
	} else {
		s.d.retrs.Add(1)
	}
	f, err := s.Fs.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	return &faultFile{File: f, d: s.d}, nil
}

type faultFile struct {
	afero.File
	d     *driver
	moved int64
}

func (f *faultFile) hiccup(n int) bool {
	if f.d.slow.Load() {
		time.Sleep(3 * time.Millisecond)
	}
	at := f.d.failAt.Load()
	return at > 0 && f.moved+int64(n) > at && f.d.failures.CompareAndSwap(0, 1)
}

func (f *faultFile) Write(p []byte) (int, error) {
	if f.hiccup(len(p)) {
		return 0, errors.New("disk hiccup")
	}
	n, err := f.File.Write(p)
	f.moved += int64(n)
	return n, err
}

func (f *faultFile) Read(p []byte) (int, error) {
	if f.hiccup(len(p)) {
		return 0, errors.New("disk hiccup")
	}
	n, err := f.File.Read(p)
	f.moved += int64(n)
	return n, err
}

func startServer(t *testing.T, opts ...func(*driver)) (*driver, string) {
	t.Helper()
	term.Silent = true
	d := &driver{fs: afero.NewMemMapFs()}
	for _, o := range opts {
		o(d)
	}
	srv := ftpserver.NewFtpServer(d)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Stop() })
	return d, srv.Addr()
}

// tree is the sample content used by upload and download tests.
func tree() map[string][]byte {
	files := map[string][]byte{
		"a.txt":              []byte("hello"),
		"sub/b.bin":          make([]byte, 2<<20),
		"sub/deeper/c.empty": {},
	}
	rand.Read(files["sub/b.bin"])
	return files
}

// makeTree writes the sample tree into a temp dir.
func makeTree(t *testing.T) (string, map[string][]byte) {
	t.Helper()
	dir := t.TempDir()
	files := tree()
	for name, data := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, files
}

// seed puts the sample tree on the server under /up, plus an empty folder.
func seed(t *testing.T, d *driver) map[string][]byte {
	t.Helper()
	files := tree()
	for name, data := range files {
		if err := afero.WriteFile(d.fs, "/up/"+name, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.fs.MkdirAll("/up/empty", 0o755); err != nil {
		t.Fatal(err)
	}
	return files
}

// onTerminal makes downloads save files: under go test stdout is a pipe.
func onTerminal(t *testing.T) {
	t.Helper()
	tty := true
	term.StdoutIsTTY = &tty
	t.Cleanup(func() { term.StdoutIsTTY = nil })
}

// capture runs fn with stdout piped, as when gett feeds another program.
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

// waitFor polls until cond holds, failing the test after a few seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
