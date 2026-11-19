package split

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"gett/internal/term"
)

const benchSize = 2 << 30

// benchFile writes 2 GiB that do not repeat.
func benchFile(b *testing.B) string {
	b.Helper()
	tty := true
	term.StdoutIsTTY = &tty
	term.Silent = true
	b.Cleanup(func() { term.StdoutIsTTY = nil; term.Silent = false })
	file := filepath.Join(b.TempDir(), "f.bin")
	f, err := os.Create(file)
	if err != nil {
		b.Fatal(err)
	}
	block := data(1, 64<<20)
	for n := 0; n < benchSize; n += len(block) {
		block[0] = byte(n >> 26) // every block differs from the last
		if _, err := f.Write(block); err != nil {
			b.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}
	return file
}

func BenchmarkSplit(b *testing.B) {
	file := benchFile(b)
	b.SetBytes(benchSize)
	for i := 0; i < b.N; i++ {
		out := filepath.Join(b.TempDir(), "parts")
		if err := Split(context.Background(), file, 100_000_000, "100MB", out); err != nil {
			b.Fatal(err)
		}
		os.RemoveAll(out)
	}
}

func BenchmarkJoin(b *testing.B) {
	file := benchFile(b)
	parts := filepath.Join(b.TempDir(), "parts")
	if err := Split(context.Background(), file, 100_000_000, "100MB", parts); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(benchSize)
	for i := 0; i < b.N; i++ {
		out := filepath.Join(b.TempDir(), "out")
		if err := Join(context.Background(), filepath.Join(parts, "f.bin.001"), out, false); err != nil {
			b.Fatal(err)
		}
		os.RemoveAll(out)
	}
}
