package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gett/internal/errs"
	"gett/internal/term"
)

const (
	scanMaxFile = 200 << 20 // bytes; bigger files are listed as not scanned
	scanTimeout = "300"     // seconds, inside yr
)

// finding is one rule matching one file.
type finding struct {
	path, rule, desc, author string
	score                    int
}

// report is what the user sees: findings and what could not be checked.
type report struct {
	files    int
	bytes    int64
	findings []finding
	skipped  []string // "path (reason)"
	stale    string   // rules date when older than the refresh window
	off      string   // why there was no scan at all
}

// scan runs YARA-X over /work inside the sandbox and turns the machine
// output into a report. yr prints one JSON line per file with matches and
// nothing for clean files; errors go to stderr with exit code 0, and files
// over the size cap are skipped silently, so the host-side walk of the same
// folder supplies the counts and the skipped list.
var scan = func(ctx context.Context, m msb, name, work string, t tools) (*report, error) {
	rep := &report{}
	err := filepath.WalkDir(work, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(work, p)
		rel = filepath.ToSlash(rel)
		if fi.Size() > scanMaxFile {
			rep.skipped = append(rep.skipped, fmt.Sprintf("%s (%s, over %s)", rel, human(fi.Size()), human(scanMaxFile)))
			return nil
		}
		rep.files++
		rep.bytes += fi.Size()
		return nil
	})
	if err != nil {
		return nil, &errs.Write{Err: err}
	}
	if t.yr == "" || t.rules == "" {
		rep.off = "scanner not available"
		return rep, nil
	}
	if !t.stale.IsZero() && time.Since(t.stale) > rulesTTL {
		rep.stale = t.stale.Format("2006-01-02")
	}

	stdout, stderr, err := m.exec(ctx, name, "yr", "scan", "--recursive=32", "--output-format", "ndjson", "-m", "-e",
		"--timeout", scanTimeout, "--skip-larger", strconv.Itoa(scanMaxFile), "-C", guestRules+"/core.yarc", guestWork)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil && strings.TrimSpace(stdout) == "" {
		return nil, &errs.Sandbox{Msg: "the scan did not run (" + term.Sanitize(lastLine(stderr)) + ")"}
	}
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row struct {
			Path  string `json:"path"`
			Rules []struct {
				Identifier string     `json:"identifier"`
				Meta       [][]string `json:"meta"`
			} `json:"rules"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			term.Debug("scan line not understood: %s", term.Sanitize(line))
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(row.Path, guestWork), "/")
		for _, r := range row.Rules {
			f := finding{path: rel, rule: r.Identifier}
			for _, kv := range r.Meta {
				if len(kv) != 2 {
					continue
				}
				switch kv[0] {
				case "description":
					f.desc = kv[1]
				case "author":
					f.author = kv[1]
				case "score":
					f.score, _ = strconv.Atoi(kv[1])
				}
			}
			rep.findings = append(rep.findings, f)
		}
	}
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "warning") {
			continue
		}
		term.Debug("yr: %s", term.Sanitize(line))
		rep.skipped = append(rep.skipped, scanError(line))
	}
	sort.SliceStable(rep.findings, func(i, j int) bool {
		if rep.findings[i].path != rep.findings[j].path {
			return rep.findings[i].path < rep.findings[j].path
		}
		return rep.findings[i].score > rep.findings[j].score
	})
	return rep, nil
}

// scanError turns "error: can't open `/work/x`: No such file ..." into
// "x (can't open: No such file ...)".
func scanError(line string) string {
	line = strings.TrimPrefix(line, "error: ")
	path := ""
	if i := strings.Index(line, "`"); i >= 0 {
		if j := strings.Index(line[i+1:], "`"); j >= 0 {
			path = line[i+1 : i+1+j]
			before := strings.TrimSpace(line[:i])
			after := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line[i+1+j+1:]), ":"))
			line = before + ": " + after
		}
	}
	rel := strings.TrimPrefix(strings.TrimPrefix(path, guestWork), "/")
	if rel == "" {
		return term.Sanitize(line)
	}
	return fmt.Sprintf("%s (%s)", rel, term.Sanitize(strings.TrimSpace(line)))
}

// lines renders the report: one line per finding and one line for what
// could not be scanned. Nothing at all when the scan found nothing.
func (r *report) lines() []string {
	var out []string
	if r.off != "" {
		return []string{"not scanned: " + r.off}
	}
	w := 0
	for _, f := range r.findings {
		if len(f.path) > w {
			w = len(f.path)
		}
	}
	if w > 40 {
		w = 40
	}
	for _, f := range r.findings {
		desc := f.desc
		if f.author != "" {
			desc = strings.TrimSpace(desc + " (" + f.author + ")")
		}
		out = append(out, strings.TrimRight(fmt.Sprintf("%-*s  %s  %s", w, term.Sanitize(f.path), term.Sanitize(f.rule), term.Sanitize(oneLine(desc))), " "))
	}
	if len(r.skipped) > 0 {
		out = append(out, "not scanned: "+strings.Join(r.skipped, ", "))
	}
	if r.stale != "" {
		term.Debug("rules from %s", r.stale)
	}
	return out
}

func (r *report) print() {
	for _, l := range r.lines() {
		term.Info("%s", l)
	}
}

// text is the report as the sandbox's login banner.
func (r *report) text(name string) string {
	s := "gett sandbox " + name + "\n"
	if l := r.lines(); len(l) > 0 {
		s += strings.Join(l, "\n") + "\n"
	}
	return s
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 100 {
		s = s[:97] + "..."
	}
	return s
}
