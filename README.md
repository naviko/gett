# gett

Simple utility for downloading files, extracting archives, FTP, network tools, API calls using sandboxed environments with lightweight virus scanning to keep things safe. 

```
gett URL                      download (http, ftp, torrent, ipfs)
gett URL extract              download and unpack
gett extract FILE [DIR]       unpack zip, tar, 7z, rar, gz ...
gett URL scan                 unpack into a sandbox, scan, report results, dispose of sandbox
gett URL sandbox              same, keep the sandbox: ssh name
gett ftp upload URL FILE|DIR  upload
gett ftp ls URL               list
gett ftp rm URL               delete
gett transfer [DIR]           share or download on the local network (experimental)
gett net ping HOST            network tools
gett send URL                 request
```

## Install

```
brew install naviko/tap/gett
```

Or download the file for the system from
https://github.com/naviko/gett/releases/latest, rename it to `gett`
(`gett.exe` on Windows), and put it on the PATH.

```
gett-darwin-arm64        macOS, Apple Silicon
gett-darwin-amd64        macOS, Intel
gett-windows-amd64.exe   Windows 64-bit
gett-windows-386.exe     Windows 32-bit
gett-windows-arm64.exe   Windows on ARM
gett-linux-amd64         Linux 64-bit
gett-linux-arm64         Linux ARM
```

## Download

```
gett https://example.com/video.mp4                    download
gett https://example.com/video.mp4 movie.mp4          save as movie.mp4
gett https://example.com/video.mp4 movies/            save into movies/
gett https://example.com/report | less                piped: streams, saves nothing
gett https://user:pass@example.com/private.zip        with a login
gett https://example.com/file "Authorization: Bearer abc"
gett https://example.com/file cookies.txt             with a cookie file
gett https://example.com/src.tar.gz extract           download and unpack
gett ftp://user:pass@host/backups/report.pdf          from FTP
gett "magnet:?xt=urn:btih:..."                        torrent (experimental)
gett https://example.com/linux.torrent movies/        .torrent file or URL (experimental)
gett ipfs://bafy.../photos/cat.jpg                    IPFS (experimental)
```

Downloads use several connections. Running the same command again resumes an
interrupted download. Existing files are not overwritten; the new file is
named `video (1).mp4`. Redirects are followed and expired links are renewed.
The file name comes from the server or the URL; a missing extension is
detected from the content. Torrents stop when complete and do not seed. IPFS
downloads use public gateways; `IPFS_GATEWAY=https://a,https://b` selects
other gateways.

## FTP

```
gett ftp://host/pub/linux.iso                             download a file
gett ftp://host/pub/photos/                               download a folder into ./photos/
gett ftp://host/pub/README | less                         pipe a file
gett ftp://host:2121/pub/linux.iso                        another port
gett ftp://host//var/log/syslog                           absolute path (two slashes)

gett ftp upload ftp://user:pass@host/backups ./photos     upload a folder
gett ftp upload ftp://user:pass@host/backups report.pdf   upload a file
gett ftp upload ftps://user:pass@host/backups ./site      upload over TLS

gett ftp ls ftp://host/pub/                               list a folder
gett ftp rm ftp://host/backups/old.zip                    delete a file
gett ftp rm ftp://host/backups/2019/                      delete a folder and everything in it
```

The login is taken from the URL; without one, `anonymous` is used. TLS is
used when the server offers it, and always with `ftps://`. Uploads and folder
downloads transfer several files at once, skip files already present, and
resume partial files when the command is run again. Deleting a folder
requires the trailing `/`.

## Transfer (experimental)

```
gett transfer photos/                        share a folder on the local network, Ctrl-C ends it
gett transfer photos/ videos/ report.pdf     share several
gett transfer photos/ tiger                  with a password
gett transfer                                list what is shared, numbered
gett transfer tiger                          also the shares that password unlocks
gett transfer ls 1.2                         list inside an item
gett transfer 1 <folder>/                    download item 1 into <folder>/
gett transfer 1.3 2.1 3                      several items, into the current folder
gett transfer 2.1 | sha256sum                a file streams to stdout
gett transfer 1.4 extract                    unpack on arrival; also scan, sandbox
```

A listing shows every computer sharing on the local network, its items
numbered under a `shared by:` line, and ends with the commands to type next:

```
shared by: laptop
1      photos/                           2.3 GiB  312 files
1.1    2024/                           820.1 MiB  120 files
1.2    cover.jpg                         1.2 MiB
2      report.pdf                        4.1 MiB

ex: gett transfer 1                download item 1 into this folder
    gett transfer 1 <folder>/      into <folder>/
    gett transfer ls 1.1           list inside 1.1
```

Numbers mean the last list printed; `ls` with a number lists inside a
folder. With nothing on the network the listing says `no shares found`.
Piped, the listing is tab-separated with the sharer on every line. A
password word after the shared paths hides them from public listings; a
receiver that gave it once keeps it until the share is gone, and the
`shared by:` line shows it. A download keeps the folder tree, never
overwrites (`photos (1)`), skips what is already there, and resumes when
the same command is run again. The sharer prints one line when a download
starts and one when it ends, runs until Ctrl-C and keeps nothing on disk. Connections are
direct and ignore proxy settings; `verbose` shows addresses and ports, which
is what to open in a firewall.

## Extract

```
gett https://example.com/src.tar.gz extract              download, unpack into ./src/
gett https://example.com/game.zip extract games/         unpack into games/
gett https://example.com/game.zip g.zip extract          save as g.zip, unpack into ./g/
gett https://example.com/app.log.gz extract              one compressed file: gives app.log
gett extract src.tar.gz                                  unpack a local file, into ./src/
gett extract game.zip games/                             into games/
gett extract firmware.bin                                a zip in disguise opens anyway
gett extract app.log.gz | grep ERROR                     stream a compressed file
gett extract https://example.com/src.tar.gz              same as the first line
```

Supported formats: zip, 7z, rar, tar, tar.gz, tar.xz, tar.zst, tar.bz2, gz,
xz, zst, bz2. The format is detected from the file content. The result is
placed beside the archive in a folder named after it. An archive with a
single top-level folder is unpacked as that folder. Existing files are not
overwritten; the new folder is named `src (1)/`. The archive is kept. Entries
that would be written outside the folder are skipped. Zip archives are
unpacked on all cores. A file that is not an archive, is encrypted, or is
damaged is left unchanged.

## Sandbox (experimental)

```
gett https://example.com/tool.tar.gz scan           unpack into a sandbox, scan, report results, dispose of sandbox
gett https://example.com/tool.tar.gz sandbox        same, keep the sandbox: ssh tool
gett ftp://host/pub/photos/ sandbox                 download a folder into a sandbox, scan, keep the sandbox
gett sandbox tool.tar.gz                            put a local file or folder into a sandbox, scan, keep the sandbox
gett sandbox                                        list sandboxes
gett sandbox rm tool                                delete a sandbox
gett sandbox reset                                  delete all sandboxes, reset to initial state
```

Scanning uses YARA-X with the YARA Forge rules.

Requires [Microsandbox](https://microsandbox.dev). gett offers to install it
on first use.

## Request

```
gett send https://api.example.com/users                             GET
gett send https://api.example.com/users POST '{"name":"Ada"}'       JSON body
gett send https://api.example.com/users/1 PUT name=Ada              form field
gett send https://api.example.com/users/1 PUT name=Ada age=36       several fields
gett send https://api.example.com/upload photo=@cat.jpg             file upload
gett send https://api.example.com/upload photo=@cat.jpg title=Cat   file and fields
gett send https://api.example.com/upload front=@a.jpg back=@b.jpg   several files
gett send https://api.example.com/import @data.xml                  body from a file
gett send https://api.example.com/users/1 DELETE                    any method
gett send https://api.example.com/users "X-Token: abc" 30s          header, timeout
gett send https://api.example.com/users | jq '.[0]'                 piped: raw body
gett send login.json                                                saved request(s)
```

A request with a body is sent as POST unless a method is given. The method
word can go anywhere after the URL. A request has one body: JSON, form
fields, or a file. Fields alone are sent as
`application/x-www-form-urlencoded`, fields with a file upload as
`multipart/form-data`. Two bodies at once is an error. On a terminal, the
status, headers and formatted JSON are shown.

A saved request is a JSON file, or a list of requests to run in order. Words
on the command line apply to every request in the file. Secrets are read
from environment variables, `${VAR}` or `${VAR:-default}`:

```json
[
  {
    "method": "post",
    "url": "${API}/login",
    "headers": {"Authorization": "Bearer ${TOKEN}"},
    "query": {"lang": "en"},
    "json": {"user": "ada", "password": "${PASSWORD}"}
  },
  {
    "method": "post",
    "url": "${API}/upload",
    "auth": "ada:${PASSWORD}",
    "form": {"title": "Cat"},
    "files": {"photo": "cat.jpg"}
  },
  {
    "method": "put",
    "url": "${API}/import",
    "body": "<note>hello</note>"
  }
]
```

`auth` is `user:pass` for basic authentication. `json`, `form` with `files`,
and `body` are the three kinds of body; a request has one.

## Coin (experimental)

```
gett coin btc 1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa   address: balance, transactions
gett coin btc 4a5e1e4baab89f3a32518a88c31bc87f...  transaction: status, amount, fee
gett coin eth 0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045
gett coin doge DH5yaieqoZN36fDVciNyRueRGvGLR3mr7L | jq .final_balance
```

Coins: `btc` `eth` `ltc` `doge` `dash` `bch` `sol` `trx`. Data comes from
public explorers. A summary is shown on a terminal, raw JSON when piped.

## Net (experimental)

```
gett net ip                        addresses per interface, gateway, dns servers, public ip
gett net dns example.com           A, AAAA, CNAME, MX, NS and TXT records
gett net dns example.com MX        one record type
gett net dns example.com 1.1.1.1   ask that server
gett net dns 93.184.216.34         reverse lookup
gett net ping example.com          4 echo requests, then loss and min/avg/max
gett net ping example.com 10       10 requests; Ctrl+C stops early
gett net trace example.com         route hop by hop, 30 hops max, 3 probes per hop
gett net port example.com 443      open, closed or no answer within 3 seconds
gett net tls example.com           certificate: names, issuer, validity, protocol, cipher
gett net tls example.com:8443      another port
gett net whois example.com         registry record; also for an ip address
gett net speed                     latency, download and upload, measured against Cloudflare
```

`dns` asks the system's servers unless one is given. `tls` shows an untrusted
certificate as well, with the reason. `whois` uses RDAP, the registry
protocol that replaced port-43 whois. `ping` and `trace` may need elevated
rights.

## Other examples

```
gett https://api.example.com/me "X-Token: abc"      header; any "Name: value" word is one
gett https://example.com/file cookies.txt           cookies from a Netscape cookies.txt, as browsers export
gett https://example.com/file 5m                    stop after 5 minutes; also 30s, 1h30m
gett https://example.com/file verbose               requests, replies, headers, redirects, retries
gett https://example.com/file silent                no progress, no notes, errors only
gett https://example.com/a.zip extract 5m silent    words combine, in any order
gett https://example.com/tool.tar.gz scan verbose   sandbox commands and scanner output
```

A header word is sent with every request. A cookie file is recognised by its
contents and sent to the hosts it names. A timeout covers the whole command;
a download it stops resumes on the next run. `verbose` prints every request
and reply with its headers, redirects, retries and resume decisions to
stderr, with `Authorization` and `Cookie` values hidden. `silent` prints
nothing but errors.

`gett` without arguments prints the examples and the defaults. `gett version`
prints the version. Proxies are read from `HTTP_PROXY`, `HTTPS_PROXY` and
`NO_PROXY`.

## Errors

One line per error, with curl's exit codes:

```
exit  message                                                  meaning
2     don't know what "x" means                                a word gett does not understand
2     game.rar needs a password, left as is                    archive not unpacked; the file is kept
2     net trace needs elevated rights                          run with sudo or as Administrator
3     not a valid URL: x                                       the URL could not be parsed
5     could not reach proxy: ...                               HTTP_PROXY or HTTPS_PROXY is wrong
6     could not resolve host example.invalid                   DNS has no address for the name
6     no gateway could serve ipfs://...                        no IPFS gateway has the content
7     could not connect (example.com)                          connection refused or network unreachable
7     could not connect to laptop (192.168.1.20:41234)         transfer: the sharer is listed but refuses connections
9     server said 550 File unavailable                         FTP: no such file, or no permission
22    server returned 404 Not Found for https://...            HTTP error status
23    could not write file: ...                                disk full, folder missing, or no permission
25    upload failed: ...                                       FTP: upload incomplete, or a command refused
26    open data.xml: no such file or directory                 a local file could not be read
28    timed out (example.com)                                  no connection or no data within the limit
33    the file on the server changed; run again to start over  resume impossible
35    TLS handshake failed (example.com): ...                  server does not speak TLS
47    more than 100 redirects                                  redirect loop
52    server closed the connection early (example.com)         no or partial response
56    connection reset (example.com)                           connection dropped by the server or network
60    TLS certificate problem (example.com): ...               expired, self-signed, or wrong name
67    login denied by example.com                              FTP user or password wrong
130   interrupted                                              Ctrl+C
```

Anything else exits 1. When running the same command again would finish the
transfer, the line ends with `Run the same command again to resume`.

## Build

Requires Go 1.27 or newer. The result is one static binary.

```
CGO_ENABLED=0 go build -ldflags "-s -w" -o gett .
```

Cross-compile with `GOOS` and `GOARCH`, for example `GOOS=windows GOARCH=amd64`.
The version is read from `version.txt`.

## Release

```
./build.sh          0.1.0 -> 0.1.1
./build.sh minor    0.1.0 -> 0.2.0
./build.sh major    0.1.0 -> 1.0.0
```

Release notes go in `changes-<version>.txt` first. The script updates
`version.txt`, builds every platform into `dist/`, writes `SHA256SUMS`,
commits, tags, pushes and publishes a GitHub release. It stops when there are
uncommitted changes or `gh auth login` has not been run.

## License

MIT. See [LICENSE](LICENSE).
