package split

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gett/internal/errs"
	"gett/internal/term"
)

func setup(t *testing.T) string {
	t.Helper()
	tty := true
	term.StdoutIsTTY = &tty
	term.Silent = true
	t.Cleanup(func() { term.StdoutIsTTY = nil; term.Silent = false })
	return t.TempDir()
}

func data(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func put(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// said runs f with messages switched on and returns what was printed.
func said(t *testing.T, f func() error) (string, error) {
	t.Helper()
	capture, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	stderr := os.Stderr
	os.Stderr, term.Silent = capture, false
	err = f()
	os.Stderr, term.Silent = stderr, true
	capture.Close()
	return string(get(t, capture.Name())), err
}

// spread writes b into dir in pieces of size bytes under the given names, the
// way any splitter would.
func spread(t *testing.T, dir string, b []byte, size int, names []string) {
	t.Helper()
	for i, name := range names {
		from, to := i*size, min((i+1)*size, len(b))
		put(t, filepath.Join(dir, name), b[from:to])
	}
	if len(names)*size < len(b) {
		t.Fatalf("%d names of %d bytes do not hold %d bytes", len(names), size, len(b))
	}
}

func TestRoundTrip(t *testing.T) {
	for _, c := range []struct {
		name        string
		total, size int
		parts       int
		last        string
	}{
		{"an exact multiple", 5000, 1000, 5, "f.bin.005"},
		{"a remainder", 5300, 1000, 6, "f.bin.006"},
		{"parts longer than a buffer", 9<<20 + 123, 5<<19 + 7, 4, "f.bin.004"},
		{"more than 999 parts", 1500, 1, 1500, "f.bin.1500"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := setup(t)
			want := data(1, c.total)
			file := filepath.Join(dir, "f.bin")
			put(t, file, want)
			text := fmt.Sprintf("%dB", c.size)

			out, err := said(t, func() error { return Split(context.Background(), file, int64(c.size), text, "") })
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, fmt.Sprintf("into %d parts of %s, in ", c.parts, text)) || strings.Count(out, "\n") != 1 {
				t.Errorf("the finish line: %q", out)
			}

			// The parts, one after the other, are the file. That is all another tool needs.
			var all []byte
			for i := 1; i <= c.parts; i++ {
				p := get(t, filepath.Join(dir, fmt.Sprintf("f.bin.%03d", i)))
				if i < c.parts && len(p) != c.size {
					t.Fatalf("part %d holds %d bytes, want %d", i, len(p), c.size)
				}
				all = append(all, p...)
			}
			if !bytes.Equal(all, want) {
				t.Fatal("the parts put end to end are not the file")
			}
			if !exists(filepath.Join(dir, c.last)) || exists(filepath.Join(dir, c.last+".part")) {
				t.Fatalf("the last part should be %s", c.last)
			}
			if n := len(names(t, dir)); n != c.parts+1 {
				t.Fatalf("%d files in the folder, want the original and %d parts", n, c.parts)
			}

			back := filepath.Join(dir, "back")
			out, err = said(t, func() error { return Join(context.Background(), filepath.Join(dir, "f.bin.001"), back, false) })
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(get(t, filepath.Join(back, "f.bin")), want) {
				t.Fatal("the joined file is not the original")
			}
			if !strings.Contains(out, fmt.Sprintf("joined %d parts into ", c.parts)) || strings.Count(out, "\n") != 1 {
				t.Errorf("the finish line: %q", out)
			}
			if !reflect.DeepEqual(names(t, back), []string{"f.bin"}) {
				t.Fatalf("the folder holds %v", names(t, back))
			}
		})
	}
}

func TestSplitInto(t *testing.T) {
	dir := setup(t)
	want := data(1, 2500)
	file := filepath.Join(dir, "f.bin")
	put(t, file, want)
	usb := filepath.Join(dir, "usb", "deep")
	out, err := said(t, func() error { return Split(context.Background(), file, 1000, "1KB", usb) })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names(t, usb), []string{"f.bin.001", "f.bin.002", "f.bin.003"}) {
		t.Fatalf("the folder holds %v", names(t, usb))
	}
	if !strings.Contains(out, "f.bin.001 to f.bin.003") {
		t.Errorf("parts written elsewhere should be named: %q", out)
	}
	if !reflect.DeepEqual(names(t, dir), []string{"f.bin", "usb"}) {
		t.Fatalf("nothing belongs beside the file: %v", names(t, dir))
	}

	// A second run does not touch the first set.
	if err := Split(context.Background(), file, 2000, "2KB", usb); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names(t, usb), []string{"f (1).bin.001", "f (1).bin.002", "f.bin.001", "f.bin.002", "f.bin.003"}) {
		t.Fatalf("the folder holds %v", names(t, usb))
	}
	if err := Join(context.Background(), filepath.Join(usb, "f (1).bin.002"), "", false); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(get(t, filepath.Join(usb, "f (1).bin")), want) {
		t.Fatal("the second set does not join into the file")
	}
}

func TestNothingToSplit(t *testing.T) {
	dir := setup(t)
	for name, n := range map[string]int{"small.bin": 1000, "exact.bin": 1001, "empty.bin": 0} {
		put(t, filepath.Join(dir, name), data(1, n))
		out, err := said(t, func() error { return Split(context.Background(), filepath.Join(dir, name), 1001, "1001B", "") })
		if err != nil || !strings.Contains(out, "is not larger than 1001B, nothing to split") {
			t.Fatalf("%s: %q %v", name, out, err)
		}
	}
	if len(names(t, dir)) != 3 {
		t.Fatalf("nothing should be written: %v", names(t, dir))
	}

	put(t, filepath.Join(dir, "big.bin"), data(1, 20000))
	err := Split(context.Background(), filepath.Join(dir, "big.bin"), 1, "1B", "")
	if errs.Code(err) != 2 || !strings.Contains(err.Error(), "1B parts would make 20000 files; use a larger size") {
		t.Fatalf("too many parts: %v", err)
	}
	err = Split(context.Background(), dir, 1000, "1KB", "")
	if errs.Code(err) != 2 || !strings.Contains(err.Error(), "is not a file") {
		t.Fatalf("a folder: %v", err)
	}
	if err := Split(context.Background(), filepath.Join(dir, "missing"), 1000, "1KB", ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing file: %v", err)
	}
	if len(names(t, dir)) != 4 {
		t.Fatalf("nothing should be written: %v", names(t, dir))
	}
}

// sequences builds the names other tools give their parts, without using
// the code under test.
func numbered(format string, from, n int) []string {
	var out []string
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf(format, from+i))
	}
	return out
}

func lettered(prefix string, n int) []string {
	var out []string
	for a := 'a'; a <= 'y'; a++ { // split never starts a two-letter suffix with z
		for b := 'a'; b <= 'z'; b++ {
			out = append(out, prefix+string(a)+string(b))
		}
	}
	for i := 0; len(out) < n; i++ {
		out = append(out, fmt.Sprintf("%sz%c%c%c", prefix, 'a'+i/676, 'a'+i/26%26, 'a'+i%26))
	}
	return out[:n]
}

func TestJoinOtherTools(t *testing.T) {
	for _, c := range []struct {
		what  string
		parts []string
		ask   string // the name handed to join
		want  string // what the joined file is called
	}{
		{"7-Zip, any part", numbered("movie.mkv.%03d", 1, 5), "movie.mkv.003", "movie.mkv"},
		{"7-Zip, one part", numbered("movie.mkv.%03d", 1, 1), "movie.mkv.001", "movie.mkv"},
		{"the name without a counter", numbered("movie.mkv.%03d", 1, 5), "movie.mkv", "movie.mkv"},
		{"counting from zero", numbered("disk.img.%03d", 0, 4), "disk.img.000", "disk.img"},
		{"two digits", numbered("a.bin.%02d", 1, 12), "a.bin.12", "a.bin"},
		{"one digit, past nine", numbered("a.bin.%d", 1, 11), "a.bin.1", "a.bin"},
		{"split", lettered("x", 5), "xaa", "x"},
		{"split, with a prefix", lettered("backup.tar.gz.", 4), "backup.tar.gz.ac", "backup.tar.gz"},
		{"split, a prefix that ends in letters", lettered("dataset", 3), "datasetab", "dataset"},
		{"split, past yz", lettered("x", 653), "xzaab", "x"},
		{"split -d", numbered("x%02d", 0, 13), "x05", "x"},
		{"split -d, past 89", append(numbered("x%02d", 0, 90), numbered("x%d", 9000, 5)...), "x9003", "x"},
		{"part and a number", numbered("photo.part%d", 1, 3), "photo.part2", "photo"},
		{"part and two digits", numbered("photo.part%02d", 1, 3), "photo.part01", "photo"},
	} {
		t.Run(c.what, func(t *testing.T) {
			dir := setup(t)
			want := data(2, 10*len(c.parts)-3)
			spread(t, dir, want, 10, c.parts)
			put(t, filepath.Join(dir, "unrelated.txt"), []byte("not a part"))
			put(t, filepath.Join(dir, "xylophone"), []byte("not a part either"))
			out := filepath.Join(dir, "out")
			if err := Join(context.Background(), filepath.Join(dir, c.ask), out, false); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(names(t, out), []string{c.want}) {
				t.Fatalf("joined into %v, want %s", names(t, out), c.want)
			}
			if !bytes.Equal(get(t, filepath.Join(out, c.want)), want) {
				t.Fatal("the joined file is not the original")
			}
		})
	}
}

func refused(t *testing.T, err error, reason string) {
	t.Helper()
	if errs.Code(err) != 2 || !errors.As(err, new(*errs.Archive)) || !strings.Contains(err.Error(), reason) {
		t.Fatalf("want a refusal saying %q, got %v", reason, err)
	}
}

func TestJoinRefusals(t *testing.T) {
	dir := setup(t)
	ctx := context.Background()
	want := data(3, 47)
	all := numbered("movie.mkv.%03d", 1, 5)
	fresh := func() string {
		d := t.TempDir()
		spread(t, d, want, 10, all)
		return d
	}
	nothing := func(d string, n int) {
		t.Helper()
		if len(names(t, d)) != n {
			t.Fatalf("a refused join wrote something: %v", names(t, d))
		}
	}

	d := fresh()
	os.Remove(filepath.Join(d, "movie.mkv.003"))
	for _, ask := range []string{"movie.mkv.001", "movie.mkv.005", "movie.mkv"} {
		err := Join(ctx, filepath.Join(d, ask), "", false)
		refused(t, err, filepath.Join(d, "movie.mkv.003")+" is missing, nothing joined")
	}
	refused(t, Join(ctx, filepath.Join(d, "movie.mkv.001"), "", true), "is missing") // force is for sizes only
	nothing(d, 4)

	d = fresh()
	os.Remove(filepath.Join(d, "movie.mkv.001"))
	refused(t, Join(ctx, filepath.Join(d, "movie.mkv.002"), "", false), filepath.Join(d, "movie.mkv.001")+" is missing, nothing joined")
	nothing(d, 4)

	// A part cut short, as a download that stopped would leave it.
	d = fresh()
	put(t, filepath.Join(d, "movie.mkv.002"), want[10:15])
	refused(t, Join(ctx, filepath.Join(d, "movie.mkv.001"), "", false), filepath.Join(d, "movie.mkv.002")+" is not the same size as the other parts, nothing joined; add force to join anyway")
	nothing(d, 5)
	if err := Join(ctx, filepath.Join(d, "movie.mkv.001"), "", true); err != nil {
		t.Fatalf("force: %v", err)
	}
	if len(get(t, filepath.Join(d, "movie.mkv"))) != len(want)-5 {
		t.Fatal("force should join the parts as they are")
	}

	d = fresh()
	put(t, filepath.Join(d, "movie.mkv.005"), want[:11]) // a last part longer than the others
	refused(t, Join(ctx, filepath.Join(d, "movie.mkv.001"), "", false), "movie.mkv.005 is not the same size")

	put(t, filepath.Join(dir, "notes.txt"), []byte("notes"))
	put(t, filepath.Join(dir, "report.2024"), []byte("report"))
	put(t, filepath.Join(dir, "take1"), []byte("one"))
	put(t, filepath.Join(dir, "take2"), []byte("two"))
	put(t, filepath.Join(dir, "xaa"), []byte("alone"))
	for _, name := range []string{"notes.txt", "report.2024", "take1", "take2", "xaa"} {
		refused(t, Join(ctx, filepath.Join(dir, name), "", false), filepath.Join(dir, name)+" is not part of a split file")
	}
	for _, name := range []string{"movie.part1.rar", "movie.r00", "movie.z01", "MOVIE.PART02.RAR"} {
		put(t, filepath.Join(dir, name), []byte("volume"))
		refused(t, Join(ctx, filepath.Join(dir, name), "", false), filepath.Join(dir, name)+" is an archive volume, not a split file")
	}
	if err := Join(ctx, filepath.Join(dir, "missing.001"), "", false); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing file: %v", err)
	}
	nothing(dir, 9)
}

func TestJoinNeverOverwrites(t *testing.T) {
	dir := setup(t)
	want := data(4, 25)
	spread(t, dir, want, 10, numbered("f.bin.%03d", 1, 3))
	put(t, filepath.Join(dir, "f.bin"), []byte("already here"))
	if err := Join(context.Background(), filepath.Join(dir, "f.bin"), "", false); err != nil {
		t.Fatal(err)
	}
	if string(get(t, filepath.Join(dir, "f.bin"))) != "already here" {
		t.Fatal("an existing file was overwritten")
	}
	if !bytes.Equal(get(t, filepath.Join(dir, "f (1).bin")), want) {
		t.Fatal("the joined file should be numbered")
	}
	if len(names(t, dir)) != 5 {
		t.Fatalf("the parts are kept and nothing else is left: %v", names(t, dir))
	}
}

func TestCancel(t *testing.T) {
	dir := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	file := filepath.Join(dir, "f.bin")
	put(t, file, data(5, 5000))
	if err := Split(ctx, file, 1000, "1KB", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled, got %v", err)
	}
	if len(names(t, dir)) != 1 {
		t.Fatalf("an interrupted split left files behind: %v", names(t, dir))
	}

	parts := filepath.Join(dir, "parts")
	spread(t, parts, data(5, 45), 10, numbered("f.bin.%03d", 1, 5))
	if err := Join(ctx, filepath.Join(parts, "f.bin.001"), "", false); !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled, got %v", err)
	}
	if len(names(t, parts)) != 5 {
		t.Fatalf("an interrupted join left files behind: %v", names(t, parts))
	}
}

func TestJoinPiped(t *testing.T) {
	dir := setup(t)
	piped := false
	term.StdoutIsTTY = &piped
	want := data(7, 9<<20)
	spread(t, dir, want, 4<<20, numbered("f.bin.%03d", 1, 3))
	capture, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = capture
	err = Join(context.Background(), filepath.Join(dir, "f.bin.002"), "", false)
	os.Stdout = stdout
	capture.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(get(t, capture.Name()), want) {
		t.Fatal("the file should go to stdout, in order")
	}
	if len(names(t, dir)) != 3 {
		t.Fatalf("piping should save nothing: %v", names(t, dir))
	}
}

func TestNext(t *testing.T) {
	for _, c := range []struct {
		s, alphabet string
		want        []string
	}{
		{"001", digits, []string{"002"}},
		{"009", digits, []string{"010"}},
		{"999", digits, []string{"1000"}},
		{"9", digits, []string{"10"}},
		{"88", digits, []string{"89"}},
		{"89", digits, []string{"90", "9000"}},
		{"9000", digits, []string{"9001"}},
		{"9899", digits, []string{"9900", "990000"}},
		{"aa", letters, []string{"ab"}},
		{"az", letters, []string{"ba"}},
		{"yz", letters, []string{"za", "zaaa"}},
		{"zaaa", letters, []string{"zaab"}},
		{"zyzz", letters, []string{"zzaa", "zzaaaa"}},
		{"zz", letters, nil},
	} {
		if got := next(c.s, c.alphabet); !reflect.DeepEqual(got, c.want) {
			t.Errorf("next(%q) = %q, want %q", c.s, got, c.want)
		}
	}
}
