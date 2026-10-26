package netx

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"
)

var defaultTypes = []uint16{dns.TypeA, dns.TypeAAAA, dns.TypeCNAME, dns.TypeMX, dns.TypeNS, dns.TypeTXT}

// DNS prints the records for name. extra is a record type (MX) or a server
// to ask; empty means all common types from the system resolver.
func DNS(ctx context.Context, name string, extra []string) error {
	name = hostOf(name)
	types := defaultTypes
	server := ""
	for _, e := range extra {
		if t, ok := dns.StringToType[strings.ToUpper(e)]; ok {
			types = []uint16{t}
			continue
		}
		server = e
	}
	if ip := net.ParseIP(name); ip != nil {
		rev, err := dns.ReverseAddr(ip.String())
		if err != nil {
			return err
		}
		name, types = rev, []uint16{dns.TypePTR}
	}
	server, err := pickServer(ctx, server)
	if err != nil {
		return err
	}
	c := &dns.Client{Timeout: 5 * time.Second}
	found, nxdomain := 0, false
	for _, t := range types {
		m := new(dns.Msg)
		m.SetQuestion(dns.Fqdn(name), t)
		m.RecursionDesired = true
		r, _, err := c.ExchangeContext(ctx, m, server)
		if err != nil {
			return fmt.Errorf("%s did not answer (%w)", server, err)
		}
		if r.Rcode == dns.RcodeNameError {
			nxdomain = true
			break
		}
		for _, rr := range r.Answer {
			if v := value(rr); v != "" {
				found++
				line("%-6s %-10d %s", dns.TypeToString[rr.Header().Rrtype], rr.Header().Ttl, sanitize(v))
			}
		}
	}
	if nxdomain || found == 0 {
		return &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	}
	return nil
}

// pickServer uses the one given, else the system resolver, else 1.1.1.1.
func pickServer(ctx context.Context, server string) (string, error) {
	if server == "" {
		if s := systemDNS(); s[0] != "-" {
			server = s[0]
		} else {
			server = "1.1.1.1"
		}
	}
	host, port, err := net.SplitHostPort(server)
	if err != nil {
		host, port = server, "53"
	}
	if net.ParseIP(host) == nil {
		ip, err := resolve(ctx, host)
		if err != nil {
			return "", err
		}
		host = ip.String()
	}
	return net.JoinHostPort(host, port), nil
}

func value(rr dns.RR) string {
	switch r := rr.(type) {
	case *dns.A:
		return r.A.String()
	case *dns.AAAA:
		return r.AAAA.String()
	case *dns.CNAME:
		return r.Target
	case *dns.MX:
		return fmt.Sprintf("%d %s", r.Preference, r.Mx)
	case *dns.NS:
		return r.Ns
	case *dns.TXT:
		return strings.Join(r.Txt, " ")
	case *dns.PTR:
		return r.Ptr
	case *dns.SOA:
		return fmt.Sprintf("%s %s serial %d", r.Ns, r.Mbox, r.Serial)
	case *dns.SRV:
		return fmt.Sprintf("%d %d %d %s", r.Priority, r.Weight, r.Port, r.Target)
	case *dns.CAA:
		return fmt.Sprintf("%d %s %s", r.Flag, r.Tag, r.Value)
	}
	s := rr.String()
	if i := strings.Index(s, "\t"); i >= 0 {
		f := strings.Fields(s)
		if len(f) > 4 {
			return strings.Join(f[4:], " ")
		}
	}
	return s
}
