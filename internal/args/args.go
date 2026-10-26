// Package args recognizes free-form arguments by their shape, so gett needs
// no flags.
package args

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/mengzhuo/cookiestxt"
)

type Mode int

const (
	Download Mode = iota
	Send
	FTP
	Transfer
)

type KV struct{ Key, Value string }

type Opts struct {
	Headers    http.Header
	CookieFile string
	Method     string // send
	JSON       string // send
	Form       []KV   // send
	Files      []KV   // send, name=@path
	BodyFile   string // send, @path
	Timeout    time.Duration
	Verbose    bool
	Silent     bool
	Name       string   // download: output file name
	Extract    bool     // download: unpack the saved file
	ExtractTo  string   // download: where to unpack, "" = beside the file
	Sandbox    bool     // download: unpack and scan in a microVM, keep it
	Scan       bool     // download: unpack and scan in a microVM, remove it
	Paths      []string // transfer: what to share, or where to pull into
	Items      []string // transfer: numbers like "1" and "2.3"
	Password   string   // transfer: the word after what is shared, or the one to unlock with
	Ls         bool     // transfer: list inside an item
}

var (
	headerRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]*:(\s.*|[^/\s].*)?$`)
	methodRe = regexp.MustCompile(`^[A-Z][A-Z-]{2,}$`)
	itemRe   = regexp.MustCompile(`^\d+(\.\d+)*$`)
)

// Parse classifies every argument. Order does not matter, with one
// exception: the word right after "extract" is where to unpack. In Transfer
// mode a path is something to share or the destination, digits with dots
// are an item number, and any other bare word is a password.
func Parse(mode Mode, rest []string) (*Opts, error) {
	o := &Opts{Headers: http.Header{}}
	afterExtract := false
	for _, a := range rest {
		dest := afterExtract
		afterExtract = false
		switch strings.ToLower(a) {
		case "extract":
			if mode != Download && mode != Transfer {
				return nil, fmt.Errorf("don't know what %q means", a)
			}
			o.Extract = true
			afterExtract = true
			continue
		case "sandbox", "scan":
			if mode != Download && mode != Transfer {
				return nil, fmt.Errorf("don't know what %q means", a)
			}
			if strings.ToLower(a) == "sandbox" {
				o.Sandbox = true
			} else {
				o.Scan = true
			}
			continue
		case "verbose":
			o.Verbose = true
			continue
		case "silent", "quiet":
			o.Silent = true
			continue
		}
		if d, ok := duration(a); ok {
			o.Timeout = d
			continue
		}
		if mode == Transfer {
			// Cookies and headers mean nothing here, and "a:b" is a password.
			switch {
			case dest:
				o.ExtractTo = a
			case strings.ToLower(a) == "ls":
				o.Ls = true
			case itemRe.MatchString(a):
				o.Items = append(o.Items, a)
			case strings.HasSuffix(a, "/") || strings.HasSuffix(a, string(os.PathSeparator)) || exists(a):
				o.Paths = append(o.Paths, a)
			case o.Password != "":
				return nil, fmt.Errorf("two passwords given: %q and %q", o.Password, a)
			default:
				o.Password = a
			}
			continue
		}
		if IsCookieFile(a) {
			o.CookieFile = a
			continue
		}
		if headerRe.MatchString(a) && !strings.Contains(a, "://") {
			k, v, _ := strings.Cut(a, ":")
			o.Headers.Add(strings.TrimSpace(k), strings.TrimSpace(v))
			continue
		}
		if mode == Send {
			switch {
			case methodRe.MatchString(a):
				o.Method = a
			case strings.HasPrefix(a, "{") || strings.HasPrefix(a, "["):
				o.JSON = a
			case strings.HasPrefix(a, "@"):
				o.BodyFile = a[1:]
			case strings.Contains(a, "="):
				k, v, _ := strings.Cut(a, "=")
				if strings.HasPrefix(v, "@") {
					o.Files = append(o.Files, KV{k, v[1:]})
				} else {
					o.Form = append(o.Form, KV{k, v})
				}
			default:
				return nil, fmt.Errorf("don't know what %q means", a)
			}
			continue
		}
		if mode == Download {
			if dest {
				o.ExtractTo = a
				continue
			}
			if o.Name != "" {
				return nil, fmt.Errorf("two file names given: %q and %q", o.Name, a)
			}
			o.Name = a
			continue
		}
		return nil, fmt.Errorf("don't know what %q means", a)
	}
	return o, nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// duration accepts "30s", "5m", "1h30m". A bare number is not a timeout.
func duration(s string) (time.Duration, bool) {
	if s == "" || !strings.ContainsAny(s, "smh") {
		return 0, false
	}
	if c := s[0]; c < '0' || c > '9' {
		return 0, false
	}
	d, err := time.ParseDuration(s)
	return d, err == nil && d > 0
}

// IsCookieFile reports whether path is an existing Netscape cookies.txt.
func IsCookieFile(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > 16<<20 {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			if strings.Contains(line, "Netscape") || strings.Contains(line, "HTTP Cookie File") {
				return true
			}
			continue
		}
		if strings.HasPrefix(line, "{") || strings.HasPrefix(line, "[") {
			return false
		}
		_, err := cookiestxt.ParseLine(sc.Text())
		return err == nil
	}
	return false
}
