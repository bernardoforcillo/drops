package pg_test

import (
	"context"
	"errors"
	"reflect"
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

// CopyFrom ran the validators and nothing else: it wrote whatever
// tenant the struct happened to carry, which for a freshly decoded
// payload is the zero value — the case Entity.CreateMany's own doc
// names as "rows landed outside every tenant, reported as a successful
// insert", here at a hundred thousand rows a batch. The assertions are
// on the values handed to the driver, because that is where a COPY's
// payload is: there is no statement to read.

func TestCopyFromStampsTheCtxTenant(t *testing.T) {
	ent := bulkSchema()
	drv := &copyingDriver{returns: 2}
	db := pg.New(drv)

	rs := []bulkRow{{Name: "a"}, {Name: "b"}}
	n, err := pg.CopyFrom(db, pg.WithTenant(context.Background(), int64(3)), ent, rs)
	if err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}
	if got, want := n, int64(2); got != want {
		t.Errorf("rows accepted: got = %v, want %v", got, want)
	}
	at := -1
	for i, c := range drv.cols {
		if c == "tenantId" {
			at = i
		}
	}
	if at < 0 {
		t.Fatalf("copied columns name the tenant: got = %v", drv.cols)
	}
	for i, row := range drv.rows {
		if got, want := row[at], any(int64(3)); !reflect.DeepEqual(got, want) {
			t.Errorf("row %d tenant on the wire: got = %v, want %v", i, got, want)
		}
	}
	// rs is written through on Entity.CreateMany's terms, so the caller
	// holds the rows as they were stored.
	for i := range rs {
		if got, want := rs[i].TenantID, int64(3); got != want {
			t.Errorf("rs[%d].TenantID after CopyFrom: got = %v, want %v", i, got, want)
		}
	}
}

func TestCopyFromWithoutATenantFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
		rows []bulkRow
		want error
	}{
		{
			name: "no tenant on ctx",
			ctx:  context.Background(),
			rows: []bulkRow{{Name: "a"}},
			want: pg.ErrTenantMissing,
		},
		{
			name: "a row owned by another tenant",
			ctx:  pg.WithTenant(context.Background(), int64(3)),
			rows: []bulkRow{{Name: "a"}, {TenantID: 9, Name: "b"}},
			want: pg.ErrTenantMismatch,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ent := bulkSchema()
			drv := &copyingDriver{}
			db := pg.New(drv)

			_, err := pg.CopyFrom(db, tt.ctx, ent, tt.rows)
			if !errors.Is(err, tt.want) {
				t.Errorf("error: got = %v, want %v", err, tt.want)
			}
			// A COPY that fails halfway is worse than one that never
			// starts: the refusal has to land before any bytes go on
			// the wire.
			if got, want := drv.calls.Load(), int32(0); got != want {
				t.Errorf("Copy calls: got = %v, want %v", got, want)
			}
		})
	}
}
