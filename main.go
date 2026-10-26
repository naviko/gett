// gett downloads, uploads and sends requests. No flags.
package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"gett/internal/args"
	"gett/internal/coin"
	"gett/internal/download"
	"gett/internal/errs"
	"gett/internal/extract"
	"gett/internal/ftpx"
	"gett/internal/httpx"
	"gett/internal/ipfs"
	"gett/internal/netx"
	"gett/internal/sandbox"
	"gett/internal/send"
	"gett/internal/term"
	"gett/internal/torrentdl"
	"gett/internal/transfer"

	"github.com/spf13/cobra"
)

// version comes from version.txt, which build.sh bumps on each release.
//
//go:embed version.txt
var versionFile string

var version = strings.TrimSpace(versionFile)

const help = `gett %s

Download
  gett https://example.com/video.mp4                        download
  gett https://example.com/video.mp4 movie.mp4              save as movie.mp4
  gett https://example.com/video.mp4 movies/                save into movies/
  gett https://example.com/report.pdf | less                piped: streams, saves nothing
  gett https://user:pass@example.com/private.zip            with a login
  gett "magnet:?xt=urn:btih:..."                            torrent (experimental)
  gett https://example.com/linux.torrent movies/            from a .torrent file or URL
  gett ipfs://bafy.../photos/cat.jpg                        IPFS (experimental)

Extract
  gett https://example.com/src.tar.gz extract               download, then unpack into src/
  gett https://example.com/game.zip extract games/          unpack into games/
  gett extract src.tar.gz                                   unpack a file, into src/
  gett extract game.zip games/                              into games/
  gett extract app.log.gz                                   a .gz or .xz: gives app.log
  gett extract app.log.gz | grep ERROR                      or stream it

Sandbox (experimental)
  gett https://example.com/tool.tar.gz scan                 unpack into a sandbox, scan, report, dispose
  gett https://example.com/tool.tar.gz sandbox              same, keep the sandbox: ssh tool
  gett sandbox tool.tar.gz                                  put a local file or folder into a sandbox, scan, keep the sandbox
  gett sandbox                                              list sandboxes
  gett sandbox rm tool                                      delete a sandbox
  gett sandbox reset                                        delete all sandboxes, reset to initial state

FTP
  gett ftp://user:pass@host/backups/report.pdf              download a file
  gett ftp://user:pass@host/backups/                        download a folder into backups/
  gett ftp upload ftp://user:pass@host/backups ./photos     upload a folder
  gett ftp upload ftp://user:pass@host/backups report.pdf   upload a file
  gett ftp ls ftp://host/pub/                               list a folder
  gett ftp rm ftp://host/old/2019/                          delete a folder and its contents

Transfer (experimental)
  gett transfer photos/                                     share a folder on the local network, Ctrl-C ends it
  gett transfer photos/ videos/ report.pdf                  share several
  gett transfer photos/ tiger                               with a password
  gett transfer                                             list what is shared, numbered
  gett transfer tiger                                       also the shares that password unlocks
  gett transfer ls 1.2                                      list inside an item
  gett transfer 1 <folder>/                                 download item 1 into <folder>/
  gett transfer 1.3 2.1 3                                   several items, into the current folder
  gett transfer 2.1 | sha256sum                             a file streams to stdout
  gett transfer 1.4 extract                                 unpack on arrival; also scan, sandbox

Request
  gett send https://api.example.com/users                   GET
  gett send https://api.example.com/users POST '{"a":1}'    JSON body
  gett send https://api.example.com/users/1 PUT name=Ada    form field
  gett send https://api.example.com/upload photo=@cat.jpg   file upload
  gett send https://api.example.com/import @data.xml        body from a file
  gett send https://api.example.com/users/1 DELETE          any method
  gett send login.json                                      saved request(s)

Net (experimental)
  gett net ip                                               addresses, gateway, dns servers, public ip
  gett net dns example.com                                  dns records
  gett net ping example.com                                 ping
  gett net trace example.com                                traceroute
  gett net port example.com 443                             is the port open
  gett net tls example.com                                  certificate details
  gett net whois example.com                                registry record
  gett net speed                                            download and upload speed

Coin (experimental)
  gett coin btc 1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa          address: balance, transactions
  gett coin btc 4a5e1e4baab89f3a32518a88c31bc87f...         transaction: status, amount, fee
  gett coin eth 0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045  also ltc doge dash bch sol trx

Other examples
  gett https://api.example.com/me "X-Token: abc"            header
  gett https://example.com/file cookies.txt                 cookie file (Netscape format)
  gett https://example.com/file 5m                          give up after 5 minutes
  gett https://example.com/file verbose                     show everything
  gett https://example.com/file silent                      only errors
  gett https://example.com/a.zip extract 5m silent          several at once

Defaults, nothing to set
  connections    up to 16 in parallel when the server allows ranges
  resume         automatic: run the same command again
  file name      from the server or the URL, extension kept or detected
  overwrite      never, "name (1).ext" instead
  redirects      followed, including HTML forwarders; expired links are renewed
  stdout         download streams to stdout when piped
  timeout        none; 30s to connect, 30s without data
  retries        3 with backoff, 429 and 5xx retried
  proxy          HTTP_PROXY / HTTPS_PROXY / NO_PROXY
  TLS            verified; ftp:// uses TLS when offered, ftps:// always
  ftp            up to 8 connections, resumes, skips files already there
  transfer       local network only, up to 16 connections, resumes, never overwrites
  extract        every core for zip, never overwrites, archive kept
  sandbox        ubuntu, 1 GiB, 2 cpus, internet yes, your machine no
  net            ping 4 times, trace 30 hops, 3s to connect
  torrent        stops when complete, never seeds
  ipfs           public gateways, override with IPFS_GATEWAY=https://a,https://b
  coin           public explorers, summary on a terminal, raw JSON when piped
  send method    GET, or POST when a body is given
  output         summary on a terminal, raw body when piped
  exit codes     same as curl

  gett version   print the version
`

func main() {
	httpx.Version = version
	root := &cobra.Command{
		Use:                   "gett URL [name] [...]",
		Args:                  cobra.ArbitraryArgs,
		SilenceUsage:          true,
		SilenceErrors:         true,
		DisableFlagsInUseLine: true,
		RunE:                  runDownload,
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.Version = version
	root.SetVersionTemplate("gett {{.Version}}\n")
	root.SetHelpFunc(func(*cobra.Command, []string) { fmt.Printf(help, version) })
	root.AddCommand(
		&cobra.Command{Use: "ftp upload URL FILE|DIR | ls URL | rm URL [...]", Args: cobra.MinimumNArgs(1), DisableFlagsInUseLine: true, RunE: runFTP},
		&cobra.Command{Use: "extract FILE|URL [DIR] [...]", Args: cobra.MinimumNArgs(1), DisableFlagsInUseLine: true, RunE: runExtract},
		&cobra.Command{Use: "transfer [PATH...] [PASSWORD] | [NUMBER...] [DIR] | ls NUMBER [...]", Args: cobra.ArbitraryArgs, DisableFlagsInUseLine: true, RunE: runTransfer},
		&cobra.Command{Use: "sandbox [FILE|DIR|URL] | rm NAME... | reset", Args: cobra.ArbitraryArgs, DisableFlagsInUseLine: true, RunE: runSandbox},
		&cobra.Command{Use: "net ip | dns NAME | ping HOST | trace HOST | port HOST PORT | tls HOST | whois NAME | speed", Args: cobra.MinimumNArgs(1), DisableFlagsInUseLine: true, RunE: runNet},
		&cobra.Command{Use: "coin NAME ADDRESS|TXHASH [...]", Args: cobra.ArbitraryArgs, DisableFlagsInUseLine: true, RunE: runCoin},
		&cobra.Command{Use: "send URL|request.json [...]", Args: cobra.MinimumNArgs(1), DisableFlagsInUseLine: true, RunE: runSend},
		&cobra.Command{Use: "version", Run: func(*cobra.Command, []string) { fmt.Println("gett " + version) }},
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", term.Sanitize(errs.Message(err)))
		os.Exit(errs.Code(err))
	}
}

// setup parses the free arguments and applies the output mode.
func setup(cmd *cobra.Command, mode args.Mode, rest []string) (context.Context, *args.Opts, error) {
	o, err := args.Parse(mode, rest)
	if err != nil {
		return nil, nil, &errs.Usage{Msg: err.Error()}
	}
	term.Setup(o.Verbose, o.Silent)
	ctx := cmd.Context()
	if o.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.Timeout)
		go func() { <-ctx.Done(); cancel() }()
	}
	return ctx, o, nil
}

func runDownload(cmd *cobra.Command, a []string) error {
	if len(a) == 0 {
		return cmd.Help()
	}
	ctx, o, err := setup(cmd, args.Download, a[1:])
	if err != nil {
		return err
	}
	client, err := httpx.NewClient(httpx.Config{CookieFile: o.CookieFile, Retries: true})
	if err != nil {
		return err
	}
	saved, err := fetch(ctx, a[0], o, client)
	if err != nil {
		return err
	}
	if o.Sandbox || o.Scan {
		return box(ctx, saved, o, client)
	}
	if !o.Extract {
		return nil
	}
	return unpack(ctx, saved, o.ExtractTo)
}

// box is the "sandbox" or "scan" word after a download: the file goes into
// a sandbox, is unpacked and scanned there. The download succeeded, so
// nothing about the sandbox turns into an error.
func box(ctx context.Context, saved string, o *args.Opts, client *http.Client) error {
	if saved == "" {
		term.Info("nothing saved, nothing to scan")
		return nil
	}
	mode := sandbox.Throwaway
	if o.Sandbox {
		mode = sandbox.Keep
	}
	err := sandbox.Open(ctx, saved, mode, client)
	var se *errs.Sandbox
	switch {
	case errors.Is(err, sandbox.ErrNotInstalled):
		return nil
	case errors.As(err, &se):
		term.Info("%s", se.Msg)
		return nil
	}
	return err
}

// runSandbox handles "gett sandbox" (list), "gett sandbox FILE|DIR|URL",
// "gett sandbox rm NAME..." and "gett sandbox reset".
func runSandbox(cmd *cobra.Command, a []string) error {
	if len(a) == 0 {
		ctx, _, err := setup(cmd, args.FTP, nil)
		if err != nil {
			return err
		}
		return quiet(sandbox.List(ctx, os.Stdout))
	}
	switch strings.ToLower(a[0]) {
	case "rm":
		if len(a) < 2 {
			return &errs.Usage{Msg: "gett sandbox rm NAME [NAME...]"}
		}
		ctx, _, err := setup(cmd, args.FTP, nil)
		if err != nil {
			return err
		}
		return quiet(sandbox.Remove(ctx, a[1:]))
	case "reset":
		ctx, _, err := setup(cmd, args.FTP, a[1:])
		if err != nil {
			return err
		}
		return sandbox.Reset(ctx)
	}
	if strings.Contains(a[0], "://") || torrentdl.IsTorrent(a[0]) {
		return runDownload(cmd, append([]string{a[0], "sandbox"}, a[1:]...))
	}
	ctx, o, err := setup(cmd, args.FTP, a[1:])
	if err != nil {
		return err
	}
	client, err := httpx.NewClient(httpx.Config{CookieFile: o.CookieFile, Retries: true})
	if err != nil {
		return err
	}
	return quiet(sandbox.Open(ctx, a[0], sandbox.Keep, client))
}

// runNet handles "gett net <action> ...". Positional values come first,
// then words; a value is positional when it is not a word gett knows.
func runNet(cmd *cobra.Command, a []string) error {
	const usage = "gett net ip | dns NAME | ping HOST | trace HOST | port HOST PORT | tls HOST | whois NAME | speed"
	action := strings.ToLower(a[0])
	var pos []string
	rest := a[1:]
	for len(rest) > 0 && !isWord(rest[0]) {
		pos, rest = append(pos, rest[0]), rest[1:]
	}
	need := map[string]int{"ip": 0, "speed": 0, "dns": 1, "ping": 1, "trace": 1, "tls": 1, "whois": 1, "port": 2}
	n, ok := need[action]
	if !ok {
		return &errs.Usage{Msg: usage}
	}
	if len(pos) < n {
		return &errs.Usage{Msg: usage}
	}
	ctx, o, err := setup(cmd, args.FTP, rest)
	if err != nil {
		return err
	}
	client, err := httpx.NewClient(httpx.Config{CookieFile: o.CookieFile})
	if err != nil {
		return err
	}
	switch action {
	case "ip":
		return netx.IP(ctx, client)
	case "dns":
		return netx.DNS(ctx, pos[0], pos[1:])
	case "ping":
		count := 4
		if len(pos) > 1 {
			if count, err = strconv.Atoi(pos[1]); err != nil || count < 1 {
				return &errs.Usage{Msg: "gett net ping HOST [COUNT]"}
			}
		}
		return netx.Ping(ctx, pos[0], count)
	case "trace":
		return netx.Trace(ctx, pos[0])
	case "port":
		return netx.Port(ctx, pos[0], pos[1])
	case "tls":
		return netx.TLS(ctx, pos[0])
	case "whois":
		return netx.Whois(ctx, client, pos[0])
	}
	return netx.Speed(ctx, client)
}

// isWord reports an argument gett's parser already understands (verbose,
// a timeout, a header), as opposed to a positional value.
func isWord(s string) bool {
	_, err := args.Parse(args.FTP, []string{s})
	return err == nil
}

// quiet turns "microsandbox is not installed" into a clean exit: the install
// line was already printed.
func quiet(err error) error {
	if errors.Is(err, sandbox.ErrNotInstalled) {
		return nil
	}
	return err
}

// fetch downloads src by whatever it is and returns the saved path, empty
// when the data went to stdout.
func fetch(ctx context.Context, src string, o *args.Opts, client *http.Client) (string, error) {
	if isFTP(src) {
		return ftpx.Download(ctx, src, o.Name)
	}
	if torrentdl.IsTorrent(src) {
		return torrentdl.Run(ctx, torrentdl.Options{Source: src, Name: o.Name, Headers: o.Headers, HTTP: client})
	}
	url, key := src, ""
	if ipfs.IsIPFS(url) {
		var err error
		if url, key, err = ipfs.Resolve(ctx, url, client, o.Headers); err != nil {
			return "", err
		}
	}
	return download.Run(ctx, download.Options{URL: url, Name: o.Name, Key: key, Headers: o.Headers, Client: client})
}

// unpack is the "extract" word after a download. The download succeeded, so
// nothing about the archive itself turns into an error: gett says what it
// found and leaves the file as it is.
func unpack(ctx context.Context, saved, dest string) error {
	if saved == "" {
		term.Debug("nothing saved, extract skipped")
		return nil
	}
	if fi, err := os.Stat(saved); err == nil && fi.IsDir() {
		term.Info("extract applies to a single file, %s left as is", saved)
		return nil
	}
	err := extract.Run(ctx, saved, dest)
	var ae *errs.Archive
	if errors.As(err, &ae) {
		term.Info("%s", ae.Msg)
		return nil
	}
	return err
}

// runExtract handles "gett extract FILE [DIR]". Given a URL instead of a
// file, it is "gett URL extract [DIR]".
func runExtract(cmd *cobra.Command, a []string) error {
	if strings.Contains(a[0], "://") || torrentdl.IsTorrent(a[0]) {
		return runDownload(cmd, append([]string{a[0], "extract"}, a[1:]...))
	}
	// The words after the file parse as they would after "extract" on a
	// download: the first bare word is the folder, the rest are words.
	ctx, o, err := setup(cmd, args.Download, append([]string{"extract"}, a[1:]...))
	if err != nil {
		return err
	}
	if o.Name != "" {
		return &errs.Usage{Msg: fmt.Sprintf("don't know what %q means", o.Name)}
	}
	return extract.Run(ctx, a[0], o.ExtractTo)
}

func runCoin(cmd *cobra.Command, a []string) error {
	if len(a) < 2 || !coin.IsChain(a[0]) {
		return &errs.Usage{Msg: fmt.Sprintf("gett coin NAME ADDRESS or gett coin NAME TXHASH (coins: %s)", strings.Join(coin.Names(), " "))}
	}
	ctx, o, err := setup(cmd, args.FTP, a[2:])
	if err != nil {
		return err
	}
	client, err := httpx.NewClient(httpx.Config{CookieFile: o.CookieFile})
	if err != nil {
		return err
	}
	return coin.Run(ctx, a[0], a[1], client, o.Headers)
}

// runTransfer handles "gett transfer": share paths, list what is shared on
// the local network, list inside an item, or download items by number.
func runTransfer(cmd *cobra.Command, a []string) error {
	ctx, o, err := setup(cmd, args.Transfer, a)
	if err != nil {
		return err
	}
	switch {
	case o.Ls:
		if len(o.Items) != 1 || len(o.Paths) > 0 {
			return &errs.Usage{Msg: "gett transfer ls NUMBER"}
		}
		return transfer.Ls(ctx, o.Items[0], os.Stdout)
	case len(o.Items) > 0:
		if len(o.Paths) > 1 {
			return &errs.Usage{Msg: fmt.Sprintf("two destinations given: %q and %q", o.Paths[0], o.Paths[1])}
		}
		dest := ""
		if len(o.Paths) == 1 {
			dest = o.Paths[0]
		}
		saved, err := transfer.Pull(ctx, o.Items, dest, o.Password)
		if err != nil || !(o.Sandbox || o.Scan || o.Extract) {
			return err
		}
		client, err := httpx.NewClient(httpx.Config{Retries: true})
		if err != nil {
			return err
		}
		for _, s := range saved {
			if o.Sandbox || o.Scan {
				err = box(ctx, s, o, client)
			} else {
				err = unpack(ctx, s, o.ExtractTo)
			}
			if err != nil {
				return err
			}
		}
		return nil
	case len(o.Paths) > 0:
		if o.Sandbox || o.Scan || o.Extract {
			return &errs.Usage{Msg: "scan, sandbox and extract apply to a download"}
		}
		return transfer.Share(ctx, o.Paths, o.Password)
	}
	return transfer.List(ctx, o.Password, os.Stdout)
}

func isFTP(s string) bool {
	l := strings.ToLower(s)
	return strings.HasPrefix(l, "ftp://") || strings.HasPrefix(l, "ftps://")
}

// runFTP handles "gett ftp upload URL FILE|DIR", "gett ftp ls URL" and
// "gett ftp rm URL".
func runFTP(cmd *cobra.Command, a []string) error {
	const usage = "gett ftp upload URL FILE|DIR, gett ftp ls URL, gett ftp rm URL"
	word := strings.ToLower(a[0])
	switch word {
	case "upload":
		if len(a) < 3 {
			return &errs.Usage{Msg: "gett ftp upload ftp://host/dir FILE|DIR"}
		}
		ctx, _, err := setup(cmd, args.FTP, a[3:])
		if err != nil {
			return err
		}
		return ftpx.Upload(ctx, a[1], a[2])
	case "ls", "rm":
		if len(a) < 2 {
			return &errs.Usage{Msg: "gett ftp " + word + " ftp://host/path"}
		}
		ctx, _, err := setup(cmd, args.FTP, a[2:])
		if err != nil {
			return err
		}
		if word == "ls" {
			return ftpx.List(ctx, a[1], os.Stdout)
		}
		return ftpx.Remove(ctx, a[1])
	}
	return &errs.Usage{Msg: usage}
}

func runSend(cmd *cobra.Command, a []string) error {
	ctx, o, err := setup(cmd, args.Send, a[1:])
	if err != nil {
		return err
	}
	client, err := httpx.NewClient(httpx.Config{CookieFile: o.CookieFile})
	if err != nil {
		return err
	}
	var reqs []send.Request
	if fi, serr := os.Stat(a[0]); serr == nil && fi.Mode().IsRegular() && !strings.Contains(a[0], "://") {
		if reqs, err = send.Load(a[0]); err != nil {
			return err
		}
		for i := range reqs {
			merge(&reqs[i], o)
		}
	} else {
		reqs = []send.Request{{
			URL: a[0], Method: o.Method, Headers: o.Headers, JSON: o.JSON,
			Form: o.Form, Files: o.Files, BodyFile: o.BodyFile,
		}}
	}
	for i, r := range reqs {
		if i > 0 && term.StdoutTTY() {
			fmt.Println(strings.Repeat("─", 60))
		}
		if len(reqs) > 1 && term.StdoutTTY() {
			fmt.Printf("%s %s\n", or(r.Method, "GET"), r.URL)
		}
		if err := send.Do(ctx, client, r); err != nil {
			return err
		}
	}
	return nil
}

// merge lets words on the command line add to a stored request.
func merge(r *send.Request, o *args.Opts) {
	if o.Method != "" {
		r.Method = o.Method
	}
	for k, vs := range o.Headers {
		r.Headers[k] = vs
	}
	if o.JSON != "" {
		r.JSON = o.JSON
	}
	r.Form = append(r.Form, o.Form...)
	r.Files = append(r.Files, o.Files...)
	if o.BodyFile != "" {
		r.BodyFile = o.BodyFile
	}
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
