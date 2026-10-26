package transfer

import "testing"

// plateau models a link that gives 20% more per connection up to eight
// connections and nothing beyond.
func plateau(conns int) float64 {
	n := min(conns, 8)
	r := 1e6
	for i := startConns; i < n; i++ {
		r *= 1.2
	}
	return r
}

func TestControlClimbsToPlateau(t *testing.T) {
	c := newControl()
	changes, windows := 0, 0
	last := c.conns
	for !c.settled || c.probing {
		conns, _ := c.observe(plateau(c.conns))
		if conns != last {
			changes++
			last = conns
		}
		if conns < minConns || conns > maxConns {
			t.Fatalf("window %d: %d connections", windows, conns)
		}
		if windows++; windows > 40 {
			t.Fatal("never settled")
		}
	}
	if c.conns != 8 {
		t.Fatalf("settled at %d connections, want 8", c.conns)
	}
	// Four probes up, a fifth that did not help and was reverted: six
	// changes in about a dozen windows.
	if changes != 6 || windows > 14 {
		t.Fatalf("%d changes in %d windows to reach the plateau", changes, windows)
	}
}

func TestControlIgnoresNoise(t *testing.T) {
	c := newControl()
	for i := 0; i < 10; i++ {
		c.observe(plateau(c.conns))
	}
	changes := 0
	last := c.conns
	for i := 0; i < 60; i++ {
		r := plateau(c.conns)
		if i%2 == 0 {
			r *= 1.03
		} else {
			r *= 0.97
		}
		conns, _ := c.observe(r)
		if conns != last {
			changes++
			last = conns
		}
	}
	// Only the periodic probes move it, and each one is reverted.
	if changes > 2*(60/reprobeEvery)+2 {
		t.Fatalf("%d changes on a noisy plateau", changes)
	}
	if c.conns != 8 {
		t.Fatalf("drifted to %d connections", c.conns)
	}
}

func TestControlBacksOffAndHolds(t *testing.T) {
	c := newControl()
	for i := 0; i < 10; i++ {
		c.observe(plateau(c.conns))
	}
	was := c.conns
	conns, _ := c.observe(plateau(c.conns) * 0.6)
	if conns != was-1 || c.hold != 2 {
		t.Fatalf("after a drop: %d connections, hold %d", conns, c.hold)
	}
	for i := 0; i < 2; i++ {
		if conns, _ = c.observe(plateau(c.conns) * 0.6); conns != was-1 {
			t.Fatalf("held window %d changed to %d", i, conns)
		}
	}
	if conns, _ = c.observe(plateau(c.conns) * 0.35); conns != was-2 {
		t.Fatalf("second drop: %d, want %d", conns, was-2)
	}
}

func TestControlReprobes(t *testing.T) {
	c := newControl()
	for !c.settled || c.probing {
		c.observe(plateau(c.conns))
	}
	at := c.conns
	probed := 0
	for i := 0; i < reprobeEvery+3; i++ {
		conns, _ := c.observe(plateau(c.conns))
		if conns == at+1 {
			probed++
		}
	}
	if probed == 0 {
		t.Fatal("never probed again")
	}
	if c.conns != at {
		t.Fatalf("probe that did not help was kept: %d, want %d", c.conns, at)
	}
}

func TestControlBounds(t *testing.T) {
	c := newControl()
	r := 1e6
	for i := 0; i < 200; i++ {
		r *= 1.5
		if conns, _ := c.observe(r); conns > maxConns {
			t.Fatalf("over the cap: %d", conns)
		}
	}
	if c.conns != maxConns {
		t.Fatalf("an ever rising rate should reach the cap, got %d", c.conns)
	}
	for i := 0; i < 200; i++ {
		r /= 2
		if conns, _ := c.observe(r); conns < minConns {
			t.Fatalf("under the floor: %d", conns)
		}
	}
	if c.conns != minConns {
		t.Fatalf("an ever falling rate should reach the floor, got %d", c.conns)
	}
}

func TestControlSetMax(t *testing.T) {
	c := newControl()
	c.conns = 10
	c.setMax(8)
	if c.max != 8 || c.conns != 8 {
		t.Fatalf("setMax(8): max %d conns %d", c.max, c.conns)
	}
	c.setMax(100)
	if c.max != maxConns {
		t.Fatalf("setMax(100): max %d", c.max)
	}
	c.setMax(0)
	if c.max != minConns || c.conns != minConns {
		t.Fatalf("setMax(0): max %d conns %d", c.max, c.conns)
	}
}

func TestUnitFor(t *testing.T) {
	mib := int64(1 << 20)
	cases := []struct {
		rate  float64
		conns int
		want  int64
	}{
		{0, 4, firstUnit},
		{-1, 4, firstUnit},
		{float64(1 * mib), 4, minUnit},
		{float64(42 * mib), 4, 10 * mib},
		{float64(42*mib) + 12345, 4, 10 * mib},
		{float64(400 * mib), 4, maxUnit},
		{float64(9 * mib), 0, 9 * mib},
	}
	for _, c := range cases {
		if got := unitFor(c.rate, c.conns); got != c.want {
			t.Errorf("unitFor(%v, %d) = %d, want %d", c.rate, c.conns, got, c.want)
		}
	}
	c := newControl()
	if _, u := c.observe(0); u != firstUnit {
		t.Errorf("first unit %d, want %d", u, firstUnit)
	}
}
