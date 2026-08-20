package mirror_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/mirror"
)

// fakeSource hands out prepared batches and records commits.
type fakeSource struct {
	batches   [][]mirror.Change
	pos       int
	committed int
	fetchErr  error
	commitErr error
}

func (s *fakeSource) Fetch(context.Context, int) ([]mirror.Change, func(context.Context) error, error) {
	if s.fetchErr != nil {
		return nil, nil, s.fetchErr
	}
	if s.pos >= len(s.batches) {
		return nil, nil, nil
	}
	b := s.batches[s.pos]
	s.pos++
	return b, func(context.Context) error {
		if s.commitErr != nil {
			return s.commitErr
		}
		s.committed++
		return nil
	}, nil
}

type recSink struct {
	name    string
	seen    [][]mirror.Change
	failFor int // fail this many times, then succeed
}

func (s *recSink) Name() string { return s.name }

// Version-aware, so it can stand in for ClickHouse as a fill-reseed
// target. It records rather than deduplicates; what the declaration
// buys these tests is the constructor check, and the ordering the
// declaration promises is asserted on the versions it recorded.
func (s *recSink) VersionAware() bool { return true }
func (s *recSink) Apply(_ context.Context, c []mirror.Change) error {
	if s.failFor > 0 {
		s.failFor--
		return errors.New("boom")
	}
	s.seen = append(s.seen, append([]mirror.Change(nil), c...))
	return nil
}

func chg(op mirror.Op, key string) mirror.Change {
	return mirror.Change{Op: op, Key: key, At: time.Unix(1700000000, 0)}
}

func TestPumpAppliesToEverySinkThenCommits(t *testing.T) {
	src := &fakeSource{batches: [][]mirror.Change{{chg(mirror.OpInsert, "1"), chg(mirror.OpInsert, "2")}}}
	a, b := &recSink{name: "a"}, &recSink{name: "b"}
	p, err := mirror.NewPump(src, a, b)
	if err != nil {
		t.Fatal(err)
	}
	n, err := p.Step(context.Background())
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if n != 2 {
		t.Errorf("handled %d changes, want 2", n)
	}
	if len(a.seen) != 1 || len(b.seen) != 1 {
		t.Errorf("both sinks should have seen the batch: a=%d b=%d", len(a.seen), len(b.seen))
	}
	if src.committed != 1 {
		t.Errorf("committed %d times, want 1", src.committed)
	}
}

// The guarantee that makes at-least-once safe: nothing is
// acknowledged unless every sink took the batch.
func TestPumpDoesNotCommitWhenASinkFails(t *testing.T) {
	src := &fakeSource{batches: [][]mirror.Change{{chg(mirror.OpInsert, "1")}}}
	ok := &recSink{name: "ok"}
	bad := &recSink{name: "bad", failFor: 1}
	p, _ := mirror.NewPump(src, ok, bad)

	if _, err := p.Step(context.Background()); err == nil {
		t.Fatal("expected the sink failure to surface")
	}
	if src.committed != 0 {
		t.Errorf("a failed batch must not be acknowledged, committed=%d", src.committed)
	}
}

// A sink after the failing one must not see the batch: the batch is
// going to be replayed in full, and applying it now would only widen
// the window in which the sinks disagree.
func TestPumpStopsAtTheFirstFailingSink(t *testing.T) {
	src := &fakeSource{batches: [][]mirror.Change{{chg(mirror.OpInsert, "1")}}}
	bad := &recSink{name: "bad", failFor: 1}
	later := &recSink{name: "later"}
	p, _ := mirror.NewPump(src, bad, later)

	if _, err := p.Step(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	if len(later.seen) != 0 {
		t.Errorf("sink after the failure should not have been applied to")
	}
}

func TestPumpNamesTheFailingSink(t *testing.T) {
	src := &fakeSource{batches: [][]mirror.Change{{chg(mirror.OpInsert, "1")}}}
	p, _ := mirror.NewPump(src, &recSink{name: "clickhouse:docs", failFor: 1})
	_, err := p.Step(context.Background())
	if err == nil || !contains(err.Error(), "clickhouse:docs") {
		t.Errorf("error should name the sink, got %v", err)
	}
}

// A commit failure leaves the batch applied but unacknowledged, so it
// replays. The pump must surface it rather than pretend it landed.
func TestPumpSurfacesCommitFailure(t *testing.T) {
	src := &fakeSource{
		batches:   [][]mirror.Change{{chg(mirror.OpInsert, "1")}},
		commitErr: errors.New("connection lost"),
	}
	p, _ := mirror.NewPump(src, &recSink{name: "a"})
	if _, err := p.Step(context.Background()); err == nil {
		t.Fatal("expected the commit failure to surface")
	}
}

func TestPumpIdleWhenSourceIsEmpty(t *testing.T) {
	p, _ := mirror.NewPump(&fakeSource{}, &recSink{name: "a"})
	n, err := p.Step(context.Background())
	if err != nil || n != 0 {
		t.Errorf("idle step = %d, %v", n, err)
	}
}

func TestPumpNormalizesBeforeApplying(t *testing.T) {
	src := &fakeSource{batches: [][]mirror.Change{{{Op: mirror.OpInsert, Key: "1"}}}}
	sink := &recSink{name: "a"}
	p, _ := mirror.NewPump(src, sink)
	if _, err := p.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := sink.seen[0][0]
	if got.Version == 0 {
		t.Error("sink received an unversioned change; ReplacingMergeTree would have nothing to order on")
	}
	if got.At.IsZero() {
		t.Error("sink received a change with no timestamp")
	}
}

func TestPumpRetriesThenSucceeds(t *testing.T) {
	src := &fakeSource{batches: [][]mirror.Change{{chg(mirror.OpInsert, "1")}}}
	sink := &recSink{name: "a", failFor: 2}
	p, _ := mirror.NewPump(src, sink)

	var attempts int
	for i := 0; i < 3; i++ {
		if _, err := p.Step(context.Background()); err != nil {
			attempts++
			// Replay the same batch, as a real source would.
			src.pos = 0
		}
	}
	if attempts != 2 {
		t.Errorf("expected 2 failed attempts, got %d", attempts)
	}
	if len(sink.seen) != 1 {
		t.Errorf("expected the batch to land once it stopped failing, got %d", len(sink.seen))
	}
}

func TestPumpRunStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p, _ := mirror.NewPump(&fakeSource{}, &recSink{name: "a"})
	p.WithIdleInterval(time.Millisecond)
	cancel()
	if err := p.Run(ctx); err != nil {
		t.Errorf("Run after cancel = %v, want nil", err)
	}
}

func TestPumpRunGivesUpAfterMaxAttempts(t *testing.T) {
	src := &fakeSource{fetchErr: errors.New("source down")}
	p, _ := mirror.NewPump(src, &recSink{name: "a"})
	p.WithMaxAttempts(2).WithBackoff(func(int) time.Duration { return 0 })

	var seen int
	p.OnError(func(error) { seen++ })
	if err := p.Run(context.Background()); err == nil {
		t.Fatal("expected Run to give up and return the error")
	}
	if seen != 2 {
		t.Errorf("OnError fired %d times, want 2", seen)
	}
}

func TestNewPumpValidates(t *testing.T) {
	if _, err := mirror.NewPump(nil, &recSink{}); err == nil {
		t.Error("expected an error for a nil source")
	}
	if _, err := mirror.NewPump(&fakeSource{}); err == nil {
		t.Error("expected an error for no sinks")
	}
	if _, err := mirror.NewPump(&fakeSource{}, nil); err == nil {
		t.Error("expected an error for a nil sink")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
