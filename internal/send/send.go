// Package send makes one HTTP request and shows the response.
package send

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"gett/internal/args"
	"gett/internal/errs"
	"gett/internal/httpx"
	"gett/internal/term"

	"github.com/gabriel-vasile/mimetype"
)

type Request struct {
	Method   string
	URL      string
	Headers  http.Header
	Query    map[string]string
	JSON     string
	Form     []args.KV
	Files    []args.KV
	BodyFile string
	Body     []byte
	User     string
	Pass     string
}

// Do sends the request and renders the response.
func Do(ctx context.Context, client *http.Client, r Request) error {
	body, ctype, length, err := buildBody(r)
	if err != nil {
		return err
	}
	method := r.Method
	if method == "" {
		method = http.MethodGet
		if body != nil {
			method = http.MethodPost
		}
	}
	u, err := httpx.ParseURL(r.URL)
	if err != nil {
		return err
	}
	if len(r.Query) > 0 {
		q := u.Query()
		for k, v := range r.Query {
			q.Set(k, v)
		}
		u.RawQuery = q.Encode()
	}
	req, err := httpx.NewRequest(ctx, method, u.String(), body, r.Headers)
	if err != nil {
		return err
	}
	if ctype != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", ctype)
	}
	if body != nil {
		req.ContentLength = length
	}
	if r.User != "" {
		req.SetBasicAuth(r.User, r.Pass)
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return render(resp, time.Since(start))
}

// buildBody returns the body reader, its content type and its length, or
// nil for none. A request carries one body: form fields (with or without
// files), JSON, a file, or raw bytes. The length is always known, so no
// request goes out chunked; some servers refuse those.
func buildBody(r Request) (io.Reader, string, int64, error) {
	kinds := 0
	for _, has := range []bool{len(r.Form) > 0 || len(r.Files) > 0, r.JSON != "", r.BodyFile != "", len(r.Body) > 0} {
		if has {
			kinds++
		}
	}
	if kinds > 1 {
		return nil, "", 0, &errs.Usage{Msg: "one body only: JSON, form fields or a file"}
	}
	switch {
	case len(r.Files) > 0:
		return multipartBody(r)
	case r.JSON != "":
		if !json.Valid([]byte(r.JSON)) {
			return nil, "", 0, &errs.Usage{Msg: "body is not valid JSON"}
		}
		return strings.NewReader(r.JSON), "application/json", int64(len(r.JSON)), nil
	case len(r.Form) > 0:
		// Fields in the order given; url.Values would sort them.
		var b strings.Builder
		for i, kv := range r.Form {
			if i > 0 {
				b.WriteByte('&')
			}
			b.WriteString(url.QueryEscape(kv.Key) + "=" + url.QueryEscape(kv.Value))
		}
		return strings.NewReader(b.String()), "application/x-www-form-urlencoded", int64(b.Len()), nil
	case r.BodyFile != "":
		f, err := os.Open(r.BodyFile)
		if err != nil {
			return nil, "", 0, err
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, "", 0, err
		}
		return f, fileType(r.BodyFile), fi.Size(), nil
	case len(r.Body) > 0:
		return bytes.NewReader(r.Body), mimetype.Detect(r.Body).String(), int64(len(r.Body)), nil
	}
	return nil, "", 0, nil
}

func fileType(path string) string {
	if m, err := mimetype.DetectFile(path); err == nil {
		return m.String()
	}
	return "application/octet-stream"
}

type filePart struct {
	key, path, ctype string
	size             int64
}

// multipartBody streams fields and files without buffering them in memory.
// The length is computed first by writing the same framing to a counter,
// with the file contents left out and their sizes added.
func multipartBody(r Request) (io.Reader, string, int64, error) {
	var files []filePart
	for _, kv := range r.Files {
		fi, err := os.Stat(kv.Value)
		if err != nil {
			return nil, "", 0, err
		}
		files = append(files, filePart{kv.Key, kv.Value, fileType(kv.Value), fi.Size()})
	}
	boundary := multipart.NewWriter(io.Discard).Boundary()
	write := func(w io.Writer, content bool) (err error) {
		mw := multipart.NewWriter(w)
		if err := mw.SetBoundary(boundary); err != nil {
			return err
		}
		for _, kv := range r.Form {
			if err := mw.WriteField(kv.Key, kv.Value); err != nil {
				return err
			}
		}
		for _, fp := range files {
			h := textproto.MIMEHeader{}
			h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`,
				strings.ReplaceAll(fp.key, `"`, `\"`), strings.ReplaceAll(filepath.Base(fp.path), `"`, `\"`)))
			h.Set("Content-Type", fp.ctype)
			part, err := mw.CreatePart(h)
			if err != nil {
				return err
			}
			if !content {
				continue
			}
			f, err := os.Open(fp.path)
			if err != nil {
				return err
			}
			_, err = io.Copy(part, f)
			f.Close()
			if err != nil {
				return err
			}
		}
		return mw.Close()
	}
	var n int64
	if err := write(counter{&n}, false); err != nil {
		return nil, "", 0, err
	}
	for _, fp := range files {
		n += fp.size
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(write(pw, true)) }()
	return pr, "multipart/form-data; boundary=" + boundary, n, nil
}

type counter struct{ n *int64 }

func (c counter) Write(p []byte) (int, error) { *c.n += int64(len(p)); return len(p), nil }

// render prints the response: readable on a terminal, raw body when piped.
func render(resp *http.Response, elapsed time.Duration) error {
	if !term.StdoutTTY() {
		_, err := io.Copy(os.Stdout, resp.Body)
		return err
	}
	fmt.Printf("%s %s  %s\n", resp.Proto, term.Sanitize(resp.Status), elapsed.Round(time.Millisecond))

	keys := make([]string, 0, len(resp.Header))
	for k := range resp.Header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, k := range keys {
		for _, v := range resp.Header[k] {
			fmt.Fprintf(tw, "%s\t%s\n", k+":", term.Sanitize(v))
		}
	}
	tw.Flush()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	fmt.Println()
	ctype, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if isJSON(ctype, body) {
		if err := printJSON(body); err == nil {
			return nil
		}
	}
	if isText(ctype, body) {
		s := term.SanitizeText(string(body))
		fmt.Print(s)
		if !strings.HasSuffix(s, "\n") {
			fmt.Println()
		}
		return nil
	}
	if ctype == "" {
		ctype = mimetype.Detect(body).String()
	}
	fmt.Printf("[%s, %d bytes]\n", ctype, len(body))
	return nil
}

func isJSON(ctype string, body []byte) bool {
	if strings.HasSuffix(ctype, "json") || strings.HasSuffix(ctype, "+json") {
		return true
	}
	t := bytes.TrimSpace(body)
	return len(t) > 0 && (t[0] == '{' || t[0] == '[') && json.Valid(t)
}

func printJSON(body []byte) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func isText(ctype string, body []byte) bool {
	if strings.HasPrefix(ctype, "text/") || strings.Contains(ctype, "xml") || strings.Contains(ctype, "javascript") ||
		strings.Contains(ctype, "x-www-form-urlencoded") || strings.Contains(ctype, "yaml") {
		return true
	}
	if ctype != "" && !strings.HasSuffix(ctype, "/octet-stream") {
		return false
	}
	return utf8.Valid(body) && !bytes.ContainsRune(body, 0)
}
