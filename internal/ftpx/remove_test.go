package ftpx

import (
	"context"
	"testing"

	"gett/internal/errs"
)

func TestRemoveFile(t *testing.T) {
	d, addr := startServer(t)
	seed(t, d)
	if err := Remove(context.Background(), "ftp://u:p@"+addr+"/up/a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.fs.Stat("/up/a.txt"); err == nil {
		t.Fatal("file still there")
	}
	if _, err := d.fs.Stat("/up/sub/b.bin"); err != nil {
		t.Fatal("another file went missing")
	}
}

func TestRemoveFolderRecursive(t *testing.T) {
	d, addr := startServer(t)
	seed(t, d)
	if err := Remove(context.Background(), "ftp://u:p@"+addr+"/up/sub/"); err != nil {
		t.Fatal(err)
	}
	// The in-memory server forgets a removed folder but keeps its children
	// unless they were deleted one by one, so check every path.
	for _, p := range []string{"/up/sub/deeper/c.empty", "/up/sub/deeper", "/up/sub/b.bin", "/up/sub"} {
		if _, err := d.fs.Stat(p); err == nil {
			t.Fatalf("%s still there", p)
		}
	}
	if _, err := d.fs.Stat("/up/a.txt"); err != nil {
		t.Fatal("a file outside the folder went missing")
	}
}

func TestRemoveFolderWithoutSlashRefused(t *testing.T) {
	d, addr := startServer(t)
	seed(t, d)
	err := Remove(context.Background(), "ftp://u:p@"+addr+"/up/sub")
	if errs.Code(err) != 2 {
		t.Fatalf("want exit 2, got %d (%v)", errs.Code(err), err)
	}
	if _, err := d.fs.Stat("/up/sub/b.bin"); err != nil {
		t.Fatal("folder content was deleted")
	}
}

func TestRemoveRootRefused(t *testing.T) {
	d, addr := startServer(t)
	seed(t, d)
	for _, u := range []string{"ftp://u:p@" + addr + "/", "ftp://u:p@" + addr} {
		if err := Remove(context.Background(), u); errs.Code(err) != 2 {
			t.Fatalf("%s: want exit 2, got %d (%v)", u, errs.Code(err), err)
		}
	}
	if _, err := d.fs.Stat("/up/a.txt"); err != nil {
		t.Fatal("content was deleted")
	}
}
