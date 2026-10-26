package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"gett/internal/download"
	"gett/internal/errs"
	"gett/internal/extract"
	"gett/internal/term"
)

// Pull downloads the numbered items of the last list into dest, the current
// folder when empty, and returns the saved paths. When stdout is piped the
// one file streams there and nothing is saved.
func Pull(ctx context.Context, numbers []string, dest, password string) ([]string, error) {
	s := loadLast()
	if s == nil {
		return nil, &errs.Usage{Msg: "run gett transfer first to see what is shared"}
	}
	items, err := s.pick(numbers)
	if err != nil {
		return nil, err
	}
	piped := !term.StdoutTTY()
	if piped && len(items) != 1 {
		return nil, &errs.Usage{Msg: "one file at a time when piped"}
	}
	if piped && items[0].Dir {
		return nil, &errs.Usage{Msg: "a folder cannot be piped; run on a terminal to save it"}
	}
	if dest != "" {
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return nil, &errs.Write{Err: err}
		}
	}
	peers := map[int]*peer{}
	var saved []string
	for _, it := range items {
		sh := s.Sharers[it.Sharer]
		p := peers[it.Sharer]
		if p == nil {
			if p, err = reach(ctx, sh, s.passwordsFor(sh.Name, password)); err != nil {
				return saved, resumable(ctx, err)
			}
			peers[it.Sharer] = p
		}
		fresh, err := recheck(ctx, p, it)
		if err != nil {
			if errors.Is(err, errChanged) {
				List(ctx, password, os.Stdout)
				return saved, &errs.Range{Msg: fmt.Sprintf("%s on %s changed; run again with the numbers above", it.Path, sh.Name)}
			}
			return saved, resumable(ctx, err)
		}
		if piped {
			return nil, resumable(ctx, stream(ctx, p, it, fresh))
		}
		out, err := pullItem(ctx, p, it, fresh, dest)
		if err != nil {
			return saved, resumable(ctx, err)
		}
		if out != "" {
			saved = append(saved, out)
		}
	}
	return saved, nil
}

// resumable marks an error the same command can finish.
func resumable(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil || (errs.CanResume(err) && status(err) == 0) {
		return &errs.Resumable{Err: err}
	}
	return err
}

// pick resolves numbers to items, dropping repeats and items inside a
// selected folder.
func (s *saved) pick(numbers []string) ([]savedItem, error) {
	var items []savedItem
	for _, n := range numbers {
		_, it, ok := s.sharer(n)
		if !ok {
			return nil, &errs.Usage{Msg: "no item " + n + " in the last list"}
		}
		items = append(items, it)
	}
	var out []savedItem
	for i, it := range items {
		keep := true
		for j, other := range items {
			if i != j && (other.N == it.N && j < i || strings.HasPrefix(it.N, other.N+".")) {
				keep = false
			}
		}
		if keep {
			out = append(out, it)
		} else {
			term.Debug("%s is inside another selected item", it.N)
		}
	}
	return out, nil
}

// recheck asks the sharer about the item again: same name, same kind, and
// for a file the same size, or the list the numbers came from is stale.
func recheck(ctx context.Context, p *peer, it savedItem) (item, error) {
	parent, base := path.Split(it.Path)
	kids, err := p.ls(ctx, strings.TrimSuffix(parent, "/"))
	if err != nil {
		if status(err) == http.StatusNotFound {
			return item{}, errChanged
		}
		return item{}, err
	}
	for _, k := range kids {
		if k.Name == base {
			if k.Dir != it.Dir || (!k.Dir && k.Size != it.Size) {
				return item{}, errChanged
			}
			return k, nil
		}
	}
	return item{}, errChanged
}

// stream sends one file to stdout.
func stream(ctx context.Context, p *peer, it savedItem, fresh item) error {
	hdr := http.Header{"Gett-Stamp": {stampOf(fresh)}}
	resp, err := p.do(ctx, http.MethodGet, "/v1/file", url.Values{"path": {it.Path}}, nil, hdr)
	if err != nil {
		return describe(err, p, it)
	}
	defer resp.Body.Close()
	p.note(ctx, it.Path, false, true)
	prog := term.NewProgress()
	bar := prog.Bar("stdout", fresh.Size, false)
	crc := crc32.New(castagnoli)
	buf := buffers.Get().(*[]byte)
	defer buffers.Put(buf)
	_, err = io.CopyBuffer(io.MultiWriter(bar.Writer(os.Stdout), crc), struct{ io.Reader }{resp.Body}, *buf)
	if err == nil {
		if want := resp.Trailer.Get("Gett-Crc32"); want != "" && want != fmt.Sprintf("%08x", crc.Sum32()) {
			err = fmt.Errorf("data from %s arrived damaged", p.Name)
		}
	}
	finish(p, it.Path, err == nil)
	if err != nil {
		bar.Abort()
		prog.Shutdown()
		var pe *fs.PathError
		if errors.As(err, &pe) {
			return &errs.Write{Err: err}
		}
		return err
	}
	bar.Done()
	prog.Wait()
	return nil
}

func stampOf(it item) string { return strconvItoa(it.Size) + "-" + strconvItoa(it.MTime.UnixNano()) }

// finish tells the sharer the pull ended, even when the command is being
// interrupted.
func finish(p *peer, path string, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	p.note(ctx, path, true, ok)
}

// describe turns a refused request into a line about this item.
func describe(err error, p *peer, it savedItem) error {
	switch status(err) {
	case http.StatusNotFound:
		return &errs.Range{Msg: fmt.Sprintf("%s is gone from %s", it.Path, p.Name)}
	case http.StatusForbidden:
		return &errs.Range{Msg: fmt.Sprintf("%s is not readable on %s", it.Path, p.Name)}
	}
	return err
}

// pullItem saves one item under dest: resumed when a matching sidecar is
// there, skipped when an identical copy is, under the next free name when
// something else holds the name.
func pullItem(ctx context.Context, p *peer, it savedItem, fresh item, dest string) (string, error) {
	name := download.Clean(path.Base(it.Path))
	for i := 0; ; i++ {
		cand := name
		if i > 0 {
			cand = numbered(name, i, it.Dir)
		}
		full := filepath.Join(dest, cand)
		part, meta := full+".part", full+".gett"
		side := loadSidecar(meta)
		matches := side != nil && side.matches(p.Name, it.Path, it.Dir, fresh.Size, fresh.MTime)
		final, partial := download.Exists(full), download.Exists(part)
		switch {
		case final && identical(full, it.Dir, fresh):
			if matches && !partial {
				os.Remove(meta) // the last run got as far as the rename
			}
			term.Info("%s already there", full)
			return full, nil
		case matches && partial:
			term.Info("resuming %s", full)
		case matches && !final:
			os.Remove(meta) // a sidecar with nothing beside it
			side = nil
		case final || partial || side != nil:
			continue
		default:
			side = nil
		}
		return full, pullInto(ctx, p, it, fresh, full, part, meta, side)
	}
}

// identical reports whether what is at full is the item as the sharer has
// it: same size and mtime for a file, same size and file count for a
// folder.
func identical(full string, dir bool, fresh item) bool {
	fi, err := os.Lstat(full)
	if err != nil || fi.IsDir() != dir {
		return false
	}
	if !dir {
		return fi.Size() == fresh.Size && fi.ModTime().Truncate(time.Second).Equal(fresh.MTime.Truncate(time.Second))
	}
	var size int64
	files := 0
	filepath.WalkDir(full, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") && p != full {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				size += info.Size()
				files++
			}
		}
		return nil
	})
	return size == fresh.Size && files == fresh.Files
}

// pullInto fetches the item as full.part beside its sidecar and renames it
// when complete. A file that changes on the sharer starts over once.
func pullInto(ctx context.Context, p *peer, it savedItem, fresh item, full, part, meta string, side *sidecar) error {
	p.note(ctx, it.Path, false, true)
	prog := term.NewProgress()
	bar := prog.Bar(filepath.Base(full), fresh.Size, false)
	var err error
	for restarts := 0; ; restarts++ {
		if it.Dir {
			err = pullFolder(ctx, p, it, fresh, part, meta, side, bar)
		} else {
			err = pullFile(ctx, p, it, fresh, part, meta, side, bar)
		}
		if !errors.Is(err, errChanged) {
			break
		}
		if restarts > 0 {
			err = &errs.Range{Msg: fmt.Sprintf("%s keeps changing on %s", it.Path, p.Name)}
			break
		}
		if fresh, err = recheck(ctx, p, it); err != nil {
			break
		}
		term.Info("%s changed on %s, starting over", it.Path, p.Name)
		os.Remove(part)
		os.Remove(meta)
		side = nil
		bar.SetCurrent(0)
	}
	finish(p, it.Path, err == nil)
	if err != nil {
		bar.Abort()
		prog.Shutdown()
		return err
	}
	if err := os.Rename(part, full); err != nil {
		bar.Abort()
		prog.Shutdown()
		return &errs.Write{Err: err}
	}
	os.Remove(meta)
	bar.Done()
	prog.Wait()
	if it.Dir {
		term.Info("saved %s (%s, %s)", full, download.Human(fresh.Size), plural(fresh.Files, "file"))
	} else {
		os.Chtimes(full, fresh.MTime, fresh.MTime)
		term.Info("saved %s (%s)", full, download.Human(fresh.Size))
	}
	return nil
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

// pullFile moves one file through the engine.
func pullFile(ctx context.Context, p *peer, it savedItem, fresh item, part, meta string, side *sidecar, bar *term.Bar) error {
	return pullBig(ctx, p, it.Path, fresh, part, meta, side, bar, nil)
}

func pullBig(ctx context.Context, p *peer, wire string, fresh item, part, meta string, side *sidecar, bar *term.Bar, busy *atomic.Int32) error {
	f, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return &errs.Write{Err: err}
	}
	defer f.Close()
	if side == nil {
		if err := f.Truncate(fresh.Size); err != nil {
			return &errs.Write{Err: err}
		}
		side = &sidecar{Kind: "file", Sharer: p.Name, Path: wire, Size: fresh.Size, MTime: fresh.MTime}
		if err := side.save(meta); err != nil {
			return &errs.Write{Err: err}
		}
	}
	q := newQueue(fresh.Size, side.Done)
	for _, d := range side.Done {
		bar.Add(int(d.End - d.Start))
	}
	e := &engine{ctx: ctx, p: p, path: wire, stamp: stampOf(fresh), f: f, meta: meta, side: side, q: q, bar: bar, busy: busy}
	if err := e.run(); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return &errs.Write{Err: err}
	}
	return f.Close()
}

// pullFolder fills part with the folder's files: the small ones as one tar
// stream, the big ones one at a time through the engine, side by side.
// What is already there with the right size is left alone.
func pullFolder(ctx context.Context, p *peer, it savedItem, fresh item, part, meta string, side *sidecar, bar *term.Bar) error {
	var flat struct {
		Files []fileEntry `json:"files"`
		Dirs  []string    `json:"dirs"`
	}
	if err := p.getJSON(ctx, "/v1/files", url.Values{"path": {it.Path}}, &flat); err != nil {
		return describe(err, p, it)
	}
	if err := os.MkdirAll(part, 0o755); err != nil {
		return &errs.Write{Err: err}
	}
	if side == nil {
		side = &sidecar{Kind: "folder", Sharer: p.Name, Path: it.Path}
		if err := side.save(meta); err != nil {
			return &errs.Write{Err: err}
		}
	}
	for _, d := range flat.Dirs {
		if err := os.MkdirAll(filepath.Join(part, filepath.FromSlash(d)), 0o755); err != nil {
			return &errs.Write{Err: err}
		}
	}
	want := make(map[string]fileEntry, len(flat.Files))
	for _, f := range flat.Files {
		want[f.Path] = f
	}
	// What the last run left: complete files count, anything else of the
	// wrong size goes, a big file in progress keeps its sidecar.
	have := map[string]int64{}
	root, err := os.OpenRoot(part)
	if err != nil {
		return &errs.Write{Err: err}
	}
	defer root.Close()
	fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		if strings.HasSuffix(rel, ".part") || strings.HasSuffix(rel, ".gett") || strings.HasSuffix(rel, ".gett.tmp") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if w, ok := want[rel]; ok && w.Size == info.Size() {
			have[rel] = info.Size()
			bar.Add(int(info.Size()))
		} else {
			root.Remove(rel)
		}
		return nil
	})
	var small []fileEntry
	var big []fileEntry
	for _, f := range flat.Files {
		if _, ok := have[f.Path]; ok {
			continue
		}
		if f.Size <= smallMax {
			small = append(small, f)
		} else {
			big = append(big, f)
		}
	}
	slices.SortFunc(big, func(a, b fileEntry) int { return strings.Compare(a.Path, b.Path) })

	var busy atomic.Int32
	g, gctx := errgroup.WithContext(ctx)
	if len(small) > 0 {
		busy.Add(1)
		g.Go(func() error {
			defer busy.Add(-1)
			return pullSmall(gctx, p, it.Path, part, have, bar)
		})
	}
	g.Go(func() error {
		for _, f := range big {
			local := filepath.Join(part, filepath.FromSlash(f.Path))
			fp, fm := local+".part", local+".gett"
			fside := loadSidecar(fm)
			if fside != nil && !fside.matches(p.Name, it.Path+"/"+f.Path, false, f.Size, f.MTime) {
				os.Remove(fp)
				os.Remove(fm)
				fside = nil
			}
			one := item{Name: path.Base(f.Path), Size: f.Size, MTime: f.MTime}
			err := pullBig(gctx, p, it.Path+"/"+f.Path, one, fp, fm, fside, bar, &busy)
			switch {
			case err == nil:
			case status(err) == http.StatusNotFound:
				term.Info("skipped %s/%s: gone from %s", it.Path, f.Path, p.Name)
				os.Remove(fp)
				os.Remove(fm)
				continue
			case status(err) == http.StatusForbidden:
				term.Info("skipped %s/%s: not readable on %s", it.Path, f.Path, p.Name)
				os.Remove(fp)
				os.Remove(fm)
				continue
			default:
				return err
			}
			if err := os.Rename(fp, local); err != nil {
				return &errs.Write{Err: err}
			}
			os.Remove(fm)
			os.Chtimes(local, f.MTime, f.MTime)
		}
		return nil
	})
	if err := g.Wait(); err != nil {
		return err
	}
	// A small file the sharer did not send is gone or unreadable there.
	for _, f := range small {
		if _, err := root.Stat(f.Path); err != nil {
			term.Info("skipped %s/%s: not served by %s", it.Path, f.Path, p.Name)
		}
	}
	return nil
}

// pullSmall asks for every small file not already there as one tar stream.
func pullSmall(ctx context.Context, p *peer, wire, part string, have map[string]int64, bar *term.Bar) error {
	req := tarRequest{Max: smallMax}
	for rel, size := range have {
		req.Have = append(req.Have, struct {
			Path string `json:"path"`
			Size int64  `json:"size"`
		}{rel, size})
	}
	body, _ := json.Marshal(req)
	resp, err := p.do(ctx, http.MethodPost, "/v1/tar", url.Values{"path": {wire}}, strings.NewReader(string(body)), http.Header{"Content-Type": {"application/json"}})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _, err = extract.ExtractTar(ctx, bar.Reader(resp.Body), part, nil)
	return err
}
