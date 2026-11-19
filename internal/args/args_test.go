package args

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestShapes(t *testing.T) {
	dir := t.TempDir()
	cookies := filepath.Join(dir, "cookies.txt")
	os.WriteFile(cookies, []byte("# Netscape HTTP Cookie File\n.example.com\tTRUE\t/\tFALSE\t0\tsid\tabc\n"), 0o644)
	notCookies := filepath.Join(dir, "video.mp4")
	os.WriteFile(notCookies, []byte("\x00\x00\x00\x18ftypmp42"), 0o644)

	o, err := Parse(Send, []string{"POST", `{"a":1}`, "Accept: application/json", "X-Token:abc", "30s", "verbose", cookies, "name=value", "photo=@pic.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	if o.Method != "POST" || o.JSON != `{"a":1}` || o.Timeout != 30*time.Second || !o.Verbose || o.CookieFile != cookies {
		t.Fatalf("bad parse: %+v", o)
	}
	if o.Headers.Get("Accept") != "application/json" || o.Headers.Get("X-Token") != "abc" {
		t.Fatalf("headers: %v", o.Headers)
	}
	if len(o.Form) != 1 || o.Form[0] != (KV{"name", "value"}) || len(o.Files) != 1 || o.Files[0] != (KV{"photo", "pic.jpg"}) {
		t.Fatalf("form/files: %+v %+v", o.Form, o.Files)
	}

	o, err = Parse(Download, []string{notCookies, "silent", "5m"})
	if err != nil {
		t.Fatal(err)
	}
	if o.Name != notCookies || !o.Silent || o.Timeout != 5*time.Minute || o.CookieFile != "" {
		t.Fatalf("bad download parse: %+v", o)
	}

	o, err = Parse(Download, []string{"README"})
	if err != nil || o.Name != "README" || o.Method != "" {
		t.Fatalf("caps word in download mode should be a name: %+v %v", o, err)
	}

	if _, err := Parse(Download, []string{"a", "b"}); err == nil {
		t.Fatal("two names should fail")
	}

	// "extract" takes the word after it as the destination; the name goes before.
	o, err = Parse(Download, []string{"extract"})
	if err != nil || !o.Extract || o.ExtractTo != "" || o.Name != "" {
		t.Fatalf("extract alone: %+v %v", o, err)
	}
	o, err = Parse(Download, []string{"src.zip", "extract", "out", "verbose"})
	if err != nil || !o.Extract || o.ExtractTo != "out" || o.Name != "src.zip" || !o.Verbose {
		t.Fatalf("name, extract, dir: %+v %v", o, err)
	}
	o, err = Parse(Download, []string{"extract", "verbose", "src.zip"})
	if err != nil || o.ExtractTo != "" || o.Name != "src.zip" || !o.Verbose {
		t.Fatalf("a reserved word after extract is not a folder: %+v %v", o, err)
	}
	if _, err := Parse(FTP, []string{"extract"}); err == nil {
		t.Fatal("extract means nothing outside downloads")
	}
	if _, err := Parse(Send, []string{"whatever"}); err == nil {
		t.Fatal("unknown word in send mode should fail")
	}
	if _, err := Parse(FTP, []string{"30"}); err == nil {
		t.Fatal("bare number is not a timeout")
	}
}

func TestTransferWords(t *testing.T) {
	dir := t.TempDir()
	photos := filepath.Join(dir, "photos")
	os.Mkdir(photos, 0o755)
	report := filepath.Join(dir, "report.pdf")
	os.WriteFile(report, []byte("%PDF"), 0o644)

	o, err := Parse(Transfer, []string{photos, report, "tiger", "verbose", "30s"})
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Paths) != 2 || o.Paths[0] != photos || o.Paths[1] != report || o.Password != "tiger" || !o.Verbose || o.Timeout != 30*time.Second {
		t.Fatalf("share parse: %+v", o)
	}

	o, err = Parse(Transfer, []string{"1", "2.3", "1.5", "keep/"})
	if err != nil || len(o.Items) != 3 || o.Items[2] != "1.5" || len(o.Paths) != 1 || o.Paths[0] != "keep/" || o.Password != "" {
		t.Fatalf("pull parse: %+v %v", o, err)
	}

	o, err = Parse(Transfer, []string{"ls", "1.2"})
	if err != nil || !o.Ls || len(o.Items) != 1 || o.Items[0] != "1.2" {
		t.Fatalf("ls parse: %+v %v", o, err)
	}

	o, err = Parse(Transfer, []string{"photoz"})
	if err != nil || o.Password != "photoz" || len(o.Paths) != 0 {
		t.Fatalf("a word that is not a path is a password: %+v %v", o, err)
	}
	o, err = Parse(Transfer, []string{"photoz/"})
	if err != nil || len(o.Paths) != 1 || o.Password != "" {
		t.Fatalf("a trailing slash makes a path: %+v %v", o, err)
	}
	o, err = Parse(Transfer, []string{"a:b"})
	if err != nil || o.Password != "a:b" || len(o.Headers) != 0 {
		t.Fatalf("a header shape is a password in transfer mode: %+v %v", o, err)
	}
	if _, err := Parse(Transfer, []string{"tiger", "lion"}); err == nil {
		t.Fatal("two passwords should fail")
	}

	o, err = Parse(Transfer, []string{"1.4", "extract", "out", "scan"})
	if err != nil || !o.Extract || o.ExtractTo != "out" || !o.Scan || len(o.Items) != 1 {
		t.Fatalf("download words after a pull: %+v %v", o, err)
	}
	if _, err := Parse(FTP, []string{"extract"}); err == nil {
		t.Fatal("extract still means nothing in ftp mode")
	}
}

func TestForce(t *testing.T) {
	o, err := Parse(Download, []string{"force", "movie.mp4"})
	if err != nil || !o.Force || o.Name != "movie.mp4" {
		t.Fatalf("download: %+v, %v", o, err)
	}
	for _, m := range []Mode{Send, FTP} {
		if _, err := Parse(m, []string{"force"}); err == nil {
			t.Fatalf("mode %d accepted force", m)
		}
	}
}

func TestCompressWords(t *testing.T) {
	o, err := Parse(Compress, []string{"photos/", "report.pdf", "backup.xgett", "tiger", "verbose", "30s"})
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Paths) != 2 || o.Paths[0] != "photos/" || o.Paths[1] != "report.pdf" || o.Name != "backup.xgett" || o.Password != "tiger" || !o.Verbose || o.Timeout != 30*time.Second {
		t.Fatalf("inputs, name, password, words: %+v", o)
	}
	o, err = Parse(Compress, []string{"photos/", "backup.xgett"})
	if err != nil || len(o.Paths) != 1 || o.Name != "backup.xgett" || o.Password != "" {
		t.Fatalf("no password: %+v %v", o, err)
	}
	if _, err := Parse(Compress, []string{"photos/", "tiger"}); err == nil || err.Error() != CompressUsage {
		t.Fatalf("the .xgett name is required: %v", err)
	}
	if _, err := Parse(Compress, nil); err == nil {
		t.Fatal("nothing at all should fail")
	}

	// Position decides: before the name everything is something to pack,
	// even a word gett knows or another .xgett.
	o, err = Parse(Compress, []string{"old.xgett", "verbose", "5m", "all.XGETT"})
	if err != nil || len(o.Paths) != 3 || o.Name != "all.XGETT" || o.Verbose || o.Timeout != 0 {
		t.Fatalf("words before the name are inputs: %+v %v", o, err)
	}
	o, err = Parse(Compress, []string{"b.xgett"})
	if err != nil || len(o.Paths) != 0 || o.Name != "b.xgett" {
		t.Fatalf("a name alone parses, with nothing to pack: %+v %v", o, err)
	}

	// After the name any word is the password, whatever it looks like.
	for _, word := range []string{"a:b", "force", "extract", "scan", "photos/", "README"} {
		o, err = Parse(Compress, []string{"a", "b.xgett", word})
		if err != nil || o.Password != word || len(o.Headers) != 0 || o.Extract || o.Scan || o.Force {
			t.Fatalf("%q after the name is the password: %+v %v", word, o, err)
		}
	}
	if _, err := Parse(Compress, []string{"a", "b.xgett", "tiger", "lion"}); err == nil {
		t.Fatal("two passwords should fail")
	}
}

func TestUnpackWords(t *testing.T) {
	o, err := Parse(Unpack, []string{"out/", "tiger", "silent", "30s"})
	if err != nil || len(o.Paths) != 2 || o.Paths[0] != "out/" || o.Paths[1] != "tiger" || !o.Silent || o.Timeout != 30*time.Second {
		t.Fatalf("folder, key, words: %+v %v", o, err)
	}
	o, err = Parse(Unpack, []string{"extract", "a:b", "force"})
	if err != nil || len(o.Paths) != 3 || o.Extract || o.Force || len(o.Headers) != 0 {
		t.Fatalf("every bare word is kept as typed: %+v %v", o, err)
	}
	o, err = Parse(Unpack, nil)
	if err != nil || len(o.Paths) != 0 {
		t.Fatalf("no words: %+v %v", o, err)
	}
}
