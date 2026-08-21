package sqlite_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/sqlite"
)

// The entity surface, against an axis the TABLE declares.
//
// Every method here used to inject the predicate itself, from a list of
// call sites — and the list was never complete: Page injected the
// tenant and not the guard, Patch did the same, and neither Find nor a
// bare db.Select was on it at all. These pin that each method now gets
// the axis from the executor, which is one list rather than eight.

type sePost struct {
	ID       int64  `drop:"id"`
	TenantID int64  `drop:"tenantId"`
	Title    string `drop:"title"`
}

// seEntity builds the table and entity through Entity.ScopeByTenant,
// which is the spelling the readme shows and which declares both halves
// of the axis at once.
func seEntity(name string) (*sqlite.Table, *sqlite.Entity[sePost], *sqlite.Col[string]) {
	tbl := sqlite.NewTable(name)
	sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
	tenant := sqlite.Add(tbl, sqlite.BigInt("tenantId").NotNull())
	title := sqlite.Add(tbl, sqlite.Text("title"))
	ent := sqlite.NewEntity[sePost](tbl).ScopeByTenant(tenant)
	return tbl, ent, title
}

// Get, Update, Delete, Query, Page and Patch each carry the axis, and
// each refuses without a tenant.
func TestEveryEntityMethodCarriesTheAxis(t *testing.T) {
	tests := []struct {
		name string
		run  func(db *sqlite.DB, e *sqlite.Entity[sePost], title *sqlite.Col[string], ctx context.Context) error
		want string
		args []any
	}{
		{"Get", func(db *sqlite.DB, e *sqlite.Entity[sePost], title *sqlite.Col[string], ctx context.Context) error {
			_, err := e.Get(db, ctx, int64(1))
			return err
		},
			`SELECT "se_posts"."id", "se_posts"."tenantId", "se_posts"."title" FROM "se_posts" ` +
				`WHERE ("se_posts"."id" = ?) AND ("se_posts"."tenantId" = ?)`,
			[]any{int64(1), scopeTenant}},

		{"Update", func(db *sqlite.DB, e *sqlite.Entity[sePost], title *sqlite.Col[string], ctx context.Context) error {
			return e.Update(db, ctx, &sePost{ID: 1, TenantID: scopeTenant, Title: "x"})
		},
			`UPDATE "se_posts" SET "tenantId" = ?, "title" = ? ` +
				`WHERE ("se_posts"."id" = ?) AND ("se_posts"."tenantId" = ?)`,
			[]any{scopeTenant, "x", int64(1), scopeTenant}},

		{"Delete", func(db *sqlite.DB, e *sqlite.Entity[sePost], title *sqlite.Col[string], ctx context.Context) error {
			_, err := e.Delete(db, ctx, int64(1))
			return err
		},
			`DELETE FROM "se_posts" WHERE ("se_posts"."id" = ?) AND ("se_posts"."tenantId" = ?)`,
			[]any{int64(1), scopeTenant}},

		{"Query", func(db *sqlite.DB, e *sqlite.Entity[sePost], title *sqlite.Col[string], ctx context.Context) error {
			_, err := e.Query(db).All(ctx)
			return err
		},
			`SELECT "se_posts"."id", "se_posts"."tenantId", "se_posts"."title" FROM "se_posts" ` +
				`WHERE ("se_posts"."tenantId" = ?)`,
			[]any{scopeTenant}},

		{"Page", func(db *sqlite.DB, e *sqlite.Entity[sePost], title *sqlite.Col[string], ctx context.Context) error {
			_, err := e.Page(db).OrderBy(sqlite.Asc(e.PK())).Limit(2).All(ctx)
			return err
		},
			`SELECT "se_posts"."id", "se_posts"."tenantId", "se_posts"."title" FROM "se_posts" ` +
				`WHERE ("se_posts"."tenantId" = ?) ORDER BY "se_posts"."id" ASC LIMIT ?`,
			[]any{scopeTenant, int64(3)}},

		{"Patch", func(db *sqlite.DB, e *sqlite.Entity[sePost], title *sqlite.Col[string], ctx context.Context) error {
			_, err := e.Patch(db, ctx, int64(1), sqlite.Set(title, "x"))
			return err
		},
			`UPDATE "se_posts" SET "title" = ? ` +
				`WHERE ("se_posts"."id" = ?) AND ("se_posts"."tenantId" = ?)`,
			[]any{"x", int64(1), scopeTenant}},

		{"Find", func(db *sqlite.DB, e *sqlite.Entity[sePost], title *sqlite.Col[string], ctx context.Context) error {
			var out []sePost
			return db.Find(e.Table()).All(ctx, &out)
		},
			`SELECT "se_posts"."id", "se_posts"."tenantId", "se_posts"."title" FROM "se_posts" ` +
				`WHERE ("se_posts"."tenantId" = ?)`,
			[]any{scopeTenant}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, ent, title := seEntity("se_posts")
			drv := dropstest.New().
				AlwaysRows([]string{"id", "tenantId", "title"}, []any{int64(1), scopeTenant, "x"})
			db := sqlite.New(drv)

			if err := tc.run(db, ent, title, scopeCtx()); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			last := drv.Last()
			if last.SQL != tc.want {
				t.Errorf("sql = %v, want %v", last.SQL, tc.want)
			}
			if !sameArgs(last.Args, tc.args) {
				t.Errorf("args = %v, want %v", last.Args, tc.args)
			}

			// And the same call with no tenant on ctx refuses, sending
			// nothing at all.
			drv.Reset()
			if err := tc.run(db, ent, title, context.Background()); !errors.Is(err, sqlite.ErrTenantMissing) {
				t.Fatalf("bare ctx: err = %v, want %v", err, sqlite.ErrTenantMissing)
			}
			if got := drv.SQL(); len(got) != 0 {
				t.Errorf("bare ctx: statements = %v, want none", got)
			}
		})
	}
}

// EntityQuery.Unscoped is narrower than SelectBuilder.Unscoped, on
// purpose: it drops the DEFAULT filters and keeps the context filters.
//
// The two lists are not the same kind of thing. Widening a default
// scope when the caller asked to widen it costs nothing; dropping the
// row-visibility boundary hands the request every tenant's rows, on the
// one method a caller reaches for while thinking about soft-deleted
// rows rather than about tenancy. drops/pg makes the other trade; this
// dialect already behaved this way, and widening every existing
// Unscoped() call to span tenants would be a scoping regression shipped
// under the banner of scoping work.
func TestEntityQueryUnscopedKeepsTheTenantAndDropsTheDefaultFilter(t *testing.T) {
	tbl := sqlite.NewTable("su_posts")
	sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
	tenant := sqlite.Add(tbl, sqlite.BigInt("tenantId").NotNull())
	sqlite.Add(tbl, sqlite.Text("title"))
	sqlite.SoftDelete(tbl)
	ent := sqlite.NewEntity[sePost](tbl, sqlite.AllowUnmappedColumns("deletedAt")).
		ScopeByTenant(tenant)

	drv := dropstest.New().AlwaysRows([]string{"id", "tenantId", "title"})
	db := sqlite.New(drv)

	if _, err := ent.Query(db).Unscoped().All(scopeCtx()); err != nil {
		t.Fatalf("All: %v", err)
	}
	got := drv.LastSQL()
	if strings.Contains(got, `"deletedAt" IS NULL`) {
		t.Errorf("Unscoped did not drop the default filter:\n%s", got)
	}
	if !strings.Contains(got, `("su_posts"."tenantId" = ?)`) {
		t.Errorf("Unscoped dropped the tenant axis:\n%s", got)
	}

	// And it still refuses on a ctx with no tenant, rather than
	// widening to every tenant because the caller asked for hidden rows.
	drv.Reset()
	if _, err := ent.Query(db).Unscoped().All(context.Background()); !errors.Is(err, sqlite.ErrTenantMissing) {
		t.Fatalf("err = %v, want %v", err, sqlite.ErrTenantMissing)
	}
	if len(drv.SQL()) != 0 {
		t.Errorf("statements = %v, want none", drv.SQL())
	}
}

// An EntityQuery is a value a caller may execute twice, and the second
// execution must not carry the first one's tenant. The old
// scopesApplied flag existed because the predicate was appended to the
// shared builder; resolving onto a copy removes both the flag and the
// bug it half-covered.
func TestAnEntityQueryIsReusableAcrossRequests(t *testing.T) {
	_, ent, _ := seEntity("sq_posts")
	drv := dropstest.New().AlwaysRows([]string{"id", "tenantId", "title"})
	db := sqlite.New(drv)

	q := ent.Query(db)
	for _, tenant := range []int64{scopeTenant, 44, scopeTenant} {
		if _, err := q.All(sqlite.WithTenant(context.Background(), tenant)); err != nil {
			t.Fatalf("All: %v", err)
		}
		last := drv.Last()
		if n := strings.Count(last.SQL, `"sq_posts"."tenantId" = ?`); n != 1 {
			t.Fatalf("the predicate appeared %d times: %s", n, last.SQL)
		}
		if !sameArgs(last.Args, []any{tenant}) {
			t.Errorf("args = %v, want %v", last.Args, []any{tenant})
		}
	}
}

// A DEFAULT filter can hold a statement too — the invariant is about
// what the renderer reads, not about when the list was declared. Left
// unwalked, DefaultFilter(NotIn(col, <select over a scoped table>))
// rendered the inner statement with none of its context filters, on any
// ctx at all.
func TestADefaultFilterHoldingAStatementIsResolved(t *testing.T) {
	blocked, blockedID, _ := scopedTable("df_blocked")
	notes := sqlite.NewTable("df_notes")
	noteID := sqlite.Add(notes, sqlite.BigInt("id").PrimaryKey())
	db := sqlite.New(nil)
	notes.DefaultFilter(sqlite.NotIn(noteID, db.Select(blockedID).From(blocked)))

	sel := db.Select(noteID).From(notes)
	sql, args := renderCtx(t, sel, scopeCtx())
	checkSQL(t, sql,
		`SELECT "df_notes"."id" FROM "df_notes" WHERE ("df_notes"."id" NOT IN (`+
			`SELECT "df_blocked"."id" FROM "df_blocked" WHERE ("df_blocked"."tenantId" = ?)))`,
		args, []any{scopeTenant})

	if _, _, err := sel.ToSQLCtx(context.Background()); !errors.Is(err, sqlite.ErrTenantMissing) {
		t.Errorf("err = %v, want %v", err, sqlite.ErrTenantMissing)
	}
}

// A default filter with NO statement in it — every soft-delete guard
// there is — is not rebuilt, not re-wrapped, and renders the bytes it
// always did whether or not a ctx is in hand.
func TestAPlainDefaultFilterRendersIdenticallyWithAndWithoutACtx(t *testing.T) {
	notes := sqlite.NewTable("dp_notes")
	noteID := sqlite.Add(notes, sqlite.BigInt("id").PrimaryKey())
	sqlite.SoftDelete(notes)
	db := sqlite.New(nil)

	sel := db.Select(noteID).From(notes)
	bare, _ := sel.ToSQL()
	withCtx, _ := renderCtx(t, sel, scopeCtx())
	if bare != withCtx {
		t.Errorf("ToSQL = %v, ToSQLCtx = %v; a filter with no statement in it must render identically",
			bare, withCtx)
	}
	if want := `SELECT "dp_notes"."id" FROM "dp_notes" WHERE ("dp_notes"."deletedAt" IS NULL)`; bare != want {
		t.Errorf("got = %v, want %v", bare, want)
	}
}

// A statement drops did not build, handed in as a filter's predicate,
// is asked for its ctx form so the refusal propagates — the whole
// statement is refused rather than sent with a foreign statement inside
// it that nobody scoped.
func TestAForeignStatementInAFilterIsAskedForItsCtxForm(t *testing.T) {
	notes := sqlite.NewTable("fs_notes")
	noteID := sqlite.Add(notes, sqlite.BigInt("id").PrimaryKey())
	refusal := errors.New("the foreign statement refused")
	notes.ContextFilter(func(context.Context) (drops.Expression, error) {
		return sqlite.Exists(foreignStatement{err: refusal}), nil
	})

	db := sqlite.New(nil)
	if _, _, err := db.Select(noteID).From(notes).ToSQLCtx(scopeCtx()); !errors.Is(err, refusal) {
		t.Fatalf("err = %v, want %v", err, refusal)
	}
}

// foreignStatement is a caller's own statement type: it renders, and it
// answers ToSQLCtx, which is the only way into it. Satisfying the
// interface structurally is the point — drops did not build it and
// still asks it what this ctx would send.
type foreignStatement struct{ err error }

func (f foreignStatement) WriteSQL(b *drops.Builder) { b.WriteString("SELECT 1") }

func (f foreignStatement) ToSQLCtx(context.Context) (string, []any, error) {
	return "", nil, f.err
}
