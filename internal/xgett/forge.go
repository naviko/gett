package xgett

import (
	"context"
	"os"
	"path/filepath"
)

// Forge writes an archive holding exactly these entries, with body(i) as the
// bytes of file i. Nothing about the entries is checked, which is the point:
// tests of the unpacking side use it to build archives Pack would never
// write, such as one whose paths climb out of the folder.
func Forge(path, password string, entries []Entry, body func(i int) []byte) error {
	for i := range entries {
		if entries[i].Kind == File {
			entries[i].Size = int64(len(body(i)))
		}
	}
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	read := func(i int, dst []byte, off int64) error {
		copy(dst, body(i)[off:])
		return nil
	}
	_, err = write(context.Background(), out, filepath.Base(path), password, entries, read, func(int) {})
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}
