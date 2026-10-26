package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"gett/internal/term"
)

// ErrNotInstalled means microsandbox is missing and was not installed. The
// callers turn it into a note and exit 0: the download already happened.
var ErrNotInstalled = errors.New("microsandbox is not installed")

// installCmd is the official one-liner for this platform.
func installCmd() string {
	if runtime.GOOS == "windows" {
		return "irm https://install.microsandbox.dev/windows | iex"
	}
	return "curl -fsSL https://install.microsandbox.dev | sh"
}

// supported reports a platform microsandbox can run on, with the reason
// when it cannot.
func supported() (bool, string) {
	if runtime.GOOS == "darwin" && runtime.GOARCH != "arm64" {
		return false, "sandboxes need an Apple Silicon Mac"
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		return false, "sandboxes need macOS, Linux or Windows"
	}
	return true, ""
}

// findMsb looks on PATH, then where the installer puts it: a fresh install
// is not on PATH in the shell that ran gett.
func findMsb() (string, bool) {
	exe := "msb"
	if runtime.GOOS == "windows" {
		exe = "msb.exe"
	}
	if p, err := exec.LookPath(exe); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs, true
		}
		return p, true
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		filepath.Join(home, ".local", "bin", exe),
		filepath.Join(home, ".microsandbox", "bin", exe),
	} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, true
		}
	}
	return "", false
}

// ask prints the one question gett ever asks and reads the answer; tests
// replace it.
var ask = func(question string) (yes bool) {
	fmt.Fprint(os.Stderr, question)
	return readYes(os.Stdin)
}

// readYes reads one answer: Enter, y or yes mean yes. End of input before
// an answer means no; nothing is ever installed without an answer.
func readYes(r io.Reader) bool {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "" || line == "y" || line == "yes"
}

// installer runs the official install script with its output on the
// terminal; tests replace it.
var installer = func(ctx context.Context) error {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", installCmd())
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", installCmd())
	}
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stderr, os.Stderr, os.Stdin
	return cmd.Run()
}

// stdinTTY reports whether a question can be asked; tests replace it.
var stdinTTY = func() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// ensureMsb returns the msb binary, offering to install it once on a
// terminal. Declined, piped or failed: the install line is printed and
// ErrNotInstalled returned.
func ensureMsb(ctx context.Context) (msb, error) {
	if ok, why := supported(); !ok {
		term.Info("%s", why)
		return msb{}, ErrNotInstalled
	}
	if p, ok := findMsb(); ok {
		return msb{path: p}, nil
	}
	if !term.StdoutTTY() || !stdinTTY() {
		term.Info("sandboxes run in microsandbox, which is not installed; to install it:\n  %s", installCmd())
		return msb{}, ErrNotInstalled
	}
	if !ask("sandboxes run in microsandbox, which is not installed.\ninstall it now into ~/.microsandbox? [Y/n] ") {
		term.Info("to install it later:\n  %s", installCmd())
		return msb{}, ErrNotInstalled
	}
	if err := installer(ctx); err != nil {
		term.Info("microsandbox did not install (%s)", term.Sanitize(err.Error()))
		return msb{}, ErrNotInstalled
	}
	if p, ok := findMsb(); ok {
		return msb{path: p}, nil
	}
	term.Info("microsandbox installed but msb was not found; open a new terminal and run the command again")
	return msb{}, ErrNotInstalled
}
