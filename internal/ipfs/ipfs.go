// Package ipfs turns an IPFS address into a URL on a public HTTP gateway, so
// the ordinary download engine can fetch it.
package ipfs

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"gett/internal/errs"
	"gett/internal/httpx"
	"gett/internal/term"
)

// Gateways is tried when IPFS_GATEWAY is not set. All are probed at once and
// the first one that answers wins. ipfs.io, dweb.link and w3s.link are not
// here: since 2026 they only serve browsers (HTTP 429 for everything else).
var Gateways = []string{
	"https://ipfs.filebase.io",
	"https://gateway.lighthouse.storage",
	"https://gateway.pinata.cloud",
	"https://ipfs.raribleuserdata.com",
	"https://storry.tv",
}

const probeTimeout = 15 * time.Second

// IsIPFS reports whether s is an IPFS or IPNS address.
func IsIPFS(s string) bool {
	l := strings.ToLower(s)
	return strings.HasPrefix(l, "ipfs://") || strings.HasPrefix(l, "ipns://") ||
		strings.HasPrefix(l, "/ipfs/") || strings.HasPrefix(l, "/ipns/")
}

type addr struct {
	kind string // "ipfs" or "ipns"
	root string // CID or IPNS name
	path string // "" or "/sub/path", kept as typed
}

func parse(s string) (addr, error) {
	l := strings.ToLower(s)
	var rest string
	a := addr{}
	switch {
	case strings.HasPrefix(l, "ipfs://"):
		a.kind, rest = "ipfs", s[len("ipfs://"):]
	case strings.HasPrefix(l, "ipns://"):
		a.kind, rest = "ipns", s[len("ipns://"):]
	case strings.HasPrefix(l, "/ipfs/"):
		a.kind, rest = "ipfs", s[len("/ipfs/"):]
	case strings.HasPrefix(l, "/ipns/"):
		a.kind, rest = "ipns", s[len("/ipns/"):]
	default:
		return a, &errs.BadURL{URL: s}
	}
	rest = strings.TrimPrefix(rest, "/")
	a.root, a.path, _ = strings.Cut(rest, "/")
	if a.root == "" || strings.ContainsAny(a.root, " ?#") {
		return a, &errs.BadURL{URL: s}
	}
	if a.path != "" {
		a.path = "/" + a.path
	}
	return a, nil
}

// gateways returns the override list from IPFS_GATEWAY or the default.
func gateways() []string {
	env := strings.TrimSpace(os.Getenv("IPFS_GATEWAY"))
	if env == "" {
		return Gateways
	}
	var out []string
	for _, g := range strings.Split(env, ",") {
		g = strings.TrimRight(strings.TrimSpace(g), "/")
		if g == "" {
			continue
		}
		if !strings.Contains(g, "://") {
			g = "https://" + g
		}
		out = append(out, g)
	}
	if len(out) == 0 {
		return Gateways
	}
	return out
}

// Resolve probes every gateway for the address and returns the URL of the
// first that serves it. A directory is requested as a tar archive. For an
// immutable file it also returns a key that names the content regardless of
// gateway, so a rerun resumes even when another gateway wins.
func Resolve(ctx context.Context, source string, client *http.Client, headers http.Header) (url, key string, err error) {
	a, err := parse(source)
	if err != nil {
		return "", "", err
	}
	gws := gateways()
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	type hit struct {
		url string
		dir bool
	}
	hits := make(chan hit, len(gws))
	fails := make(chan error, len(gws))
	for _, gw := range gws {
		u := gw + "/" + a.kind + "/" + a.root + a.path
		go func() {
			req, err := httpx.NewRequest(ctx, http.MethodGet, u, nil, headers)
			if err != nil {
				fails <- err
				return
			}
			req.Header.Set("Range", "bytes=0-0")
			resp, err := client.Do(req)
			if err != nil {
				fails <- err
				return
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
				fails <- &errs.HTTPStatus{Code: resp.StatusCode, Status: term.Sanitize(resp.Status), URL: u}
				return
			}
			dir := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") &&
				resp.Header.Get("X-Ipfs-Roots") != "" && path.Ext(a.path) == ""
			// The gateway URL as asked, not where it redirected: the download
			// follows the redirect itself and so carries the right Referer.
			hits <- hit{u, dir}
		}()
	}
	var last error
	for range gws {
		select {
		case h := <-hits:
			term.Debug("ipfs: %s", term.Sanitize(h.url))
			if h.dir {
				term.Debug("ipfs: %s is a directory, fetching as tar", a.root)
				return h.url + "?format=tar", "", nil
			}
			if a.kind == "ipfs" { // content-addressed, so the same bytes everywhere
				key = "ipfs://" + a.root + a.path
			}
			return h.url, key, nil
		case err := <-fails:
			term.Debug("ipfs: %s", term.Sanitize(errs.Message(err)))
			last = err
		case <-ctx.Done():
			return "", "", &errs.Gateway{Msg: fmt.Sprintf("no gateway answered for %s within %s", a.root, probeTimeout)}
		}
	}
	return "", "", &errs.Gateway{Msg: fmt.Sprintf("no gateway could serve %s (%s)", a.root, errs.Message(last))}
}
