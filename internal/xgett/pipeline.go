package xgett

import (
	"context"

	"golang.org/x/sync/errgroup"
)

// ordered hands the items next produces to work on w goroutines, and the
// results to emit in the order the items were produced. next and emit each
// run on one goroutine of their own. At most 2w items wait for emit, which is
// what keeps memory bounded when emit is the slow part. The first error stops
// everything, and ordered returns only when every goroutine has finished, so
// the caller may clean up straight after.
func ordered[I, O any](ctx context.Context, w int, n uint64, next func(k uint64) (I, error), work func(I) (O, error), emit func(O) error) error {
	type job struct {
		in  I
		out chan O
	}
	g, ctx := errgroup.WithContext(ctx)
	jobs := make(chan job, w)
	order := make(chan chan O, 2*w)

	g.Go(func() error {
		defer close(jobs)
		defer close(order)
		for k := uint64(0); k < n; k++ {
			in, err := next(k)
			if err != nil {
				return err
			}
			j := job{in, make(chan O, 1)} // room for the result, so work never waits
			select {
			case order <- j.out:
			case <-ctx.Done():
				return ctx.Err()
			}
			select {
			case jobs <- j:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
	for i := 0; i < w; i++ {
		g.Go(func() error {
			for j := range jobs {
				if err := ctx.Err(); err != nil {
					return err
				}
				out, err := work(j.in)
				if err != nil {
					return err
				}
				j.out <- out
			}
			return nil
		})
	}
	g.Go(func() error {
		for out := range order {
			select {
			case o := <-out:
				if err := emit(o); err != nil {
					return err
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
	return g.Wait()
}

// pool keeps block buffers for reuse. All are the same size and are handed
// out and taken back whole; when the pool is empty a new one is made, and
// when it is full a returned one is dropped.
type pool struct {
	free chan []byte
	size int
}

func newPool(keep, size int) *pool { return &pool{make(chan []byte, keep), size} }

func (p *pool) get() []byte {
	select {
	case b := <-p.free:
		return b
	default:
		return make([]byte, p.size)
	}
}

// put takes back a buffer from get, or any slice of it that starts where it
// starts.
func (p *pool) put(b []byte) {
	if cap(b) < p.size {
		return
	}
	select {
	case p.free <- b[:p.size]:
	default:
	}
}
