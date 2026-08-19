package mirror

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Pump moves changes from a [Source] into one or more [Sink]s.
//
// It is deliberately small: fetch a batch, apply it to every sink,
// acknowledge it, repeat. The interesting decisions are in what it
// refuses to do — it does not acknowledge a batch any sink rejected,
// it does not apply to the remaining sinks after one fails, and it
// does not reorder changes.
type Pump struct {
	src      Source
	sinks    []Sink
	batch    int
	idle     time.Duration
	onError  func(error)
	maxRetry int
	backoff  func(attempt int) time.Duration
	now      func() time.Time
}

// NewPump wires a source to sinks.
func NewPump(src Source, sinks ...Sink) (*Pump, error) {
	if src == nil {
		return nil, errors.New("drops/mirror: NewPump needs a source")
	}
	if len(sinks) == 0 {
		return nil, errors.New("drops/mirror: NewPump needs at least one sink")
	}
	for i, s := range sinks {
		if s == nil {
			return nil, fmt.Errorf("drops/mirror: NewPump sink %d is nil", i)
		}
	}
	return &Pump{
		src:      src,
		sinks:    sinks,
		batch:    500,
		idle:     time.Second,
		maxRetry: 5,
		backoff:  func(attempt int) time.Duration { return time.Duration(1<<attempt) * 100 * time.Millisecond },
		now:      time.Now,
	}, nil
}

// WithBatch caps how many changes are fetched and applied at once.
// Defaults to 500 — large enough that a ClickHouse insert is worth
// making, small enough that a retry is cheap.
func (p *Pump) WithBatch(n int) *Pump {
	if n > 0 {
		p.batch = n
	}
	return p
}

// WithIdleInterval sets how long to wait after an empty fetch.
// Defaults to a second.
func (p *Pump) WithIdleInterval(d time.Duration) *Pump {
	if d > 0 {
		p.idle = d
	}
	return p
}

// WithMaxAttempts caps the retries of a failing batch before Run
// gives up and returns the error. Defaults to 5. Zero means retry
// forever, which is the right setting for a mirror that must not skip
// a change — the alternative to blocking is diverging.
func (p *Pump) WithMaxAttempts(n int) *Pump {
	if n >= 0 {
		p.maxRetry = n
	}
	return p
}

// WithBackoff overrides the delay between retries of a batch.
func (p *Pump) WithBackoff(fn func(attempt int) time.Duration) *Pump {
	if fn != nil {
		p.backoff = fn
	}
	return p
}

// OnError registers a callback for every failed attempt, including
// ones that are about to be retried. Without it failures are silent
// until the attempt budget runs out.
func (p *Pump) OnError(fn func(error)) *Pump {
	p.onError = fn
	return p
}

// Step performs a single fetch-apply-commit cycle and reports how many
// changes were handled. Zero means the source was idle.
//
// Exposed separately from Run so a caller can drive the pump from
// their own scheduler, and so tests do not have to race a goroutine.
func (p *Pump) Step(ctx context.Context) (int, error) {
	changes, commit, err := p.src.Fetch(ctx, p.batch)
	if err != nil {
		return 0, err
	}
	if len(changes) == 0 {
		if commit != nil {
			// A fetch can return no mirror changes but still have
			// consumed source rows that need acknowledging.
			return 0, commit(ctx)
		}
		return 0, nil
	}
	now := p.now()
	for i := range changes {
		changes[i] = changes[i].Normalized(now)
	}
	for _, s := range p.sinks {
		if err := s.Apply(ctx, changes); err != nil {
			// Stop at the first failure. Carrying on would
			// acknowledge nothing anyway, and re-applying the
			// batch is how it recovers — so the sinks that already
			// took it will see it again, which is exactly what
			// their idempotence is for.
			return 0, fmt.Errorf("drops/mirror: sink %s: %w", s.Name(), err)
		}
	}
	if commit != nil {
		if err := commit(ctx); err != nil {
			// The batch is applied but not acknowledged, so it will
			// be replayed. That is the at-least-once seam, and it
			// is why sinks must be idempotent.
			return 0, fmt.Errorf("drops/mirror: commit: %w", err)
		}
	}
	return len(changes), nil
}

// Run pumps until ctx is cancelled.
//
// A batch that fails is retried with backoff rather than skipped: the
// point of a mirror is that it does not silently diverge, so falling
// behind is the correct failure mode and dropping a change is not.
// Run returns the last error once the attempt budget is exhausted.
func (p *Pump) Run(ctx context.Context) error {
	attempt := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		n, err := p.Step(ctx)
		switch {
		case err != nil:
			if p.onError != nil {
				p.onError(err)
			}
			attempt++
			if p.maxRetry > 0 && attempt >= p.maxRetry {
				return err
			}
			if !sleepCtx(ctx, p.backoff(attempt)) {
				return nil
			}
		case n == 0:
			attempt = 0
			if !sleepCtx(ctx, p.idle) {
				return nil
			}
		default:
			attempt = 0
		}
	}
}

// sleepCtx waits for d, reporting false if ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
