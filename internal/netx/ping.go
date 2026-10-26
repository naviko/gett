package netx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

const (
	pingInterval = time.Second
	pingWait     = 2 * time.Second
)

// probe is an ICMP echo socket: unprivileged when the system allows it,
// raw otherwise. Both read echo replies and time-exceeded messages.
type probe struct {
	c    *icmp.PacketConn
	v6   bool
	raw  bool
	id   int
	dest net.IP
}

// open tries the unprivileged socket, then the raw one. Refused twice for
// lack of rights means the command needs elevation; anything else is
// reported as it is.
func open(dest net.IP, cmd string) (*probe, error) {
	p := &probe{v6: dest.To4() == nil, id: os.Getpid() & 0xffff, dest: dest}
	nets := [][2]string{{"udp4", "0.0.0.0"}, {"ip4:icmp", "0.0.0.0"}}
	if p.v6 {
		nets = [][2]string{{"udp6", "::"}, {"ip6:ipv6-icmp", "::"}}
	}
	var last error
	for i, n := range nets {
		c, err := icmp.ListenPacket(n[0], n[1])
		if err == nil {
			p.c, p.raw = c, i == 1
			if p.v6 {
				p.c.IPv6PacketConn().SetControlMessage(ipv6.FlagHopLimit, true)
			} else {
				p.c.IPv4PacketConn().SetControlMessage(ipv4.FlagTTL, true)
			}
			return p, nil
		}
		last = err
		if !denied(err) && !strings.Contains(err.Error(), "protocol not supported") {
			return nil, err
		}
	}
	if denied(last) {
		return nil, elevated(cmd)
	}
	return nil, last
}

func (p *probe) close() { p.c.Close() }

func (p *probe) setTTL(ttl int) error {
	if p.v6 {
		return p.c.IPv6PacketConn().SetHopLimit(ttl)
	}
	return p.c.IPv4PacketConn().SetTTL(ttl)
}

// send writes one echo request with seq.
func (p *probe) send(seq int) error {
	typ := icmp.Type(ipv4.ICMPTypeEcho)
	if p.v6 {
		typ = ipv6.ICMPTypeEchoRequest
	}
	m := icmp.Message{Type: typ, Code: 0, Body: &icmp.Echo{ID: p.id, Seq: seq, Data: []byte("gett ping")}}
	b, err := m.Marshal(nil)
	if err != nil {
		return err
	}
	var dst net.Addr = &net.UDPAddr{IP: p.dest}
	if p.raw {
		dst = &net.IPAddr{IP: p.dest}
	}
	_, err = p.c.WriteTo(b, dst)
	return err
}

// answer is what came back for a probe.
type answer struct {
	from     net.IP
	ttl      int
	seq      int
	exceeded bool // time exceeded (traceroute hop) rather than an echo reply
}

// recv waits until deadline for a message about seq. Replies to other
// sequences, or from other programs, are skipped.
func (p *probe) recv(seq int, deadline time.Time) (*answer, error) {
	buf := make([]byte, 1500)
	for {
		p.c.SetReadDeadline(deadline)
		var n, ttl int
		var peer net.Addr
		var err error
		if p.v6 {
			var cm *ipv6.ControlMessage
			n, cm, peer, err = p.c.IPv6PacketConn().ReadFrom(buf)
			if cm != nil {
				ttl = cm.HopLimit
			}
		} else {
			var cm *ipv4.ControlMessage
			n, cm, peer, err = p.c.IPv4PacketConn().ReadFrom(buf)
			if cm != nil {
				ttl = cm.TTL
			}
		}
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return nil, nil
			}
			return nil, err
		}
		proto := 1
		if p.v6 {
			proto = 58
		}
		m, err := icmp.ParseMessage(proto, buf[:n])
		if err != nil {
			continue
		}
		from := addrIP(peer)
		switch body := m.Body.(type) {
		case *icmp.Echo:
			if m.Type == ipv4.ICMPTypeEchoReply || m.Type == ipv6.ICMPTypeEchoReply {
				// Unprivileged sockets rewrite the id, so match on seq only.
				if body.Seq == seq {
					return &answer{from: from, ttl: ttl, seq: seq}, nil
				}
			}
		case *icmp.TimeExceeded:
			if s, ok := innerSeq(body.Data, p.v6); ok && s == seq {
				return &answer{from: from, ttl: ttl, seq: seq, exceeded: true}, nil
			}
		case *icmp.DstUnreach:
			if s, ok := innerSeq(body.Data, p.v6); ok && s == seq {
				return &answer{from: from, ttl: ttl, seq: seq, exceeded: true}, nil
			}
		}
	}
}

// innerSeq digs the echo sequence out of the original datagram quoted in
// a time-exceeded or unreachable message.
func innerSeq(data []byte, v6 bool) (int, bool) {
	proto, hdr := 1, 0
	if v6 {
		proto, hdr = 58, ipv6.HeaderLen
	} else {
		if len(data) < ipv4.HeaderLen {
			return 0, false
		}
		hdr = int(data[0]&0x0f) * 4
	}
	if len(data) < hdr+8 {
		return 0, false
	}
	m, err := icmp.ParseMessage(proto, data[hdr:])
	if err != nil {
		return 0, false
	}
	if e, ok := m.Body.(*icmp.Echo); ok {
		return e.Seq, true
	}
	return 0, false
}

func addrIP(a net.Addr) net.IP {
	switch v := a.(type) {
	case *net.UDPAddr:
		return v.IP
	case *net.IPAddr:
		return v.IP
	}
	return nil
}

// Ping sends count echo requests and prints a line per reply and a summary.
// Ctrl-C stops early; the summary still prints.
func Ping(ctx context.Context, host string, count int) error {
	host = hostOf(host)
	dest, err := resolve(ctx, host)
	if err != nil {
		return err
	}
	p, err := open(dest, "ping")
	if err != nil {
		return err
	}
	defer p.close()
	line("ping %s (%s)", sanitize(host), dest)

	var sent, got int
	var min, max, sum time.Duration
	for seq := 1; seq <= count; seq++ {
		if ctx.Err() != nil {
			break
		}
		start := time.Now()
		if err := p.send(seq); err != nil {
			return err
		}
		sent++
		a, err := p.recv(seq, start.Add(pingWait))
		if err != nil {
			return err
		}
		if a == nil || a.exceeded {
			line("no reply  seq %d", seq)
		} else {
			rtt := time.Since(start)
			got++
			sum += rtt
			if min == 0 || rtt < min {
				min = rtt
			}
			if rtt > max {
				max = rtt
			}
			if a.ttl > 0 {
				line("reply from %s  seq %d  ttl %d  %s", a.from, seq, a.ttl, ms(rtt))
			} else {
				line("reply from %s  seq %d  %s", a.from, seq, ms(rtt))
			}
		}
		if seq < count {
			select {
			case <-ctx.Done():
			case <-time.After(pingInterval - time.Since(start)):
			}
		}
	}
	loss := 0
	if sent > 0 {
		loss = (sent - got) * 100 / sent
	}
	if got == 0 {
		line("%d sent, 0 received, 100%% loss", sent)
	} else {
		line("%d sent, %d received, %d%% loss, min %s avg %s max %s", sent, got, loss,
			strings.TrimSuffix(ms(min), " ms"), strings.TrimSuffix(ms(sum/time.Duration(got)), " ms"), ms(max))
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if got == 0 {
		return fmt.Errorf("no reply from %s", host)
	}
	return nil
}
