package transfer

import (
	"archive/tar"
	"context"
	"io"
	"os"

	"gett/internal/term"
)

// writeTar streams the given files of a share as one tar: each header comes
// from a fresh stat, a file the receiver already has (same path and size)
// is left out, and a file that vanished or cannot be read is skipped. The
// receiver notices a missing file on the next run and asks again.
func writeTar(ctx context.Context, w io.Writer, sh *share, folder *node, files []*node, have map[string]int64, max int64) error {
	tw := tar.NewWriter(w)
	buf := buffers.Get().(*[]byte)
	defer buffers.Put(buf)
	for _, n := range files {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel := folder.relTo(n)
		fi, err := sh.Root.Stat(n.Rel)
		if err != nil || !fi.Mode().IsRegular() || fi.Size() > max {
			term.Debug("tar: skipping %s", n.Rel)
			continue
		}
		if size, ok := have[rel]; ok && size == fi.Size() {
			continue
		}
		f, err := sh.Root.Open(n.Rel)
		if err != nil {
			term.Debug("tar: skipping %s: %v", n.Rel, err)
			continue
		}
		// PAX keeps the mtime exact, so the copy matches the original.
		h := &tar.Header{Typeflag: tar.TypeReg, Name: rel, Mode: int64(0o644 | fi.Mode()&0o111), Size: fi.Size(), ModTime: fi.ModTime(), Format: tar.FormatPAX}
		if err := tw.WriteHeader(h); err != nil {
			f.Close()
			return err
		}
		_, err = io.CopyBuffer(tw, io.LimitReader(f, fi.Size()), *buf)
		f.Close()
		if err != nil {
			return err
		}
	}
	return tw.Close()
}

var _ = os.ModePerm
