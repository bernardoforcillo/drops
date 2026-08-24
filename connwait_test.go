package drops_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
)

// The whole point of the WaitKnown flag: a slot nobody filled in must
// not read as a pool that handed a connection over instantly.
func TestConnWaitUnreportedIsUnknownNotZero(t *testing.T) {
	_, waited := drops.NewConnWait(context.Background())
	d, ok := waited()
	if ok {
		t.Fatalf("nobody reported, yet the slot claims to know: %v", d)
	}
	if d != 0 {
		t.Errorf("unknown wait should read as zero, got %v", d)
	}
}

func TestConnWaitRoundTrip(t *testing.T) {
	ctx, waited := drops.NewConnWait(context.Background())
	drops.ReportConnWait(ctx, 7*time.Millisecond)
	d, ok := waited()
	if !ok || d != 7*time.Millisecond {
		t.Fatalf("got %v ok=%v, want 7ms true", d, ok)
	}
}

// A driver that got a connection with no wait at all reports zero, and
// that zero is a measurement — ok must be true.
func TestConnWaitZeroIsAMeasurement(t *testing.T) {
	ctx, waited := drops.NewConnWait(context.Background())
	drops.ReportConnWait(ctx, 0)
	d, ok := waited()
	if !ok {
		t.Fatal("a reported zero wait is knowledge, not the absence of it")
	}
	if d != 0 {
		t.Errorf("got %v, want 0", d)
	}
}

// pg captures the WAL position with a second statement on the same
// context after a write. That statement acquires a connection too, and
// its wait must not be added to the wait of the statement being timed.
func TestConnWaitKeepsTheFirstReport(t *testing.T) {
	ctx, waited := drops.NewConnWait(context.Background())
	drops.ReportConnWait(ctx, 3*time.Millisecond)
	drops.ReportConnWait(ctx, 50*time.Millisecond)
	if d, _ := waited(); d != 3*time.Millisecond {
		t.Errorf("got %v, want the first report of 3ms", d)
	}
}

// Drivers report unconditionally; a context with no slot has to absorb
// that silently rather than panic.
func TestReportConnWaitWithoutSlotIsANoOp(t *testing.T) {
	drops.ReportConnWait(context.Background(), time.Second)
}

func TestConnWaitIsRaceFree(t *testing.T) {
	ctx, waited := drops.NewConnWait(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			drops.ReportConnWait(ctx, time.Duration(i)*time.Millisecond)
			_, _ = waited()
		}(i)
	}
	wg.Wait()
	if _, ok := waited(); !ok {
		t.Error("at least one report should have landed")
	}
}

func TestQueryEventQueryDuration(t *testing.T) {
	cases := []struct {
		name  string
		event drops.QueryEvent
		want  time.Duration
		ok    bool
	}{
		{
			name:  "unmeasured returns the total and says so",
			event: drops.QueryEvent{Duration: 10 * time.Millisecond},
			want:  10 * time.Millisecond,
			ok:    false,
		},
		{
			name: "measured subtracts the queue time",
			event: drops.QueryEvent{
				Duration: 10 * time.Millisecond, WaitDuration: 4 * time.Millisecond, WaitKnown: true,
			},
			want: 6 * time.Millisecond,
			ok:   true,
		},
		{
			name: "a wait longer than the total clamps rather than going negative",
			event: drops.QueryEvent{
				Duration: 1 * time.Millisecond, WaitDuration: 5 * time.Millisecond, WaitKnown: true,
			},
			want: 0,
			ok:   true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := c.event.QueryDuration()
			if got != c.want || ok != c.ok {
				t.Errorf("got (%v, %v), want (%v, %v)", got, ok, c.want, c.ok)
			}
		})
	}
}
