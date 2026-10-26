package netx

import (
	"bytes"
	"context"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func capture(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	Out = &buf
	t.Cleanup(func() { Out = nil })
	err := fn()
	return buf.String(), err
}

func TestPort(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	out, err := capture(t, func() error { return Port(context.Background(), "127.0.0.1", port) })
	if err != nil || !strings.HasSuffix(strings.TrimSpace(out), " open") {
		t.Fatalf("open: %q %v", out, err)
	}
	l.Close()
	out, err = capture(t, func() error { return Port(context.Background(), "127.0.0.1", port) })
	if err != nil || !strings.HasSuffix(strings.TrimSpace(out), " closed") {
		t.Fatalf("closed: %q %v", out, err)
	}
}

func TestTLS(t *testing.T) {
	srv := httptest.NewTLSServer(nil)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "https://")
	out, err := capture(t, func() error { return TLS(context.Background(), host) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"issuer", "valid", "protocol", "cipher", "trust      not trusted:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

// dnsServer answers A and MX for example.test and NXDOMAIN for anything else.
func dnsServer(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		q := r.Question[0]
		if q.Name != "example.test." {
			m.Rcode = dns.RcodeNameError
		} else {
			switch q.Qtype {
			case dns.TypeA:
				m.Answer = append(m.Answer, &dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(192, 0, 2, 1)})
			case dns.TypeMX:
				m.Answer = append(m.Answer, &dns.MX{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeMX, Class: dns.ClassINET, Ttl: 300}, Preference: 10, Mx: "mail.example.test."})
			}
		}
		w.WriteMsg(m)
	})}
	go srv.ActivateAndServe()
	t.Cleanup(func() { srv.Shutdown() })
	return pc.LocalAddr().String()
}

func TestDNS(t *testing.T) {
	server := dnsServer(t)
	out, err := capture(t, func() error { return DNS(context.Background(), "example.test", []string{server}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "A      60         192.0.2.1") || !strings.Contains(out, "MX     300        10 mail.example.test.") {
		t.Fatalf("records:\n%s", out)
	}
	out, err = capture(t, func() error { return DNS(context.Background(), "example.test", []string{"MX", server}) })
	if err != nil || strings.Contains(out, "192.0.2.1") || !strings.Contains(out, "mail.example.test") {
		t.Fatalf("one type:\n%s %v", out, err)
	}
	_, err = capture(t, func() error { return DNS(context.Background(), "nope.test", []string{server}) })
	var de *net.DNSError
	if err == nil || !asDNS(err, &de) || !de.IsNotFound {
		t.Fatalf("want not-found, got %v", err)
	}
}

func asDNS(err error, target **net.DNSError) bool {
	de, ok := err.(*net.DNSError)
	if ok {
		*target = de
	}
	return ok
}

func TestInnerSeq(t *testing.T) {
	// An IPv4 header (20 bytes) followed by an ICMP echo with seq 7.
	hdr := make([]byte, 20)
	hdr[0] = 0x45
	echo := []byte{8, 0, 0, 0, 0x12, 0x34, 0, 7}
	if seq, ok := innerSeq(append(hdr, echo...), false); !ok || seq != 7 {
		t.Fatalf("seq = %d, %v", seq, ok)
	}
	if _, ok := innerSeq(hdr[:10], false); ok {
		t.Fatal("short data must not parse")
	}
}

func TestUsable(t *testing.T) {
	mk := func(s string) net.Addr { _, n, _ := net.ParseCIDR(s); return n }
	got := usable([]net.Addr{mk("fe80::1/64"), mk("2001:db8::1/64"), mk("10.0.0.5/24"), mk("2001:db8::2/64"), mk("2001:db8::3/64")})
	want := []string{"10.0.0.0/24", "2001:db8::/64", "2001:db8::/64"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("usable = %v", got)
	}
}

func TestMs(t *testing.T) {
	if ms(1500*time.Microsecond) != "1.5 ms" {
		t.Fatal(ms(1500 * time.Microsecond))
	}
}
