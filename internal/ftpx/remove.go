package ftpx

import (
	"context"
	"path"

	"gett/internal/errs"
	"gett/internal/term"

	"github.com/jlaffaye/ftp"
)

// Remove deletes the file at rawURL, or with a trailing slash the folder and
// everything in it, naming each path as it goes.
func Remove(ctx context.Context, rawURL string) error {
	srv, p, folder, err := parse(rawURL)
	if err != nil {
		return err
	}
	if folder && (p == "." || p == "/") {
		return &errs.Usage{Msg: "refusing to delete the whole server; name the folder, like ftp://host/dir/"}
	}
	conn, err := srv.dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Quit()
	if !folder {
		e, err := stat(conn, p)
		if err != nil {
			return err
		}
		if e.Type == ftp.EntryTypeFolder {
			return &errs.Usage{Msg: p + " is a folder; add a trailing slash to delete it and everything in it"}
		}
		if err := conn.Delete(p); err != nil {
			return err
		}
		term.Info("deleted %s", p)
		return nil
	}
	var files, folders int
	if err := removeTree(ctx, conn, p, &files, &folders); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	term.Info("deleted %d files, %d folders", files, folders)
	return nil
}

// removeTree deletes everything under dir, then dir itself.
func removeTree(ctx context.Context, c *ftp.ServerConn, dir string, files, folders *int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := c.List(dir)
	if err != nil {
		return err
	}
	for _, e := range children(dir, entries) {
		p := path.Join(dir, e.Name)
		if e.Type == ftp.EntryTypeFolder {
			if err := removeTree(ctx, c, p, files, folders); err != nil {
				return err
			}
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.Delete(p); err != nil {
			return err
		}
		term.Info("deleted %s", p)
		*files++
	}
	if err := c.RemoveDir(dir); err != nil {
		return err
	}
	term.Info("deleted %s/", dir)
	*folders++
	return nil
}
