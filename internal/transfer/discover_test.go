package transfer

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"
)

// loopback points the beacon at a plain socket on 127.0.0.1 so tests need
// no multicast.
func loopback(t *testing.T) {
	t.Helper()
	lo, err := net.InterfaceByName("lo0")
	if err != nil {
		if lo, err = net.InterfaceByName("lo"); err != nil {
			t.Skip("no loopback interface")
		}
	}
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	addr := c.LocalAddr().(*net.UDPAddr)
	c.Close()
	oldGroup, oldListen, oldIfaces := groupAddr, listenBeacon, multicastIfaces
	groupAddr = addr
	listenBeacon = func(*net.Interface) (*net.UDPConn, error) { return net.ListenUDP("udp4", addr) }
	multicastIfaces = func() []net.Interface { return []net.Interface{*lo} }
	t.Cleanup(func() { groupAddr, listenBeacon, multicastIfaces = oldGroup, oldListen, oldIfaces })
}

func TestBeaconRoundTrip(t *testing.T) {
	setup(t)
	loopback(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go announce(ctx, "laptop", 41234, "abc123")
	time.Sleep(50 * time.Millisecond)

	peers, err := finder.Find(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || peers[0].Name != "laptop" || peers[0].Port != 41234 || peers[0].ID != "abc123" || peers[0].API != api || peers[0].Addr != "127.0.0.1" {
		t.Fatalf("peers: %+v", peers)
	}

	// A targeted find returns as soon as the sharer answers.
	start := time.Now()
	if peers, err = finder.Find(ctx, "abc123"); err != nil || len(peers) != 1 {
		t.Fatalf("targeted: %+v %v", peers, err)
	}
	if time.Since(start) > discoverWait/2 {
		t.Fatal("targeted find waited the whole window")
	}
	if peers, _ = finder.Find(ctx, "other"); len(peers) != 0 {
		t.Fatalf("a query for another sharer was answered: %+v", peers)
	}

	cancel()
	time.Sleep(20 * time.Millisecond)
	if peers, _ = finder.Find(context.Background(), ""); len(peers) != 0 {
		t.Fatalf("a stopped sharer answered: %+v", peers)
	}
}

func TestBeaconIgnoresGarbage(t *testing.T) {
	setup(t)
	loopback(t)
	c, err := listenBeacon(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	go func() {
		buf := make([]byte, 1500)
		for {
			n, src, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			var q packet
			json.Unmarshal(buf[:n], &q)
			c.WriteToUDP([]byte("garbage"), src)
			other, _ := json.Marshal(packet{T: "a", Q: "not-the-nonce", API: api, ID: "x", Name: "ghost", Port: 1})
			c.WriteToUDP(other, src)
			good, _ := json.Marshal(packet{T: "a", Q: q.Q, API: api, ID: "id1", Name: "real", Port: 2})
			c.WriteToUDP(good, src)
			c.WriteToUDP(good, src) // a duplicate
		}
	}()
	peers, err := finder.Find(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || peers[0].Name != "real" {
		t.Fatalf("peers: %+v", peers)
	}
}

func TestDedupe(t *testing.T) {
	peers := dedupe([]Peer{{ID: "b", Name: "macbook"}, {ID: "a", Name: "macbook"}, {ID: "c", Name: "desk"}, {ID: "d", Name: "macbook"}})
	var names []string
	for _, p := range peers {
		names = append(names, p.Name)
	}
	if want := []string{"desk", "macbook", "macbook-2", "macbook-3"}; !equal(names, want) {
		t.Fatalf("names %v, want %v", names, want)
	}
	if peers[1].ID != "a" {
		t.Fatal("lowest id should keep the plain name")
	}
}

func TestNoInterface(t *testing.T) {
	old := multicastIfaces
	multicastIfaces = func() []net.Interface { return nil }
	defer func() { multicastIfaces = old }()
	if _, err := finder.Find(context.Background(), ""); err != errNoInterface {
		t.Fatalf("err %v", err)
	}
}
