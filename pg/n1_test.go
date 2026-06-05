package pg_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

func TestN1DetectorFlagsRepeatedQueries(t *testing.T) {
	_, ent := entUsersSchema()
	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{cols: []string{"id", "name", "email"}}, nil
	}}
	db := pg.New(fd).WithHook(pg.N1Hook)

	ctx, finish := pg.WithN1Detector(context.Background())

	// Simulate an N+1: same query fired 6 times with different
	// args. drops parametrises, so the SQL skeleton is identical.
	for i := int64(1); i <= 6; i++ {
		if _, err := ent.Get(db, ctx, i); err != nil && !strings.Contains(err.Error(), "no rows") {
			t.Fatalf("Get: %v", err)
		}
	}
	r := finish(5)
	if r.IsClean() {
		t.Error("expected the repeated SELECT to trip the detector")
	}
	if r.Total < 6 {
		t.Errorf("expected at least 6 tracked queries, got %d", r.Total)
	}
	if len(r.Patterns) != 1 || r.Patterns[0].Count != 6 {
		t.Errorf("expected 1 pattern with count 6, got %+v", r.Patterns)
	}
}

func TestN1DetectorClean(t *testing.T) {
	_, ent := entUsersSchema()
	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{cols: []string{"id", "name", "email"}}, nil
	}}
	db := pg.New(fd).WithHook(pg.N1Hook)

	ctx, finish := pg.WithN1Detector(context.Background())
	_, _ = ent.Get(db, ctx, int64(1))
	_, _ = ent.Get(db, ctx, int64(2))
	r := finish(5)
	if !r.IsClean() {
		t.Errorf("2 queries should not trip the threshold of 5: %+v", r.Patterns)
	}
}

func TestN1DetectorWithoutWrappingIsNoop(t *testing.T) {
	_, ent := entUsersSchema()
	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{cols: []string{"id", "name", "email"}}, nil
	}}
	db := pg.New(fd).WithHook(pg.N1Hook)

	// Untracked context — the hook should never panic and never
	// record anything anywhere.
	for i := int64(1); i <= 10; i++ {
		_, _ = ent.Get(db, context.Background(), i)
	}
	// No assertion needed; we're checking the hook is safe on
	// untracked contexts.
}

func TestN1DetectorPatternsAreSortedByCount(t *testing.T) {
	_, ent := entUsersSchema()
	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{cols: []string{"id", "name", "email"}}, nil
	}}
	db := pg.New(fd).WithHook(pg.N1Hook)
	ctx, finish := pg.WithN1Detector(context.Background())

	// Pattern A: 7 calls; pattern B: 5 calls.
	for i := int64(1); i <= 7; i++ {
		_, _ = ent.Get(db, ctx, i)
	}
	for i := int64(1); i <= 5; i++ {
		_, _ = ent.Query(db).Limit(int64(i)).All(ctx)
	}
	r := finish(5)
	if len(r.Patterns) < 2 {
		t.Fatalf("expected 2 patterns to trip the threshold, got %d", len(r.Patterns))
	}
	if r.Patterns[0].Count < r.Patterns[1].Count {
		t.Errorf("patterns should be sorted by count desc, got %+v", r.Patterns)
	}
}
