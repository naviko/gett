package transfer

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"gett/internal/download"
	"gett/internal/term"
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func strconvItoa(n int64) string { return strconv.FormatInt(n, 10) }

// Share serves paths to the local network until ctx ends. With a password
// the shares are seen only by receivers that give it.
func Share(ctx context.Context, paths []string, password string) error {
	idx, err := build(ctx, paths, password)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return errors.New("could not listen: " + err.Error())
	}
	port := ln.Addr().(*net.TCPAddr).Port
	name := hostName()
	id := randomID()
	srv := &http.Server{
		Handler:     newHandler(idx, printEvent),
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	go srv.Serve(ln)
	go announce(ctx, name, port, id)
	for _, sh := range idx.shares {
		line := sh.Name
		if sh.Tree.Dir {
			line += "/"
		}
		if sh.Password != "" {
			line += "   password:" + sh.Password
		}
		term.Info("%s", line)
	}
	term.Debug("name %s, port %d", name, port)
	<-ctx.Done()
	srv.Close()
	return nil
}

func randomID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// event is a receiver starting or finishing an item. Share prints it;
// tests record it.
type event struct {
	Receiver, Addr string
	Path           string
	Dir            bool
	Size           int64
	Done, OK       bool
	Took           time.Duration
}

// printEvent is one line per start and one per end, nothing in between.
func printEvent(ev event) {
	who := ev.Receiver
	if term.Verbose {
		who += " (" + ev.Addr + ")"
	}
	what := ev.Path
	if ev.Dir {
		what += "/"
	}
	switch {
	case !ev.Done:
		term.Info("%s   %s   %s", who, what, download.Human(ev.Size))
	case ev.OK:
		term.Info("%s   %s   done %s", who, what, ev.Took.Round(time.Second))
	default:
		term.Info("%s   %s   stopped %s", who, what, ev.Took.Round(time.Second))
	}
}

// handler serves the /v1/ API of a sharer.
type handler struct {
	idx    *index
	notify func(event)
	mux    *http.ServeMux
	wrong  sync.Mutex // one wrong-password answer at a time, each after a pause
	mu     sync.Mutex
	active map[string]time.Time // receiver and path → when the pull started
}

func newHandler(idx *index, notify func(event)) *handler {
	h := &handler{idx: idx, notify: notify, mux: http.NewServeMux(), active: map[string]time.Time{}}
	h.mux.HandleFunc("GET /v1/ls", h.ls)
	h.mux.HandleFunc("GET /v1/files", h.files)
	h.mux.HandleFunc("GET /v1/file", h.file)
	h.mux.HandleFunc("POST /v1/tar", h.tar)
	h.mux.HandleFunc("POST /v1/start", h.start)
	h.mux.HandleFunc("POST /v1/done", h.done)
	return h
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Gett-Api") != strconv.Itoa(api) {
		http.Error(w, "api "+strconv.Itoa(api), http.StatusUpgradeRequired)
		return
	}
	h.penalize(r)
	h.mux.ServeHTTP(w, r)
}

// penalize makes a request that carries a password no share has wait its
// turn and a second: one guess per second, whatever the request.
func (h *handler) penalize(r *http.Request) {
	for _, pw := range r.Header.Values("Gett-Password") {
		if h.unlocks(pw) {
			continue
		}
		h.wrong.Lock()
		select {
		case <-time.After(wrongDelay):
		case <-r.Context().Done():
		}
		h.wrong.Unlock()
		return
	}
}

func (h *handler) unlocks(pw string) bool {
	for _, sh := range h.idx.shares {
		if sh.Password != "" && subtle.ConstantTimeCompare([]byte(sh.Password), []byte(pw)) == 1 {
			return true
		}
	}
	return false
}

// allowed reports whether the request may see a share: public, or the
// right password among the ones it carries.
func allowed(sh *share, r *http.Request) bool {
	if sh.Password == "" {
		return true
	}
	for _, pw := range r.Header.Values("Gett-Password") {
		if subtle.ConstantTimeCompare([]byte(sh.Password), []byte(pw)) == 1 {
			return true
		}
	}
	return false
}

// find resolves the path query to a node the request may see. A share the
// request may not see does not exist as far as it is concerned.
func (h *handler) find(w http.ResponseWriter, r *http.Request) (*share, *node, bool) {
	sh, n := h.idx.lookup(r.URL.Query().Get("path"))
	if sh == nil || !allowed(sh, r) || n == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return nil, nil, false
	}
	return sh, n, true
}

// item is one line of a listing on the wire.
type item struct {
	Name     string    `json:"name"`
	Dir      bool      `json:"dir,omitempty"`
	Size     int64     `json:"size"`
	MTime    time.Time `json:"mtime"`
	Files    int       `json:"files,omitempty"`
	Password string    `json:"password,omitempty"`
}

func (h *handler) ls(w http.ResponseWriter, r *http.Request) {
	var items []item
	if r.URL.Query().Get("path") == "" {
		for _, sh := range h.idx.shares {
			if !allowed(sh, r) {
				continue
			}
			n := sh.Tree
			items = append(items, item{Name: sh.Name, Dir: n.Dir, Size: n.Size, MTime: n.MTime, Files: n.Files, Password: sh.Password})
		}
		writeJSON(w, map[string]any{"items": items})
		return
	}
	sh, n, ok := h.find(w, r)
	if !ok {
		return
	}
	if !n.Dir {
		http.Error(w, "not a folder", http.StatusBadRequest)
		return
	}
	for _, k := range n.Kids {
		it := item{Name: k.Name, Dir: k.Dir, Size: k.Size, MTime: k.MTime, Files: k.Files}
		if !k.Dir {
			// A file may have changed since the walk; the receiver decides on
			// what is there now.
			if fi, err := sh.Root.Stat(k.Rel); err == nil {
				it.Size, it.MTime = fi.Size(), fi.ModTime()
			}
		}
		items = append(items, it)
	}
	writeJSON(w, map[string]any{"items": items})
}

type fileEntry struct {
	Path  string    `json:"path"`
	Size  int64     `json:"size"`
	MTime time.Time `json:"mtime"`
}

// files lists everything below a folder flat, so a receiver can plan a
// whole folder with one request.
func (h *handler) files(w http.ResponseWriter, r *http.Request) {
	_, n, ok := h.find(w, r)
	if !ok {
		return
	}
	if !n.Dir {
		http.Error(w, "not a folder", http.StatusBadRequest)
		return
	}
	files, dirs := []fileEntry{}, []string{}
	n.each(func(k *node) {
		if k.Dir {
			dirs = append(dirs, n.relTo(k))
		} else {
			files = append(files, fileEntry{Path: n.relTo(k), Size: k.Size, MTime: k.MTime})
		}
	})
	writeJSON(w, map[string]any{"files": files, "dirs": dirs})
}

// file serves one file or one byte range of it, with a CRC-32C of the
// bytes sent in a trailer so the receiver can check every unit.
func (h *handler) file(w http.ResponseWriter, r *http.Request) {
	sh, n, ok := h.find(w, r)
	if !ok {
		return
	}
	if n.Dir {
		http.Error(w, "a folder", http.StatusBadRequest)
		return
	}
	fi, err := sh.Root.Stat(n.Rel)
	if err != nil {
		fileError(w, err)
		return
	}
	if s := r.Header.Get("Gett-Stamp"); s != "" && s != stamp(fi) {
		http.Error(w, "changed", http.StatusPreconditionFailed)
		return
	}
	size := fi.Size()
	start, end := int64(0), size-1
	status := http.StatusOK
	if rng := r.Header.Get("Range"); rng != "" {
		a, b, ok := parseRange(rng, size)
		if !ok {
			w.Header().Set("Content-Range", "bytes */"+strconvItoa(size))
			http.Error(w, "range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		start, end, status = a, b, http.StatusPartialContent
	}
	f, err := sh.Root.Open(n.Rel)
	if err != nil {
		fileError(w, err)
		return
	}
	defer f.Close()
	length := end - start + 1
	if size == 0 {
		length = 0
	}
	hd := w.Header()
	hd.Set("Content-Type", "application/octet-stream")
	hd.Set("Accept-Ranges", "bytes")
	hd.Set("Trailer", "Gett-Crc32")
	if status == http.StatusPartialContent {
		hd.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	}
	w.WriteHeader(status)
	crc := crc32.New(castagnoli)
	buf := buffers.Get().(*[]byte)
	defer buffers.Put(buf)
	_, err = io.CopyBuffer(io.MultiWriter(w, crc), io.NewSectionReader(f, start, length), *buf)
	if err != nil {
		return // the receiver went away or the file shrank; it will ask again
	}
	hd.Set("Gett-Crc32", fmt.Sprintf("%08x", crc.Sum32()))
}

// parseRange reads a single "bytes=a-b" range against size. An open end
// means to the last byte.
func parseRange(h string, size int64) (start, end int64, ok bool) {
	spec, found := strings.CutPrefix(h, "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	a, b, _ := strings.Cut(spec, "-")
	start, err := strconv.ParseInt(a, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end = size - 1
	if b != "" {
		if end, err = strconv.ParseInt(b, 10, 64); err != nil || end < start {
			return 0, 0, false
		}
		end = min(end, size-1)
	}
	return start, end, true
}

func fileError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, fs.ErrPermission):
		http.Error(w, "not readable", http.StatusForbidden)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// tarRequest is what a receiver asks for in one tar stream: files below a
// folder up to max bytes each, except the ones it already has.
type tarRequest struct {
	Max  int64 `json:"max"`
	Have []struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
	} `json:"have"`
}

func (h *handler) tar(w http.ResponseWriter, r *http.Request) {
	sh, n, ok := h.find(w, r)
	if !ok {
		return
	}
	if !n.Dir {
		http.Error(w, "not a folder", http.StatusBadRequest)
		return
	}
	var req tarRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	have := make(map[string]int64, len(req.Have))
	for _, f := range req.Have {
		have[f.Path] = f.Size
	}
	var files []*node
	n.each(func(k *node) {
		if !k.Dir && k.Size <= req.Max {
			files = append(files, k)
		}
	})
	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)
	writeTar(r.Context(), w, sh, n, files, have, req.Max)
}

// notice is the body of start and done: which item, and whether the pull
// ended well.
type notice struct {
	Path string `json:"path"`
	OK   bool   `json:"ok"`
}

func (h *handler) start(w http.ResponseWriter, r *http.Request) { h.note(w, r, false) }
func (h *handler) done(w http.ResponseWriter, r *http.Request)  { h.note(w, r, true) }

func (h *handler) note(w http.ResponseWriter, r *http.Request, done bool) {
	var body notice
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	q.Set("path", body.Path)
	r.URL.RawQuery = q.Encode()
	_, n, ok := h.find(w, r)
	if !ok {
		return
	}
	who := term.Sanitize(r.Header.Get("Gett-Name"))
	if who == "" {
		who = "?"
	}
	if len(who) > 32 {
		who = who[:32]
	}
	addr, _, _ := net.SplitHostPort(r.RemoteAddr)
	ev := event{Receiver: who, Addr: addr, Path: body.Path, Dir: n.Dir, Size: n.Size, Done: done, OK: body.OK}
	key := who + "\x00" + body.Path
	h.mu.Lock()
	if done {
		if t, ok := h.active[key]; ok {
			ev.Took = time.Since(t)
			delete(h.active, key)
		}
	} else {
		h.active[key] = time.Now()
	}
	h.mu.Unlock()
	h.notify(ev)
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
