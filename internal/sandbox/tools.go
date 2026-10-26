package sandbox

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gett/internal/errs"
	"gett/internal/httpx"
	"gett/internal/term"
)

// The scanner and the rules gett puts into every sandbox. The yr version and
// checksums are pinned here so a gett release always pairs with a scanner
// it was tested with; updating is a matter of changing these lines.
const (
	yrVersion = "1.22.0"
	yrBase    = "https://github.com/VirusTotal/yara-x/releases/download/v" + yrVersion + "/yara-x-v" + yrVersion + "-"
	rulesURL  = "https://github.com/YARAHQ/yara-forge/releases/latest/download/yara-forge-rules-core.zip"
	rulesTTL  = 7 * 24 * time.Hour
	gettBase  = "https://github.com/naviko/gett/releases/download/"
)

var yrSums = map[string]string{ // guest arch -> sha256 of the tarball
	"aarch64": "b1b90f0686ae3e9f607c07b743fd96d6283d9ea5e2fd764919b8505de8b63d4e",
	"x86_64":  "7f470770b2c1a0d97f02298fc194978d999074d147d003d33dd62775761439db",
}

// tools are the host-side paths copied or mounted into the guest.
type tools struct {
	gett  string // gett for Linux, required
	yr    string // YARA-X, "" when unavailable
	rules string // folder holding core.yarc, "" when unavailable
	stale time.Time
}

// ensureTools fetches what is missing; tests replace it.
var ensureTools = func(ctx context.Context, client *http.Client) (tools, error) {
	bin := filepath.Join(Home(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return tools{}, &errs.Write{Err: err}
	}
	var t tools
	var err error
	if t.gett, err = ensureGett(ctx, client, bin); err != nil {
		return t, err
	}
	if t.yr, err = ensureYr(ctx, client, bin); err != nil {
		term.Info("scanner not available, scanning skipped (%s)", term.Sanitize(err.Error()))
		return t, nil
	}
	if t.rules, t.stale, err = ensureRules(ctx, client); err != nil {
		term.Info("rules not available, scanning skipped (%s)", term.Sanitize(err.Error()))
		t.yr = ""
	}
	return t, nil
}

// guestArch is the Linux architecture the sandbox runs: the host's.
func guestArch() (yr, gett string) {
	if runtime.GOARCH == "arm64" {
		return "aarch64", "arm64"
	}
	return "x86_64", "amd64"
}

// ensureGett keeps a Linux build of the running gett version for the guest,
// checked against the release's SHA256SUMS. A version with no release yet
// falls back to the latest one.
func ensureGett(ctx context.Context, client *http.Client, bin string) (string, error) {
	_, arch := guestArch()
	dst := filepath.Join(bin, "gett-"+httpx.Version+"-linux-"+arch)
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	tags := []string{"v" + httpx.Version}
	if httpx.Version == "dev" {
		tags = nil
	}
	tags = append(tags, "latest")
	var lastErr error
	for _, tag := range tags {
		base := gettBase + tag + "/"
		if tag == "latest" {
			base = "https://github.com/naviko/gett/releases/latest/download/"
		}
		sums, err := fetchBytes(ctx, client, base+"SHA256SUMS")
		if err != nil {
			lastErr = err
			continue
		}
		want := sumFor(sums, "gett-linux-"+arch)
		if want == "" {
			lastErr = errors.New("no checksum for gett-linux-" + arch)
			continue
		}
		term.Info("getting gett for the sandbox")
		if err := fetchFile(ctx, client, base+"gett-linux-"+arch, dst, want, 0o755); err != nil {
			lastErr = err
			continue
		}
		return dst, nil
	}
	return "", &errs.Sandbox{Msg: "could not get gett for the sandbox (" + term.Sanitize(lastErr.Error()) + ")"}
}

// ensureYr keeps the pinned YARA-X CLI for the guest.
func ensureYr(ctx context.Context, client *http.Client, bin string) (string, error) {
	arch, _ := guestArch()
	dst := filepath.Join(bin, "yr-"+yrVersion)
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	term.Info("getting the scanner")
	tgz := dst + ".tgz"
	if err := fetchFile(ctx, client, yrBase+arch+"-unknown-linux-gnu.tar.gz", tgz, yrSums[arch], 0o644); err != nil {
		return "", err
	}
	defer os.Remove(tgz)
	if err := untarOne(tgz, "yr", dst); err != nil {
		return "", err
	}
	return dst, nil
}

// ensureRules keeps the YARA Forge core rules, refreshed weekly, and the
// compiled form made inside a throwaway sandbox (that is where yr runs).
func ensureRules(ctx context.Context, client *http.Client) (dir string, stamp time.Time, err error) {
	dir = filepath.Join(Home(), "rules")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", stamp, &errs.Write{Err: err}
	}
	yarc := filepath.Join(dir, "core.yarc")
	if fi, err := os.Stat(yarc); err == nil {
		stamp = fi.ModTime()
		if time.Since(stamp) < rulesTTL {
			return dir, stamp, nil
		}
	}
	term.Info("getting rules")
	zipPath := filepath.Join(dir, "core.zip")
	if err := fetchFile(ctx, client, rulesURL, zipPath, "", 0o644); err != nil {
		if !stamp.IsZero() {
			term.Debug("rules refresh failed (%s), using the ones from %s", err, stamp.Format("2006-01-02"))
			return dir, stamp, nil
		}
		return "", stamp, err
	}
	defer os.Remove(zipPath)
	if err := unzipOne(zipPath, "yara-rules-core.yar", filepath.Join(dir, "core.yar")); err != nil {
		return "", stamp, err
	}
	if err := compileRules(ctx, dir); err != nil {
		if !stamp.IsZero() {
			term.Debug("rules compile failed (%s), using the ones from %s", err, stamp.Format("2006-01-02"))
			return dir, stamp, nil
		}
		return "", stamp, err
	}
	return dir, time.Now(), nil
}

// compileRules runs yr compile in a short-lived sandbox with the rules
// folder mounted read-write, producing core.yarc next to core.yar.
func compileRules(ctx context.Context, dir string) error {
	m, err := ensureMsb(ctx)
	if err != nil {
		return err
	}
	yr := filepath.Join(Home(), "bin", "yr-"+yrVersion)
	const name = "gett-rules"
	m.run(ctx, "rm", "-f", name, "-q")
	if _, err := m.run(ctx, "create", Image(), "-n", name, "--label", label, "-m", memory, "-c", cpus,
		"--mount-dir", dir+":"+guestRules, "--copy-file", yr+":/usr/local/bin/yr", "-q"); err != nil {
		return &errs.Sandbox{Msg: "could not start a sandbox to compile the rules (" + term.Sanitize(err.Error()) + ")"}
	}
	defer m.run(context.WithoutCancel(ctx), "rm", "-f", name, "-q")
	_, stderr, err := m.exec(ctx, name, "yr", "compile", guestRules+"/core.yar", "-o", guestRules+"/core.yarc.tmp")
	if err != nil {
		return &errs.Sandbox{Msg: "rules did not compile (" + term.Sanitize(lastLine(stderr)) + ")"}
	}
	if err := os.Rename(filepath.Join(dir, "core.yarc.tmp"), filepath.Join(dir, "core.yarc")); err != nil {
		return &errs.Write{Err: err}
	}
	return nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// fetchBytes downloads a small text file.
func fetchBytes(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &errs.HTTPStatus{Code: resp.StatusCode, Status: resp.Status, URL: url}
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// fetchFile downloads url to dst with a progress bar, checks the sha256 when
// given, and sets the mode. Nothing is left behind on failure.
func fetchFile(ctx context.Context, client *http.Client, url, dst, sum string, mode os.FileMode) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &errs.HTTPStatus{Code: resp.StatusCode, Status: resp.Status, URL: url}
	}
	part := dst + ".part"
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return &errs.Write{Err: err}
	}
	prog := term.NewProgress()
	bar := prog.Bar(filepath.Base(dst), resp.ContentLength, false)
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(bar.Writer(f), h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		bar.Abort()
		prog.Shutdown()
		os.Remove(part)
		return err
	}
	bar.Done()
	prog.Wait()
	if sum != "" && hex.EncodeToString(h.Sum(nil)) != sum {
		os.Remove(part)
		return fmt.Errorf("%s does not match its checksum", filepath.Base(dst))
	}
	if err := os.Rename(part, dst); err != nil {
		os.Remove(part)
		return &errs.Write{Err: err}
	}
	return nil
}

// sumFor reads "hash  name" lines, as build.sh writes them.
func sumFor(sums []byte, name string) string {
	sc := bufio.NewScanner(strings.NewReader(string(sums)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return f[0]
		}
	}
	return ""
}

// untarOne pulls a single file out of a .tar.gz.
func untarOne(tgz, name, dst string) error {
	f, err := os.Open(tgz)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("%s not in %s", name, filepath.Base(tgz))
		}
		if err != nil {
			return err
		}
		if filepath.Base(h.Name) == name && h.Typeflag == tar.TypeReg {
			return writeFrom(tr, dst, 0o755)
		}
	}
}

// unzipOne pulls a single file out of a zip.
func unzipOne(zipPath, name, dst string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if filepath.Base(f.Name) != name || f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		return writeFrom(rc, dst, 0o644)
	}
	return fmt.Errorf("%s not in %s", name, filepath.Base(zipPath))
}

func writeFrom(r io.Reader, dst string, mode os.FileMode) error {
	part := dst + ".part"
	out, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return &errs.Write{Err: err}
	}
	_, err = io.Copy(out, r)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(part)
		return &errs.Write{Err: err}
	}
	if err := os.Rename(part, dst); err != nil {
		os.Remove(part)
		return &errs.Write{Err: err}
	}
	return nil
}
