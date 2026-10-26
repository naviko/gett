package sandbox

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gett/internal/term"
)

// fakeMsb writes a stand-in msb script first on PATH. It logs every call,
// keeps the set of sandbox names in a state file, and answers ls from it.
func fakeMsb(t *testing.T) (logFile string) {
	t.Helper()
	dir := t.TempDir()
	logFile = filepath.Join(dir, "calls.log")
	state := filepath.Join(dir, "names")
	script := `#!/bin/sh
echo "$@" >> "` + logFile + `"
case "$1" in
  create) shift; while [ $# -gt 0 ]; do if [ "$1" = "-n" ]; then echo "$2" >> "` + state + `"; fi; shift; done ;;
  rm) shift; while [ $# -gt 0 ]; do case "$1" in -f|-q|--label|gett) ;; *) if ! grep -qx "$1" "` + state + `" 2>/dev/null; then echo "error: sandbox not found: $1" >&2; exit 1; fi; grep -vx "$1" "` + state + `" > "` + state + `.n" 2>/dev/null; mv "` + state + `.n" "` + state + `";; esac; shift; done ;;
  ls) if echo "$@" | grep -q json; then printf '['; first=1; for n in $(cat "` + state + `" 2>/dev/null); do [ $first = 1 ] || printf ','; first=0; printf '{"name":"%s","status":"Stopped","image":"ubuntu"}' "$n"; done; printf ']\n'; else cat "` + state + `" 2>/dev/null || true; fi ;;
  exec) exit 0 ;;
  *) exit 0 ;;
esac
`
	p := filepath.Join(dir, "msb")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logFile
}

func setup(t *testing.T) (home, ssh string) {
	t.Helper()
	root := t.TempDir()
	home, ssh = filepath.Join(root, "gett"), filepath.Join(root, "ssh")
	Home = func() string { return home }
	sshHome = func() string { return ssh }
	tty := true
	term.StdoutIsTTY = &tty
	term.Silent = true
	ensureTools = func(context.Context, *http.Client) (tools, error) {
		bin := filepath.Join(home, "bin")
		os.MkdirAll(bin, 0o755)
		os.WriteFile(filepath.Join(bin, "gett"), []byte("#!/bin/sh\n"), 0o755)
		return tools{gett: filepath.Join(bin, "gett")}, nil
	}
	scan = func(context.Context, msb, string, string, tools) (*report, error) {
		return &report{files: 2, bytes: 10, findings: []finding{{path: "a/evil.sh", rule: "TEST_Rule", desc: "a test", author: "gett"}}}, nil
	}
	t.Cleanup(func() {
		term.StdoutIsTTY = nil
		term.Silent = false
	})
	return home, ssh
}

func calls(t *testing.T, logFile string) string {
	b, _ := os.ReadFile(logFile)
	return string(b)
}

func writeZip(t *testing.T, p string) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("tool/a.txt")
	w.Write([]byte("hi"))
	zw.Close()
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOpenKeepAndRemove(t *testing.T) {
	logFile := fakeMsb(t)
	home, ssh := setup(t)
	// A user config that already has a Host block: the Include must still
	// land first, and "tool" must not shadow the user's own "tool" host.
	os.MkdirAll(ssh, 0o700)
	os.WriteFile(filepath.Join(ssh, "config"), []byte("Host tool\n  HostName 10.0.0.5\n"), 0o600)
	os.WriteFile(filepath.Join(home, "key"), []byte("k"), 0o600)
	os.WriteFile(filepath.Join(home, "key.pub"), []byte("ssh-ed25519 AAAA"), 0o600)

	dir := t.TempDir()
	z := filepath.Join(dir, "tool.zip")
	writeZip(t, z)
	if err := Open(context.Background(), z, Keep, nil); err != nil {
		t.Fatal(err)
	}
	log := calls(t, logFile)
	if !strings.Contains(log, "create ubuntu -n tool-2 --label gett") {
		t.Fatalf("expected tool-2 (tool is a Host in the user's config):\n%s", log)
	}
	if !strings.Contains(log, "--mount-dir "+filepath.Join(home, "tool-2", "work")+":/root:quota=8G") || !strings.Contains(log, "--copy-file "+filepath.Join(home, "bin", "gett")+":/usr/local/bin/gett") {
		t.Fatalf("mount or copy missing:\n%s", log)
	}
	if !strings.Contains(log, "exec tool-2 -t -q -- bash -c gett extract /root/'tool.zip'") || !strings.Contains(log, "stop tool-2 -q") {
		t.Fatalf("extract or stop missing:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(home, "tool-2", "work", "tool.zip")); err != nil {
		t.Fatal("archive not copied into the work folder")
	}
	if b, _ := os.ReadFile(filepath.Join(home, "tool-2", "findings")); strings.TrimSpace(string(b)) != "1" {
		t.Fatalf("findings file: %q", b)
	}
	cfg, _ := os.ReadFile(filepath.Join(ssh, "config"))
	if !strings.HasPrefix(string(cfg), includeLine()+"\n\nHost tool\n") {
		t.Fatalf("Include must come first:\n%s", cfg)
	}
	alias, _ := os.ReadFile(filepath.Join(home, "ssh_config"))
	if !strings.Contains(string(alias), "Host tool-2\n") || !strings.Contains(string(alias), "ssh serve tool-2 --stdio") {
		t.Fatalf("alias block wrong:\n%s", alias)
	}

	// A second sandbox does not add the Include twice.
	z2 := filepath.Join(dir, "game.zip")
	writeZip(t, z2)
	if err := Open(context.Background(), z2, Keep, nil); err != nil {
		t.Fatal(err)
	}
	cfg, _ = os.ReadFile(filepath.Join(ssh, "config"))
	if strings.Count(string(cfg), includeLine()) != 1 {
		t.Fatalf("Include duplicated:\n%s", cfg)
	}

	// rm: unknown names are notes, known ones go away with folder and alias.
	if err := Remove(context.Background(), []string{"nosuch,tool-2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "tool-2")); err == nil {
		t.Fatal("folder still there after rm")
	}
	alias, _ = os.ReadFile(filepath.Join(home, "ssh_config"))
	if strings.Contains(string(alias), "Host tool-2\n") || !strings.Contains(string(alias), "Host game\n") {
		t.Fatalf("alias file after rm:\n%s", alias)
	}
	cfg, _ = os.ReadFile(filepath.Join(ssh, "config"))
	if !strings.Contains(string(cfg), includeLine()) {
		t.Fatal("rm must leave the Include in place")
	}

	// reset: everything gone, the user's config back as it was.
	if err := Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(home); err == nil {
		t.Fatal("home still there after reset")
	}
	cfg, _ = os.ReadFile(filepath.Join(ssh, "config"))
	if string(cfg) != "Host tool\n  HostName 10.0.0.5\n" {
		t.Fatalf("user config not restored:\n%q", cfg)
	}
	if !strings.Contains(calls(t, logFile), "rm -f game") {
		t.Fatal("reset did not remove the remaining sandbox")
	}
}

func TestThrowawayAndFolder(t *testing.T) {
	logFile := fakeMsb(t)
	home, ssh := setup(t)
	os.MkdirAll(home, 0o700)
	os.WriteFile(filepath.Join(home, "key"), []byte("k"), 0o600)
	os.WriteFile(filepath.Join(home, "key.pub"), []byte("ssh-ed25519 AAAA"), 0o600)
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("a"), 0o644)
	if err := Open(context.Background(), src, Throwaway, nil); err != nil {
		t.Fatal(err)
	}
	log := calls(t, logFile)
	if strings.Contains(log, "gett extract") {
		t.Fatal("a folder has nothing to unpack")
	}
	if !strings.Contains(log, "rm -f "+filepath.Base(src)) && !strings.Contains(log, "rm -f "+stem(filepath.Base(src))) {
		t.Fatalf("throwaway sandbox not removed:\n%s", log)
	}
	if entries, _ := os.ReadDir(home); len(entries) != 4 { // key, key.pub, authorized, bin
		t.Fatalf("throwaway left state behind: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(home, stem(filepath.Base(src)))); err == nil {
		t.Fatal("throwaway left its folder behind")
	}
	if _, err := os.Stat(filepath.Join(ssh, "config")); err == nil {
		t.Fatal("throwaway must not touch ssh config")
	}
}

func TestNotInstalled(t *testing.T) {
	setup(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	stdinTTY = func() bool { return false }
	err := Open(context.Background(), "nope.zip", Keep, nil)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing file is reported before anything else: %v", err)
	}
	src := filepath.Join(t.TempDir(), "x.zip")
	writeZip(t, src)
	if err := Open(context.Background(), src, Keep, nil); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("want ErrNotInstalled when piped, got %v", err)
	}
	stdinTTY = func() bool { return true }
	asked := 0
	ask = func(string) bool { asked++; return false }
	if err := Open(context.Background(), src, Keep, nil); !errors.Is(err, ErrNotInstalled) || asked != 1 {
		t.Fatalf("declined install: %v, asked %d", err, asked)
	}
	ask = func(string) bool { return true }
	installer = func(context.Context) error { return errors.New("no network") }
	if err := Open(context.Background(), src, Keep, nil); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("failed install: %v", err)
	}
}

func TestReadYes(t *testing.T) {
	for in, want := range map[string]bool{"\n": true, "y\n": true, "YES\n": true, "n\n": false, "no\n": false, "": false, "y": true} {
		if got := readYes(strings.NewReader(in)); got != want {
			t.Errorf("readYes(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestStem(t *testing.T) {
	for in, want := range map[string]string{
		"tool.tar.gz": "tool", "Some File-1.2.zip": "some-file-1-2", "gett-1.0.8.tgz": "gett-1-0-8",
		"README.md": "readme", "noext": "noext", "....": "sandbox", "disk.img.xz": "disk-img",
	} {
		if got := stem(in); got != want {
			t.Errorf("stem(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReportLines(t *testing.T) {
	r := &report{files: 3, bytes: 2048}
	if got := r.lines(); len(got) != 0 {
		t.Fatalf("a clean scan prints nothing, got %q", got)
	}
	r.findings = []finding{{path: "a/b.exe", rule: "X_Rule", desc: "Detects X", author: "me"}}
	r.skipped = []string{"big.iso (3.0 GiB, over 200.0 MiB)"}
	got := r.lines()
	if len(got) != 2 || got[0] != "a/b.exe  X_Rule  Detects X (me)" || got[1] != "not scanned: big.iso (3.0 GiB, over 200.0 MiB)" {
		t.Fatalf("report: %q", got)
	}
	if scanError("error: can't open `/root/x/y`: No such file or directory (os error 2)") != "x/y (can't open: No such file or directory (os error 2))" {
		t.Fatalf("scanError: %q", scanError("error: can't open `/root/x/y`: No such file or directory (os error 2)"))
	}
}
