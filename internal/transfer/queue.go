package transfer

import (
	"cmp"
	"slices"
	"sync"
)

// span is the byte range [Start, End) of a file.
type span struct{ Start, End int64 }

// merge sorts spans by start and joins the ones that overlap or touch;
// empty spans are dropped.
func merge(spans []span) []span {
	out := make([]span, 0, len(spans))
	for _, s := range spans {
		if s.End > s.Start {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b span) int { return cmp.Compare(a.Start, b.Start) })
	n := 0
	for _, s := range out {
		if n > 0 && s.Start <= out[n-1].End {
			out[n-1].End = max(out[n-1].End, s.End)
			continue
		}
		out[n] = s
		n++
	}
	return out[:n]
}

// complement is the parts of [0, size) that done leaves uncovered.
func complement(size int64, done []span) []span {
	var gaps []span
	var at int64
	for _, s := range merge(done) {
		if s.Start >= size {
			break
		}
		if s.Start > at {
			gaps = append(gaps, span{at, s.Start})
		}
		at = s.End
	}
	if at < size {
		gaps = append(gaps, span{at, size})
	}
	return gaps
}

// queue hands out the missing parts of a file one unit at a time and keeps
// track of which bytes have arrived.
type queue struct {
	mu       sync.Mutex
	gaps     []span // not handed out yet; in file order, failed spans first
	done     []span // arrived, merged
	inflight int
}

// newQueue makes a queue for a file of size bytes of which done has arrived.
func newQueue(size int64, done []span) *queue {
	return &queue{gaps: complement(size, done), done: merge(done)}
}

// next carves up to unit bytes off the first gap and counts them in flight.
// ok is false when nothing is left to hand out.
func (q *queue) next(unit int64) (s span, ok bool) {
	if unit <= 0 {
		unit = 1
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.gaps) == 0 {
		return span{}, false
	}
	g := &q.gaps[0]
	s = span{g.Start, min(g.Start+unit, g.End)}
	g.Start = s.End
	if g.Start >= g.End {
		q.gaps = q.gaps[1:]
	}
	q.inflight++
	return s, true
}

// finish records s as arrived.
func (q *queue) finish(s span) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.done = merge(append(q.done, s))
	q.inflight--
}

// fail puts s back at the front so it is handed out again before anything else.
func (q *queue) fail(s span) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.gaps = append([]span{s}, q.gaps...)
	q.inflight--
}

// snapshot is a copy of the arrived spans, merged.
func (q *queue) snapshot() []span {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.done)
}

// remaining is the number of bytes not handed out yet. Bytes in flight are
// not counted: they belong to a worker until it finishes or fails them.
func (q *queue) remaining() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	var n int64
	for _, g := range q.gaps {
		n += g.End - g.Start
	}
	return n
}

// complete reports whether every byte has arrived: no gaps and nothing in flight.
func (q *queue) complete() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.gaps) == 0 && q.inflight == 0
}

// inFlight is the number of spans handed out and neither finished nor failed.
func (q *queue) inFlight() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.inflight
}
