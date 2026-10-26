// Package torrentdl downloads the content of a magnet link or .torrent file.
// It never seeds and stops as soon as the download is complete.
package torrentdl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gett/internal/download"
	"gett/internal/errs"
	"gett/internal/httpx"
	"gett/internal/term"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

type Options struct {
	Source  string // magnet link, .torrent path, or .torrent URL
	Name    string // destination typed by the user, "" = automatic
	Headers http.Header
	HTTP    *http.Client // used to fetch a .torrent URL
}

// testHook lets tests point a torrent at an in-process seeder.
var testHook func(*torrent.Torrent)

// IsTorrent reports whether s is a magnet link or a .torrent file/URL.
func IsTorrent(s string) bool {
	l := strings.ToLower(s)
	if strings.HasPrefix(l, "magnet:") {
		return true
	}
	path, _, _ := strings.Cut(l, "?")
	return strings.HasSuffix(path, ".torrent")
}

// Run downloads o.Source. A single-file torrent streams to stdout when piped.
func Run(ctx context.Context, o Options) (saved string, err error) {
	mi, magnet, hash, err := load(ctx, o)
	if err != nil {
		return "", err
	}
	dir, name, err := download.Target(o.Name)
	if err != nil {
		return "", &errs.Write{Err: err}
	}
	toStdout := !term.StdoutTTY()

	// Data lives in <dir>/<infohash>.part while downloading. Its presence is
	// what makes a rerun resume: the library re-checks the pieces on disk.
	part := filepath.Join(dir, hash+".part")
	resumed := download.Exists(part)
	if toStdout {
		part, err = os.MkdirTemp("", "gett-")
		if err != nil {
			return "", &errs.Write{Err: err}
		}
		defer os.RemoveAll(part)
	} else if err := os.MkdirAll(part, 0o755); err != nil {
		return "", &errs.Write{Err: err}
	}

	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = part
	cfg.NoUpload = true
	cfg.Seed = false
	cfg.ListenPort = 0
	cfg.NoDefaultPortForwarding = true
	cfg.Slogger = logger()
	cfg.HTTPUserAgent = "gett/" + httpx.Version
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		return "", fmt.Errorf("torrent client: %w", err)
	}
	defer cl.Close()

	var t *torrent.Torrent
	if mi != nil {
		t, err = cl.AddTorrent(mi)
	} else {
		t, err = cl.AddMagnet(magnet)
	}
	if err != nil {
		return "", &errs.BadURL{URL: o.Source}
	}
	if testHook != nil {
		testHook(t)
	}

	prog := term.NewProgress()
	if t.Info() == nil {
		spin := prog.Bar("finding peers", 0, true)
		term.Debug("waiting for metadata of %s", hash)
		select {
		case <-t.GotInfo():
		case <-ctx.Done():
			spin.Abort()
			prog.Shutdown()
			if toStdout {
				return "", ctx.Err()
			}
			return "", &errs.Resumable{Err: ctx.Err()} // the .part directory stays
		}
		spin.Done()
	}
	info := t.Info()
	total := t.Length()
	if name == "" {
		name = download.Clean(info.Name)
	}
	if resumed {
		term.Info("resuming %s", name)
	}
	term.Debug("%s: %d files, %s", info.Name, len(info.UpvertedFiles()), download.Human(total))

	if toStdout {
		if info.IsDir() {
			prog.Shutdown()
			return "", &errs.Usage{Msg: "piping needs a single-file torrent; this one has " + fmt.Sprint(len(info.UpvertedFiles())) + " files"}
		}
		r := t.NewReader()
		r.SetContext(ctx)
		r.SetReadahead(16 << 20)
		bar := prog.Bar("stdout", total, false)
		_, err := io.Copy(bar.Writer(os.Stdout), r)
		r.Close()
		if err != nil {
			bar.Abort()
			prog.Shutdown()
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", err
		}
		bar.Done()
		prog.Wait()
		return "", nil
	}

	t.DownloadAll()
	bar := prog.Bar(name, total, false)
	if err := wait(ctx, t, bar); err != nil {
		bar.Abort()
		prog.Shutdown()
		return "", &errs.Resumable{Err: err}
	}
	bar.Done()
	prog.Wait()
	cl.Close() // flush everything to disk before moving

	final := filepath.Join(dir, name)
	for i := 1; download.Exists(final); i++ {
		final = filepath.Join(dir, download.Numbered(name, i))
	}
	if err := os.Rename(filepath.Join(part, info.Name), final); err != nil {
		return "", &errs.Write{Err: err}
	}
	os.RemoveAll(part)
	term.Info("saved %s (%s)", final, download.Human(total))
	return final, nil
}

// load resolves the source into either parsed metainfo or a magnet URI, plus
// the info hash that names the working directory.
func load(ctx context.Context, o Options) (mi *metainfo.MetaInfo, magnet, hash string, err error) {
	src := o.Source
	switch {
	case strings.HasPrefix(strings.ToLower(src), "magnet:"):
		m, err := metainfo.ParseMagnetUri(src)
		if err != nil {
			return nil, "", "", &errs.BadURL{URL: src}
		}
		return nil, src, m.InfoHash.HexString(), nil
	case strings.Contains(src, "://"):
		req, err := httpx.NewRequest(ctx, http.MethodGet, src, nil, o.Headers)
		if err != nil {
			return nil, "", "", err
		}
		resp, err := o.HTTP.Do(req)
		if err != nil {
			return nil, "", "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			return nil, "", "", &errs.HTTPStatus{Code: resp.StatusCode, Status: term.Sanitize(resp.Status), URL: resp.Request.URL.Redacted()}
		}
		mi, err = metainfo.Load(io.LimitReader(resp.Body, 64<<20))
		if err != nil {
			return nil, "", "", fmt.Errorf("%s is not a torrent file", term.Sanitize(src))
		}
	default:
		mi, err = metainfo.LoadFromFile(src)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, "", "", err
			}
			return nil, "", "", fmt.Errorf("%s is not a torrent file", src)
		}
	}
	return mi, "", mi.HashInfoBytes().HexString(), nil
}

// wait follows the download until every byte is in, reporting progress.
func wait(ctx context.Context, t *torrent.Torrent, bar *term.Bar) error {
	total := t.Length()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	last := time.Now()
	for {
		done := t.BytesCompleted()
		bar.SetCurrent(done)
		if done >= total {
			return nil
		}
		if term.Verbose && time.Since(last) >= 5*time.Second {
			s := t.Stats()
			term.Debug("%s of %s, %d peers (%d seeding)", download.Human(done), download.Human(total), s.ActivePeers, s.ConnectedSeeders)
			last = time.Now()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// logger silences the library unless verbose, where warnings go to stderr.
func logger() *slog.Logger {
	if term.Verbose {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
