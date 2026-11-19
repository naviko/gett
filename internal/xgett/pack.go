package xgett

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gett/internal/download"
	"gett/internal/errs"
	"gett/internal/term"

	"github.com/klauspost/compress/zstd"
)

// walked runs between listing the inputs and reading them. Tests change a
// file there.
var walked = func() {}

// Pack writes the inputs, files and folders, into one .xgett called name, or
// the first numbered variant of name that is free, and returns the name
// used. password may be empty. Nothing is left behind when it fails.
func Pack(ctx context.Context, inputs []string, name, password string) (string, error) {
	start := time.Now()
	final := name
	for i := 1; download.Exists(final); i++ {
		final = download.Numbered(name, i)
	}
	part := final + ".part"

	entries, src, skipped, err := walk(ctx, inputs, final, part)
	if err != nil {
		return "", err
	}
	walked()
	var files, in int64
	for i := range entries {
		switch entries[i].Kind {
		case File:
			files++
			in += entries[i].Size
		case Link:
			files++
		}
	}

	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return "", &errs.Write{Err: err}
	}
	out, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return "", &errs.Write{Err: err}
	}
	prog := term.NewProgress()
	bar := prog.Bar(filepath.Base(final), in, false)
	read := func(i int, dst []byte, off int64) error { return readAt(src[i], dst, off) }
	size, err := write(ctx, out, filepath.Base(name), password, entries, read, bar.Add)
	if err == nil {
		if serr := out.Sync(); serr != nil {
			err = &errs.Write{Err: serr}
		}
	}
	if cerr := out.Close(); err == nil && cerr != nil {
		err = &errs.Write{Err: cerr}
	}
	if err == nil {
		if rerr := os.Rename(part, final); rerr != nil {
			err = &errs.Write{Err: rerr}
		}
	}
	if err != nil {
		os.Remove(part)
		bar.Abort()
		prog.Shutdown()
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", err
	}
	bar.Done()
	prog.Wait()
	term.Info("compressed %s (%s) into %s (%s)%s", count(files), download.Human(in), final, download.Human(size), Tail(in, size, time.Since(start)))
	if skipped > 0 {
		term.Info("skipped %d entries that are not files, folders or links; verbose names them", skipped)
	}
	return final, nil
}

func count(n int64) string {
	if n == 1 {
		return "1 file"
	}
	return fmt.Sprintf("%d files", n)
}

// walk lists what the inputs hold, a folder before its contents, and for
// every entry the file on disk behind it. An input named on the command line
// is followed when it is a link; links found inside a folder are kept as
// links. The files in avoid, the archive being written, are left out.
func walk(ctx context.Context, inputs []string, avoid ...string) (es []Entry, src []string, skipped int, err error) {
	skip := map[string]bool{}
	for _, a := range avoid {
		if abs, err := filepath.Abs(a); err == nil {
			skip[abs] = true
		}
	}
	add := func(e Entry, from string) {
		es = append(es, e)
		src = append(src, from)
	}
	other := func(p string) {
		skipped++
		term.Debug("skipping %s: not a file, folder or link", term.Sanitize(p))
	}
	tops := map[string]bool{}
	n := 0
	for _, in := range inputs {
		fi, err := os.Stat(in)
		if err != nil {
			return nil, nil, 0, err
		}
		abs, err := filepath.Abs(in)
		if err != nil {
			return nil, nil, 0, err
		}
		top := filepath.Base(abs)
		if top == "." || strings.ContainsAny(top, `/\:`) { // a drive or the root itself
			top = "root"
		}
		if tops[top] {
			return nil, nil, 0, &errs.Usage{Msg: fmt.Sprintf("two things to compress are named %s", top)}
		}
		tops[top] = true

		switch {
		case fi.Mode().IsRegular():
			if !skip[abs] {
				add(Entry{Kind: File, Exec: fi.Mode()&0o111 != 0, Mtime: fi.ModTime(), Size: fi.Size(), Path: top}, in)
			}
		case fi.IsDir():
			root := in
			if real, err := filepath.EvalSymlinks(in); err == nil {
				root = real // WalkDir does not enter a link, even when it is the starting point
			}
			err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if n++; n%1024 == 0 && ctx.Err() != nil {
					return ctx.Err()
				}
				rel, err := filepath.Rel(root, p)
				if err != nil {
					return err
				}
				name := top
				if rel != "." {
					name = top + "/" + filepath.ToSlash(rel)
				}
				switch t := d.Type(); {
				case t.IsDir():
					add(Entry{Kind: Dir, Path: name}, "")
				case t.IsRegular():
					if skip[filepath.Join(abs, rel)] {
						return nil
					}
					info, err := d.Info()
					if err != nil {
						return err
					}
					add(Entry{Kind: File, Exec: info.Mode()&0o111 != 0, Mtime: info.ModTime(), Size: info.Size(), Path: name}, p)
				case t&fs.ModeSymlink != 0:
					target, err := os.Readlink(p)
					if err != nil {
						return err
					}
					add(Entry{Kind: Link, Path: name, Target: filepath.ToSlash(target)}, "")
				default:
					other(p)
				}
				return nil
			})
			if err != nil {
				return nil, nil, 0, err
			}
		default:
			other(in)
		}
	}
	return es, src, skipped, nil
}

// readAt fills dst from a file. The sizes were taken when the inputs were
// listed, so a file that is shorter now cannot be packed as listed.
func readAt(path string, dst []byte, off int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := f.ReadAt(dst, off)
	if n == len(dst) {
		return nil
	}
	if err == nil || err == io.EOF {
		return fmt.Errorf("%s changed while it was being compressed", path)
	}
	return err
}

// frame is one block on its way to disk.
type frame struct {
	buf  []byte // from the pool; buf[:n] is what gets written
	n    int
	data int // how many of the block's bytes belong to files, for the progress bar
}

// write packs entries into out: the header, then every block in order. read
// fills dst with the bytes of file i from off on. name is the archive's file
// name. It returns the bytes written.
func write(ctx context.Context, out io.Writer, name, password string, entries []Entry, read func(i int, dst []byte, off int64) error, progress func(n int)) (int64, error) {
	index := encodeIndex(entries)
	head := make([]byte, 8, 8+len(index))
	binary.LittleEndian.PutUint64(head, uint64(len(index)))
	head = append(head, index...)
	data := int64(len(head))
	total, spans, err := layout(entries, data)
	if err != nil {
		return 0, err
	}
	b := int64(1) << blockLog

	h := header{blockLog: byte(blockLog), blocks: uint64((total + b - 1) / b)}
	if _, err := rand.Read(h.salt[:]); err != nil {
		return 0, err
	}
	var key []byte
	if password != "" {
		h.flags = flagPassword
		key = passwordKey(password, h.salt[:])
	} else {
		key = plainKey(label(name), h.salt[:])
	}
	aead := newAEAD(key)
	hb := h.bytes()
	copy(hb[sealedLen:], aead.Seal(nil, checkNonce, nil, hb[:sealedLen]))
	if _, err := out.Write(hb); err != nil {
		return 0, &errs.Write{Err: err}
	}

	w := Workers()
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedFastest),
		zstd.WithEncoderConcurrency(w),
		zstd.WithEncoderCRC(false), // the seal already proves every byte
	)
	if err != nil {
		return 0, err
	}
	defer enc.Close()
	// A buffer holds a whole frame: length, method, the block packed or not,
	// and the seal. It is large enough for the worst case of packing, so
	// EncodeAll never has to move to a new one.
	bufs := newPool(3*w+4, 5+max(int(b), enc.MaxEncodedSize(int(b)))+tagLen)

	// fill copies the stream from pos on into dst: the head from memory,
	// then each file that reaches into the block.
	fill := func(dst []byte, pos int64) error {
		if pos < data {
			n := copy(dst, head[pos:])
			dst, pos = dst[n:], pos+int64(n)
		}
		i := sort.Search(len(spans), func(j int) bool { return spans[j].off+spans[j].size > pos })
		for ; len(dst) > 0 && i < len(spans); i++ {
			s := spans[i]
			from := pos - s.off
			n := int(min(s.size-from, int64(len(dst))))
			if err := read(s.idx, dst[:n], from); err != nil {
				return err
			}
			dst, pos = dst[n:], pos+int64(n)
		}
		return nil
	}

	next := func(k uint64) (uint64, error) { return k, nil }
	work := func(k uint64) (frame, error) {
		pos := int64(k) * b
		n := int(min(b, total-pos))
		plain := bufs.get()
		if err := fill(plain[5:5+n], pos); err != nil {
			return frame{}, err
		}
		packed := bufs.get()
		z := enc.EncodeAll(plain[5:5+n], packed[5:5])
		buf, m := plain, n
		buf[4] = stored
		if len(z) > 0 && len(z) < n && &z[0] == &packed[5] {
			buf, m = packed, len(z)
			buf[4] = compressed
			bufs.put(plain)
		} else {
			bufs.put(packed)
		}
		sealed := aead.Seal(buf[4:4], nonce(k), buf[4:5+m], nil)
		binary.LittleEndian.PutUint32(buf, uint32(len(sealed)))
		return frame{buf, 4 + len(sealed), n - int(max(0, min(int64(n), data-pos)))}, nil
	}
	size := int64(headerLen)
	emit := func(f frame) error {
		if _, err := out.Write(f.buf[:f.n]); err != nil {
			return &errs.Write{Err: err}
		}
		size += int64(f.n)
		progress(f.data)
		bufs.put(f.buf)
		return nil
	}
	if err := ordered(ctx, w, h.blocks, next, work, emit); err != nil {
		return 0, err
	}
	return size, nil
}
