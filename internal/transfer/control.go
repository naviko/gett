package transfer

import "math"

// The number of connections is found by climbing: add one while the rate
// keeps rising, step back when the last one did not help, and try again
// now and then in case the link or the sharer got faster. Tests shrink
// these.
var (
	minUnit      int64 = 4 << 20  // smallest work unit
	maxUnit      int64 = 64 << 20 // largest work unit
	firstUnit    int64 = 8 << 20  // unit size before the first measurement
	minConns           = 2
	maxConns           = 16
	startConns         = 4
	reprobeEvery       = 10 // settled windows between attempts to add a connection
)

// control decides how many connections to use and how big a unit should be
// from one rate measurement per second.
type control struct {
	conns   int
	base    float64 // rate at the last accepted setting
	hold    int     // windows to let a change take effect before judging it
	probing bool    // a connection was just added on trial
	settled bool
	idle    int // settled windows since the last probe
	max     int // maxConns, or less while another lane holds a connection
}

func newControl() *control {
	return &control{conns: startConns, hold: 1, max: maxConns}
}

// setMax caps the connections, for when the tar lane holds one.
func (c *control) setMax(n int) {
	c.max = min(max(n, minConns), maxConns)
	if c.conns > c.max {
		c.conns = c.max
	}
}

// observe takes the bytes moved in the last window and returns the
// connections and unit size to use next.
func (c *control) observe(rate float64) (int, int64) {
	switch {
	case c.hold > 0:
		c.hold--
		if c.hold == 0 && c.base == 0 {
			c.base = rate // the first measurement; now try one more
			c.probe()
		}
	case rate < c.base*0.8:
		// Worse than the accepted setting: one connection too many, or the
		// link got slower. Step back and hold still.
		if c.conns > minConns {
			c.conns--
		}
		c.base, c.probing, c.settled, c.hold, c.idle = rate, false, true, 2, 0
	case c.probing:
		if rate > c.base*1.1 {
			c.base = rate // it helped; keep it and try another
			c.probe()
		} else {
			c.conns-- // it did not; revert
			c.probing, c.settled, c.hold, c.idle = false, true, 1, 0
		}
	default:
		c.base, c.settled = rate, true
		c.idle++
		if c.idle >= reprobeEvery {
			c.idle = 0
			c.probe()
		}
	}
	if rate <= 0 && c.base == 0 {
		return c.conns, firstUnit
	}
	return c.conns, unitFor(rate, c.conns)
}

// probe adds a connection on trial, if there is room.
func (c *control) probe() {
	if c.conns >= c.max {
		c.probing, c.settled = false, true
		return
	}
	c.conns++
	c.probing, c.settled, c.hold = true, false, 1
}

// unitFor is about one second of data per connection, in whole MiB,
// within the limits.
func unitFor(rate float64, conns int) int64 {
	if rate <= 0 {
		return firstUnit
	}
	if conns < 1 {
		conns = 1
	}
	u := int64(math.Floor(rate/float64(conns)/float64(1<<20))) << 20
	return min(max(u, minUnit), maxUnit)
}
