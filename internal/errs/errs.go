// Package errs turns Go errors into one-line messages and curl-compatible
// exit codes.
package errs

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"syscall"
)

// Typed errors raised by gett itself.
type (
	Usage      struct{ Msg string }
	BadURL     struct{ URL string }
	HTTPStatus struct {
		Code   int
		Status string
		URL    string
	}
	Write    struct{ Err error }
	Range    struct{ Msg string }
	Resume   struct{ Msg string }
	Upload   struct{ Msg string }
	Login    struct{ Host string }
	Denied   struct{ Msg string }
	Redirect struct{ Msg string }
	// Resumable wraps a download error that running the same command fixes.
	Resumable struct{ Err error }
	// Gateway means no IPFS gateway could serve the address.
	Gateway struct{ Msg string }
	// Archive is a file that could not be unpacked: encrypted, damaged, or
	// unknown. The message is a full sentence naming the file.
	Archive struct{ Msg string }
	// Sandbox is a microVM that could not be set up or scanned. The message
	// is a full sentence; the download itself succeeded.
	Sandbox struct{ Msg string }
	// Elevated is a command the operating system refused without root or
	// Administrator rights. Cmd is the command as typed, like "net trace".
	Elevated struct{ Cmd string }
	// Peer is a computer on the local network that is listed but could not
	// be reached. Name is the computer as listed, Addr its address and port.
	Peer struct {
		Name, Addr string
		Err        error
	}
)

func (e *Usage) Error() string      { return e.Msg }
func (e *BadURL) Error() string     { return "not a valid URL: " + e.URL }
func (e *HTTPStatus) Error() string { return fmt.Sprintf("server returned %s", e.Status) }
func (e *Write) Error() string      { return "could not write file: " + e.Err.Error() }
func (e *Write) Unwrap() error      { return e.Err }
func (e *Range) Error() string      { return e.Msg }
func (e *Resume) Error() string     { return "could not resume: " + e.Msg }
func (e *Upload) Error() string     { return "upload failed: " + e.Msg }
func (e *Login) Error() string      { return "login denied by " + e.Host }
func (e *Denied) Error() string     { return "access denied: " + e.Msg }
func (e *Redirect) Error() string   { return e.Msg }
func (e *Resumable) Error() string  { return e.Err.Error() }
func (e *Gateway) Error() string    { return e.Msg }
func (e *Archive) Error() string    { return e.Msg }
func (e *Sandbox) Error() string    { return e.Msg }
func (e *Elevated) Error() string   { return e.Cmd + " needs elevated rights" }
func (e *Peer) Error() string       { return "could not connect to " + e.Name + " (" + e.Addr + ")" }
func (e *Peer) Unwrap() error       { return e.Err }
func (e *Resumable) Unwrap() error  { return e.Err }

// Code maps an error to curl's exit code for the same failure.
func Code(err error) int {
	if err == nil {
		return 0
	}
	var (
		usage    *Usage
		archive  *Archive
		sandbox  *Sandbox
		elevated *Elevated
		badURL   *BadURL
		status   *HTTPStatus
		write    *Write
		rng      *Range
		resume   *Resume
		upload   *Upload
		login    *Login
		denied   *Denied
		redirect *Redirect
		gateway  *Gateway
		peer     *Peer
		dns      *net.DNSError
		tp       *textproto.Error
		certErr  *tls.CertificateVerificationError
		pathErr  *os.PathError
	)
	switch {
	case errors.As(err, &usage), errors.As(err, &archive), errors.As(err, &sandbox), errors.As(err, &elevated):
		return 2
	case errors.As(err, &badURL):
		return 3
	case errors.As(err, &peer):
		return 7
	case errors.Is(err, context.Canceled):
		return 130
	case errors.Is(err, context.DeadlineExceeded), isTimeout(err):
		return 28
	case errors.As(err, &status):
		return 22
	case errors.As(err, &write):
		return 23
	case errors.As(err, &rng):
		return 33
	case errors.As(err, &resume):
		return 36
	case errors.As(err, &upload):
		return 25
	case errors.As(err, &login):
		return 67
	case errors.As(err, &denied):
		return 9
	case errors.As(err, &redirect):
		return 47
	case errors.As(err, &dns), errors.As(err, &gateway):
		return 6
	case isProxy(err):
		return 5
	case errors.As(err, &certErr), isX509(err):
		return 60
	case isTLS(err):
		return 35
	case errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return 7
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return 52
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return 56
	case errors.As(err, &tp):
		switch {
		case tp.Code == 530:
			return 67
		case tp.Code == 550 || tp.Code == 553:
			return 9
		default:
			return 25
		}
	case errors.As(err, &pathErr):
		if pathErr.Op == "open" || pathErr.Op == "read" || pathErr.Op == "stat" {
			return 26
		}
		return 23
	}
	return 1
}

// CanResume reports whether running the same command again can finish the
// transfer: network trouble, timeouts, interruptions and server overload can;
// a bad URL, a refused request, a changed file or a disk error cannot.
func CanResume(err error) bool {
	var st *HTTPStatus
	switch {
	case errors.As(err, new(*Usage)), errors.As(err, new(*BadURL)), errors.As(err, new(*Write)),
		errors.As(err, new(*Range)), errors.As(err, new(*Redirect)), errors.As(err, new(*Login)),
		errors.As(err, new(*Denied)), errors.As(err, new(*Archive)), errors.As(err, new(*Sandbox)), errors.As(err, new(*Elevated)), errors.As(err, new(*textproto.Error)),
		errors.As(err, new(*os.PathError)):
		return false
	case errors.As(err, &st):
		return st.Code == 429 || st.Code >= 500
	}
	return true
}

// Message gives the friendly one-liner for an error.
func Message(err error) string {
	if err == nil {
		return ""
	}
	var (
		dns    *net.DNSError
		uerr   *url.Error
		status *HTTPStatus
		tp     *textproto.Error
	)
	var rs *Resumable
	if errors.As(err, &rs) {
		return Message(rs.Err) + ". Run the same command again to resume"
	}
	var peer *Peer
	if errors.As(err, &peer) {
		return peer.Error()
	}
	host := ""
	if errors.As(err, &uerr) {
		if u, perr := url.Parse(uerr.URL); perr == nil {
			host = u.Host
		}
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "interrupted"
	case errors.Is(err, context.DeadlineExceeded), isTimeout(err):
		return "timed out" + at(host)
	case errors.As(err, &dns):
		return "could not resolve host " + dns.Name
	case errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "could not connect" + at(host)
	case isX509(err):
		return "TLS certificate problem" + at(host) + ": " + leaf(err)
	case isTLS(err):
		return "TLS handshake failed" + at(host) + ": " + leaf(err)
	case isProxy(err):
		return "could not reach proxy: " + leaf(err)
	case errors.As(err, &status):
		return fmt.Sprintf("server returned %s for %s", status.Status, status.URL)
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return "server closed the connection early" + at(host)
	case errors.Is(err, syscall.ECONNRESET):
		return "connection reset" + at(host)
	case errors.As(err, &tp):
		return fmt.Sprintf("server said %d %s", tp.Code, strings.TrimSpace(tp.Msg))
	}
	if uerr != nil {
		return leaf(err) + at(host)
	}
	return err.Error()
}

func at(host string) string {
	if host == "" {
		return ""
	}
	return " (" + host + ")"
}

// leaf returns the innermost error's text.
func leaf(err error) string {
	for {
		next := errors.Unwrap(err)
		if next == nil {
			return err.Error()
		}
		err = next
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func isProxy(err error) bool {
	var oe *net.OpError
	return errors.As(err, &oe) && oe.Op == "proxyconnect"
}

func isX509(err error) bool {
	var (
		a x509.UnknownAuthorityError
		b x509.CertificateInvalidError
		c x509.HostnameError
		d *tls.CertificateVerificationError
	)
	return errors.As(err, &a) || errors.As(err, &b) || errors.As(err, &c) || errors.As(err, &d)
}

func isTLS(err error) bool {
	var (
		a tls.RecordHeaderError
		b tls.AlertError
	)
	return errors.As(err, &a) || errors.As(err, &b)
}
