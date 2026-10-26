// Package netx is "gett net": addresses, dns, ping, traceroute, port and
// certificate checks, whois and a speed test. Plain aligned lines, the same
// on a terminal and when piped.
package netx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"gett/internal/errs"
	"gett/internal/term"
)

const (
	connectTimeout = 3 * time.Second
)

// Out is where results go; tests replace it.
var Out io.Writer = os.Stdout

func line(format string, a ...any) { fmt.Fprintf(Out, format+"\n", a...) }

// resolve picks one address for host: IPv4 when there is one, else IPv6.
func resolve(ctx context.Context, host string) (net.IP, error) {
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return ip, nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, a := range ips {
		if a.IP.To4() != nil {
			return a.IP, nil
		}
	}
	if len(ips) > 0 {
		return ips[0].IP, nil
	}
	return nil, &net.DNSError{Err: "no address", Name: host, IsNotFound: true}
}

// denied reports the operating system refusing a socket for lack of rights.
func denied(err error) bool {
	if errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "operation not permitted") || strings.Contains(s, "permission denied") || strings.Contains(s, "access is denied")
}

// elevated is the one message for a refused socket: the command, nothing else.
func elevated(cmd string) error { return &errs.Elevated{Cmd: "net " + cmd} }

// hostOf accepts a URL where a host is expected: https://example.com/x
// means example.com. Anything else is returned as typed.
func hostOf(target string) string {
	if strings.Contains(target, "://") {
		if u, err := url.Parse(target); err == nil && u.Hostname() != "" {
			return u.Hostname()
		}
	}
	if i := strings.IndexByte(target, '/'); i > 0 {
		return target[:i]
	}
	return target
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1f ms", float64(d)/float64(time.Millisecond)) }

func sanitize(s string) string { return term.Sanitize(s) }
