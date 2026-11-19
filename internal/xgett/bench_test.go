package xgett

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/chacha20poly1305"
)

// benchTree writes 256 MiB of made-up words, which pack to about a third,
// into eight files.
func benchTree(b *testing.B) (dir string, size int64) {
	b.Helper()
	dir = b.TempDir()
	r := rand.New(rand.NewSource(1))
	words := make([][]byte, 4096)
	for i := range words {
		w := make([]byte, 3+r.Intn(9))
		for j := range w {
			w[j] = byte('a' + r.Intn(26))
		}
		words[i] = w
	}
	const each = 32 << 20
	for i := 0; i < 8; i++ {
		var buf bytes.Buffer
		buf.Grow(each + 16)
		for buf.Len() < each {
			buf.Write(words[r.Intn(len(words))])
			buf.WriteByte(' ')
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d.txt", i)), buf.Bytes()[:each], 0o644); err != nil {
			b.Fatal(err)
		}
	}
	return dir, 8 * each
}

// The block sizes worth comparing, as powers of two: 1, 2 and 4 MiB. Smaller
// blocks unpack a little faster and pack a little worse; 4 MiB is the
// format's choice.
var benchBlocks = []int{20, 21, 22}

func blockName(log int) string {
	if log < 20 {
		return fmt.Sprintf("%dKiB", 1<<(log-10))
	}
	return fmt.Sprintf("%dMiB", 1<<(log-20))
}

func BenchmarkPack(b *testing.B) {
	setup(b)
	dir, size := benchTree(b)
	out := filepath.Join(b.TempDir(), "out.xgett")
	all := Workers()
	for _, log := range benchBlocks {
		for _, w := range []int{1, all} {
			b.Run(fmt.Sprintf("block=%s/workers=%d", blockName(log), w), func(b *testing.B) {
				old := blockLog
				blockLog, testWorkers = log, w
				defer func() { blockLog, testWorkers = old, 0 }()
				b.SetBytes(size)
				for i := 0; i < b.N; i++ {
					os.Remove(out)
					if _, err := Pack(context.Background(), []string{dir}, out, ""); err != nil {
						b.Fatal(err)
					}
				}
				if fi, err := os.Stat(out); err == nil {
					b.ReportMetric(float64(fi.Size())*100/float64(size), "%-of-original")
				}
			})
		}
	}
}

// BenchmarkUnpack opens every block and hands the pieces to nobody: it
// measures reading, unsealing and unpacking, not the disk being written to.
func BenchmarkUnpack(b *testing.B) {
	setup(b)
	dir, size := benchTree(b)
	all := Workers()
	for _, log := range benchBlocks {
		out := filepath.Join(b.TempDir(), "out.xgett")
		old := blockLog
		blockLog = log
		_, err := Pack(context.Background(), []string{dir}, out, "")
		blockLog = old
		if err != nil {
			b.Fatal(err)
		}
		for _, w := range []int{1, all} {
			b.Run(fmt.Sprintf("block=%s/workers=%d", blockName(log), w), func(b *testing.B) {
				testWorkers = w
				defer func() { testWorkers = 0 }()
				b.SetBytes(size)
				for i := 0; i < b.N; i++ {
					f, err := os.Open(out)
					if err != nil {
						b.Fatal(err)
					}
					a, err := Open(f, "out.xgett", "")
					if err != nil {
						b.Fatal(err)
					}
					if err := a.Each(context.Background(), func(int, int64, []byte) error { return nil }); err != nil {
						b.Fatal(err)
					}
					a.Close()
					f.Close()
				}
			})
		}
	}
}

// BenchmarkSeal is why the format seals with AES-256-GCM: it compares it
// with ChaCha20-Poly1305 on one core. A 4 MiB block is sealed and opened
// again, so each figure is half the speed of one direction.
func BenchmarkSeal(b *testing.B) {
	key := make([]byte, 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		b.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		b.Fatal(err)
	}
	chacha, err := chacha20poly1305.New(key)
	if err != nil {
		b.Fatal(err)
	}
	buf := make([]byte, 4<<20, 4<<20+tagLen)
	n := make([]byte, 12)
	for _, c := range []struct {
		name string
		aead cipher.AEAD
	}{{"chacha20poly1305", chacha}, {"aes256gcm", gcm}} {
		b.Run(c.name, func(b *testing.B) {
			b.SetBytes(int64(len(buf)))
			for i := 0; i < b.N; i++ {
				sealed := c.aead.Seal(buf[:0], n, buf, nil)
				if _, err := c.aead.Open(sealed[:0], n, sealed, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
