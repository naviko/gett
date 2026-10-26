package ftpx

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestList(t *testing.T) {
	d, addr := startServer(t)
	seed(t, d)
	url := "ftp://u:p@" + addr

	var out bytes.Buffer
	if err := List(context.Background(), url+"/up/", &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %q", out.String())
	}
	// Folders first, with a slash; then files with their size.
	if !strings.HasSuffix(lines[0], "empty/") || !strings.HasSuffix(lines[1], "sub/") {
		t.Fatalf("folders not listed first: %q", out.String())
	}
	if !strings.Contains(lines[2], "5 B") || !strings.HasSuffix(lines[2], "a.txt") {
		t.Fatalf("file line wrong: %q", lines[2])
	}
	if strings.Contains(out.String(), "..") {
		t.Fatalf("dot entries leaked: %q", out.String())
	}

	// A file lists as itself.
	out.Reset()
	if err := List(context.Background(), url+"/up/sub/b.bin", &out); err != nil {
		t.Fatal(err)
	}
	if l := strings.TrimSpace(out.String()); !strings.HasSuffix(l, "b.bin") || !strings.Contains(l, "2.0 MiB") || strings.Contains(l, "\n") {
		t.Fatalf("file listing wrong: %q", out.String())
	}

	// An empty folder prints nothing.
	out.Reset()
	if err := List(context.Background(), url+"/up/empty/", &out); err != nil || out.Len() != 0 {
		t.Fatalf("empty folder: %q, %v", out.String(), err)
	}

	// Something that is not there.
	if err := List(context.Background(), url+"/nope/", &out); err == nil {
		t.Fatal("listing a missing folder succeeded")
	}
}
