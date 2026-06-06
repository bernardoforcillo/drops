package pg_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// idemDriver fakes the row store: tracks INSERTs, returns canned
// SELECTs, and applies UPDATEs in-memory so the test can verify the
// completed flag flips correctly.
type idemDriver struct {
	mu        sync.Mutex
	rows      map[string]*idemRow
	queries   []string
	completed atomic.Bool
}

type idemRow struct {
	response  []byte
	completed bool
	expiresAt time.Time
}

func newIdemDriver() *idemDriver { return &idemDriver{rows: map[string]*idemRow{}} }

func (d *idemDriver) Exec(_ context.Context, sql string, args ...any) (drops.Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.queries = append(d.queries, sql)
	switch {
	case strings.HasPrefix(sql, "\n\t\t\tINSERT") || strings.Contains(sql, "INSERT INTO"):
		key, _ := args[0].(string)
		if _, ok := d.rows[key]; !ok {
			expires := time.Now().Add(time.Hour)
			if t, ok := args[1].(time.Time); ok {
				expires = t
			}
			d.rows[key] = &idemRow{expiresAt: expires}
		}
	case strings.Contains(sql, "UPDATE"):
		response, _ := args[0].([]byte)
		key, _ := args[1].(string)
		if row, ok := d.rows[key]; ok {
			row.response = response
			row.completed = true
		}
	case strings.Contains(sql, "DELETE"):
		// no-op for tests
	}
	return idemResult{}, nil
}

type idemResult struct{}

func (idemResult) RowsAffected() (int64, error) { return 0, nil }

func (d *idemDriver) Query(_ context.Context, sql string, args ...any) (drops.Rows, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.queries = append(d.queries, sql)
	key, _ := args[0].(string)
	row := d.rows[key]
	if row == nil {
		return &fakeRows{cols: []string{"response", "completed"}}, nil
	}
	return &fakeRows{
		cols: []string{"response", "completed"},
		data: [][]any{{row.response, row.completed}},
	}, nil
}
func (d *idemDriver) Begin(_ context.Context) (drops.Tx, error) { return idemTx{drv: d}, nil }

type idemTx struct{ drv *idemDriver }

func (tx idemTx) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	return tx.drv.Exec(ctx, sql, args...)
}
func (tx idemTx) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	return tx.drv.Query(ctx, sql, args...)
}
func (tx idemTx) Begin(ctx context.Context) (drops.Tx, error) { return tx.drv.Begin(ctx) }
func (idemTx) Commit(_ context.Context) error                 { return nil }
func (idemTx) Rollback(_ context.Context) error               { return nil }

func TestIdempotencyRunCachesResponse(t *testing.T) {
	drv := newIdemDriver()
	db := pg.New(drv)
	store := pg.NewIdempotencyStore(db, "idempotency_keys", time.Hour)

	called := atomic.Int32{}
	wantResp := []byte(`{"orderId":42}`)

	// First call: runs the closure, stores response.
	got1, err := store.Run(context.Background(), "POST-orders-7", func(tx *pg.DB) ([]byte, error) {
		called.Add(1)
		return wantResp, nil
	})
	if err != nil {
		t.Fatalf("Run #1: %v", err)
	}
	if string(got1) != string(wantResp) {
		t.Errorf("Run #1 response: %s", got1)
	}

	// Second call with the SAME key: skips the closure, returns the
	// cached response.
	got2, err := store.Run(context.Background(), "POST-orders-7", func(tx *pg.DB) ([]byte, error) {
		called.Add(1)
		return []byte("should not run"), nil
	})
	if err != nil {
		t.Fatalf("Run #2: %v", err)
	}
	if string(got2) != string(wantResp) {
		t.Errorf("Run #2 should return cached response, got: %s", got2)
	}
	if called.Load() != 1 {
		t.Errorf("closure must run exactly once across two calls, got %d", called.Load())
	}
}

func TestIdempotencyRunRollsBackOnError(t *testing.T) {
	drv := newIdemDriver()
	db := pg.New(drv)
	store := pg.NewIdempotencyStore(db, "idempotency_keys", time.Hour)

	boom := errors.New("transient")
	_, err := store.Run(context.Background(), "POST-orders-8", func(tx *pg.DB) ([]byte, error) {
		return nil, boom
	})
	if !errors.Is(err, boom) {
		t.Errorf("expected boom, got %v", err)
	}

	// Subsequent call with the same key should retry the closure
	// (the failed claim was rolled back).
	called := atomic.Int32{}
	wantResp := []byte(`{"ok":true}`)
	got, err := store.Run(context.Background(), "POST-orders-8", func(tx *pg.DB) ([]byte, error) {
		called.Add(1)
		return wantResp, nil
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if string(got) != string(wantResp) {
		t.Errorf("retry response: %s", got)
	}
	if called.Load() != 1 {
		t.Errorf("retry should invoke closure once, got %d", called.Load())
	}
}

func TestIdempotencyEmptyKeyRejected(t *testing.T) {
	store := pg.NewIdempotencyStore(pg.New(newIdemDriver()), "k", time.Hour)
	_, err := store.Run(context.Background(), "", func(tx *pg.DB) ([]byte, error) {
		return nil, nil
	})
	if !errors.Is(err, pg.ErrEmptyKey) {
		t.Errorf("expected ErrEmptyKey, got %v", err)
	}
}

func TestRunJSONRoundTrip(t *testing.T) {
	drv := newIdemDriver()
	db := pg.New(drv)
	store := pg.NewIdempotencyStore(db, "k", time.Hour)

	type res struct {
		OrderID int    `json:"orderId"`
		State   string `json:"state"`
	}

	// First call: runs the closure.
	got1, err := pg.RunJSON[res](store, context.Background(), "key-1", func(tx *pg.DB) (res, error) {
		return res{OrderID: 42, State: "captured"}, nil
	})
	if err != nil {
		t.Fatalf("RunJSON #1: %v", err)
	}
	if got1.OrderID != 42 || got1.State != "captured" {
		t.Errorf("first response wrong: %+v", got1)
	}

	// Second call: returns cached, no closure execution.
	called := atomic.Int32{}
	got2, err := pg.RunJSON[res](store, context.Background(), "key-1", func(tx *pg.DB) (res, error) {
		called.Add(1)
		return res{OrderID: 99, State: "wrong"}, nil
	})
	if err != nil {
		t.Fatalf("RunJSON #2: %v", err)
	}
	if got2.OrderID != 42 || got2.State != "captured" {
		t.Errorf("cached response wrong: %+v", got2)
	}
	if called.Load() != 0 {
		t.Error("cached call must not invoke closure")
	}
}

func TestIdempotencyTableDDL(t *testing.T) {
	tbl := pg.NewIdempotencyTable("idempotency_keys")
	if tbl.Name() != "idempotency_keys" {
		t.Errorf("table name: %s", tbl.Name())
	}
	cols := []string{"key", "response", "completed", "createdAt", "expiresAt"}
	for _, name := range cols {
		if tbl.Col(name) == nil {
			t.Errorf("expected column %q on idempotency table", name)
		}
	}
	if !tbl.Col("key").IsPrimaryKey() {
		t.Errorf(`"key" should be PRIMARY KEY`)
	}
}
