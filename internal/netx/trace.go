package netx

import (
	"context"
	"net"
	"strings"
	"time"
)

const (
	traceHops   = 30
	traceProbes = 3
	traceWait   = time.Second
	traceLookup = 200 * time.Millisecond
)

// Trace prints the route to host: one line per hop with up to three round
// trip times, "*" for a probe that got no answer.
func Trace(ctx context.Context, host string) error {
	host = hostOf(host)
	dest, err := resolve(ctx, host)
	if err != nil {
		return err
	}
	p, err := open(dest, "trace")
	if err != nil {
		return err
	}
	defer p.close()
	line("trace to %s (%s), %d hops max", sanitize(host), dest, traceHops)

	seq := 0
	for ttl := 1; ttl <= traceHops; ttl++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := p.setTTL(ttl); err != nil {
			return err
		}
		var from net.IP
		var rtts []string
		reached := false
		for i := 0; i < traceProbes; i++ {
			seq++
			start := time.Now()
			if err := p.send(seq); err != nil {
				return err
			}
			a, err := p.recv(seq, start.Add(traceWait))
			if err != nil {
				return err
			}
			if a == nil {
				rtts = append(rtts, "*")
				continue
			}
			rtts = append(rtts, ms(time.Since(start)))
			from = a.from
			if !a.exceeded || a.from.Equal(dest) {
				reached = true
			}
		}
		where := "*"
		if from != nil {
			where = from.String()
			if name := quickName(ctx, from); name != "" {
				where = name + " (" + from.String() + ")"
			}
		}
		line("%2d  %-50s %s", ttl, sanitize(where), strings.Join(rtts, "  "))
		if reached {
			return nil
		}
	}
	return nil
}

// quickName is the reverse lookup when it answers fast, else nothing.
func quickName(ctx context.Context, ip net.IP) string {
	ctx, cancel := context.WithTimeout(ctx, traceLookup)
	defer cancel()
	names, err := net.DefaultResolver.LookupAddr(ctx, ip.String())
	if err != nil || len(names) == 0 {
		return ""
	}
	return strings.TrimSuffix(names[0], ".")
}
