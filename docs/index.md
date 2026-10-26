# gett

One static binary. No flags: every option is a word on the command line.

```
brew install naviko/tap/gett
```

Or take the file for your system from the
[latest release](https://github.com/naviko/gett/releases/latest), rename it
to `gett`, and put it on the PATH. Linux, macOS and Windows, Intel and ARM.

## Download a file

```
gett https://example.com/video.mp4                    download into the current folder
gett https://example.com/video.mp4 movies/            download into movies/
gett https://example.com/video.mp4 movie.mp4          save as movie.mp4
gett https://user:pass@example.com/private.zip        download with a login
gett https://example.com/report | less                stream to the pipe, save nothing
```

Several connections are used when the server allows it. If the download is
interrupted, running the same command again resumes it. An existing file is
never overwritten; the new one is named `video (1).mp4`.

## Download and unpack

```
gett https://example.com/src.tar.gz extract           download, unpack into ./src/
gett https://example.com/game.zip extract games/      download, unpack into games/
gett extract archive.rar                              unpack a local file into ./archive/
gett extract app.log.gz | grep ERROR                  stream a compressed file
```

zip, 7z, rar, tar, tar.gz, tar.xz, tar.zst, tar.bz2, gz, xz, zst and bz2.
The format is detected from the content, so a renamed file opens too.

## Share files with a computer in the same room

```
gett transfer photos/                                 share a folder on the local network, Ctrl+C ends it
gett transfer photos/ tiger                           share a folder with the password tiger
gett transfer                                         list what is shared on the local network, numbered
gett transfer tiger                                   list what is shared, including shares with the password tiger
gett transfer 1 <folder>/                             download item 1 into <folder>/
gett transfer 1.3 2.1                                 download items 1.3 and 2.1 into the current folder
```

Computers find each other on the local network; nothing is set up and
nothing leaves the network. Big files travel over several connections at
once, small files as one stream, and an interrupted download resumes when
the command is run again.

## Check a download before trusting it

```
gett https://example.com/tool.tar.gz scan             unpack into a sandbox, scan, report, dispose
gett https://example.com/tool.tar.gz sandbox          unpack into a sandbox, scan, keep the sandbox: ssh tool
gett sandbox installer.zip                            put a local file into a sandbox, scan, keep the sandbox
```

The sandbox is a small Linux machine with its own disk and network; scanning
uses YARA-X with the YARA Forge rules. Requires
[Microsandbox](https://microsandbox.dev); gett offers to install it.

## Move files to and from an FTP server

```
gett ftp://host/pub/photos/                           download a folder into ./photos/
gett ftp upload ftp://user:pass@host/backups ./photos upload a folder
gett ftp ls ftp://host/pub/                           list a folder
gett ftp rm ftp://host/backups/2019/                  delete a folder and everything in it
```

Folder transfers run several files at once, skip files already present and
resume partial files when the command is run again.

## Call an API

```
gett send https://api.example.com/users                       GET
gett send https://api.example.com/users POST '{"name":"Ada"}' POST with a JSON body
gett send https://api.example.com/upload photo=@cat.jpg       upload a file as a form field
gett send https://api.example.com/users "X-Token: abc" 30s    send a header, give up after 30 seconds
gett send https://api.example.com/users | jq '.[0]'           raw body to the pipe
gett send login.json                                          run the request saved in login.json
```

On a terminal the status, headers and formatted JSON are shown. A saved
request is a JSON file; secrets come from `${VAR}` environment variables.

## Look at the network

```
gett net ip                                           addresses, gateway, dns servers, public ip
gett net dns example.com                              dns records
gett net ping example.com                             ping
gett net trace example.com                            route hop by hop
gett net port example.com 443                         open, closed or no answer within 3 seconds
gett net tls example.com                              certificate: names, issuer, validity, protocol, cipher
gett net whois example.com                            registry record
gett net speed                                        download and upload speed
```

## Look up a coin address

```
gett coin btc 1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa      balance and transactions of an address
gett coin eth 0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045  coins: btc eth ltc doge dash bch sol trx
```

## Words that work everywhere

```
gett https://example.com/file "X-Token: abc"          send a header
gett https://example.com/file cookies.txt             send cookies from a Netscape cookies.txt
gett https://example.com/file 5m                      give up after 5 minutes
gett https://example.com/file verbose                 show every request and reply
gett https://example.com/file silent                  show errors only
```

Words combine in any order. Errors are one line each, with curl's exit
codes. The full manual is the [README](https://github.com/naviko/gett#readme).
