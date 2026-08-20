package mirror_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/mirror"
)

// The pump is the long-running loop of the mirror package: started
// once at boot with `go pump.Run(ctx)` and expected to stop when the
// process's root context is cancelled. It is not started by a
// constructor and hands back no stop function, so the context is the
// only handle anyone has on it — which makes honouring the context the
// whole of its concurrency contract.

// blockingSource never has anything to fetch, so Run spends its life
// in the idle sleep. That is where cancellation has to be noticed: a
// sleep that only watches the clock leaves the goroutine alive for a
// whole idle interval past shutdown, and with a long interval, for
// ever in practice.
type blockingSource struct{ fetched chan struct{} }

func (s *blockingSource) Fetch(ctx context.Context, _ int) ([]mirror.Change, func(context.Context) error, error) {
	select {
	case s.fetched <- struct{}{}:
	default:
	}
	return nil, nil, nil
}

type nopSink struct{}

func (nopSink) Name() string                                 { return "nop" }
func (nopSink) Apply(context.Context, []mirror.Change) error { return nil }

func TestPumpRunStopsWhenItsContextIsCancelled(t *testing.T) {
	src := &blockingSource{fetched: make(chan struct{}, 1)}
	p, err := mirror.NewPump(src, nopSink{})
	if err != nil {
		t.Fatalf("NewPump: %v", err)
	}
	p.WithIdleInterval(time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	select {
	case <-src.fetched:
	case <-time.After(time.Second):
		t.Fatal("the pump never fetched")
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v after cancellation, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after its context was cancelled; the idle sleep ignores it")
	}
}

// The retry path has its own sleep, and it is the one that runs when
// the far side is down — which is exactly when a deploy is most likely
// to be cancelling the process.
type failingSource struct{ err error }

func (s *failingSource) Fetch(context.Context, int) ([]mirror.Change, func(context.Context) error, error) {
	return nil, nil, s.err
}

func TestPumpRunStopsWhileBackingOff(t *testing.T) {
	src := &failingSource{err: errors.New("source is down")}
	p, err := mirror.NewPump(src, nopSink{})
	if err != nil {
		t.Fatalf("NewPump: %v", err)
	}
	p.WithBackoff(func(int) time.Duration { return time.Hour })

	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan struct{}, 1)
	p.OnError(func(error) {
		select {
		case errs <- struct{}{}:
		default:
		}
	})

	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	select {
	case <-errs:
	case <-time.After(time.Second):
		t.Fatal("the pump never reported the fetch failure")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after its context was cancelled; the backoff sleep ignores it")
	}
}
