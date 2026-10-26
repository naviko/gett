package errs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/textproto"
	"net/url"
	"syscall"
	"testing"
)

func TestCodes(t *testing.T) {
	cases := []struct {
		err  error
		code int
	}{
		{&Usage{"x"}, 2},
		{&BadURL{"x"}, 3},
		{&url.Error{Op: "Get", URL: "http://h/", Err: &net.DNSError{Name: "h", IsNotFound: true}}, 6},
		{&url.Error{Op: "Get", URL: "http://h/", Err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}}, 7},
		{&HTTPStatus{404, "404 Not Found", "u"}, 22},
		{&Write{errors.New("disk")}, 23},
		{&Upload{"x"}, 25},
		{context.DeadlineExceeded, 28},
		{&Range{"x"}, 33},
		{&Resume{"x"}, 36},
		{&Login{"h"}, 67},
		{&textproto.Error{Code: 530, Msg: "no"}, 67},
		{&textproto.Error{Code: 550, Msg: "no"}, 9},
		{&Resumable{context.Canceled}, 130},
		{fmt.Errorf("wrapped: %w", &Resumable{&Write{errors.New("x")}}), 23},
	}
	for _, c := range cases {
		if got := Code(c.err); got != c.code {
			t.Errorf("Code(%v) = %d, want %d", c.err, got, c.code)
		}
	}
	msg := Message(&url.Error{Op: "Get", URL: "http://h.example/", Err: &net.DNSError{Name: "h.example", IsNotFound: true}})
	if msg != "could not resolve host h.example" {
		t.Errorf("message = %q", msg)
	}
	if m := Message(&Resumable{context.Canceled}); m != "interrupted. Run the same command again to resume" {
		t.Errorf("resumable message = %q", m)
	}
}

func TestCanResume(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{context.Canceled, true},
		{context.DeadlineExceeded, true},
		{&url.Error{Op: "Get", URL: "http://h/", Err: &net.DNSError{Name: "h"}}, true},
		{io.ErrUnexpectedEOF, true},
		{fmt.Errorf("short: %w", io.ErrUnexpectedEOF), true},
		{errors.New("connection stalled"), true},
		{&HTTPStatus{503, "503 Service Unavailable", "u"}, true},
		{&HTTPStatus{429, "429 Too Many Requests", "u"}, true},
		{&HTTPStatus{403, "403 Forbidden", "u"}, false},
		{&HTTPStatus{404, "404 Not Found", "u"}, false},
		{&Write{errors.New("disk")}, false},
		{&Range{"changed"}, false},
		{&BadURL{"x"}, false},
		{&Redirect{"loop"}, false},
		{&Usage{"x"}, false},
		{&Login{"h"}, false},
		{&textproto.Error{Code: 550, Msg: "denied"}, false},
		{&fs.PathError{Op: "open", Path: "x", Err: fs.ErrNotExist}, false},
		{errors.New("connection to h keeps dropping"), true},
	}
	for _, c := range cases {
		if got := CanResume(c.err); got != c.want {
			t.Errorf("CanResume(%v) = %v, want %v", c.err, got, c.want)
		}
	}
	if m := Message(&HTTPStatus{403, "403 Forbidden", "https://cdn/x"}); m != "server returned 403 Forbidden for https://cdn/x" {
		t.Errorf("status message = %q", m)
	}
}
