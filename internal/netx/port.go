package netx

import (
	"context"
	"errors"
	"net"
	"syscall"
)

// Port reports whether host:port accepts a TCP connection: open, closed or
// no answer. All three are answers, exit 0.
func Port(ctx context.Context, host, port string) error {
	host = hostOf(host)
	d := net.Dialer{Timeout: connectTimeout}
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	switch {
	case err == nil:
		c.Close()
		line("%s %s open", sanitize(host), port)
	case errors.Is(err, syscall.ECONNREFUSED):
		line("%s %s closed", sanitize(host), port)
	case isTimeout(err):
		line("%s %s no answer", sanitize(host), port)
	default:
		return err
	}
	return nil
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
