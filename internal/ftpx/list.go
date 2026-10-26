package ftpx

import (
	"context"
	"fmt"
	"io"
	"sort"

	"gett/internal/download"
	"gett/internal/term"

	"github.com/jlaffaye/ftp"
)

// List prints what is at rawURL: size, date and name, folders first and with
// a trailing slash, links with their target. Nothing else goes to w.
func List(ctx context.Context, rawURL string, w io.Writer) error {
	srv, p, folder, err := parse(rawURL)
	if err != nil {
		return err
	}
	conn, err := srv.dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Quit()
	entries, err := conn.List(p)
	if err != nil {
		if folder {
			return err
		}
		e, serr := stat(conn, p) // a file lists as itself
		if serr != nil || e.Type == ftp.EntryTypeFolder {
			return err
		}
		entries = []*ftp.Entry{e}
	} else {
		entries = children(p, entries)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		fi, fj := entries[i].Type == ftp.EntryTypeFolder, entries[j].Type == ftp.EntryTypeFolder
		if fi != fj {
			return fi
		}
		return entries[i].Name < entries[j].Name
	})
	for _, e := range entries {
		size, date, name := "", "", term.Sanitize(e.Name)
		if e.Type == ftp.EntryTypeFolder {
			name += "/"
		} else {
			size = download.Human(int64(e.Size))
		}
		if e.Type == ftp.EntryTypeLink && e.Target != "" {
			name += " -> " + term.Sanitize(e.Target)
		}
		if !e.Time.IsZero() {
			date = e.Time.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(w, "%10s  %16s  %s\n", size, date, name)
	}
	return nil
}
