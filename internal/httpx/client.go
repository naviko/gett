// Package httpx builds the HTTP client shared by download and send.
package httpx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gett/internal/errs"
	"gett/internal/term"

	"github.com/hashicorp/go-retryablehttp"
	"github.com/mengzhuo/cookiestxt"
	"golang.org/x/net/html"
	"golang.org/x/net/publicsuffix"
)

var Version = "dev"

const maxRedirects = 100

type Config struct {
	CookieFile string
	Retries    bool // retry transient failures (safe for GET)
}

// Transport returns the base transport: HTTP/2, proxy from environment,
// 30s connect timeout.
func Transport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   256,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// NewClient builds the client. Redirects are followed, cookies kept.
func NewClient(cfg Config) (*http.Client, error) {
	var rt http.RoundTripper = &verboseTransport{Transport()}
	if cfg.Retries {
		rc := retryablehttp.NewClient()
		rc.RetryMax = 3
		rc.Logger = nil
		rc.ErrorHandler = retryablehttp.PassthroughErrorHandler
		// The inner client must not follow redirects: the outer one does,
		// with the cookie jar, the hop limit and the debug log.
		rc.HTTPClient = &http.Client{Transport: rt, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		rt = &retryablehttp.RoundTripper{Client: rc}
	}
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return nil, err
	}
	if cfg.CookieFile != "" {
		if err := loadCookies(jar, cfg.CookieFile); err != nil {
			return nil, fmt.Errorf("cookies: %w", err)
		}
	}
	return &http.Client{
		Transport: rt,
		Jar:       jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return &errs.Redirect{Msg: fmt.Sprintf("more than %d redirects", maxRedirects)}
			}
			term.Debug("-> redirect to %s", term.Sanitize(req.URL.Redacted()))
			return nil
		},
	}, nil
}

func loadCookies(jar http.CookieJar, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	cookies, err := cookiestxt.Parse(f)
	if err != nil {
		return err
	}
	byHost := map[string][]*http.Cookie{}
	for _, c := range cookies {
		host := strings.TrimPrefix(c.Domain, ".")
		scheme := "http"
		if c.Secure {
			scheme = "https"
		}
		byHost[scheme+"://"+host] = append(byHost[scheme+"://"+host], c)
	}
	for base, cs := range byHost {
		u, err := url.Parse(base + "/")
		if err != nil {
			continue
		}
		jar.SetCookies(u, cs)
	}
	return nil
}

// NewRequest builds a request with the default User-Agent and user headers.
// Basic auth in the URL is applied by net/http itself.
func NewRequest(ctx context.Context, method, rawURL string, body io.Reader, headers http.Header) (*http.Request, error) {
	u, err := ParseURL(rawURL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, &errs.BadURL{URL: rawURL}
	}
	req.Header.Set("User-Agent", "gett/"+Version)
	for k, vs := range headers {
		req.Header.Del(k)
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return req, nil
}

// ParseURL accepts URLs without a scheme and defaults them to https.
func ParseURL(raw string) (*url.URL, error) {
	s := raw
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, &errs.BadURL{URL: raw}
	}
	return u, nil
}

var metaRefresh = regexp.MustCompile(`(?i)^\s*\d+\s*[;,]\s*url\s*=\s*['"]?([^'"]+)`)

// Forwarder returns the URL an HTML page or Refresh header forwards to,
// or "" if there is none. head is the start of the body.
func Forwarder(resp *http.Response, head []byte) string {
	target := ""
	if r := resp.Header.Get("Refresh"); r != "" {
		if m := metaRefresh.FindStringSubmatch(r); m != nil {
			target = m[1]
		}
	}
	if target == "" && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		z := html.NewTokenizer(bytes.NewReader(head))
		for target == "" {
			tt := z.Next()
			if tt == html.ErrorToken {
				break
			}
			if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
				continue
			}
			name, hasAttr := z.TagName()
			if string(name) != "meta" || !hasAttr {
				continue
			}
			var equiv, content string
			for {
				k, v, more := z.TagAttr()
				switch strings.ToLower(string(k)) {
				case "http-equiv":
					equiv = strings.ToLower(string(v))
				case "content":
					content = string(v)
				}
				if !more {
					break
				}
			}
			if equiv == "refresh" {
				if m := metaRefresh.FindStringSubmatch(content); m != nil {
					target = m[1]
				}
			}
		}
	}
	if target == "" {
		return ""
	}
	ref, err := url.Parse(strings.TrimSpace(target))
	if err != nil {
		return ""
	}
	abs := resp.Request.URL.ResolveReference(ref)
	if abs.Scheme != "http" && abs.Scheme != "https" {
		return ""
	}
	return abs.String()
}

// verboseTransport logs each exchange in verbose mode, masking secrets.
type verboseTransport struct{ rt http.RoundTripper }

var secretHeaders = map[string]bool{"Authorization": true, "Proxy-Authorization": true, "Cookie": true, "Set-Cookie": true}

func (v *verboseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if term.Verbose {
		term.Debug("> %s %s", req.Method, term.Sanitize(req.URL.Redacted()))
		logHeaders(">", req.Header)
	}
	start := time.Now()
	resp, err := v.rt.RoundTrip(req)
	if term.Verbose {
		if err != nil {
			term.Debug("< error after %s: %s", time.Since(start).Round(time.Millisecond), term.Sanitize(err.Error()))
		} else {
			term.Debug("< %s %s (%s)", resp.Proto, term.Sanitize(resp.Status), time.Since(start).Round(time.Millisecond))
			logHeaders("<", resp.Header)
		}
	}
	return resp, err
}

func logHeaders(prefix string, h http.Header) {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, val := range h[k] {
			if secretHeaders[k] {
				val = mask(val)
			}
			term.Debug("%s %s: %s", prefix, k, term.Sanitize(val))
		}
	}
}

func mask(v string) string {
	if i := strings.Index(v, " "); i > 0 && i < 10 {
		return v[:i] + " ****"
	}
	if i := strings.Index(v, "="); i > 0 {
		return v[:i] + "=****"
	}
	return "****"
}

// ContentRangeTotal parses "bytes 0-99/1234" and returns 1234.
func ContentRangeTotal(h string) int64 {
	_, total, ok := strings.Cut(h, "/")
	if !ok {
		return -1
	}
	n, err := strconv.ParseInt(strings.TrimSpace(total), 10, 64)
	if err != nil {
		return -1
	}
	return n
}
