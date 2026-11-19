package xgett

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The sample in testdata was written by the first version of the format.
// Every later gett must still open it: that is the promise a file format
// makes. It is recreated only on purpose:
//
//	XGETT_GOLDEN=1 go test -run TestGolden ./internal/xgett
const sample = "testdata/v1.xgett"

func pattern(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return string(b)
}

var sampleHolds = map[string]string{
	"v1/":             "",
	"v1/hello.txt":    "hello from gett\n",
	"v1/empty":        "",
	"v1/sub/":         "",
	"v1/sub/data.bin": pattern(10_000),
	"v1/void/":        "",
}

func TestGolden(t *testing.T) {
	dir := setup(t)
	if os.Getenv("XGETT_GOLDEN") != "" {
		smallBlocks(t) // several blocks, so the sample covers a file lying across them
		for name, body := range sampleHolds {
			p := filepath.Join(dir, filepath.FromSlash(name))
			if strings.HasSuffix(name, "/") {
				if err := os.MkdirAll(p, 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				put(t, p, body)
			}
		}
		os.Remove(sample)
		if _, err := Pack(context.Background(), []string{filepath.Join(dir, "v1")}, sample, ""); err != nil {
			t.Fatal(err)
		}
	}
	if !exists(sample) {
		t.Skip(sample + " is missing; create it once with XGETT_GOLDEN=1 go test -run TestGolden ./internal/xgett")
	}
	got, _, err := unpacked(t, sample, "")
	if err != nil {
		t.Fatal(err)
	}
	same(t, got, sampleHolds)
}

// FuzzOpen feeds Open and Each files that are not archives, or not quite.
// Whatever they hold, the answer is an error or the contents, never a crash.
func FuzzOpen(f *testing.F) {
	if b, err := os.ReadFile(sample); err == nil {
		f.Add(b)
	}
	f.Add([]byte(magic))
	f.Add((&header{blockLog: minBlockLog, blocks: 1}).bytes())
	f.Fuzz(func(t *testing.T, data []byte) {
		p := filepath.Join(t.TempDir(), "v1.xgett")
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		file, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		a, err := Open(file, "v1.xgett", "")
		if err != nil {
			return
		}
		defer a.Close()
		a.Each(context.Background(), func(int, int64, []byte) error { return nil })
	})
}
