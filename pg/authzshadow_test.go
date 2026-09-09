package pg_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

type shadowInvoice struct {
	ID        int64 `drop:"id"`
	CreatedBy int64 `drop:"createdBy"`
}

// guardRowsDriver answers the guarded and unguarded reads with
// different row sets, chosen by whether the statement carries the
// guard's predicate. That is what lets the comparison be exercised
// without a server.
type guardRowsDriver struct {
	guarded   []shadowInvoice
	unguarded []shadowInvoice
	marker    string
}

func (d *guardRowsDriver) Exec(context.Context, string, ...any) (drops.Result, error) {
	return nil, nil
}

func (d *guardRowsDriver) Query(_ context.Context, sql string, _ ...any) (drops.Rows, error) {
	if strings.Contains(sql, d.marker) {
		return &invoiceRows{rows: d.guarded}, nil
	}
	return &invoiceRows{rows: d.unguarded}, nil
}

func (d *guardRowsDriver) Begin(context.Context) (drops.Tx, error) { return nil, nil }

type invoiceRows struct {
	rows []shadowInvoice
	i    int
}

func (r *invoiceRows) Next() bool {
	if r.i >= len(r.rows) {
		return false
	}
	r.i++
	return true
}

func (r *invoiceRows) Scan(dest ...any) error {
	row := r.rows[r.i-1]
	if len(dest) >= 1 {
		if p, ok := dest[0].(*int64); ok {
			*p = row.ID
		}
	}
	if len(dest) >= 2 {
		if p, ok := dest[1].(*int64); ok {
			*p = row.CreatedBy
		}
	}
	return nil
}

func (r *invoiceRows) Columns() ([]string, error) { return []string{"id", "createdBy"}, nil }
func (r *invoiceRows) Close() error               { return nil }
func (r *invoiceRows) Err() error                 { return nil }

func shadowEntity(drv drops.Driver) (*pg.Entity[shadowInvoice], *pg.DB) {
	invoices := pg.NewTable("invoices")
	pg.Add(invoices, pg.BigSerial("id").PrimaryKey())
	pg.Add(invoices, pg.BigInt("createdBy").NotNull())

	ent := pg.NewEntity[shadowInvoice](invoices).
		AuthorizeWith(pg.OwnerGuard{Owner: invoices.Col("createdBy")})
	return ent, pg.New(drv)
}

func TestCompareGuardAgreesWhenThePredicateIsRight(t *testing.T) {
	rows := []shadowInvoice{{ID: 1, CreatedBy: 7}, {ID: 2, CreatedBy: 9}}
	drv := &guardRowsDriver{
		unguarded: rows,
		guarded:   []shadowInvoice{{ID: 1, CreatedBy: 7}},
		marker:    `"createdBy"`,
	}
	ent, db := shadowEntity(drv)
	ctx := pg.WithSubject(context.Background(), int64(7))

	diff, err := pg.CompareGuard(ctx, db, ent,
		func(inv shadowInvoice) bool { return inv.CreatedBy == 7 }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !diff.Agrees() {
		t.Errorf("expected agreement, got:\n%s", diff)
	}
	if diff.Rows != 2 || diff.Allowed != 1 {
		t.Errorf("Rows=%d Allowed=%d, want 2 and 1", diff.Rows, diff.Allowed)
	}
}

// The direction that matters: a row the predicate returned that the
// rule denies is a row a user can see and should not.
func TestCompareGuardReportsALeak(t *testing.T) {
	rows := []shadowInvoice{{ID: 1, CreatedBy: 7}, {ID: 2, CreatedBy: 9}}
	drv := &guardRowsDriver{
		unguarded: rows,
		guarded:   rows, // a predicate that filters nothing
		marker:    `"createdBy"`,
	}
	ent, db := shadowEntity(drv)
	ctx := pg.WithSubject(context.Background(), int64(7))

	diff, err := pg.CompareGuard(ctx, db, ent,
		func(inv shadowInvoice) bool { return inv.CreatedBy == 7 }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if diff.Agrees() {
		t.Fatal("a predicate that filters nothing was reported as agreeing")
	}
	if len(diff.Leaked) != 1 || diff.Leaked[0].ID != 2 {
		t.Errorf("Leaked = %+v, want invoice 2", diff.Leaked)
	}
	if len(diff.Hidden) != 0 {
		t.Errorf("Hidden = %+v, want none", diff.Hidden)
	}
	if !strings.Contains(diff.String(), "the rule denies") {
		t.Errorf("String() does not describe the leak:\n%s", diff)
	}
}

// The other direction — usually a NULL comparison, since a WHERE
// drops what it cannot call true.
func TestCompareGuardReportsHiddenRows(t *testing.T) {
	rows := []shadowInvoice{{ID: 1, CreatedBy: 7}, {ID: 2, CreatedBy: 7}}
	drv := &guardRowsDriver{
		unguarded: rows,
		guarded:   []shadowInvoice{{ID: 1, CreatedBy: 7}},
		marker:    `"createdBy"`,
	}
	ent, db := shadowEntity(drv)
	ctx := pg.WithSubject(context.Background(), int64(7))

	diff, err := pg.CompareGuard(ctx, db, ent,
		func(inv shadowInvoice) bool { return inv.CreatedBy == 7 }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Hidden) != 1 || diff.Hidden[0].ID != 2 {
		t.Errorf("Hidden = %+v, want invoice 2", diff.Hidden)
	}
	if !strings.Contains(diff.String(), "the guard hid") {
		t.Errorf("String() does not describe the hidden row:\n%s", diff)
	}
}

func TestCompareGuardValidatesItsInputs(t *testing.T) {
	drv := &guardRowsDriver{marker: `"createdBy"`}
	ent, db := shadowEntity(drv)
	ctx := pg.WithSubject(context.Background(), int64(7))

	if _, err := pg.CompareGuard[shadowInvoice](ctx, db, nil, func(shadowInvoice) bool { return true }, nil); err == nil {
		t.Error("expected an error for a nil entity")
	}
	if _, err := pg.CompareGuard(ctx, db, ent, nil, nil); err == nil {
		t.Error("expected an error for a nil rule")
	}

	// An entity with no guard has nothing to compare, and saying so
	// beats reporting perfect agreement.
	unguarded := pg.NewTable("plain")
	pg.Add(unguarded, pg.BigSerial("id").PrimaryKey())
	pg.Add(unguarded, pg.BigInt("createdBy").NotNull())
	plain := pg.NewEntity[shadowInvoice](unguarded)
	if _, err := pg.CompareGuard(ctx, db, plain, func(shadowInvoice) bool { return true }, nil); err == nil {
		t.Error("expected an error for an entity with no guard")
	}
}

// The guarded read fails closed without a subject, and that is the
// behaviour under test rather than a harness problem.
func TestCompareGuardNeedsASubject(t *testing.T) {
	drv := &guardRowsDriver{marker: `"createdBy"`}
	ent, db := shadowEntity(drv)
	if _, err := pg.CompareGuard(context.Background(), db, ent,
		func(shadowInvoice) bool { return true }, nil); err == nil {
		t.Error("expected the guarded read to fail without a subject")
	}
}
