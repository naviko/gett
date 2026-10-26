// Package sandbox puts a download into a Microsandbox sandbox, unpacks and
// scans it there, and makes the VM reachable with "ssh NAME". Nothing from
// the download is parsed on the host: the guest runs gett's own Linux build
// to unpack and YARA-X to scan.
package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gett/internal/download"
	"gett/internal/errs"
	"gett/internal/term"
)

const (
	label      = "gett"
	guestWork  = "/root" // the home folder: ssh lands on the files
	guestRules = "/rules"
	memory     = "1G"
	cpus       = "2"
	minQuota   = 8 << 30 // bytes; the work folder's quota is at least this
	maxName    = 32
)

// Home is where gett keeps sandbox state: ~/.gett/sandbox. Tests move it.
var Home = func() string {
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".gett", "sandbox")
}

// Image is the OCI image sandboxes boot from.
func Image() string {
	if v := os.Getenv("GETT_SANDBOX_IMAGE"); v != "" {
		return v
	}
	return "ubuntu"
}

// Mode says what happens to the sandbox after the report.
type Mode int

const (
	Keep      Mode = iota // leave it, print "ssh NAME"
	Throwaway             // remove it right after the scan
)

// msb runs the microsandbox CLI.
type msb struct{ path string }

var errNotFound = errors.New("sandbox not found")

// run executes msb with args and returns its stdout. Stderr becomes the error.
func (m msb) run(ctx context.Context, args ...string) (string, error) {
	term.Debug("msb %s", term.Sanitize(strings.Join(args, " ")))
	cmd := exec.CommandContext(ctx, m.path, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		msg = strings.TrimPrefix(msg, "error: ")
		if strings.Contains(msg, "sandbox not found") {
			return out.String(), errNotFound
		}
		return out.String(), errors.New(msg)
	}
	return out.String(), nil
}

// exec runs a command inside a sandbox, starting it if it is stopped, and
// returns stdout and stderr separately.
func (m msb) exec(ctx context.Context, name string, command ...string) (stdout, stderr string, err error) {
	args := append([]string{"exec", name, "--no-stdin", "-q", "--"}, command...)
	term.Debug("msb %s", term.Sanitize(strings.Join(args, " ")))
	cmd := exec.CommandContext(ctx, m.path, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	return out.String(), errb.String(), err
}

// execTTY runs a command inside a sandbox with a terminal, for programs that
// behave differently when piped. Output comes back as one stream.
func (m msb) execTTY(ctx context.Context, name string, command ...string) (string, error) {
	args := append([]string{"exec", name, "-t", "-q", "--"}, command...)
	term.Debug("msb %s", term.Sanitize(strings.Join(args, " ")))
	cmd := exec.CommandContext(ctx, m.path, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return strings.ReplaceAll(out.String(), "\r\n", "\n"), err
}

// names lists every sandbox msb knows, gett's or not.
func (m msb) names(ctx context.Context, onlyGett bool) ([]string, error) {
	args := []string{"ls", "-q"}
	if onlyGett {
		args = append(args, "--label", label)
	}
	out, err := m.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" && l != "No sandboxes found." {
			names = append(names, l)
		}
	}
	return names, nil
}

// Open copies src (a file or a folder) into a new sandbox, unpacks and
// scans it there, prints the report and, in Keep mode, the ssh line.
func Open(ctx context.Context, src string, mode Mode, client *http.Client) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	m, err := ensureMsb(ctx)
	if err != nil {
		return err
	}
	tools, err := ensureTools(ctx, client)
	if err != nil {
		return err
	}
	if err := ensureKey(ctx, m); err != nil {
		return err
	}

	name, err := pick(ctx, m, stem(filepath.Base(src)))
	if err != nil {
		return &errs.Sandbox{Msg: "could not list sandboxes (" + term.Sanitize(err.Error()) + ")"}
	}
	dir := filepath.Join(Home(), name)
	work := filepath.Join(dir, "work")
	size, err := copyIn(src, fi, work)
	if err != nil {
		os.RemoveAll(dir)
		return err
	}

	args := []string{"create", Image(), "-n", name, "--label", label, "-m", memory, "-c", cpus,
		"--mount-dir", fmt.Sprintf("%s:%s:quota=%dG", work, guestWork, quotaGiB(size)),
		"--copy-file", tools.gett + ":/usr/local/bin/gett",
		"-w", guestWork,
		"--shell", "/bin/bash", "-H", name, "-q"}
	if tools.yr != "" {
		args = append(args, "--copy-file", tools.yr+":/usr/local/bin/yr")
	}
	if tools.rules != "" {
		args = append(args, "--mount-dir", tools.rules+":"+guestRules+":ro")
	}
	if _, err := m.run(ctx, args...); err != nil {
		os.RemoveAll(dir)
		return &errs.Sandbox{Msg: fmt.Sprintf("sandbox %s could not start (%s); your files are in %s", name, term.Sanitize(err.Error()), src)}
	}
	fail := func(err error) error {
		m.run(context.WithoutCancel(ctx), "rm", "-f", name, "-q")
		os.RemoveAll(dir)
		return err
	}

	var note string
	if !fi.IsDir() {
		if note, err = guestExtract(ctx, m, name, work, filepath.Base(src)); err != nil {
			return fail(err)
		}
	}

	rep, err := scan(ctx, m, name, work, tools)
	if err != nil {
		return fail(err)
	}
	if note != "" {
		rep.skipped = append(rep.skipped, note)
	}
	if ctx.Err() != nil {
		return fail(ctx.Err())
	}

	if mode == Throwaway {
		rep.print()
		m.run(context.WithoutCancel(ctx), "rm", "-f", name, "-q")
		os.RemoveAll(dir)
		return nil
	}

	if err := os.WriteFile(filepath.Join(dir, "findings"), []byte(fmt.Sprintf("%d\n", len(rep.findings))), 0o600); err != nil {
		return fail(&errs.Write{Err: err})
	}
	banner := filepath.Join(dir, "banner")
	if err := os.WriteFile(banner, []byte(rep.text(name)), 0o600); err == nil {
		m.run(ctx, "copy", "-q", banner, name+":/etc/motd")
	}
	if err := addHost(name, m.path); err != nil {
		return fail(err)
	}
	added, err := ensureInclude()
	if err != nil {
		return fail(err)
	}
	m.run(context.WithoutCancel(ctx), "stop", name, "-q")

	term.Info("sandbox %s is ready", name)
	rep.print()
	if added {
		term.Info("added \"%s\" to %s", includeLine(), filepath.Join(sshHome(), "config"))
	}
	term.Info("  ssh %s", name)
	return nil
}

// guestExtract unpacks the archive inside the sandbox with gett's own Linux
// build, so no host parser ever sees it. The guest gett needs a terminal on
// stdout to save files rather than stream them, and its messages go to a
// log on the shared folder so no progress bar lands in the capture. Only
// "extracted ..." is shown; a file that is not an archive was simply copied
// and needs no comment; an archive that could not be opened comes back as a
// note for the report's "not scanned" list.
func guestExtract(ctx context.Context, m msb, name, work, file string) (note string, err error) {
	const log = ".gett-extract.log"
	out, err := m.execTTY(ctx, name, "bash", "-c", fmt.Sprintf("gett extract %s/%s 2>%s/%s", guestWork, shellQuote(file), guestWork, log))
	logPath := filepath.Join(work, log)
	msgs, _ := os.ReadFile(logPath)
	os.Remove(logPath)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 2 {
			detail := lastLine(string(msgs))
			if detail == "" {
				detail = lastLine(out)
			}
			return "", &errs.Sandbox{Msg: fmt.Sprintf("unpacking inside sandbox %s failed (%s)", name, term.Sanitize(detail))}
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(string(msgs)), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "gett: "))
		switch {
		case strings.HasPrefix(line, "extracted "):
			term.Info("%s", term.Sanitize(strings.Replace(line, " to "+guestWork+"/", " to ~/", 1)))
		case strings.HasSuffix(line, "is not an archive, left as is"):
			// copied in as it is; nothing to say
		case strings.HasSuffix(line, ", left as is"):
			// "/work/x.zip needs a password, left as is" -> "x.zip (needs a password)"
			msg := strings.TrimSuffix(line, ", left as is")
			rel := strings.TrimPrefix(strings.TrimPrefix(msg, guestWork), "/")
			if i := strings.Index(rel, " "); i > 0 {
				note = fmt.Sprintf("%s (%s)", rel[:i], rel[i+1:])
			} else {
				note = term.Sanitize(msg)
			}
		}
	}
	return note, nil
}

// shellQuote makes a file name safe for the guest's bash.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// copyIn puts src into work and reports its size. A folder keeps its name.
func copyIn(src string, fi os.FileInfo, work string) (int64, error) {
	if err := os.MkdirAll(work, 0o755); err != nil {
		return 0, &errs.Write{Err: err}
	}
	dst := filepath.Join(work, filepath.Base(src))
	if fi.IsDir() {
		if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
			return 0, &errs.Write{Err: err}
		}
		var n int64
		filepath.WalkDir(dst, func(_ string, d os.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				if i, err := d.Info(); err == nil {
					n += i.Size()
				}
			}
			return nil
		})
		return n, nil
	}
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return 0, &errs.Write{Err: err}
	}
	n, err := io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, &errs.Write{Err: err}
	}
	return n, nil
}

// quotaGiB sizes the work folder's quota: four times the input, at least
// minQuota, so a decompression bomb fills the quota and not the disk.
func quotaGiB(size int64) int64 {
	q := size * 4
	if q < minQuota {
		q = minQuota
	}
	return (q + (1<<30 - 1)) >> 30
}

var unsafeName = regexp.MustCompile(`[^a-z0-9-]+`)

// stem turns a file or folder name into a sandbox name: tool.tar.gz → tool.
func stem(base string) string {
	s := strings.ToLower(base)
	stripped := false
	for _, ext := range []string{".tar.gz", ".tar.xz", ".tar.zst", ".tar.bz2", ".tar.lz4", ".tar.br", ".tgz", ".tbz2", ".txz"} {
		if strings.HasSuffix(s, ext) && len(s) > len(ext) {
			s = s[:len(s)-len(ext)]
			stripped = true
			break
		}
	}
	// A plain extension is short and has a letter in it: ".zip", ".7z",
	// ".md". The ".8" of gett-1.0.8 is part of the name.
	if e := filepath.Ext(s); !stripped && e != "" && len(e) < len(s) && len(e) <= 5 && strings.ContainsAny(e, "abcdefghijklmnopqrstuvwxyz") {
		s = s[:len(s)-len(e)]
	}
	s = strings.Trim(unsafeName.ReplaceAllString(s, "-"), "-")
	if len(s) > maxName {
		s = strings.Trim(s[:maxName], "-")
	}
	if s == "" {
		s = "sandbox"
	}
	return s
}

// pick finds a free name: not an existing sandbox (gett's or not) and not a
// Host in the user's ssh config.
func pick(ctx context.Context, m msb, want string) (string, error) {
	existing, err := m.names(ctx, false)
	if err != nil {
		return "", err
	}
	taken := map[string]bool{}
	for _, n := range existing {
		taken[n] = true
	}
	name := want
	for i := 2; taken[name] || hostTaken(name); i++ {
		name = fmt.Sprintf("%s-%d", want, i)
	}
	return name, nil
}

// List prints gett's sandboxes: name, state, findings, folder.
func List(ctx context.Context, w io.Writer) error {
	m, err := ensureMsb(ctx)
	if err != nil {
		return err
	}
	out, err := m.run(ctx, "ls", "--label", label, "--format", "json")
	if err != nil {
		return &errs.Sandbox{Msg: "could not list sandboxes (" + term.Sanitize(err.Error()) + ")"}
	}
	var rows []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil && strings.TrimSpace(out) != "" {
		return &errs.Sandbox{Msg: "could not read the sandbox list (" + term.Sanitize(err.Error()) + ")"}
	}
	if len(rows) == 0 {
		term.Info("no sandboxes")
		return nil
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	for _, r := range rows {
		findings := ""
		if b, err := os.ReadFile(filepath.Join(Home(), r.Name, "findings")); err == nil {
			if n := strings.TrimSpace(string(b)); n != "0" && n != "" {
				findings = n + " findings"
				if n == "1" {
					findings = "1 finding"
				}
			}
		}
		name := term.Sanitize(r.Name)
		fmt.Fprintf(w, "%-24s %-28s %-8s %-12s %s\n", name, "ssh "+name, strings.ToLower(r.Status), findings, filepath.Join(Home(), r.Name, "work"))
	}
	return nil
}

// Remove deletes sandboxes by name: the VM (stopped first if running), the
// files and the ssh alias. Unknown names are noted and skipped.
func Remove(ctx context.Context, names []string) error {
	m, err := ensureMsb(ctx)
	if err != nil {
		return err
	}
	for _, name := range splitNames(names) {
		_, err := m.run(ctx, "rm", "-f", name)
		switch {
		case errors.Is(err, errNotFound):
			if _, serr := os.Stat(filepath.Join(Home(), name)); serr != nil {
				term.Info("no sandbox called %s", term.Sanitize(name))
				continue
			}
		case err != nil:
			return &errs.Sandbox{Msg: fmt.Sprintf("could not remove %s (%s)", name, term.Sanitize(err.Error()))}
		}
		removeHost(name)
		os.RemoveAll(filepath.Join(Home(), name))
		term.Info("removed %s", term.Sanitize(name))
	}
	return nil
}

// Reset removes every sandbox gett made, gett's folder, and the Include line
// in ~/.ssh/config, leaving the machine as it was before the first sandbox.
func Reset(ctx context.Context) error {
	var names []string
	if p, ok := findMsb(); ok {
		m := msb{path: p}
		var err error
		if names, err = m.names(ctx, true); err != nil {
			return &errs.Sandbox{Msg: "could not list sandboxes (" + term.Sanitize(err.Error()) + ")"}
		}
		for _, n := range names {
			if _, err := m.run(ctx, "rm", "-f", n); err != nil && !errors.Is(err, errNotFound) {
				return &errs.Sandbox{Msg: fmt.Sprintf("could not remove %s (%s)", n, term.Sanitize(err.Error()))}
			}
			term.Info("removed %s", term.Sanitize(n))
		}
	}
	_, homeErr := os.Stat(Home())
	hadInclude := false
	if b, err := os.ReadFile(filepath.Join(sshHome(), "config")); err == nil {
		hadInclude = strings.Contains(string(b), includeLine())
	}
	if err := removeInclude(); err != nil {
		return err
	}
	if err := os.RemoveAll(Home()); err != nil {
		return &errs.Write{Err: err}
	}
	if len(names) == 0 && homeErr != nil && !hadInclude {
		term.Info("no sandboxes")
	}
	return nil
}

func splitNames(args []string) []string {
	var out []string
	for _, a := range args {
		for _, n := range strings.FieldsFunc(a, func(r rune) bool { return r == ',' || r == ' ' }) {
			out = append(out, n)
		}
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// human is download.Human, here so the report code reads cleanly.
func human(n int64) string { return download.Human(n) }
