package pg_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/pg"
)

// pg/tenant.go promises that "every statement that reads or writes it
// takes the tenant from ctx and carries the predicate". db.Insert()
// kept none of it: the builder never saw a ctx, so a row went to the
// server with whatever the caller bound for the tenant column —
// typically nothing, which is the column's DEFAULT or NULL and belongs
// to no tenant at all. The entity methods stamped; the builder the
// readme also shows did not, so the two spellings of "write a row"
// disagreed about a promise the package makes for the table.
//
// The assertions are on the rendered statement and its arguments
// rather than on a round trip: the failure is a wrong (or missing)
// bound value, and a fixture holding one tenant's rows cannot see it.

// gadget is the row type behind the raw-insert fixture. It exists so
// the tenant axis can be declared the way an application declares it.
type gadget struct {
	ID       int64  `drop:"id"`
	TenantID int64  `drop:"tenantId"`
	Name     string `drop:"name"`
}

// gadgetSchema returns a table whose tenant axis is declared, plus the
// column handles a raw INSERT binds through.
func gadgetSchema() (tbl *pg.Table, id *pg.Col[int64], tenant *pg.Col[int64], name *pg.Col[string]) {
	tbl = pg.NewTable("gadgets")
	id = pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	tenant = pg.Add(tbl, pg.BigInt("tenantId").NotNull())
	name = pg.Add(tbl, pg.Text("name").NotNull())
	pg.NewEntity[gadget](tbl).ScopeByTenant(tenant)
	return tbl, id, tenant, name
}

// tenantCtx is the ctx an application request carries.
func tenantCtx(t any) context.Context { return pg.WithTenant(context.Background(), t) }

func TestInsertBuilderStampsTheCtxTenant(t *testing.T) {
	tbl, _, tenant, name := gadgetSchema()
	tests := []struct {
		name     string
		rows     [][]pg.ColumnValue
		wantSQL  string
		wantArgs []any
	}{
		{
			name:     "column not bound at all",
			rows:     [][]pg.ColumnValue{{name.Val("a")}},
			wantSQL:  `INSERT INTO "gadgets" ("name", "tenantId") VALUES ($1, $2)`,
			wantArgs: []any{"a", int64(3)},
		},
		{
			name:     "column bound to the ctx tenant by hand",
			rows:     [][]pg.ColumnValue{{tenant.Val(3), name.Val("a")}},
			wantSQL:  `INSERT INTO "gadgets" ("tenantId", "name") VALUES ($1, $2)`,
			wantArgs: []any{int64(3), "a"},
		},
		{
			name: "batch where only the first row names the tenant",
			rows: [][]pg.ColumnValue{
				{tenant.Val(3), name.Val("a")},
				{name.Val("b")},
			},
			wantSQL:  `INSERT INTO "gadgets" ("tenantId", "name") VALUES ($1, $2), ($3, $4)`,
			wantArgs: []any{int64(3), "a", int64(3), "b"},
		},
		{
			name: "batch where no row names the tenant",
			rows: [][]pg.ColumnValue{
				{name.Val("a")},
				{name.Val("b")},
			},
			wantSQL:  `INSERT INTO "gadgets" ("name", "tenantId") VALUES ($1, $2), ($3, $4)`,
			wantArgs: []any{"a", int64(3), "b", int64(3)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			drv := dropstest.New()
			db := pg.New(drv)
			if _, err := db.Insert(tbl).Rows(tt.rows...).Exec(tenantCtx(int64(3))); err != nil {
				t.Fatalf("Exec: %v", err)
			}
			if got := drv.LastSQL(); got != tt.wantSQL {
				t.Errorf("rendered SQL:\n got = %v\nwant = %v", got, tt.wantSQL)
			}
			if got := drv.Last().Args; !reflect.DeepEqual(got, tt.wantArgs) {
				t.Errorf("bound arguments: got = %v, want %v", got, tt.wantArgs)
			}
		})
	}
}

// TestInsertBuilderWithoutATenantFailsClosed pins the refusal half. An
// INSERT has no WHERE clause for a predicate to narrow, so a ctx with
// no tenant cannot be answered with a narrower statement — only with
// no statement.
func TestInsertBuilderWithoutATenantFailsClosed(t *testing.T) {
	tbl, id, tenant, name := gadgetSchema()
	tests := []struct {
		name string
		ctx  context.Context
		run  func(*pg.DB, context.Context) error
		want error
	}{
		{
			name: "exec with no tenant on ctx",
			ctx:  context.Background(),
			run: func(db *pg.DB, ctx context.Context) error {
				_, err := db.Insert(tbl).Row(name.Val("a")).Exec(ctx)
				return err
			},
			want: pg.ErrTenantMissing,
		},
		{
			name: "one with no tenant on ctx",
			ctx:  context.Background(),
			run: func(db *pg.DB, ctx context.Context) error {
				var g gadget
				return db.Insert(tbl).Row(name.Val("a")).Returning(id).One(ctx, &g)
			},
			want: pg.ErrTenantMissing,
		},
		{
			name: "all with no tenant on ctx",
			ctx:  context.Background(),
			run: func(db *pg.DB, ctx context.Context) error {
				var gs []gadget
				return db.Insert(tbl).Row(name.Val("a")).Returning(id).All(ctx, &gs)
			},
			want: pg.ErrTenantMissing,
		},
		{
			name: "upsert with no tenant on ctx",
			ctx:  context.Background(),
			run: func(db *pg.DB, ctx context.Context) error {
				_, err := db.Insert(tbl).Row(name.Val("a")).
					OnConflictUpdate(id).Set(name.Expr(pg.Excluded(name))).
					Done().Exec(ctx)
				return err
			},
			want: pg.ErrTenantMissing,
		},
		{
			name: "row owned by another tenant",
			ctx:  tenantCtx(int64(3)),
			run: func(db *pg.DB, ctx context.Context) error {
				_, err := db.Insert(tbl).Row(tenant.Val(9), name.Val("a")).Exec(ctx)
				return err
			},
			want: pg.ErrTenantMismatch,
		},
		{
			name: "second row of a batch owned by another tenant",
			ctx:  tenantCtx(int64(3)),
			run: func(db *pg.DB, ctx context.Context) error {
				_, err := db.Insert(tbl).
					Row(tenant.Val(3), name.Val("a")).
					Row(tenant.Val(9), name.Val("b")).
					Exec(ctx)
				return err
			},
			want: pg.ErrTenantMismatch,
		},
		{
			name: "tenant bound to an expression nothing can compare",
			ctx:  tenantCtx(int64(3)),
			run: func(db *pg.DB, ctx context.Context) error {
				_, err := db.Insert(tbl).
					Row(tenant.Expr(drops.Raw(`(SELECT 9)`)), name.Val("a")).
					Exec(ctx)
				return err
			},
			want: pg.ErrTenantMismatch,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			drv := dropstest.New()
			db := pg.New(drv)
			err := tt.run(db, tt.ctx)
			if !errors.Is(err, tt.want) {
				t.Errorf("error: got = %v, want %v", err, tt.want)
			}
			// Failing closed means no statement at all, not a statement
			// the server happens to reject.
			if got, want := len(drv.Statements()), 0; got != want {
				t.Errorf("statements sent: got = %v, want %v (%v)", got, want, drv.Statements())
			}
		})
	}
}

// TestInsertOnConflictUpdateIsGatedOnTheTenant covers the half of the
// promise stamping does not reach. A primary key is unique across the
// whole table and not per tenant, so the row an INSERT collides with
// may belong to somebody else; an ungated DO UPDATE rewrites that row's
// data and its owner, which is tenant A taking tenant B's row by
// guessing an id.
func TestInsertOnConflictUpdateIsGatedOnTheTenant(t *testing.T) {
	tbl, id, tenant, name := gadgetSchema()
	drv := dropstest.New()
	db := pg.New(drv)

	_, err := db.Insert(tbl).Row(name.Val("a")).
		OnConflictUpdate(id).
		Set(name.Expr(pg.Excluded(name)), tenant.Expr(pg.Excluded(tenant))).
		Done().Exec(tenantCtx(int64(3)))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	want := `INSERT INTO "gadgets" ("name", "tenantId") VALUES ($1, $2) ` +
		`ON CONFLICT ("id") DO UPDATE SET "name" = EXCLUDED."name" ` +
		`WHERE "tenantId" = EXCLUDED."tenantId"`
	if got := drv.LastSQL(); got != want {
		t.Errorf("rendered SQL:\n got = %v\nwant = %v", got, want)
	}
	if got, want := drv.Last().Args, []any{"a", int64(3)}; !reflect.DeepEqual(got, want) {
		t.Errorf("bound arguments: got = %v, want %v", got, want)
	}
}

// TestInsertOnConflictUpdateKeepsTheCallersOwnGate pins that the gate
// is added to what the caller wrote rather than instead of it.
func TestInsertOnConflictUpdateKeepsTheCallersOwnGate(t *testing.T) {
	tbl, id, _, name := gadgetSchema()
	drv := dropstest.New()
	db := pg.New(drv)

	_, err := db.Insert(tbl).Row(name.Val("a")).
		OnConflictUpdate(id).
		Set(name.Expr(pg.Excluded(name))).
		Where(pg.Ne(name, "frozen")).
		Done().Exec(tenantCtx(int64(3)))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	want := `INSERT INTO "gadgets" ("name", "tenantId") VALUES ($1, $2) ` +
		`ON CONFLICT ("id") DO UPDATE SET "name" = EXCLUDED."name" ` +
		`WHERE ("gadgets"."name" <> $3) AND "tenantId" = EXCLUDED."tenantId"`
	if got := drv.LastSQL(); got != want {
		t.Errorf("rendered SQL:\n got = %v\nwant = %v", got, want)
	}
}

// TestInsertOnConflictUpdateWithNothingLeftDoesNothing pins the edge
// dropping the tenant assignment creates: on a table whose only
// non-key column is the tenant axis the SET list comes out empty, and
// "DO UPDATE SET" with no assignments is a syntax error rather than a
// no-op.
func TestInsertOnConflictUpdateWithNothingLeftDoesNothing(t *testing.T) {
	type membership struct {
		ID       int64 `drop:"id"`
		TenantID int64 `drop:"tenantId"`
	}
	tbl := pg.NewTable("memberships_raw")
	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	tenant := pg.Add(tbl, pg.BigInt("tenantId").NotNull())
	pg.NewEntity[membership](tbl).ScopeByTenant(tenant)

	drv := dropstest.New()
	db := pg.New(drv)
	_, err := db.Insert(tbl).Row(id.Val(1)).
		OnConflictUpdate(id).Set(tenant.Expr(pg.Excluded(tenant))).
		Done().Exec(tenantCtx(int64(3)))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	want := `INSERT INTO "memberships_raw" ("id", "tenantId") VALUES ($1, $2) ` +
		`ON CONFLICT ("id") DO NOTHING`
	if got := drv.LastSQL(); got != want {
		t.Errorf("rendered SQL:\n got = %v\nwant = %v", got, want)
	}
}

// TestInsertResolvesTheStatementsWrittenInsideIt covers the operand
// positions an INSERT offers a *SelectBuilder. Each of them used to be
// rendered through WriteSQL, which has no ctx and so writes a SELECT
// over a tenant-scoped table with none of its scoping — a read across
// every customer, in a statement whose own scoping looks present.
func TestInsertResolvesTheStatementsWrittenInsideIt(t *testing.T) {
	tbl, id, _, name := gadgetSchema()
	notes, noteID, _, noteBody := noteSchema()
	_ = noteBody

	tests := []struct {
		name  string
		build func(*pg.DB) *pg.InsertBuilder
	}{
		{
			name: "subquery bound as a row value",
			build: func(db *pg.DB) *pg.InsertBuilder {
				sub := db.Select(noteID).From(notes).Limit(1)
				return db.Insert(tbl).Row(name.Expr(pg.Subquery(sub)))
			},
		},
		{
			name: "subquery in a RETURNING term",
			build: func(db *pg.DB) *pg.InsertBuilder {
				sub := db.Select(noteID).From(notes).Limit(1)
				return db.Insert(tbl).Row(name.Val("a")).Returning(pg.Subquery(sub))
			},
		},
		{
			name: "subquery assigned by the conflict update",
			build: func(db *pg.DB) *pg.InsertBuilder {
				sub := db.Select(noteID).From(notes).Limit(1)
				return db.Insert(tbl).Row(name.Val("a")).
					OnConflictUpdate(id).Set(name.Expr(pg.Subquery(sub))).Done()
			},
		},
		{
			name: "subquery gating the conflict update",
			build: func(db *pg.DB) *pg.InsertBuilder {
				sub := db.Select(noteID).From(notes).Limit(1)
				return db.Insert(tbl).Row(name.Val("a")).
					OnConflictUpdate(id).Set(name.Expr(pg.Excluded(name))).
					Where(pg.Exists(sub)).Done()
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			drv := dropstest.New()
			db := pg.New(drv)
			if _, err := tt.build(db).Exec(tenantCtx(int64(3))); err != nil {
				t.Fatalf("Exec: %v", err)
			}
			sql := drv.LastSQL()
			if got, want := strings.Contains(sql, `FROM "notes"`), true; got != want {
				t.Fatalf("statement embeds the subquery: got = %v, want %v (%v)", got, want, sql)
			}
			if got, want := strings.Contains(sql, `"notes"."tenantId" = `), true; got != want {
				t.Errorf("subquery carries its own tenant axis: got = %v, want %v (%v)", got, want, sql)
			}
			// The tenant is bound twice: once by the INSERT's own
			// stamping, once by the subquery's predicate.
			var tenants int
			for _, a := range drv.Last().Args {
				if v, ok := a.(int64); ok && v == 3 {
					tenants++
				}
			}
			if got, want := tenants, 2; got != want {
				t.Errorf("tenant bindings: got = %v, want %v (%v)", got, want, drv.Last().Args)
			}
		})
	}
}

// note is the row type of the second scoped table, the one the
// subquery tests read from.
type note struct {
	ID       int64  `drop:"id"`
	TenantID int64  `drop:"tenantId"`
	Body     string `drop:"body"`
}

func noteSchema() (tbl *pg.Table, id *pg.Col[int64], tenant *pg.Col[int64], body *pg.Col[string]) {
	tbl = pg.NewTable("notes")
	id = pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	tenant = pg.Add(tbl, pg.BigInt("tenantId").NotNull())
	body = pg.Add(tbl, pg.Text("body").NotNull())
	pg.NewEntity[note](tbl).ScopeByTenant(tenant)
	return tbl, id, tenant, body
}

// TestInsertHookBindingsGoThroughTheSameRules covers the operand
// position no call site shows. An InsertHook is registered on the table
// and reaches every INSERT into it, and InsertHookCtx.SetExpr takes any
// expression — so a hook is somewhere a *SelectBuilder can be handed
// to, one layer in from the caller's own binding, and it was rendered
// by a renderer that had no ctx.
func TestInsertHookBindingsGoThroughTheSameRules(t *testing.T) {
	t.Run("hook-bound subquery carries its own tenant axis", func(t *testing.T) {
		tbl, id, _, name := gadgetSchema()
		notes, noteID, _, _ := noteSchema()
		drv := dropstest.New()
		db := pg.New(drv)

		sub := db.Select(noteID).From(notes).Limit(1)
		tbl.OnInsert(pg.InsertHookFunc(func(c *pg.InsertHookCtx) {
			c.Set(name.Expr(pg.Subquery(sub)))
		}))
		if _, err := db.Insert(tbl).Row(id.Val(1)).Exec(tenantCtx(int64(3))); err != nil {
			t.Fatalf("Exec: %v", err)
		}
		sql := drv.LastSQL()
		if got, want := strings.Contains(sql, `"notes"."tenantId" = `), true; got != want {
			t.Errorf("hook's subquery carries its tenant axis: got = %v, want %v (%v)", got, want, sql)
		}
	})

	t.Run("hook-bound tenant column is checked, not stamped twice", func(t *testing.T) {
		tbl, id, tenant, _ := gadgetSchema()
		drv := dropstest.New()
		db := pg.New(drv)

		tbl.OnInsert(pg.InsertHookFunc(func(c *pg.InsertHookCtx) {
			c.Set(tenant.Val(3))
		}))
		if _, err := db.Insert(tbl).Row(id.Val(1)).Exec(tenantCtx(int64(3))); err != nil {
			t.Fatalf("Exec: %v", err)
		}
		// A column named twice is 42701 from the server, so the
		// stamping has to see what the hook bound.
		want := `INSERT INTO "gadgets" ("id", "tenantId") VALUES ($1, $2)`
		if got := drv.LastSQL(); got != want {
			t.Errorf("rendered SQL:\n got = %v\nwant = %v", got, want)
		}
	})

	t.Run("hook binding another tenant is refused", func(t *testing.T) {
		tbl, id, tenant, _ := gadgetSchema()
		drv := dropstest.New()
		db := pg.New(drv)

		tbl.OnInsert(pg.InsertHookFunc(func(c *pg.InsertHookCtx) {
			c.Set(tenant.Val(9))
		}))
		_, err := db.Insert(tbl).Row(id.Val(1)).Exec(tenantCtx(int64(3)))
		if !errors.Is(err, pg.ErrTenantMismatch) {
			t.Errorf("error: got = %v, want %v", err, pg.ErrTenantMismatch)
		}
		if got, want := len(drv.Statements()), 0; got != want {
			t.Errorf("statements sent: got = %v, want %v (%v)", got, want, drv.Statements())
		}
	})
}
