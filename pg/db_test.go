package pg_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/pg"
)

// db.ExecExpr took a ctx and threw it away: it rendered through
// drops.String, which is the context-free path, and handed the result
// to Exec together with the very ctx it had just ignored. All four
// builders satisfy drops.Expression, so the method accepted them
// happily — and on a table whose rows belong to a tenant it sent
// DELETE FROM "ax_rows" with no WHERE clause at all, destroying every
// tenant's rows and reporting a success. The tests below are on the
// statement the recorder saw, because that is the only place the
// difference shows: the call returns nil either way.
//
// The rule these tests hold the method to, stated once here and in the
// doc comment on ExecExpr: if a method takes a ctx, the ctx reaches the
// render, or the doc says why it cannot.

// axTable is the fixture the ExecExpr tests share: a table scoped on
// both axes, so each of the four statement shapes has something a ctx
// must contribute — a predicate for the three that carry a WHERE
// clause, a stamped column for the INSERT that does not.
func axTable() (tbl *pg.Table, tenant *pg.Col[int64], name *pg.Col[string]) {
	tbl = pg.NewTable("ax_rows")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	ten := pg.Add(tbl, pg.BigInt("tenantId").NotNull())
	nm := pg.Add(tbl, pg.Text("name").NotNull())
	tbl.ContextFilter(pg.TenantFilter(ten)).ScopeWritesByTenant(ten)
	return tbl, ten, nm
}

func TestExecExprResolvesTheStatementAgainstItsCtx(t *testing.T) {
	tests := []struct {
		name     string
		build    func(db *pg.DB, tbl *pg.Table, name *pg.Col[string]) drops.Expression
		wantSQL  string
		wantArgs []any
	}{
		{
			name: "select",
			build: func(db *pg.DB, tbl *pg.Table, _ *pg.Col[string]) drops.Expression {
				return db.Select().From(tbl)
			},
			wantSQL:  `SELECT * FROM "ax_rows" WHERE ("ax_rows"."tenantId" = $1)`,
			wantArgs: []any{int64(7)},
		},
		{
			name: "update",
			build: func(db *pg.DB, tbl *pg.Table, name *pg.Col[string]) drops.Expression {
				return db.Update(tbl).Set(name.Val("x"))
			},
			wantSQL:  `UPDATE "ax_rows" SET "name" = $1 WHERE ("ax_rows"."tenantId" = $2)`,
			wantArgs: []any{"x", int64(7)},
		},
		{
			name: "delete",
			build: func(db *pg.DB, tbl *pg.Table, _ *pg.Col[string]) drops.Expression {
				return db.Delete(tbl)
			},
			wantSQL:  `DELETE FROM "ax_rows" WHERE ("ax_rows"."tenantId" = $1)`,
			wantArgs: []any{int64(7)},
		},
		{
			name: "insert",
			build: func(db *pg.DB, tbl *pg.Table, name *pg.Col[string]) drops.Expression {
				return db.Insert(tbl).Row(name.Val("x"))
			},
			wantSQL:  `INSERT INTO "ax_rows" ("name", "tenantId") VALUES ($1, $2)`,
			wantArgs: []any{"x", int64(7)},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tbl, _, name := axTable()
			drv := dropstest.New()
			db := pg.New(drv)
			ctx := pg.WithTenant(context.Background(), int64(7))

			if _, err := db.ExecExpr(ctx, tc.build(db, tbl, name)); err != nil {
				t.Fatalf("ExecExpr: %v", err)
			}
			if got := drv.LastSQL(); got != tc.wantSQL {
				t.Errorf("rendered SQL: got = %v, want %v", got, tc.wantSQL)
			}
			if got := drv.Last().Args; !reflect.DeepEqual(got, tc.wantArgs) {
				t.Errorf("bound arguments: got = %v, want %v", got, tc.wantArgs)
			}
		})
	}
}

func TestExecExprFailsClosedWhenTheCtxNamesNoTenant(t *testing.T) {
	tests := []struct {
		name  string
		build func(db *pg.DB, tbl *pg.Table, name *pg.Col[string]) drops.Expression
	}{
		{
			name: "select",
			build: func(db *pg.DB, tbl *pg.Table, _ *pg.Col[string]) drops.Expression {
				return db.Select().From(tbl)
			},
		},
		{
			name: "update",
			build: func(db *pg.DB, tbl *pg.Table, name *pg.Col[string]) drops.Expression {
				return db.Update(tbl).Set(name.Val("x"))
			},
		},
		{
			name: "delete",
			build: func(db *pg.DB, tbl *pg.Table, _ *pg.Col[string]) drops.Expression {
				return db.Delete(tbl)
			},
		},
		{
			name: "insert",
			build: func(db *pg.DB, tbl *pg.Table, name *pg.Col[string]) drops.Expression {
				return db.Insert(tbl).Row(name.Val("x"))
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tbl, _, name := axTable()
			drv := dropstest.New()
			db := pg.New(drv)

			_, err := db.ExecExpr(context.Background(), tc.build(db, tbl, name))
			if !errors.Is(err, pg.ErrTenantMissing) {
				t.Fatalf("ExecExpr error: got = %v, want %v", err, pg.ErrTenantMissing)
			}
			// The refusal is worth nothing if the statement went out
			// anyway: an unscoped DELETE is destructive before the
			// error is read.
			if stmts := drv.Statements(); len(stmts) != 0 {
				t.Errorf("statements sent after a refusal: got = %v, want none", stmts)
			}
		})
	}
}

// TestExecExprReachesAStatementWrappedInAnExpression covers the operand
// position round 4 built opExpr for: the statement is not the argument,
// it is inside it. A parenthesised SELECT is a statement PostgreSQL
// accepts, so this is a shape ExecExpr can genuinely be handed.
func TestExecExprReachesAStatementWrappedInAnExpression(t *testing.T) {
	tbl, _, _ := axTable()
	drv := dropstest.New()
	db := pg.New(drv)
	ctx := pg.WithTenant(context.Background(), int64(7))

	if _, err := db.ExecExpr(ctx, pg.Subquery(db.Select().From(tbl))); err != nil {
		t.Fatalf("ExecExpr: %v", err)
	}
	want := `(SELECT * FROM "ax_rows" WHERE ("ax_rows"."tenantId" = $1))`
	if got := drv.LastSQL(); got != want {
		t.Errorf("rendered SQL: got = %v, want %v", got, want)
	}
	if got, wantArgs := drv.Last().Args, []any{int64(7)}; !reflect.DeepEqual(got, wantArgs) {
		t.Errorf("bound arguments: got = %v, want %v", got, wantArgs)
	}
}

// TestExecExprSurfacesADeferredBuilderError holds ExecExpr to the same
// contract as the builders' own executors: a cursor that failed to
// decode is reported, not run. AfterCursor fails closed by appending a
// false predicate, so the statement would have been sent, affected
// nothing, and returned no error — the caller reads "page was empty"
// where the truth is "the cursor was corrupt".
func TestExecExprSurfacesADeferredBuilderError(t *testing.T) {
	tbl, _, name := axTable()
	drv := dropstest.New()
	db := pg.New(drv)
	ctx := pg.WithTenant(context.Background(), int64(7))

	spec := pg.NewCursorSpec(pg.OrderKey{Col: name})
	sel := db.Select().From(tbl).AfterCursor(spec, pg.Cursor("not-a-cursor"))

	if _, err := db.ExecExpr(ctx, sel); err == nil {
		t.Fatalf("ExecExpr error: got = nil, want the cursor decode failure")
	}
	if stmts := drv.Statements(); len(stmts) != 0 {
		t.Errorf("statements sent for a builder that never rendered: got = %v, want none", stmts)
	}
}

// TestExecExprRendersExpressionsWithNoStatementInsideUnchanged is the
// byte-identity guard. An expression drops cannot resolve — a DDL
// helper, a [drops.Raw], a bare predicate — has nothing a ctx can
// contribute to, and must reach the driver exactly as drops.String
// renders it. The escape hatch stays an escape hatch.
func TestExecExprRendersExpressionsWithNoStatementInsideUnchanged(t *testing.T) {
	tbl, tenant, name := axTable()

	corpus := []struct {
		name string
		expr drops.Expression
	}{
		{"create table", pg.CreateTable(tbl)},
		{"create table if not exists", pg.CreateTableIfNotExists(tbl)},
		{"drop table if exists", pg.DropTableIfExists(tbl)},
		{"create extension", pg.CreateExtensionIfNotExists("vector")},
		{"raw no args", drops.Raw("VACUUM ANALYZE")},
		{"caller's own closure with an arg", drops.ExprFunc(func(b *drops.Builder) {
			b.WriteString("SELECT set_config('x', ")
			b.AddArg("on")
			b.WriteString(", false)")
		})},
		{"predicate", pg.Eq(name, "a")},
		{"conjunction", pg.And(pg.Eq(name, "a"), pg.Gt(tenant, int64(1)))},
		{"disjunction", pg.Or(pg.Eq(name, "a"), pg.IsNull(name))},
		{"negation", pg.Not(pg.Eq(name, "a"))},
		{"in list", pg.In(tenant, int64(1), int64(2))},
		{"between", pg.Between(tenant, int64(1), int64(9))},
		{"like", pg.Like(name, "a%")},
	}

	for _, tc := range corpus {
		t.Run(tc.name, func(t *testing.T) {
			wantSQL, wantArgs := drops.String(tc.expr)

			drv := dropstest.New()
			db := pg.New(drv)
			if _, err := db.ExecExpr(pg.WithTenant(context.Background(), int64(7)), tc.expr); err != nil {
				t.Fatalf("ExecExpr: %v", err)
			}
			if got := drv.LastSQL(); got != wantSQL {
				t.Errorf("rendered SQL: got = %v, want %v", got, wantSQL)
			}
			if got := drv.Last().Args; !reflect.DeepEqual(got, wantArgs) {
				t.Errorf("bound arguments: got = %v, want %v", got, wantArgs)
			}
		})
	}
}
