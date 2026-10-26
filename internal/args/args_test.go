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
