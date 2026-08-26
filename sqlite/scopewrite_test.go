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

// The write side of the axis: what a ctx does to an INSERT, which has
// no WHERE clause for a predicate to reach.
//
// An INSERT does not choose rows, it produces one, so the only two
// things a ctx can do are STAMP the tenant and REFUSE when it cannot
// say which tenant that is. Both are asserted here on rendered SQL and
// args, for the reason scope_test.go gives.

// writeTable declares both halves of the axis — the read filter and the
// write column — the way Entity.ScopeByTenant does for you. The typed
// handles come back because binding a value into an INSERT is done
// through (*Col[T]).Val, which is the only public way in.
func writeTable(name string) (tbl *sqlite.Table, tenant *sqlite.Col[int64], title *sqlite.Col[string]) {
	t := sqlite.NewTable(name)
	sqlite.Add(t, sqlite.BigInt("id").PrimaryKey())
	tenantCol := sqlite.Add(t, sqlite.BigInt("tenantId").NotNull())
	titleCol := sqlite.Add(t, sqlite.Text("title"))
	t.ContextFilter(sqlite.TenantFilter(tenantCol)).ScopeWritesByTenant(tenantCol)
	return t, tenantCol, titleCol
}

// A row that says nothing about the tenant column gets the ctx tenant
// stamped onto it. Without that it landed on the column's DEFAULT or on
// NULL — a row belonging to no tenant, invisible to the very next
// SELECT, and reported as a successful insert.
func TestInsertStampsTheCtxTenant(t *testing.T) {
	posts, _, title := writeTable("wi_posts")
	db := sqlite.New(nil)

	ins := db.Insert(posts).Values(title.Val("hello"))
	sql, args := renderCtx(t, ins, scopeCtx())
	checkSQL(t, sql, `INSERT INTO "wi_posts" ("title", "tenantId") VALUES (?, ?)`,
		args, []any{"hello", scopeTenant})
}

// Every row of a batch is stamped, not only the first. The column list
// comes from row zero, so a stamp applied unevenly would bind values
// under the wrong names — a well-formed INSERT of shifted data.
func TestInsertStampsEveryRowOfABatch(t *testing.T) {
	posts, _, title := writeTable("wb_posts")
	db := sqlite.New(nil)

	ins := db.Insert(posts).
		Values(title.Val("a")).
		Values(title.Val("b"))
	sql, args := renderCtx(t, ins, scopeCtx())
	checkSQL(t, sql,
		`INSERT INTO "wb_posts" ("title", "tenantId") VALUES (?, ?), (?, ?)`,
		args, []any{"a", scopeTenant, "b", scopeTenant})
}

// No tenant on ctx is a refusal and no statement at all.
func TestInsertWithoutTenantRefusesAndSendsNothing(t *testing.T) {
	posts, _, title := writeTable("wn_posts")
	drv := dropstest.New()
	db := sqlite.New(drv)

	_, err := db.Insert(posts).Values(title.Val("x")).Exec(context.Background())
	if !errors.Is(err, sqlite.ErrTenantMissing) {
		t.Fatalf("err = %v, want %v", err, sqlite.ErrTenantMissing)
	}
	if len(drv.SQL()) != 0 {
		t.Errorf("statements = %v, want none", drv.SQL())
	}
}

// A row that binds the tenant column itself is verified rather than
// stamped over, and a row bound to somebody else's tenant is refused.
func TestInsertVerifiesABoundTenant(t *testing.T) {
	posts, tenant, title := writeTable("wv_posts")
	db := sqlite.New(nil)

	ok := db.Insert(posts).Values(title.Val("x"), tenant.Val(scopeTenant))
	sql, args := renderCtx(t, ok, scopeCtx())
	checkSQL(t, sql, `INSERT INTO "wv_posts" ("title", "tenantId") VALUES (?, ?)`,
		args, []any{"x", scopeTenant})

	bad := db.Insert(posts).Values(title.Val("x"), tenant.Val(int64(999)))
	if _, _, err := bad.ToSQLCtx(scopeCtx()); !errors.Is(err, sqlite.ErrTenantMismatch) {
		t.Fatalf("err = %v, want %v", err, sqlite.ErrTenantMismatch)
	}
}

// A tenant column bound to an expression cannot be compared with the
// ctx tenant, so it is refused rather than accepted on the strength of
// not having been checked.
//
// The binding arrives through an InsertHook, which is the only way an
// INSERT can carry one — and is the operand position that matters most,
// because a hook is registered on the table and reaches every INSERT
// into it while showing at no call site.
func TestInsertRefusesAnOpaqueTenantBinding(t *testing.T) {
	posts, tenant, title := writeTable("wo_posts")
	posts.OnInsert(sqlite.InsertHookFunc(func(c *sqlite.InsertHookCtx) {
		c.SetExpr(tenant.Column, drops.Raw("1"))
	}))
	db := sqlite.New(nil)

	ins := db.Insert(posts).Values(title.Val("x"))
	if _, _, err := ins.ToSQLCtx(scopeCtx()); !errors.Is(err, sqlite.ErrTenantMismatch) {
		t.Fatalf("err = %v, want %v", err, sqlite.ErrTenantMismatch)
	}
}

// A hook that binds the tenant column to the right value is not stamped
// over: the stamping sees the column as bound and verifies it instead,
// which is why the hooks run before the stamping rather than at render
// time.
func TestAHookBindingTheRightTenantIsAccepted(t *testing.T) {
	posts, tenant, title := writeTable("wh_posts")
	posts.OnInsert(sqlite.InsertHookFunc(func(c *sqlite.InsertHookCtx) {
		c.Set(tenant.Val(scopeTenant))
	}))
	db := sqlite.New(nil)

	ins := db.Insert(posts).Values(title.Val("x"))
	sql, args := renderCtx(t, ins, scopeCtx())
	checkSQL(t, sql, `INSERT INTO "wh_posts" ("title", "tenantId") VALUES (?, ?)`,
		args, []any{"x", scopeTenant})
}

// What a hook binds is walked for statements exactly as a caller's own
// binding is. Applied at render time it was reachable only through a
// renderer with no ctx — the same fail-open, one layer in from the call
// site.
func TestAHookBoundSubqueryIsResolved(t *testing.T) {
	posts, postID, _ := scopedTable("wk_posts")
	plain, plainID := plainTable("wk_plain")
	db := sqlite.New(nil)
	plain.OnInsert(sqlite.InsertHookFunc(func(c *sqlite.InsertHookCtx) {
		c.SetExpr(plain.Col("postId"), sqlite.Subquery(db.Select(postID).From(posts)))
	}))

	ins := db.Insert(plain).Values(plainID.Val(1))
	sql, args := renderCtx(t, ins, scopeCtx())
	checkSQL(t, sql,
		`INSERT INTO "wk_plain" ("id", "postId") VALUES (?, (SELECT "wk_posts"."id" `+
			`FROM "wk_posts" WHERE ("wk_posts"."tenantId" = ?)))`,
		args, []any{int64(1), scopeTenant})

	if _, _, err := ins.ToSQLCtx(context.Background()); !errors.Is(err, sqlite.ErrTenantMissing) {
		t.Errorf("err = %v, want %v", err, sqlite.ErrTenantMissing)
	}
}

// INSERT OR REPLACE is the SQLite-specific hazard, and the one place
// this dialect's answer differs from PostgreSQL's rather than merely
// porting it. On a conflict SQLite DELETES the colliding rows; a
// primary key is unique across the whole table rather than per tenant,
// so tenant A guessing an id destroys tenant B's row and takes the key,
// silently, reported as one row affected. There is no SET list to prune
// and no WHERE clause to gate, so the statement is refused.
func TestInsertOrReplaceIsRefusedOnAScopedTable(t *testing.T) {
	posts, _, title := writeTable("wr_posts")
	drv := dropstest.New()
	db := sqlite.New(drv)

	_, err := db.Insert(posts).OrReplace().Values(title.Val("x")).Exec(scopeCtx())
	if !errors.Is(err, sqlite.ErrReplaceScoped) {
		t.Fatalf("err = %v, want %v", err, sqlite.ErrReplaceScoped)
	}
	if len(drv.SQL()) != 0 {
		t.Errorf("statements = %v, want none", drv.SQL())
	}
	if !strings.Contains(err.Error(), "wr_posts") {
		t.Errorf("err = %v, want it to name the table", err)
	}
}

// OR IGNORE is not refused: it skips the colliding row instead of
// destroying it, which is the answer the refusal points callers at.
func TestInsertOrIgnoreIsPermittedAndStillStamps(t *testing.T) {
	posts, _, title := writeTable("wg_posts")
	db := sqlite.New(nil)

	ins := db.Insert(posts).OrIgnore().Values(title.Val("x"))
	sql, args := renderCtx(t, ins, scopeCtx())
	checkSQL(t, sql, `INSERT OR IGNORE INTO "wg_posts" ("title", "tenantId") VALUES (?, ?)`,
		args, []any{"x", scopeTenant})
}

// Unscoped is how a migration or an import writes rows for tenants
// other than the ctx one: nothing is stamped, nothing is required, and
// OR REPLACE is permitted — all of it visible at the call site.
func TestUnscopedInsertNeitherStampsNorRefuses(t *testing.T) {
	posts, tenant, title := writeTable("wu_posts")
	db := sqlite.New(nil)

	ins := db.Insert(posts).Unscoped().OrReplace().
		Values(title.Val("x"), tenant.Val(int64(5)))
	sql, args := renderCtx(t, ins, context.Background())
	checkSQL(t, sql,
		`INSERT OR REPLACE INTO "wu_posts" ("title", "tenantId") VALUES (?, ?)`,
		args, []any{"x", int64(5)})
}

// Stamping happens on a per-execution COPY, so a builder used for two
// requests does not carry the first request's tenant into the second.
func TestInsertIsReusableAcrossRequests(t *testing.T) {
	posts, _, title := writeTable("wc_posts")
	db := sqlite.New(nil)
	ins := db.Insert(posts).Values(title.Val("x"))

	for _, tenant := range []int64{scopeTenant, 31, scopeTenant} {
		sql, args := renderCtx(t, ins, sqlite.WithTenant(context.Background(), tenant))
		checkSQL(t, sql, `INSERT INTO "wc_posts" ("title", "tenantId") VALUES (?, ?)`,
			args, []any{"x", tenant})
	}
}

// An INSERT written as a CTE body is stamped like any other INSERT,
// rather than writing a row that belongs to nobody.
func TestInsertAsACTEBodyIsStamped(t *testing.T) {
	posts, _, title := writeTable("wt_posts")
	plain, plainID := plainTable("wt_plain")
	db := sqlite.New(nil)

	cte := sqlite.CTEDef("added", db.Insert(posts).Values(title.Val("x")))
	sel := db.Select(plainID).From(plain).With(cte)
	sql, args := renderCtx(t, sel, scopeCtx())
	checkSQL(t, sql,
		`WITH "added" AS (INSERT INTO "wt_posts" ("title", "tenantId") VALUES (?, ?)) `+
			`SELECT "wt_plain"."id" FROM "wt_plain"`,
		args, []any{"x", scopeTenant})
}

// Entity.Create is the other spelling of "write a row", and the two
// must not disagree about the promise the table makes. It stamps the
// struct field and then goes through the same builder, so the tenant is
// bound once and from the ctx.
func TestEntityCreateStampsThroughTheBuilder(t *testing.T) {
	posts, _, _ := writeTable("we_posts")
	ent := sqlite.NewEntity[wePost](posts)
	drv := dropstest.New().Rows([]string{"id"}, []any{int64(1)})
	db := sqlite.New(drv)

	row := wePost{Title: "x"}
	if err := ent.Create(db, scopeCtx(), &row); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if row.TenantID != scopeTenant {
		t.Errorf("row tenant = %v, want %v", row.TenantID, scopeTenant)
	}
	last := drv.Last()
	want := `INSERT INTO "we_posts" ("tenantId", "title") VALUES (?, ?) RETURNING "id"`
	if last.SQL != want {
		t.Errorf("sql = %v, want %v", last.SQL, want)
	}
	if !sameArgs(last.Args, []any{scopeTenant, "x"}) {
		t.Errorf("args = %v, want %v", last.Args, []any{scopeTenant, "x"})
	}
}

// And it refuses without a tenant, before anything reaches the driver.
func TestEntityCreateWithoutTenantSendsNothing(t *testing.T) {
	posts, _, _ := writeTable("wf_posts")
	ent := sqlite.NewEntity[wePost](posts)
	drv := dropstest.New()
	db := sqlite.New(drv)

	row := wePost{Title: "x"}
	if err := ent.Create(db, context.Background(), &row); !errors.Is(err, sqlite.ErrTenantMissing) {
		t.Fatalf("err = %v, want %v", err, sqlite.ErrTenantMissing)
	}
	if len(drv.SQL()) != 0 {
		t.Errorf("statements = %v, want none", drv.SQL())
	}
}

// CreateMany is this dialect's bulk path — there is no COPY to stamp
// before — and it goes through the same builder, so every row of the
// batch is stamped before any bytes reach the driver.
func TestEntityCreateManyStampsEveryRow(t *testing.T) {
	posts, _, _ := writeTable("wm_posts")
	ent := sqlite.NewEntity[wePost](posts)
	drv := dropstest.New()
	db := sqlite.New(drv)

	rows := []wePost{{Title: "a"}, {Title: "b"}}
	if _, err := ent.CreateMany(db, scopeCtx(), rows); err != nil {
		t.Fatalf("CreateMany: %v", err)
	}
	last := drv.Last()
	want := `INSERT INTO "wm_posts" ("tenantId", "title") VALUES (?, ?), (?, ?)`
	if last.SQL != want {
		t.Errorf("sql = %v, want %v", last.SQL, want)
	}
	if !sameArgs(last.Args, []any{scopeTenant, "a", scopeTenant, "b"}) {
		t.Errorf("args = %v, want %v", last.Args, []any{scopeTenant, "a", scopeTenant, "b"})
	}
}

type wePost struct {
	ID       int64  `drop:"id"`
	TenantID int64  `drop:"tenantId"`
	Title    string `drop:"title"`
}
