package pg_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/pg"
)

// The declarations and the escape hatch that decide which INSERTs the
// tenant axis reaches, and what a caller says when a write legitimately
// falls outside it.

func TestInsertToSQLCtxRendersWhatExecSends(t *testing.T) {
	tbl, _, _, name := gadgetSchema()
	drv := dropstest.New()
	db := pg.New(drv)

	ins := db.Insert(tbl).Row(name.Val("a"))
	sql, args, err := ins.ToSQLCtx(tenantCtx(int64(3)))
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	want := `INSERT INTO "gadgets" ("name", "tenantId") VALUES ($1, $2)`
	if sql != want {
		t.Errorf("rendered SQL:\n got = %v\nwant = %v", sql, want)
	}
	if got, want := args, []any{"a", int64(3)}; !reflect.DeepEqual(got, want) {
		t.Errorf("bound arguments: got = %v, want %v", got, want)
	}
	if _, err := ins.Exec(tenantCtx(int64(3))); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := drv.LastSQL(); got != sql {
		t.Errorf("Exec sends what ToSQLCtx rendered:\n got = %v\nwant = %v", got, sql)
	}

	// ToSQL has no ctx and so cannot carry the stamp — which is why it
	// is not the call to assert on, and is documented as such.
	if got, _ := ins.ToSQL(); strings.Contains(got, `"tenantId"`) {
		t.Errorf("ToSQL renders the stamped column: got = %v", got)
	}
}

// TestInsertBuilderIsReusableAcrossTenants pins the reason resolution
// renders a copy. Stamping into the builder's own rows would leave the
// first request's tenant bound in the second request's statement: the
// one failure here that is silent in the SQL and wrong in the row.
func TestInsertBuilderIsReusableAcrossTenants(t *testing.T) {
	tbl, _, _, name := gadgetSchema()
	drv := dropstest.New()
	db := pg.New(drv)

	ins := db.Insert(tbl).Row(name.Val("a"))
	if _, err := ins.Exec(tenantCtx(int64(3))); err != nil {
		t.Fatalf("first Exec: %v", err)
	}
	if _, err := ins.Exec(tenantCtx(int64(7))); err != nil {
		t.Fatalf("second Exec: %v", err)
	}
	stmts := drv.Statements()
	if got, want := len(stmts), 2; got != want {
		t.Fatalf("statements sent: got = %v, want %v", got, want)
	}
	for i, want := range [][]any{{"a", int64(3)}, {"a", int64(7)}} {
		if got := stmts[i].Args; !reflect.DeepEqual(got, want) {
			t.Errorf("statement %d arguments: got = %v, want %v", i, got, want)
		}
		if got, want := stmts[i].SQL, `INSERT INTO "gadgets" ("name", "tenantId") VALUES ($1, $2)`; got != want {
			t.Errorf("statement %d SQL:\n got = %v\nwant = %v", i, got, want)
		}
	}
}

func TestInsertUnscopedWritesOutsideTheTenantAxis(t *testing.T) {
	tbl, id, tenant, name := gadgetSchema()
	drv := dropstest.New()
	db := pg.New(drv)

	// A backfill: no tenant on the ctx at all, the tenant written per
	// row, and the conflict branch left exactly as written.
	_, err := db.Insert(tbl).
		Row(tenant.Val(9), name.Val("a")).
		OnConflictUpdate(id).
		Set(name.Expr(pg.Excluded(name)), tenant.Expr(pg.Excluded(tenant))).
		Done().Unscoped().Exec(context.Background())
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	want := `INSERT INTO "gadgets" ("tenantId", "name") VALUES ($1, $2) ` +
		`ON CONFLICT ("id") DO UPDATE SET "name" = EXCLUDED."name", "tenantId" = EXCLUDED."tenantId"`
	if got := drv.LastSQL(); got != want {
		t.Errorf("rendered SQL:\n got = %v\nwant = %v", got, want)
	}
}

// TestInsertUnscopedLeavesInnerStatementsScoped pins the boundary of
// the escape hatch: a subquery is a statement of its own and keeps its
// own axis, which is how one relation of a write is widened and no
// other.
func TestInsertUnscopedLeavesInnerStatementsScoped(t *testing.T) {
	tbl, _, tenant, name := gadgetSchema()
	notes, noteID, _, _ := noteSchema()

	t.Run("subquery keeps its predicate", func(t *testing.T) {
		drv := dropstest.New()
		db := pg.New(drv)
		sub := db.Select(noteID).From(notes).Limit(1)
		_, err := db.Insert(tbl).
			Row(tenant.Val(9), name.Expr(pg.Subquery(sub))).
			Unscoped().Exec(tenantCtx(int64(3)))
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		sql := drv.LastSQL()
		if got, want := strings.Contains(sql, `"notes"."tenantId" = `), true; got != want {
			t.Errorf("subquery keeps its tenant axis: got = %v, want %v (%v)", got, want, sql)
		}
	})

	t.Run("subquery still refuses without a tenant", func(t *testing.T) {
		drv := dropstest.New()
		db := pg.New(drv)
		sub := db.Select(noteID).From(notes).Limit(1)
		_, err := db.Insert(tbl).
			Row(tenant.Val(9), name.Expr(pg.Subquery(sub))).
			Unscoped().Exec(context.Background())
		if !errors.Is(err, pg.ErrTenantMissing) {
			t.Errorf("error: got = %v, want %v", err, pg.ErrTenantMissing)
		}
		if got, want := len(drv.Statements()), 0; got != want {
			t.Errorf("statements sent: got = %v, want %v (%v)", got, want, drv.Statements())
		}
	})
}

// TestScopeWritesByTenantNamesTheColumnAFilterCannotName covers the
// asymmetry the package documents. A ContextFilterFunc is a closure
// that answers with a predicate; nothing can ask it which column it
// names, and an INSERT has no WHERE for the predicate it does answer
// with. So a table scoped only with ContextFilter keeps its guarantee
// on every read and leaves its INSERTs to the caller, and naming the
// column is how it hands them over.
func TestScopeWritesByTenantNamesTheColumnAFilterCannotName(t *testing.T) {
	schema := func(declareWrites bool) (*pg.Table, *pg.Col[string]) {
		tbl := pg.NewTable("filtered")
		pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
		tenant := pg.Add(tbl, pg.BigInt("tenantId").NotNull())
		name := pg.Add(tbl, pg.Text("name").NotNull())
		tbl.ContextFilter(pg.TenantFilter(tenant))
		if declareWrites {
			tbl.ScopeWritesByTenant(tenant)
		}
		return tbl, name
	}

	t.Run("filter alone leaves the INSERT to the caller", func(t *testing.T) {
		tbl, name := schema(false)
		drv := dropstest.New()
		db := pg.New(drv)
		if _, err := db.Insert(tbl).Row(name.Val("a")).Exec(tenantCtx(int64(3))); err != nil {
			t.Fatalf("Exec: %v", err)
		}
		want := `INSERT INTO "filtered" ("name") VALUES ($1)`
		if got := drv.LastSQL(); got != want {
			t.Errorf("rendered SQL:\n got = %v\nwant = %v", got, want)
		}
		// The read side is untouched by any of this.
		var rows []struct{}
		if err := db.Select().From(tbl).All(context.Background(), &rows); !errors.Is(err, pg.ErrTenantMissing) {
			t.Errorf("read side still refuses: got = %v, want %v", err, pg.ErrTenantMissing)
		}
	})

	t.Run("naming the column hands the INSERT to drops", func(t *testing.T) {
		tbl, name := schema(true)
		drv := dropstest.New()
		db := pg.New(drv)
		if _, err := db.Insert(tbl).Row(name.Val("a")).Exec(tenantCtx(int64(3))); err != nil {
			t.Fatalf("Exec: %v", err)
		}
		want := `INSERT INTO "filtered" ("name", "tenantId") VALUES ($1, $2)`
		if got := drv.LastSQL(); got != want {
			t.Errorf("rendered SQL:\n got = %v\nwant = %v", got, want)
		}
		if got, want := drv.Last().Args, []any{"a", int64(3)}; !reflect.DeepEqual(got, want) {
			t.Errorf("bound arguments: got = %v, want %v", got, want)
		}
		if _, err := db.Insert(tbl).Row(name.Val("a")).Exec(context.Background()); !errors.Is(err, pg.ErrTenantMissing) {
			t.Errorf("error with no tenant on ctx: got = %v, want %v", err, pg.ErrTenantMissing)
		}
	})
}

// TestInsertIntoAnAliasedTableIsStillScoped pins that the conflict gate
// is written the way the SET list is written. A qualified
// "gadgets"."tenantId" names a relation INSERT INTO "gadgets" AS "g"
// does not have, which is 42P01 — a scoped table that could not be
// inserted into under an alias at all.
func TestInsertIntoAnAliasedTableIsStillScoped(t *testing.T) {
	tbl, id, _, name := gadgetSchema()
	drv := dropstest.New()
	db := pg.New(drv)

	_, err := db.Insert(tbl.As("g")).Row(name.Val("a")).
		OnConflictUpdate(id).Set(name.Expr(pg.Excluded(name))).
		Done().Exec(tenantCtx(int64(3)))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	want := `INSERT INTO "gadgets" AS "g" ("name", "tenantId") VALUES ($1, $2) ` +
		`ON CONFLICT ("id") DO UPDATE SET "name" = EXCLUDED."name" ` +
		`WHERE "tenantId" = EXCLUDED."tenantId"`
	if got := drv.LastSQL(); got != want {
		t.Errorf("rendered SQL:\n got = %v\nwant = %v", got, want)
	}
}
