package mirror_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/mirror"
	"github.com/bernardoforcillo/drops/pg"
)

// ----------------------------------------------------------------------
// The reseed's two source reads, and the axis they cross
//
//	A reseed reads every tenant's rows, and that is the right answer
//	for a mirror. What it may not do is read them by accident.
//
// [mirror.Reseeder] builds its own SQL: pg's SelectBuilder renders FOR
// UPDATE and never FOR SHARE, and the difference is the whole ordering
// argument in NewRepairReseeder. Building it by hand meant appending a
// *pg.Table straight to a drops.Builder — the escape hatch drops
// documents for callers, taken here by drops' own code — and the
// escape hatch skips pg's resolver entirely. Two things followed:
//
//   - a tenant-scoped source table was read with no tenant predicate
//     and refused nothing on a ctx carrying no tenant. That answer is
//     correct for a reseed and disastrous as an accident, and the SQL
//     could not tell the two apart;
//   - a predicate handed to [Reseeder.Where] was rendered raw, so a
//     subquery inside it — the ordinary way to say "seed the rows this
//     other table admits" — reached the server with none of ITS
//     scoping. That one is a cross-tenant read with no reading of it
//     that is correct.
//
// The reads go through pg's SelectBuilder now, with Unscoped stated at
// the call site for the relation and nothing stated for the
// predicates, which is what leaves them scoped. These tests pin both
// halves.

const reseedScopeTenant = int64(4242)

// reseedScopedSource is the source table with a tenant axis of its
// own, for the half of the story where a reseed deliberately reads
// past it.
func reseedScopedSource() (*pg.Table, *pg.Col[int64]) {
	t := pg.NewTable("docs")
	pg.Add(t, pg.BigSerial("id").PrimaryKey())
	pg.Add(t, pg.Text("title").NotNull())
	pg.Add(t, pg.Integer("views").NotNull())
	tenant := pg.Add(t, pg.BigInt("tenantId").NotNull())
	t.ContextFilter(pg.TenantFilter(tenant))
	return t, tenant
}

// reseedGuardTable is a scoped table a caller's predicate reads
// through. It is not the source, so nothing about the reseed's own
// authority says anything about it.
func reseedGuardTable() *pg.Table {
	t := pg.NewTable("visible_docs")
	pg.Add(t, pg.BigSerial("id").PrimaryKey())
	tenant := pg.Add(t, pg.BigInt("tenantId").NotNull())
	t.ContextFilter(pg.TenantFilter(tenant))
	return t
}

// TestReseedPredicateSubqueriesCarryTheTenantAxis is the leak: a
// statement written into a Where predicate is a statement, and it was
// rendered as raw text.
func TestReseedPredicateSubqueriesCarryTheTenantAxis(t *testing.T) {
	d := newReseedDriver(1, 2, 3)
	src := reseedSourceTable()
	guard := reseedGuardTable()
	db := pg.New(d)

	sink := &recSink{name: "sink"}
	r, err := mirror.NewFillReseeder(db, src, sink)
	if err != nil {
		t.Fatalf("NewFillReseeder: %v", err)
	}
	r.ChunkSize(10).Throttle(0).
		Where(pg.Exists(db.Select(guard.Col("id")).From(guard)))

	if err := r.Run(pg.WithTenant(context.Background(), reseedScopeTenant)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	const wantInner = `EXISTS (SELECT "visible_docs"."id" FROM "visible_docs" WHERE ("visible_docs"."tenantId" = $`
	stmts := append(d.keyScans(), d.rowReads()...)
	if len(stmts) < 2 {
		t.Fatalf("the walk issued %d source statements, want a key scan and a row read", len(stmts))
	}
	for _, s := range stmts {
		if !strings.Contains(s.sql, wantInner) {
			t.Errorf("statement does not scope the subquery in its predicate:\n  got  %s\n  want it to contain %s", s.sql, wantInner)
		}
		if !containsArg(s.args, reseedScopeTenant) {
			t.Errorf("statement binds no tenant: %s\n  args %v", s.sql, s.args)
		}
	}
}

// TestReseedRefusesAPredicateThatNeedsATenantWithoutOne is the same
// leak read from the other side: the refusal a scoped subquery owes
// its caller has to survive being written into a reseed, and nothing
// may reach the server before it.
func TestReseedRefusesAPredicateThatNeedsATenantWithoutOne(t *testing.T) {
	d := newReseedDriver(1, 2, 3)
	src := reseedSourceTable()
	guard := reseedGuardTable()
	db := pg.New(d)

	sink := &recSink{name: "sink"}
	r, err := mirror.NewFillReseeder(db, src, sink)
	if err != nil {
		t.Fatalf("NewFillReseeder: %v", err)
	}
	r.ChunkSize(10).Throttle(0).
		Where(pg.Exists(db.Select(guard.Col("id")).From(guard)))

	err = r.Run(context.Background())
	if err == nil {
		t.Fatal("Run seeded a mirror from a predicate that reads a tenant-scoped table on a ctx with no tenant")
	}
	if !errors.Is(err, pg.ErrTenantMissing) {
		t.Errorf("Run error = %v, want it to wrap pg.ErrTenantMissing", err)
	}
	if got := d.matching(`FROM "docs"`); len(got) != 0 {
		t.Errorf("%d statements reached the server before the refusal: %v", len(got), got)
	}
	if len(sink.seen) != 0 {
		t.Errorf("the sink was handed %d batches from a refused walk", len(sink.seen))
	}
}

// TestReseedReadsEveryTenantOfAScopedSource pins the decision the
// other two tests are the boundary of.
//
// A reseed rebuilds the whole mirror. Scoping it to the ctx tenant
// would leave the mirror missing every other tenant's rows while still
// reporting itself complete, and would make a reseed impossible to run
// from a maintenance ctx at all. So the source relation is read
// Unscoped, on purpose — and this test is what makes a later "helpful"
// scoping of it a failure rather than a silent change of what a mirror
// contains.
func TestReseedReadsEveryTenantOfAScopedSource(t *testing.T) {
	d := newReseedDriver(1, 2, 3)
	src, _ := reseedScopedSource()
	sink := &recSink{name: "sink"}
	r, err := mirror.NewFillReseeder(pg.New(d), src, sink)
	if err != nil {
		t.Fatalf("NewFillReseeder: %v", err)
	}
	r.ChunkSize(10).Throttle(0)

	// No tenant on the ctx at all: a reseed that had picked up the
	// source table's axis would refuse here, which is the failure this
	// pins as much as the predicate is.
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	stmts := append(d.keyScans(), d.rowReads()...)
	if len(stmts) < 2 {
		t.Fatalf("the walk issued %d source statements, want a key scan and a row read", len(stmts))
	}
	for _, s := range stmts {
		// The WHERE clause rather than the whole statement: the tenant
		// column is one of the columns a mirror carries, so it is named
		// in every select list by construction. Where it may not appear
		// is in a predicate.
		if where := reseedWhereClause(s.sql); strings.Contains(where, "tenantId") {
			t.Errorf("the reseed narrowed itself to one tenant: WHERE %s", where)
		}
	}
	if got := reseedChangeKeys(sink.seen); len(got) != 3 {
		t.Errorf("seeded %v, want every row of the source table", got)
	}
}
