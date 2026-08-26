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

// resolveExpr decided what it could walk into by NAMING types: it had
// an arm for *SelectBuilder and none for the other three builders, so
// every statement-bearing type it did not name was invisible to it.
// *UpdateBuilder, *InsertBuilder and *DeleteBuilder all satisfy
// drops.Expression, which is what a CTE body and a subquery operand are
// typed as — so
//
//	WITH moved AS (DELETE FROM ax_rows RETURNING name) SELECT * FROM moved
//
// rendered with no WHERE clause at all, on a table whose every row
// belongs to a tenant, and did not refuse on a ctx with no tenant
// either. That is a cross-tenant WRITE, reachable through the exported
// API, in the shape a real "archive these rows" query is written in.
//
// The second copy of the same list sat in cte.go: resolveCTEs asserted
// *SelectBuilder itself instead of calling resolveExpr, so a CTE body
// wrapped in Subquery or AsSubquery — an expression resolveExpr has
// walked since round 4 — was unresolved as well, even when the body was
// the SELECT the assertion was looking for.
//
// The tests in this file are on the rendered SQL and the bound
// arguments of the statement that would be sent, because that is the
// only place the difference shows: every one of these calls returned a
// nil error before the fix and a nil error after it.

// stmtTable is the fixture the tests share: a table scoped on both
// axes, so each statement shape has something the ctx must contribute —
// a predicate for the three that carry a WHERE clause, a stamped column
// for the INSERT that does not.
func stmtTable() (tbl *pg.Table, tenant *pg.Col[int64], name *pg.Col[string]) {
	tbl = pg.NewTable("ax_rows")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	ten := pg.Add(tbl, pg.BigInt("tenantId").NotNull())
	nm := pg.Add(tbl, pg.Text("name").NotNull())
	tbl.ContextFilter(pg.TenantFilter(ten)).ScopeWritesByTenant(ten)
	return tbl, ten, nm
}

func stmtCtx() context.Context { return pg.WithTenant(context.Background(), int64(7)) }

// stmtCase is one statement built around a body that is a statement in
// its own right — a CTE body, a subquery operand — with the SQL and
// arguments the ctx must produce.
type stmtCase struct {
	name     string
	build    func(db *pg.DB, tbl *pg.Table, name *pg.Col[string]) *pg.SelectBuilder
	wantSQL  string
	wantArgs []any
}

// stmtCases covers the two axes the leak spanned: which builder the
// body is (the three writes plus the SELECT that already worked), and
// how the body is wrapped (bare, Subquery, AsSubquery — the wrappers
// resolveCTEs' own type assertion could not see through).
func stmtCases() []stmtCase {
	return []stmtCase{
		{
			name: "delete cte",
			build: func(db *pg.DB, tbl *pg.Table, name *pg.Col[string]) *pg.SelectBuilder {
				return db.Select().
					With(pg.CTEDef("moved", db.Delete(tbl).Returning(name))).
					FromExpr(drops.Raw(`"moved"`))
			},
			wantSQL:  `WITH "moved" AS (DELETE FROM "ax_rows" WHERE ("ax_rows"."tenantId" = $1) RETURNING "ax_rows"."name") SELECT * FROM "moved"`,
			wantArgs: []any{int64(7)},
		},
		{
			name: "update cte",
			build: func(db *pg.DB, tbl *pg.Table, name *pg.Col[string]) *pg.SelectBuilder {
				return db.Select().
					With(pg.CTEDef("moved", db.Update(tbl).Set(name.Val("x")).Returning(name))).
					FromExpr(drops.Raw(`"moved"`))
			},
			wantSQL:  `WITH "moved" AS (UPDATE "ax_rows" SET "name" = $1 WHERE ("ax_rows"."tenantId" = $2) RETURNING "ax_rows"."name") SELECT * FROM "moved"`,
			wantArgs: []any{"x", int64(7)},
		},
		{
			name: "insert cte",
			build: func(db *pg.DB, tbl *pg.Table, name *pg.Col[string]) *pg.SelectBuilder {
				return db.Select().
					With(pg.CTEDef("moved", db.Insert(tbl).Row(name.Val("x")).Returning(name))).
					FromExpr(drops.Raw(`"moved"`))
			},
			wantSQL:  `WITH "moved" AS (INSERT INTO "ax_rows" ("name", "tenantId") VALUES ($1, $2) RETURNING "ax_rows"."name") SELECT * FROM "moved"`,
			wantArgs: []any{"x", int64(7)},
		},
		{
			name: "select cte wrapped in Subquery",
			build: func(db *pg.DB, tbl *pg.Table, _ *pg.Col[string]) *pg.SelectBuilder {
				return db.Select().
					With(pg.CTEDef("moved", pg.Subquery(db.Select().From(tbl)))).
					FromExpr(drops.Raw(`"moved"`))
			},
			wantSQL:  `WITH "moved" AS ((SELECT * FROM "ax_rows" WHERE ("ax_rows"."tenantId" = $1))) SELECT * FROM "moved"`,
			wantArgs: []any{int64(7)},
		},
		{
			name: "select cte wrapped in AsSubquery",
			build: func(db *pg.DB, tbl *pg.Table, _ *pg.Col[string]) *pg.SelectBuilder {
				return db.Select().
					With(pg.CTEDef("moved", db.Select().From(tbl).AsSubquery("m"))).
					FromExpr(drops.Raw(`"moved"`))
			},
			wantSQL:  `WITH "moved" AS ((SELECT * FROM "ax_rows" WHERE ("ax_rows"."tenantId" = $1)) AS "m") SELECT * FROM "moved"`,
			wantArgs: []any{int64(7)},
		},
		{
			name: "delete cte wrapped in Subquery",
			build: func(db *pg.DB, tbl *pg.Table, name *pg.Col[string]) *pg.SelectBuilder {
				return db.Select().
					With(pg.CTEDef("moved", pg.Subquery(db.Delete(tbl).Returning(name)))).
					FromExpr(drops.Raw(`"moved"`))
			},
			wantSQL:  `WITH "moved" AS ((DELETE FROM "ax_rows" WHERE ("ax_rows"."tenantId" = $1) RETURNING "ax_rows"."name")) SELECT * FROM "moved"`,
			wantArgs: []any{int64(7)},
		},
		{
			name: "recursive delete cte",
			build: func(db *pg.DB, tbl *pg.Table, name *pg.Col[string]) *pg.SelectBuilder {
				return db.Select().
					WithRecursive(pg.CTEDef("moved", db.Delete(tbl).Returning(name))).
					FromExpr(drops.Raw(`"moved"`))
			},
			wantSQL:  `WITH RECURSIVE "moved" AS (DELETE FROM "ax_rows" WHERE ("ax_rows"."tenantId" = $1) RETURNING "ax_rows"."name") SELECT * FROM "moved"`,
			wantArgs: []any{int64(7)},
		},
		{
			name: "delete inside an EXISTS operand",
			build: func(db *pg.DB, tbl *pg.Table, name *pg.Col[string]) *pg.SelectBuilder {
				return db.Select().
					FromExpr(drops.Raw(`"other"`)).
					Where(pg.Exists(db.Delete(tbl).Returning(name)))
			},
			wantSQL:  `SELECT * FROM "other" WHERE EXISTS (DELETE FROM "ax_rows" WHERE ("ax_rows"."tenantId" = $1) RETURNING "ax_rows"."name")`,
			wantArgs: []any{int64(7)},
		},
		{
			name: "update nested three combinators deep",
			build: func(db *pg.DB, tbl *pg.Table, name *pg.Col[string]) *pg.SelectBuilder {
				return db.Select().
					FromExpr(drops.Raw(`"other"`)).
					Where(pg.Not(pg.And(pg.Exists(db.Update(tbl).Set(name.Val("x")).Returning(name)))))
			},
			wantSQL:  `SELECT * FROM "other" WHERE (NOT EXISTS (UPDATE "ax_rows" SET "name" = $1 WHERE ("ax_rows"."tenantId" = $2) RETURNING "ax_rows"."name"))`,
			wantArgs: []any{"x", int64(7)},
		},
	}
}

// TestStatementBodiesOfEveryBuilderAreResolved is the read of the fix:
// whatever builder the body is, and however it is wrapped, the ctx
// reaches it.
func TestStatementBodiesOfEveryBuilderAreResolved(t *testing.T) {
	for _, tc := range stmtCases() {
		t.Run(tc.name, func(t *testing.T) {
			tbl, _, name := stmtTable()
			db := pg.New(dropstest.New())

			got, args, err := tc.build(db, tbl, name).ToSQLCtx(stmtCtx())
			if err != nil {
				t.Fatalf("ToSQLCtx: %v", err)
			}
			if got != tc.wantSQL {
				t.Errorf("rendered SQL:\n got = %v\nwant = %v", got, tc.wantSQL)
			}
			if !reflect.DeepEqual(args, tc.wantArgs) {
				t.Errorf("bound arguments: got = %v, want %v", args, tc.wantArgs)
			}
		})
	}
}

// TestStatementBodiesFailClosedWithNoTenant is the other half. A
// refusal is the whole promise of a tenant axis: the statement that
// cannot say which tenant it is for must not be rendered at all, and
// least of all a DELETE.
func TestStatementBodiesFailClosedWithNoTenant(t *testing.T) {
	for _, tc := range stmtCases() {
		t.Run(tc.name, func(t *testing.T) {
			tbl, _, name := stmtTable()
			db := pg.New(dropstest.New())

			sql, args, err := tc.build(db, tbl, name).ToSQLCtx(context.Background())
			if !errors.Is(err, pg.ErrTenantMissing) {
				t.Fatalf("ToSQLCtx error: got = %v, want %v", err, pg.ErrTenantMissing)
			}
			if sql != "" || len(args) != 0 {
				t.Errorf("SQL rendered for a refused statement: got = %q %v, want empty", sql, args)
			}
		})
	}
}

// TestStatementBodiesAreSentAsRendered runs the same statements through
// the executor. ToSQLCtx is where the assertion is readable; the
// executor is where the rows are actually destroyed, and the two must
// agree.
func TestStatementBodiesAreSentAsRendered(t *testing.T) {
	for _, tc := range stmtCases() {
		t.Run(tc.name, func(t *testing.T) {
			tbl, _, name := stmtTable()
			drv := dropstest.New()
			db := pg.New(drv)

			if _, err := tc.build(db, tbl, name).Rows(stmtCtx()); err != nil {
				t.Fatalf("Rows: %v", err)
			}
			if got := drv.LastSQL(); got != tc.wantSQL {
				t.Errorf("statement sent:\n got = %v\nwant = %v", got, tc.wantSQL)
			}
			if got := drv.Last().Args; !reflect.DeepEqual(got, tc.wantArgs) {
				t.Errorf("bound arguments: got = %v, want %v", got, tc.wantArgs)
			}
		})
	}
}

// TestExecExprReachesAWriteWrappedInAnExpression is the same leak at
// the other entry point. ExecExpr renders a ctxStatement through
// ToSQLCtx, so a bare write builder was always resolved there; wrapped
// in the parentheses [pg.Subquery] adds it went through resolveExpr,
// which is where the three write builders were invisible.
func TestExecExprReachesAWriteWrappedInAnExpression(t *testing.T) {
	tests := []struct {
		name     string
		build    func(db *pg.DB, tbl *pg.Table, name *pg.Col[string]) drops.Expression
		wantSQL  string
		wantArgs []any
	}{
		{
			name: "update",
			build: func(db *pg.DB, tbl *pg.Table, name *pg.Col[string]) drops.Expression {
				return pg.Subquery(db.Update(tbl).Set(name.Val("x")))
			},
			wantSQL:  `(UPDATE "ax_rows" SET "name" = $1 WHERE ("ax_rows"."tenantId" = $2))`,
			wantArgs: []any{"x", int64(7)},
		},
		{
			name: "delete",
			build: func(db *pg.DB, tbl *pg.Table, _ *pg.Col[string]) drops.Expression {
				return pg.Subquery(db.Delete(tbl))
			},
			wantSQL:  `(DELETE FROM "ax_rows" WHERE ("ax_rows"."tenantId" = $1))`,
			wantArgs: []any{int64(7)},
		},
		{
			name: "insert",
			build: func(db *pg.DB, tbl *pg.Table, name *pg.Col[string]) drops.Expression {
				return pg.Subquery(db.Insert(tbl).Row(name.Val("x")))
			},
			wantSQL:  `(INSERT INTO "ax_rows" ("name", "tenantId") VALUES ($1, $2))`,
			wantArgs: []any{"x", int64(7)},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tbl, _, name := stmtTable()
			drv := dropstest.New()
			db := pg.New(drv)

			if _, err := db.ExecExpr(stmtCtx(), tc.build(db, tbl, name)); err != nil {
				t.Fatalf("ExecExpr: %v", err)
			}
			if got := drv.LastSQL(); got != tc.wantSQL {
				t.Errorf("statement sent:\n got = %v\nwant = %v", got, tc.wantSQL)
			}
			if got := drv.Last().Args; !reflect.DeepEqual(got, tc.wantArgs) {
				t.Errorf("bound arguments: got = %v, want %v", got, tc.wantArgs)
			}

			drv2 := dropstest.New()
			db2 := pg.New(drv2)
			tbl2, _, name2 := stmtTable()
			_, err := db2.ExecExpr(context.Background(), tc.build(db2, tbl2, name2))
			if !errors.Is(err, pg.ErrTenantMissing) {
				t.Fatalf("ExecExpr error: got = %v, want %v", err, pg.ErrTenantMissing)
			}
			if stmts := drv2.Statements(); len(stmts) != 0 {
				t.Errorf("statements sent after a refusal: got = %v, want none", stmts)
			}
		})
	}
}

// TestStatementBodyIsResolvedOncePerExecution is the argument-binding
// half of the fix. Resolution is not idempotent — the target table
// still has its context filters after one pass — so a statement reached
// twice would carry the tenant predicate twice, with the same value
// bound twice: the rows come back right and nothing fails until an
// argument limit or a query log makes it visible.
//
// The same builder executed twice, and the same body attached to two
// statements, are the two ways a caller reaches one, and both must bind
// exactly one tenant argument.
func TestStatementBodyIsResolvedOncePerExecution(t *testing.T) {
	for _, tc := range stmtCases() {
		t.Run(tc.name, func(t *testing.T) {
			tbl, _, name := stmtTable()
			db := pg.New(dropstest.New())
			q := tc.build(db, tbl, name)

			first, firstArgs, err := q.ToSQLCtx(stmtCtx())
			if err != nil {
				t.Fatalf("ToSQLCtx: %v", err)
			}
			second, secondArgs, err := q.ToSQLCtx(stmtCtx())
			if err != nil {
				t.Fatalf("ToSQLCtx: %v", err)
			}
			if first != second || !reflect.DeepEqual(firstArgs, secondArgs) {
				t.Errorf("second execution differs:\n first = %v %v\nsecond = %v %v",
					first, firstArgs, second, secondArgs)
			}
			if !reflect.DeepEqual(firstArgs, tc.wantArgs) {
				t.Errorf("bound arguments: got = %v, want %v", firstArgs, tc.wantArgs)
			}

			// A second request against the same builder gets that
			// request's tenant, not the first one's.
			other, otherArgs, err := q.ToSQLCtx(pg.WithTenant(context.Background(), int64(9)))
			if err != nil {
				t.Fatalf("ToSQLCtx: %v", err)
			}
			if other != first {
				t.Errorf("second tenant renders different SQL:\n got = %v\nwant = %v", other, first)
			}
			if reflect.DeepEqual(otherArgs, firstArgs) {
				t.Errorf("second tenant bound the first tenant's arguments: %v", otherArgs)
			}
		})
	}
}

// TestUnscopedStatementBodyKeepsItsOwnScoping states the boundary the
// fix must not move: a body is a statement of its own, so the outer
// statement's Unscoped does not reach into it, and the body's own
// Unscoped is honoured.
func TestUnscopedStatementBodyKeepsItsOwnScoping(t *testing.T) {
	tbl, _, name := stmtTable()
	db := pg.New(dropstest.New())

	outer := db.Select().
		With(pg.CTEDef("moved", db.Delete(tbl).Returning(name))).
		FromExpr(drops.Raw(`"moved"`)).
		Unscoped()
	got, args, err := outer.ToSQLCtx(stmtCtx())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	want := `WITH "moved" AS (DELETE FROM "ax_rows" WHERE ("ax_rows"."tenantId" = $1) RETURNING "ax_rows"."name") SELECT * FROM "moved"`
	if got != want {
		t.Errorf("outer Unscoped reached into the body:\n got = %v\nwant = %v", got, want)
	}
	if wantArgs := []any{int64(7)}; !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("bound arguments: got = %v, want %v", args, wantArgs)
	}

	inner := db.Select().
		With(pg.CTEDef("moved", db.Delete(tbl).Unscoped().Returning(name))).
		FromExpr(drops.Raw(`"moved"`))
	got, args, err = inner.ToSQLCtx(stmtCtx())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	want = `WITH "moved" AS (DELETE FROM "ax_rows" RETURNING "ax_rows"."name") SELECT * FROM "moved"`
	if got != want {
		t.Errorf("the body's own Unscoped was ignored:\n got = %v\nwant = %v", got, want)
	}
	if len(args) != 0 {
		t.Errorf("bound arguments: got = %v, want none", args)
	}
}

// TestStatementBodiesWithNothingToResolveRenderUnchanged is the
// byte-identity corpus for this round. A body on an unscoped table, or
// one drops did not build, has nothing a ctx can contribute: it must
// reach the driver exactly as drops.String renders it, or the escape
// hatch has stopped being one.
func TestStatementBodiesWithNothingToResolveRenderUnchanged(t *testing.T) {
	plain := pg.NewTable("plain_rows")
	pg.Add(plain, pg.BigSerial("id").PrimaryKey())
	pname := pg.Add(plain, pg.Text("name").NotNull())

	db := pg.New(dropstest.New())
	corpus := []struct {
		name string
		body drops.Expression
	}{
		{"raw body", drops.Raw(`SELECT 1`)},
		{"caller's own closure", drops.ExprFunc(func(b *drops.Builder) {
			b.WriteString("SELECT ")
			b.AddArg("a")
		})},
		{"unscoped table select", db.Select().From(plain)},
		{"unscoped table delete", db.Delete(plain).Returning(pname)},
		{"unscoped table update", db.Update(plain).Set(pname.Val("x")).Returning(pname)},
		{"unscoped table insert", db.Insert(plain).Row(pname.Val("x")).Returning(pname)},
		{"predicate", pg.Eq(pname, "a")},
	}

	for _, tc := range corpus {
		t.Run(tc.name, func(t *testing.T) {
			build := func() *pg.SelectBuilder {
				return db.Select().
					With(pg.CTEDef("body", tc.body)).
					FromExpr(drops.Raw(`"body"`))
			}
			wantSQL, wantArgs := drops.String(build())

			got, args, err := build().ToSQLCtx(stmtCtx())
			if err != nil {
				t.Fatalf("ToSQLCtx: %v", err)
			}
			if got != wantSQL {
				t.Errorf("rendered SQL:\n got = %v\nwant = %v", got, wantSQL)
			}
			if !reflect.DeepEqual(args, wantArgs) {
				t.Errorf("bound arguments: got = %v, want %v", args, wantArgs)
			}
		})
	}
}

// TestNestedDeferredBuilderErrorIsSurfaced is the third copy of the
// same list, closed. renderForCtx type-asserted *SelectBuilder to check
// the deferred error a failed cursor decode leaves on the builder — a
// check that reached the statement handed to ExecExpr and no statement
// inside it. AfterCursor fails closed by appending a false predicate,
// so a CTE body whose cursor was corrupt rendered a statement that
// matches nothing and returned no error at all: the caller reads "the
// page was empty" where the truth is "the cursor was corrupt". The
// check belongs where the statement is resolved, which is where every
// executor and every nesting goes through.
func TestNestedDeferredBuilderErrorIsSurfaced(t *testing.T) {
	tbl, _, name := stmtTable()
	db := pg.New(dropstest.New())
	spec := pg.NewCursorSpec(pg.OrderKey{Col: name})

	tests := []struct {
		name  string
		build func() *pg.SelectBuilder
	}{
		{
			name: "cte body",
			build: func() *pg.SelectBuilder {
				body := db.Select().From(tbl).AfterCursor(spec, pg.Cursor("not-a-cursor"))
				return db.Select().With(pg.CTEDef("bad", body)).FromExpr(drops.Raw(`"bad"`))
			},
		},
		{
			name: "exists operand",
			build: func() *pg.SelectBuilder {
				body := db.Select().From(tbl).AfterCursor(spec, pg.Cursor("not-a-cursor"))
				return db.Select().FromExpr(drops.Raw(`"other"`)).Where(pg.Exists(body))
			},
		},
		{
			name: "union operand",
			build: func() *pg.SelectBuilder {
				body := db.Select().From(tbl).AfterCursor(spec, pg.Cursor("not-a-cursor"))
				return db.Select().From(tbl).Union(body)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sql, _, err := tc.build().ToSQLCtx(stmtCtx())
			if err == nil {
				t.Fatalf("ToSQLCtx error: got = nil, want the cursor decode failure; rendered %v", sql)
			}
			if sql != "" {
				t.Errorf("SQL rendered for a refused statement: got = %q, want empty", sql)
			}
		})
	}
}
