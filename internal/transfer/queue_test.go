package transfer

import (
	"slices"
	"testing"
)

func TestMerge(t *testing.T) {
	tests := []struct {
		name string
		in   []span
		want []span
	}{
		{"nil", nil, nil},
		{"unsorted", []span{{10, 20}, {0, 5}}, []span{{0, 5}, {10, 20}}},
		{"overlapping", []span{{0, 10}, {5, 15}}, []span{{0, 15}}},
		{"contained", []span{{0, 20}, {5, 10}}, []span{{0, 20}}},
		{"touching", []span{{0, 5}, {5, 10}}, []span{{0, 10}}},
		{"empty dropped", []span{{3, 3}, {0, 2}, {7, 5}}, []span{{0, 2}}},
		{"chain", []span{{20, 30}, {0, 10}, {10, 20}, {40, 50}}, []span{{0, 30}, {40, 50}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := merge(tt.in); !slices.Equal(got, tt.want) {
				t.Errorf("merge(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestComplement(t *testing.T) {
	tests := []struct {
		name string
		size int64
		done []span
		want []span
	}{
		{"nothing done", 100, nil, []span{{0, 100}}},
		{"start", 100, []span{{0, 30}}, []span{{30, 100}}},
		{"middle", 100, []span{{30, 60}}, []span{{0, 30}, {60, 100}}},
		{"end", 100, []span{{70, 100}}, []span{{0, 70}}},
		{"everything", 100, []span{{0, 100}}, nil},
		{"pieces", 100, []span{{50, 60}, {0, 10}, {10, 20}}, []span{{20, 50}, {60, 100}}},
		{"beyond size", 100, []span{{90, 120}}, []span{{0, 90}}},
		{"empty file", 0, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := complement(tt.size, tt.done); !slices.Equal(got, tt.want) {
				t.Errorf("complement(%d, %v) = %v, want %v", tt.size, tt.done, got, tt.want)
			}
		})
	}
}

// take is next with the result checked against want.
func take(t *testing.T, q *queue, unit int64, want span) span {
	t.Helper()
	got, ok := q.next(unit)
	if !ok || got != want {
		t.Fatalf("next(%d) = %v, %v, want %v, true", unit, got, ok, want)
	}
	return got
}

func TestQueueCarvesAcrossGaps(t *testing.T) {
	q := newQueue(100, []span{{20, 30}})
	if got := q.remaining(); got != 90 {
		t.Fatalf("remaining = %d, want 90", got)
	}
	take(t, q, 50, span{0, 20}) // the unit is larger than the first gap
	if got := q.remaining(); got != 70 {
		t.Fatalf("remaining after first unit = %d, want 70", got)
	}
	take(t, q, 50, span{30, 80})
	take(t, q, 50, span{80, 100})
	if s, ok := q.next(50); ok {
		t.Fatalf("next on an empty queue = %v, true", s)
	}
	if got := q.inFlight(); got != 3 {
		t.Fatalf("inFlight = %d, want 3", got)
	}
}

func TestQueueUnitFloor(t *testing.T) {
	q := newQueue(10, nil)
	take(t, q, 0, span{0, 1})
	take(t, q, -5, span{1, 2})
}

func TestQueueResume(t *testing.T) {
	done := []span{{60, 100}, {0, 40}}
	q := newQueue(100, done)
	take(t, q, 1000, span{40, 60})
	if s, ok := q.next(1000); ok {
		t.Fatalf("next after the only gap = %v, true", s)
	}
	if got, want := q.snapshot(), []span{{0, 40}, {60, 100}}; !slices.Equal(got, want) {
		t.Fatalf("snapshot = %v, want %v", got, want)
	}
	q.finish(span{40, 60})
	if got, want := q.snapshot(), []span{{0, 100}}; !slices.Equal(got, want) {
		t.Fatalf("snapshot after finish = %v, want %v", got, want)
	}
	if !q.complete() {
		t.Fatal("complete = false after the last span")
	}
}

func TestQueueFailReissuesFirst(t *testing.T) {
	q := newQueue(100, nil)
	a := take(t, q, 10, span{0, 10})
	take(t, q, 10, span{10, 20})
	q.fail(a)
	if got := q.inFlight(); got != 1 {
		t.Fatalf("inFlight after fail = %d, want 1", got)
	}
	if got := q.remaining(); got != 90 {
		t.Fatalf("remaining after fail = %d, want 90", got)
	}
	take(t, q, 10, a)
	take(t, q, 10, span{20, 30})
	if got := q.inFlight(); got != 3 {
		t.Fatalf("inFlight = %d, want 3", got)
	}
}

func TestQueueFinishMerges(t *testing.T) {
	q := newQueue(30, nil)
	a := take(t, q, 10, span{0, 10})
	b := take(t, q, 10, span{10, 20})
	q.finish(b)
	if got, want := q.snapshot(), []span{{10, 20}}; !slices.Equal(got, want) {
		t.Fatalf("snapshot = %v, want %v", got, want)
	}
	q.finish(a)
	if got, want := q.snapshot(), []span{{0, 20}}; !slices.Equal(got, want) {
		t.Fatalf("snapshot = %v, want %v", got, want)
	}
	snap := q.snapshot()
	snap[0].End = 5 // a copy, not the queue's own slice
	if got, want := q.snapshot(), []span{{0, 20}}; !slices.Equal(got, want) {
		t.Fatalf("snapshot after editing a copy = %v, want %v", got, want)
	}
}

func TestQueueComplete(t *testing.T) {
	q := newQueue(10, nil)
	if q.complete() {
		t.Fatal("complete with a gap")
	}
	s := take(t, q, 10, span{0, 10})
	if q.complete() {
		t.Fatal("complete with a span in flight")
	}
	q.fail(s)
	if q.complete() {
		t.Fatal("complete with a failed span back in the gaps")
	}
	take(t, q, 10, s)
	q.finish(s)
	if !q.complete() {
		t.Fatal("not complete after the last span")
	}
	if !newQueue(0, nil).complete() {
		t.Fatal("empty file not complete")
	}
}
