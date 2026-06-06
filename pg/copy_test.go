package pg_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// copyingDriver implements both drops.Driver and pg.Copier so
// CopyFrom dispatches into Copy.
type copyingDriver struct {
	calls   atomic.Int32
	table   string
	cols    []string
	rows    [][]any
	returns int64
}

func (d *copyingDriver) Exec(context.Context, string, ...any) (drops.Result, error) {
	return nil, nil
}
func (d *copyingDriver) Query(context.Context, string, ...any) (drops.Rows, error) {
	return &fakeRows{}, nil
}
func (d *copyingDriver) Begin(context.Context) (drops.Tx, error) { return nil, nil }
func (d *copyingDriver) Copy(_ context.Context, table string, cols []string, rows [][]any) (int64, error) {
	d.calls.Add(1)
	d.table = table
	d.cols = cols
	d.rows = rows
	return d.returns, nil
}

func TestCopyFromDispatchesToCopier(t *testing.T) {
	_, ent := entUsersSchema()
	drv := &copyingDriver{returns: 3}
	db := pg.New(drv)
	rows := []entUser{
		{Name: "A", Email: "a@x"},
		{Name: "B", Email: "b@x"},
		{Name: "C", Email: "c@x"},
	}
	n, err := pg.CopyFrom(db, context.Background(), ent, rows)
	if err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}
	if n != 3 {
		t.Errorf("rows accepted: %d", n)
	}
	if drv.calls.Load() != 1 {
		t.Errorf("expected 1 Copy call, got %d", drv.calls.Load())
	}
	if drv.table != "users" {
		t.Errorf("table: %s", drv.table)
	}
	if len(drv.rows) != 3 {
		t.Errorf("rows: %d", len(drv.rows))
	}
	wantCols := map[string]bool{"id": true, "name": true, "email": true}
	for _, c := range drv.cols {
		if !wantCols[c] {
			t.Errorf("unexpected column: %s", c)
		}
	}
}

func TestCopyFromReturnsErrCopyNotSupported(t *testing.T) {
	_, ent := entUsersSchema()
	db := pg.New(&fakeDriver{})
	_, err := pg.CopyFrom(db, context.Background(), ent, []entUser{
		{Name: "A", Email: "a@x"},
	})
	if !errors.Is(err, pg.ErrCopyNotSupported) {
		t.Errorf("expected ErrCopyNotSupported, got %v", err)
	}
}

func TestCopyFromEmptyBatch(t *testing.T) {
	_, ent := entUsersSchema()
	db := pg.New(&copyingDriver{})
	_, err := pg.CopyFrom(db, context.Background(), ent, nil)
	if !errors.Is(err, pg.ErrNoRowsToInsert) {
		t.Errorf("expected ErrNoRowsToInsert, got %v", err)
	}
}

func TestCopyFromRunsValidators(t *testing.T) {
	_, ent := entUsersSchema()
	boom := errors.New("invalid")
	ent.Validate(func(u *entUser) error {
		if u.Email == "" {
			return boom
		}
		return nil
	})
	drv := &copyingDriver{}
	db := pg.New(drv)
	_, err := pg.CopyFrom(db, context.Background(), ent, []entUser{
		{Name: "OK", Email: "ok@x"},
		{Name: "Bad"}, // empty email triggers validator
	})
	if !errors.Is(err, boom) {
		t.Errorf("expected validator error, got %v", err)
	}
	if drv.calls.Load() != 0 {
		t.Errorf("validator failure must abort before Copy, got %d calls", drv.calls.Load())
	}
}

func TestSupportsCopy(t *testing.T) {
	if !pg.SupportsCopy(pg.New(&copyingDriver{})) {
		t.Error("SupportsCopy should be true for Copier driver")
	}
	if pg.SupportsCopy(pg.New(&fakeDriver{})) {
		t.Error("SupportsCopy should be false for non-Copier driver")
	}
}
