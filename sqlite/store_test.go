package sqlite_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/sqlite"
)

func TestIdempotencyEmptyKey(t *testing.T) {
	db := sqlite.New(&entDriver{})
	store := sqlite.NewIdempotencyStore(db, "idem", 0)
	if _, err := store.Run(context.Background(), "", func(*sqlite.DB) ([]byte, error) {
		return nil, nil
	}); !errors.Is(err, sqlite.ErrEmptyKey) {
		t.Errorf("want ErrEmptyKey, got %v", err)
	}
}

func TestIdempotencyFirstRun(t *testing.T) {
	drv := &entDriver{}
	db := sqlite.New(drv)
	store := sqlite.NewIdempotencyStore(db, "idem", 0)

	ran := false
	out, err := store.Run(context.Background(), "k1", func(*sqlite.DB) ([]byte, error) {
		ran = true
		return []byte("payload"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ran || string(out) != "payload" {
		t.Errorf("closure ran=%v out=%s", ran, out)
	}
	joined := strings.Join(drv.queries, "\n")
	for _, want := range []string{"INSERT OR IGNORE INTO", `SELECT "response", "completed"`, `UPDATE "idem" SET "response"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in issued SQL:\n%s", want, joined)
		}
	}
}

func TestBackfillRunsToCompletion(t *testing.T) {
	drv := &entDriver{}
	db := sqlite.New(drv)

	var progress int64
	batches := [][]int64{{1, 2, 3}, {4, 5}, {}}
	call := 0
	bf := sqlite.NewBackfill(db, "job1").
		Throttle(0).
		Fetch(func(_ context.Context, lastID int64, _ int) ([]int64, int64, error) {
			b := batches[call]
			call++
			if len(b) == 0 {
				return nil, lastID, nil
			}
			return b, b[len(b)-1], nil
		}).
		Process(func(context.Context, *sqlite.DB, []int64) error { return nil }).
		OnProgress(func(processed, _ int64) { progress = processed })

	if err := bf.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if progress != 5 {
		t.Errorf("processed %d, want 5", progress)
	}
	// The final statement must be an upsert that stamps completedAt.
	joined := strings.Join(drv.queries, "\n")
	if !strings.Contains(joined, `"completedAt"`) {
		t.Errorf("completion not persisted:\n%s", joined)
	}
}
