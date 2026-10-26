package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/ipv4"

	"gett/internal/term"
)

// Sharers are found with a beacon: a receiver multicasts a query, every
// sharer answers it directly. The address a sharer is reached at is the one
// its answer came from.
var (
	groupAddr = &net.UDPAddr{IP: net.IPv4(239, 255, 71, 84), Port: 7184}

	// listenBeacon is the sharer's socket, joined to the group on one
	// interface; the others are joined after. Tests listen on loopback.
	listenBeacon = func(ifi *net.Interface) (*net.UDPConn, error) {
		return net.ListenMulticastUDP("udp4", ifi, groupAddr)
	}
)

// Peer is a sharer that answered.
type Peer struct {
	ID, Name string
	API      int
	Addr     string
	Port     int
}

// Finder finds sharers. The beacon is the only one for now; a finder for
// another network is another implementation.
type Finder interface {
	Find(ctx context.Context, want string) ([]Peer, error)
}

var finder Finder = beacon{}

// packet is a query ("q") or an answer ("a") on the wire.
type packet struct {
	T    string `json:"t"`
	Q    string `json:"q"` // nonce, answers carry the query's
	API  int    `json:"api"`
	Want string `json:"want,omitempty"` // query: one sharer's id, or every sharer
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	Port int    `json:"port,omitempty"`
}

// multicastIfaces are the interfaces worth querying: up, multicast, not
// loopback, not a tunnel, with an IPv4 address. Tests use loopback.
var multicastIfaces = func() []net.Interface {
	all, _ := net.Interfaces()
	var out []net.Interface
	for _, ifi := range all {
		bad := net.FlagLoopback | net.FlagPointToPoint
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 || ifi.Flags&bad != 0 {
			continue
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				out = append(out, ifi)
				break
			}
		}
	}
	return out
}

var errNoInterface = errors.New("no network interface for discovery")

// announce answers beacon queries for this sharer until ctx ends.
func announce(ctx context.Context, name string, port int, id string) {
	ifaces := multicastIfaces()
	if len(ifaces) == 0 {
		term.Debug("%v", errNoInterface)
		return
	}
	c, err := listenBeacon(&ifaces[0])
	if err != nil {
		term.Debug("discovery off: %v", err)
		return
	}
	defer c.Close()
	pc := ipv4.NewPacketConn(c)
	for i := range ifaces[1:] {
		if err := pc.JoinGroup(&ifaces[i+1], groupAddr); err != nil {
			term.Debug("%s: %v", ifaces[i+1].Name, err)
		}
	}
	go func() {
		<-ctx.Done()
		c.Close()
	}()
	buf := make([]byte, 1500)
	for {
		n, src, err := c.ReadFromUDP(buf)
		if err != nil {
			return
		}
		var q packet
		if json.Unmarshal(buf[:n], &q) != nil || q.T != "q" || q.Q == "" {
			continue
		}
		if q.Want != "" && q.Want != id {
			continue
		}
		b, _ := json.Marshal(packet{T: "a", Q: q.Q, API: api, ID: id, Name: name, Port: port})
		c.WriteToUDP(b, src)
	}
}

type beacon struct{}

// Find queries every interface twice and collects answers for discoverWait.
// With want set it returns as soon as that sharer answers.
func (beacon) Find(ctx context.Context, want string) ([]Peer, error) {
	ifaces := multicastIfaces()
	if len(ifaces) == 0 {
		return nil, errNoInterface
	}
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return nil, err
	}
	defer c.Close()
	nonce := randomID()
	q, _ := json.Marshal(packet{T: "q", Q: nonce, API: api, Want: want})
	pc := ipv4.NewPacketConn(c)
	send := func() {
		for i := range ifaces {
			pc.SetMulticastInterface(&ifaces[i])
			c.WriteToUDP(q, groupAddr)
		}
	}
	send()
	again := time.AfterFunc(300*time.Millisecond, send)
	defer again.Stop()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	c.SetReadDeadline(time.Now().Add(discoverWait))

	var peers []Peer
	seen := map[string]bool{}
	buf := make([]byte, 1500)
	for len(peers) < 64 {
		n, src, err := c.ReadFromUDP(buf)
		if err != nil {
			break
		}
		var a packet
		if json.Unmarshal(buf[:n], &a) != nil || a.T != "a" || a.Q != nonce || a.ID == "" || seen[a.ID] {
			continue
		}
		seen[a.ID] = true
		name := term.Sanitize(a.Name)
		if len(name) > 32 {
			name = name[:32]
		}
		peers = append(peers, Peer{ID: a.ID, Name: name, API: a.API, Addr: src.IP.String(), Port: a.Port})
		if want != "" && a.ID == want {
			break
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return dedupe(peers), nil
}

// dedupe sorts sharers by name and makes names unique: the second
// "macbook" becomes "macbook-2".
func dedupe(peers []Peer) []Peer {
	slices.SortFunc(peers, func(a, b Peer) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	for i := 1; i < len(peers); i++ {
		base := peers[i].Name
		n := 1
		for j := 0; j < i; j++ {
			if peers[j].Name == peers[i].Name {
				n++
				peers[i].Name = base + "-" + strconv.Itoa(n)
				j = -1 // check the new name against everything before it
			}
		}
	}
	return peers
}

// hostPort is how a peer is shown next to its name in verbose output.
func (p Peer) hostPort() string { return net.JoinHostPort(p.Addr, strconv.Itoa(p.Port)) }
