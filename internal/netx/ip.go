package netx

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/jackpal/gateway"
	"github.com/miekg/dns"
)

// IP prints the interfaces with their addresses, the default gateway, the
// DNS servers and the public address.
func IP(ctx context.Context, client *http.Client) error {
	ifaces, err := net.Interfaces()
	if err != nil {
		return err
	}
	var loop []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		as := usable(addrs)
		if len(as) == 0 {
			continue
		}
		l := padRow(ifc.Name, strings.Join(as, "  "))
		if ifc.Flags&net.FlagLoopback != 0 {
			loop = append(loop, l)
			continue
		}
		line("%s", l)
	}
	for _, l := range loop {
		line("%s", l)
	}
	gw := "-"
	if ip, err := gateway.DiscoverGateway(); err == nil {
		gw = ip.String()
	}
	line("%s", padRow("gateway", gw))
	line("%s", padRow("dns", strings.Join(systemDNS(), "  ")))
	line("%s", padRow("public", publicIP(ctx, client)))
	return nil
}

// usable keeps the addresses worth showing: IPv4 first, then up to two
// global IPv6; link-local ones are noise.
func usable(addrs []net.Addr) []string {
	var v4, v6 []string
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLinkLocalUnicast() {
			continue
		}
		if ipn.IP.To4() != nil {
			v4 = append(v4, a.String())
		} else if len(v6) < 2 {
			v6 = append(v6, a.String())
		}
	}
	return append(v4, v6...)
}

func padRow(k, v string) string {
	return sanitize(k) + strings.Repeat(" ", max(1, 11-len(k))) + sanitize(v)
}

// systemDNS lists the resolver's servers; "-" when they cannot be read.
func systemDNS() []string {
	if runtime.GOOS == "windows" {
		return []string{"-"}
	}
	cfg, err := dns.ClientConfigFromFile("/etc/resolv.conf")
	if err != nil || len(cfg.Servers) == 0 {
		return []string{"-"}
	}
	return cfg.Servers
}

// publicIP asks Cloudflare, which answers with a line "ip=...".
func publicIP(ctx context.Context, client *http.Client) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://1.1.1.1/cdn-cgi/trace", nil)
	if err != nil {
		return "-"
	}
	resp, err := client.Do(req)
	if err != nil {
		return "-"
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "ip="); ok {
			return v
		}
	}
	return "-"
}
