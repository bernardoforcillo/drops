package pg_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// flakyPlanDriver fails the first n calls with the cached-plan error
// and succeeds afterwards, counting every call it saw.
type flakyPlanDriver struct {
	failures int
	execs    int
	queries  int
	err      error
}

func (d *flakyPlanDriver) fail() error {
	if d.failures > 0 {
		d.failures--
		return d.err
	}
	return nil
}

func (d *flakyPlanDriver) Exec(_ context.Context, _ string, _ ...any) (drops.Result, error) {
	d.execs++
	if err := d.fail(); err != nil {
		return nil, err
	}
	return nil, nil
}

func (d *flakyPlanDriver) Query(_ context.Context, _ string, _ ...any) (drops.Rows, error) {
	d.queries++
	if err := d.fail(); err != nil {
		return nil, err
	}
	return nil, nil
}

func (d *flakyPlanDriver) Begin(_ context.Context) (drops.Tx, error) { return nil, nil }

func cachedPlanErr() error {
	return &sqlStateOnlyError{
		code: "0A000",
		msg:  `ERROR: cached plan must not change result type (SQLSTATE 0A000)`,
	}
}

func TestRetryCachedPlansRetriesOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(db *pg.DB) error
		seen func(*flakyPlanDriver) int
	}{
		{
			"exec",
			func(db *pg.DB) error { _, err := db.Exec(context.Background(), "SELECT 1"); return err },
			func(d *flakyPlanDriver) int { return d.execs },
		},
		{
			"query",
			func(db *pg.DB) error { _, err := db.Query(context.Background(), "SELECT 1"); return err },
			func(d *flakyPlanDriver) int { return d.queries },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := &flakyPlanDriver{failures: 1, err: cachedPlanErr()}
			db := pg.New(pg.RetryCachedPlans(raw))
			if err := tc.run(db); err != nil {
				t.Fatalf("statement failed after retry: %v", err)
			}
			if got := tc.seen(raw); got != 2 {
				t.Errorf("driver saw %d calls, want 2 (original + one retry)", got)
			}
		})
	}
}

// A second identical failure is a driver re-sending an invalidated
// prepared statement. Surfacing it beats looping on it.
func TestRetryCachedPlansStopsAfterOneRetry(t *testing.T) {
	raw := &flakyPlanDriver{failures: 5, err: cachedPlanErr()}
	db := pg.New(pg.RetryCachedPlans(raw))
	_, err := db.Exec(context.Background(), "SELECT 1")
	if err == nil {
		t.Fatal("expected the second failure to surface")
	}
	if !pg.IsCachedPlanChanged(err) {
		t.Errorf("surfaced error lost its classification: %v", err)
	}
	if raw.execs != 2 {
		t.Errorf("driver saw %d calls, want exactly 2", raw.execs)
	}
}

// 0A000 mostly means what it says. Retrying an unsupported feature is
// pointless, and retrying anything else is how a wrapper hides bugs.
func TestRetryCachedPlansLeavesOtherErrorsAlone(t *testing.T) {
	for _, raw := range []error{
		&sqlStateOnlyError{code: "0A000", msg: "ERROR: DEFERRABLE is not supported here"},
		&sqlStateOnlyError{code: "23505", msg: "duplicate key"},
		errors.New("no SQLSTATE at all"),
	} {
		drv := &flakyPlanDriver{failures: 5, err: raw}
		db := pg.New(pg.RetryCachedPlans(drv))
		if _, err := db.Exec(context.Background(), "SELECT 1"); err == nil {
			t.Fatalf("%v: expected failure", raw)
		}
		if drv.execs != 1 {
			t.Errorf("%v: driver saw %d calls, want 1 (no retry)", raw, drv.execs)
		}
	}
}

func TestRetryCachedPlansNilDriverPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected a panic on a nil driver")
		}
	}()
	pg.RetryCachedPlans(nil)
}
