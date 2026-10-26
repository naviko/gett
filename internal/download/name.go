package download

import (
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"gett/internal/term"

	"github.com/gabriel-vasile/mimetype"
)

// deriveName picks a file name from Content-Disposition, then the URL path,
// then the host, and gives it the right extension.
func deriveName(resp *http.Response, head []byte) string {
	name := ""
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil {
			name = params["filename"]
		}
	}
	if name == "" {
		p, _ := url.PathUnescape(resp.Request.URL.Path)
		name = path.Base(p)
		if name == "/" || name == "." || name == "" {
			name = resp.Request.URL.Hostname()
		}
	}
	return fixExt(clean(name), resp.Header.Get("Content-Type"), head)
}

// clean sandboxes a name: no directories, no control characters, never
// empty, never "." or "..".
func clean(s string) string {
	s = term.Sanitize(s)
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		s = s[i+1:]
	}
	s = strings.ReplaceAll(s, ":", "_")
	s = strings.Trim(s, " .")
	if s == "" || s == "." || s == ".." {
		s = "download"
	}
	if len(s) > 200 {
		ext := filepath.Ext(s)
		s = s[:200-len(ext)] + ext
	}
	return s
}

// fixExt adds an extension only when the name has none. The file's own
// bytes decide first; the server's Content-Type is a fallback, and a text
// type is not trusted for bytes that are not text.
func fixExt(name, ct string, head []byte) string {
	if filepath.Ext(name) != "" {
		return name
	}
	want := ""
	binary := false
	if len(head) > 0 {
		m := mimetype.Detect(head)
		if m.Is("application/octet-stream") {
			binary = true
		} else {
			want = m.Extension()
		}
	}
	if want == "" && ct != "" {
		ct, _, _ = mime.ParseMediaType(ct)
		generic := strings.HasSuffix(ct, "/octet-stream") || ct == "application/x-download" || ct == "application/force-download"
		if !generic && !(binary && strings.HasPrefix(ct, "text/")) {
			if m := mimetype.Lookup(ct); m != nil {
				want = m.Extension()
			}
			if want == "" {
				if exts, _ := mime.ExtensionsByType(ct); len(exts) > 0 {
					want = exts[0]
				}
			}
		}
	}
	if want == "" {
		return name
	}
	return name + want
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// numbered returns "stem (n).ext".
func numbered(name string, n int) string {
	ext := filepath.Ext(name)
	return fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(name, ext), n, ext)
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// Clean, Numbered, Exists and Human are shared with the torrent downloader so
// both kinds of download name and place files the same way.
func Clean(s string) string              { return clean(s) }
func Numbered(name string, n int) string { return numbered(name, n) }
func Exists(p string) bool               { return exists(p) }
func Human(n int64) string               { return human(n) }

// Target splits a user-typed destination into folder and file name. A trailing
// slash or an existing directory means "into this folder, automatic name".
// The folder is created. An empty name means "decide from the download".
func Target(dest string) (dir, name string, err error) {
	if dest == "" {
		return "", "", nil
	}
	if strings.HasSuffix(dest, "/") || strings.HasSuffix(dest, string(os.PathSeparator)) || isDir(dest) {
		dir = dest
	} else {
		dir, name = filepath.Dir(dest), clean(filepath.Base(dest))
	}
	if dir == "." {
		dir = ""
	}
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", "", err
		}
	}
	return dir, name, nil
}
