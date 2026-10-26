package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gett/internal/errs"
	"gett/internal/term"
)

// sshHome is the user's ~/.ssh; tests point it elsewhere.
var sshHome = func() string {
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".ssh")
}

func aliasFile() string { return filepath.Join(Home(), "ssh_config") }
func keyFile() string   { return filepath.Join(Home(), "key") }

// includeLine is the one line gett adds to ~/.ssh/config. Written with the
// literal "~" so the config stays valid if the home folder moves.
func includeLine() string {
	home, _ := os.UserHomeDir()
	p := aliasFile()
	if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
		p = "~/" + filepath.ToSlash(rel)
	}
	return "Include " + p
}

// ensureKey makes gett's own ed25519 key pair once and authorizes it with
// microsandbox once. gett never touches the user's own keys.
func ensureKey(ctx context.Context, m msb) error {
	if _, err := os.Stat(keyFile()); err != nil {
		if err := os.MkdirAll(Home(), 0o700); err != nil {
			return &errs.Write{Err: err}
		}
		out, err := exec.CommandContext(ctx, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", keyFile()).CombinedOutput()
		if err != nil {
			return &errs.Sandbox{Msg: "could not make an ssh key (" + term.Sanitize(strings.TrimSpace(string(out))) + ")"}
		}
	}
	marker := filepath.Join(Home(), "authorized")
	if _, err := os.Stat(marker); err == nil {
		return nil
	}
	if _, err := m.run(ctx, "ssh", "authorize", "--file", keyFile()+".pub"); err != nil {
		return &errs.Sandbox{Msg: "microsandbox did not accept the ssh key (" + term.Sanitize(err.Error()) + ")"}
	}
	return os.WriteFile(marker, []byte("ok\n"), 0o600)
}

// block is the alias that makes "ssh NAME" land in the sandbox. The transport
// is a pipe into msb, so host keys mean nothing here and would only break
// after rm + recreate.
func block(name, msbPath string) string {
	return fmt.Sprintf(`Host %s
  User root
  ProxyCommand %s ssh serve %s --stdio --no-inactivity-timeout
  IdentityFile %s
  IdentitiesOnly yes
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
  LogLevel ERROR
`, name, msbPath, name, keyFile())
}

// addHost rewrites the alias file with NAME's block added (or replaced).
func addHost(name, msbPath string) error {
	blocks := readBlocks()
	blocks[name] = block(name, msbPath)
	return writeBlocks(blocks)
}

func removeHost(name string) error {
	blocks := readBlocks()
	if _, ok := blocks[name]; !ok {
		return nil
	}
	delete(blocks, name)
	return writeBlocks(blocks)
}

// readBlocks parses gett's alias file into blocks keyed by host name.
func readBlocks() map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(aliasFile())
	if err != nil {
		return out
	}
	var name string
	var cur strings.Builder
	flush := func() {
		if name != "" {
			out[name] = cur.String()
		}
		name = ""
		cur.Reset()
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := sc.Text()
		if f := strings.Fields(line); len(f) >= 2 && strings.EqualFold(f[0], "Host") {
			flush()
			name = f[1]
		}
		if name != "" {
			cur.WriteString(line + "\n")
		}
	}
	flush()
	return out
}

func writeBlocks(blocks map[string]string) error {
	if err := os.MkdirAll(Home(), 0o700); err != nil {
		return &errs.Write{Err: err}
	}
	var buf bytes.Buffer
	buf.WriteString("# written by gett; one block per sandbox, removed by gett sandbox rm / reset\n")
	for _, name := range sortedKeys(blocks) {
		buf.WriteString("\n" + blocks[name])
	}
	if err := os.WriteFile(aliasFile(), buf.Bytes(), 0o600); err != nil {
		return &errs.Write{Err: err}
	}
	return nil
}

// ensureInclude puts gett's Include line at the top of ~/.ssh/config, where
// OpenSSH applies it unconditionally; after a Host block it would be scoped
// to that block and do nothing. Creates ~/.ssh and the file when missing.
func ensureInclude() (added bool, err error) {
	cfg := filepath.Join(sshHome(), "config")
	b, rerr := os.ReadFile(cfg)
	if rerr != nil && !os.IsNotExist(rerr) {
		return false, &errs.Write{Err: rerr}
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == includeLine() {
			return false, nil
		}
	}
	if err := os.MkdirAll(sshHome(), 0o700); err != nil {
		return false, &errs.Write{Err: err}
	}
	out := includeLine() + "\n"
	if len(b) > 0 {
		out += "\n" + string(b)
	}
	if err := os.WriteFile(cfg, []byte(out), 0o600); err != nil {
		return false, &errs.Write{Err: err}
	}
	return true, nil
}

// removeInclude takes gett's line out again, and the blank line it added.
// A config that then holds nothing else is removed.
func removeInclude() error {
	cfg := filepath.Join(sshHome(), "config")
	b, err := os.ReadFile(cfg)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(b), "\n")
	var out []string
	for i, line := range lines {
		if strings.TrimSpace(line) == includeLine() {
			if i+1 < len(lines) && strings.TrimSpace(lines[i+1]) == "" {
				lines[i+1] = "\x00" // drop the blank line gett added
			}
			continue
		}
		if line != "\x00" {
			out = append(out, line)
		}
	}
	rest := strings.TrimSpace(strings.Join(out, "\n"))
	if rest == "" {
		os.Remove(cfg)
		return nil
	}
	if err := os.WriteFile(cfg, []byte(strings.Join(out, "\n")), 0o600); err != nil {
		return &errs.Write{Err: err}
	}
	return nil
}

// hostTaken reports whether the user's own ssh config already has a Host
// with this exact name, so gett never shadows one.
func hostTaken(name string) bool {
	b, err := os.ReadFile(filepath.Join(sshHome(), "config"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || !strings.EqualFold(f[0], "Host") {
			continue
		}
		for _, h := range f[1:] {
			if h == name {
				return true
			}
		}
	}
	return false
}
