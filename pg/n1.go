package pg

import (
	"context"
	"sort"
	"sync"

	"github.com/bernardoforcillo/drops"
)

// N+1 detection works by counting how often each SQL skeleton runs
// inside a tracked context. drops queries are parametrised, so the
// SAME SQL string fired with different args is the classic N+1
// signature: a parent loop fetching child rows one at a time.
//
// Usage:
//
//	// Once, at DB setup:
//	db := pg.New(drv).WithHook(pg.N1Hook)
//
//	// In each request handler / job:
//	ctx, finish := pg.WithN1Detector(ctx)
//	defer func() {
//	    if r := finish(5); !r.IsClean() {
//	        log.Warn("N+1 candidates", "patterns", r.Patterns)
//	    }
//	}()
//
// The detector is opt-in per context. Without WithN1Detector the
// hook is a no-op, so attaching it to a global DB carries no cost
// for untracked traffic.

type n1ContextKey int

const n1TrackerKey n1ContextKey = 1

type n1Tracker struct {
	mu     sync.Mutex
	counts map[string]int
}

func (t *n1Tracker) record(sql string) {
	t.mu.Lock()
	t.counts[sql]++
	t.mu.Unlock()
}

func (t *n1Tracker) report(threshold int) N1Report {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := N1Report{Threshold: threshold}
	for sql, count := range t.counts {
		r.Total += count
		if count >= threshold {
			r.Patterns = append(r.Patterns, N1Pattern{SQL: sql, Count: count})
		}
	}
	// Stable order — busiest first, then by SQL text — keeps the
	// report deterministic for tests and tooling.
	sort.Slice(r.Patterns, func(i, j int) bool {
		if r.Patterns[i].Count != r.Patterns[j].Count {
			return r.Patterns[i].Count > r.Patterns[j].Count
		}
		return r.Patterns[i].SQL < r.Patterns[j].SQL
	})
	return r
}

// N1Report summarises the queries observed during a tracked
// context's lifetime. Patterns lists every SQL skeleton that fired
// at least Threshold times.
type N1Report struct {
	Threshold int
	Total     int
	Patterns  []N1Pattern
}

// N1Pattern is a single SQL skeleton and the number of times it ran.
type N1Pattern struct {
	SQL   string
	Count int
}

// IsClean reports whether no pattern crossed the threshold.
func (r N1Report) IsClean() bool { return len(r.Patterns) == 0 }

// WithN1Detector returns a derived context that records every SQL
// statement issued through pg.DB. Call the returned finisher with a
// threshold to produce the report; typically wired up in a `defer`
// so it runs once at the end of the request / job.
func WithN1Detector(ctx context.Context) (context.Context, func(threshold int) N1Report) {
	t := &n1Tracker{counts: map[string]int{}}
	ctx2 := context.WithValue(ctx, n1TrackerKey, t)
	return ctx2, func(threshold int) N1Report {
		if threshold < 2 {
			threshold = 2
		}
		return t.report(threshold)
	}
}

// N1Hook is a drops.Hook that records every query into the tracker
// attached to ctx by WithN1Detector. Without a tracker the hook
// is a no-op, so attaching it globally to the DB is free for
// untracked traffic.
func N1Hook(ctx context.Context, e drops.QueryEvent) {
	t, ok := ctx.Value(n1TrackerKey).(*n1Tracker)
	if !ok || t == nil {
		return
	}
	t.record(e.SQL)
}
