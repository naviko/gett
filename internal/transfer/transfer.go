// Package transfer shares folders with other computers on the local network
// and pulls what they share. A sharer walks its folders once, announces
// itself with a multicast beacon and serves files over HTTP. A receiver lists
// sharers and their files by number and pulls items: big files over several
// connections at once, small files as one tar stream, with resume.
package transfer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gett/internal/download"
)

// api is the protocol version. Two gett builds with the same api transfer
// freely, whatever their release numbers.
const api = 1

// Home is where gett keeps the last listing and remembered passwords:
// ~/.gett/transfer. Tests move it.
var Home = func() string {
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".gett", "transfer")
}

// Tunables; tests shrink them.
var (
	smallMax     int64 = 16 << 20        // files up to this size travel in the tar stream
	stallTimeout       = 2 * time.Second // no data on a connection for this long: reissue the unit
	wrongDelay         = time.Second     // pause before answering a request with a wrong password
	discoverWait       = time.Second     // how long a listing collects beacon answers
)

var buffers = sync.Pool{New: func() any { b := make([]byte, 1<<20); return &b }}

// hostName is how this computer appears to others: the hostname without
// its domain, lowercased.
func hostName() string {
	h, _ := os.Hostname()
	h, _, _ = strings.Cut(h, ".")
	h = strings.ToLower(strings.TrimSpace(h))
	if h == "" {
		return "gett"
	}
	h = download.Clean(h)
	if len(h) > 32 {
		h = h[:32]
	}
	return h
}

// numbered is the next name to try when one is taken: "name (n).ext" for a
// file, "name (n)" for a folder.
func numbered(name string, n int, dir bool) string {
	if dir {
		return fmt.Sprintf("%s (%d)", name, n)
	}
	return download.Numbered(name, n)
}
