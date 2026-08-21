package mysql_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/mysql"
)

// An INSERT has no WHERE clause, so a ctx cannot filter it. The only
// two things it can do are STAMP the tenant onto the row and REFUSE
// when it cannot say which tenant that is — and, on this dialect, gate
// the upsert branch so a collision cannot rewrite another tenant's row.

// The plain case: a row that says nothing about the tenant column gets
// it from ctx. Without this the row lands on the column's DEFAULT or on
// NULL and belongs to nobody: the very next SELECT, which does carry
// the predicate, cannot see it, and the INSERT was reported as a
// success.
func TestInsertStampsTheTenantColumn(t *testing.T) {
	posts, _, _ := scopedTable("posts")
	db := mysql.New(dropstest.New())

	sql, args := mustCtxSQL(t, db.Insert(posts).Row(mysql.Bind(posts.Col("title"), "x")))
	wantText(t, sql, "INSERT INTO `posts` (`title`, `tenantId`) VALUES (?, ?)")
	wantArgs(t, args, "x", scopeTenant)
}

// A row that binds the ctx tenant itself is left alone rather than
// stamped twice.
func TestInsertAcceptsARowThatAlreadyCarriesTheCtxTenant(t *testing.T) {
	posts, _, tenant := scopedTable("posts")
	db := mysql.New(dropstest.New())

	sql, args := mustCtxSQL(t, db.Insert(posts).Row(
		mysql.Bind(posts.Col("title"), "x"),
		mysql.Bind(tenant, scopeTenant)))
	wantText(t, sql, "INSERT INTO `posts` (`title`, `tenantId`) VALUES (?, ?)")
	wantArgs(t, args, "x", scopeTenant)
}

// A row bound to somebody else's tenant is the "background job stamped
// the wrong tenant" bug, and it is refused rather than written.
func TestInsertRefusesARowBoundToAnotherTenant(t *testing.T) {
	posts, _, tenant := scopedTable("posts")
	db := mysql.New(dropstest.New())

	_, _, err := db.Insert(posts).Row(
		mysql.Bind(posts.Col("title"), "x"),
		mysql.Bind(tenant, int64(999))).ToSQLCtx(scopeCtx())
	if !errors.Is(err, mysql.ErrTenantMismatch) {
		t.Fatalf("err = %v, want ErrTenantMismatch", err)
	}
}

// A column list is fixed by the first row, so a stamp applied to one
// row and not the rest would bind values under the wrong names. The gap
// is filled per row.
func TestInsertStampsEveryRowOfABatch(t *testing.T) {
	posts, _, tenant := scopedTable("posts")
	db := mysql.New(dropstest.New())

	sql, args := mustCtxSQL(t, db.Insert(posts).
		Row(mysql.Bind(posts.Col("title"), "a")).
		Rows([]mysql.ColumnValue{
			mysql.Bind(posts.Col("title"), "b"),
			mysql.Bind(tenant, scopeTenant),
		}))
	wantText(t, sql, "INSERT INTO `posts` (`title`, `tenantId`) VALUES (?, ?), (?, ?)")
	wantArgs(t, args, "a", scopeTenant, "b", scopeTenant)
}

// A value bound as a statement is a read that decides a write, and it
// is resolved like any other statement.
func TestInsertResolvesAStatementBoundAsAValue(t *testing.T) {
	posts, postID, _ := scopedTable("posts")
	plain, _ := plainTable("plain")
	db := mysql.New(dropstest.New())

	sql, args := mustCtxSQL(t, db.Insert(plain).Row(
		mysql.SetExpr(plain.Col("postId"), mysql.Subquery(db.Select(postID).From(posts).Limit(1)))))
	wantText(t, sql, "INSERT INTO `plain` (`postId`) VALUES ((SELECT `posts`.`id` FROM `posts` "+
		"WHERE (`posts`.`tenantId` = ?) LIMIT ?))")
	wantArgs(t, args, scopeTenant, int64(1))
}

// A tenant column bound to an expression cannot be compared with the
// ctx tenant, so drops refuses rather than assuming.
func TestInsertRefusesAnOpaqueTenantBinding(t *testing.T) {
	posts, _, tenant := scopedTable("posts")
	db := mysql.New(dropstest.New())

	_, _, err := db.Insert(posts).Row(
		mysql.Bind(posts.Col("title"), "x"),
		mysql.SetExpr(tenant, mysql.Now())).ToSQLCtx(scopeCtx())
	if !errors.Is(err, mysql.ErrTenantMismatch) {
		t.Fatalf("err = %v, want ErrTenantMismatch", err)
	}
}

// Unscoped is the escape hatch for a seeder or a backfill: no stamping,
// no refusal, and the upsert branch left as written.
func TestInsertUnscopedNeitherStampsNorRefuses(t *testing.T) {
	posts, _, _ := scopedTable("posts")
	db := mysql.New(dropstest.New())

	sql, args, err := db.Insert(posts).Row(mysql.Bind(posts.Col("title"), "x")).
		Unscoped().ToSQLCtx(context.Background())
	if err != nil {
		t.Fatalf("Unscoped still refused: %v", err)
	}
	wantText(t, sql, "INSERT INTO `posts` (`title`) VALUES (?)")
	wantArgs(t, args, "x")
}

// ----------------------------------------------------------------------
// ON DUPLICATE KEY UPDATE: the gate, and why it is wider here.
// ----------------------------------------------------------------------

// MySQL's upsert names no conflict target and takes no WHERE clause, so
// the gate goes inside each assignment: the value when the existing row
// is this tenant's, the column's own value — no change at all — when it
// is not. Without it, tenant A rewrites tenant B's row by colliding
// with it, on the primary key or on any unique index.
func TestUpsertGatesEveryAssignmentOnTheTenant(t *testing.T) {
	posts, _, _ := scopedTable("posts")
	db := mysql.New(dropstest.New())

	sql, _ := mustCtxSQL(t, db.Insert(posts).
		Row(mysql.Bind(posts.Col("id"), int64(1)), mysql.Bind(posts.Col("title"), "x")).
		OnDuplicateKeyUpdateAll())
	wantText(t, sql, "INSERT INTO `posts` (`id`, `title`, `tenantId`) VALUES (?, ?, ?) "+
		"ON DUPLICATE KEY UPDATE `title` = IF(`tenantId` = VALUES(`tenantId`), VALUES(`title`), `title`)")
}

// The assignment to the tenant column itself is dropped, not gated:
// leaving it in is what would let a collision transfer ownership, and
// dropping it is also what keeps the gate readable — nothing earlier in
// the list can have changed the tenant the gate reads.
func TestUpsertDropsTheAssignmentToTheTenantColumn(t *testing.T) {
	posts, _, tenant := scopedTable("posts")
	db := mysql.New(dropstest.New())

	sql, _ := mustCtxSQL(t, db.Insert(posts).
		Row(mysql.Bind(posts.Col("title"), "x")).
		OnDuplicateKeyUpdate(
			mysql.Bind(posts.Col("title"), "z"),
			mysql.Bind(tenant, scopeTenant)))
	if strings.Contains(sql, "`tenantId` = IF") || strings.Contains(sql, "UPDATE `tenantId`") {
		t.Errorf("the upsert can still rewrite the row's owner: %s", sql)
	}
	if !strings.Contains(sql, "`title` = IF(`tenantId` = VALUES(`tenantId`), ?, `title`)") {
		t.Errorf("the surviving assignment is not gated: %s", sql)
	}
}

// The degenerate case: a table whose only non-key column IS the tenant
// axis. Dropping that assignment leaves nothing a collision may
// legally rewrite, and OnDuplicateKeyUpdateAll still renders its
// self-assignment — MySQL's "do nothing on conflict", which rewrites
// nobody's row rather than turning the upsert back into an INSERT that
// raises 1062.
func TestUpsertAllDegradesToASelfAssignmentWhenOnlyTheTenantWasLeft(t *testing.T) {
	tbl := mysql.NewTable("keys")
	id := mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	tenant := mysql.Add(tbl, mysql.BigInt("tenantId").NotNull())
	tbl.ContextFilter(mysql.TenantFilter(tenant)).ScopeWritesByTenant(tenant)
	db := mysql.New(dropstest.New())

	sql, _ := mustCtxSQL(t, db.Insert(tbl).
		Row(mysql.Bind(id, int64(1)), mysql.Bind(tenant, scopeTenant)).
		OnDuplicateKeyUpdateAll())
	wantText(t, sql, "INSERT INTO `keys` (`id`, `tenantId`) VALUES (?, ?) "+
		"ON DUPLICATE KEY UPDATE `id` = `id`")
}

// An unscoped table upserts exactly as it always did — the gate is not
// a tax on a schema that never declared a tenant.
func TestUpsertOnAnUnscopedTableIsUnchanged(t *testing.T) {
	plain, _ := plainTable("plain")
	db := mysql.New(dropstest.New())

	sql, _ := mustCtxSQL(t, db.Insert(plain).
		Row(mysql.Bind(plain.Col("postId"), int64(2))).
		OnDuplicateKeyUpdateAll())
	wantText(t, sql, "INSERT INTO `plain` (`postId`) VALUES (?) "+
		"ON DUPLICATE KEY UPDATE `postId` = VALUES(`postId`)")
}

// And an Unscoped insert keeps its ungated branch, which is the point
// of saying so at the call site.
func TestUnscopedUpsertIsNotGated(t *testing.T) {
	posts, _, _ := scopedTable("posts")
	db := mysql.New(dropstest.New())

	sql, _, err := db.Insert(posts).
		Row(mysql.Bind(posts.Col("id"), int64(1)), mysql.Bind(posts.Col("title"), "x")).
		OnDuplicateKeyUpdateAll().Unscoped().ToSQLCtx(context.Background())
	if err != nil {
		t.Fatalf("Unscoped still refused: %v", err)
	}
	if strings.Contains(sql, "IF(") {
		t.Errorf("Unscoped kept the gate: %s", sql)
	}
}

// Resolution is not idempotent, and an INSERT is reachable twice
// whenever a caller executes the same builder again. A second pass must
// not stamp a second tenant column onto a statement that already has
// one.
func TestInsertIsReusable(t *testing.T) {
	posts, _, _ := scopedTable("posts")
	db := mysql.New(dropstest.New())

	ins := db.Insert(posts).Row(mysql.Bind(posts.Col("title"), "x"))
	first, firstArgs := mustCtxSQL(t, ins)
	second, secondArgs := mustCtxSQL(t, ins)
	wantText(t, second, first)
	if len(firstArgs) != 2 || len(secondArgs) != 2 {
		t.Fatalf("args accreted: %v then %v", firstArgs, secondArgs)
	}

	_, args, err := ins.ToSQLCtx(mysql.WithTenant(context.Background(), int64(4)))
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	wantArgs(t, args, "x", int64(4))
}

// The executor is what sends it, so the statement on the wire has to
// carry the stamp.
func TestInsertExecSendsTheStampedStatement(t *testing.T) {
	posts, _, _ := scopedTable("posts")
	drv := dropstest.New()
	db := mysql.New(drv)

	if _, err := db.Insert(posts).Row(mysql.Bind(posts.Col("title"), "x")).Exec(scopeCtx()); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := drv.LastSQL(); !strings.Contains(got, "`tenantId`") {
		t.Errorf("the INSERT went out unstamped: %s", got)
	}
	if _, err := db.Insert(posts).Row(mysql.Bind(posts.Col("title"), "x")).
		Exec(context.Background()); !errors.Is(err, mysql.ErrTenantMissing) {
		t.Errorf("err = %v, want ErrTenantMissing", err)
	}
	if len(drv.Statements()) != 1 {
		t.Errorf("the refused INSERT still went to the driver: %d statements", len(drv.Statements()))
	}
}

// ----------------------------------------------------------------------
// The Entity methods, which stamp the row as well as the statement.
// ----------------------------------------------------------------------

type scopedRow struct {
	ID       int64  `db:"id"`
	TenantID int64  `db:"tenantId"`
	Title    string `db:"title"`
}

func scopedEntity(t *testing.T) (*mysql.Entity[scopedRow], *mysql.Table) {
	t.Helper()
	tbl := mysql.NewTable("rows")
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey().AutoIncrement())
	tenant := mysql.Add(tbl, mysql.BigInt("tenantId").NotNull())
	mysql.Add(tbl, mysql.Text("title"))
	ent := mysql.NewEntity[scopedRow](tbl).ScopeByTenant(tenant)
	return ent, tbl
}

// ScopeByTenant declares both halves of the axis at once: the read-side
// filter on the table, and the column a write stamps.
func TestEntityScopeByTenantScopesReadsAndWrites(t *testing.T) {
	ent, tbl := scopedEntity(t)
	drv := dropstest.New()
	db := mysql.New(drv)
	ctx := scopeCtx()

	if _, err := ent.Get(db, ctx, int64(1)); err != nil && !strings.Contains(err.Error(), "no rows") {
		t.Fatalf("Get: %v", err)
	}
	if got := drv.LastSQL(); !strings.Contains(got, "`rows`.`tenantId` = ?") {
		t.Errorf("Get went out unscoped: %s", got)
	}

	row := scopedRow{Title: "x"}
	if err := ent.Create(db, ctx, &row); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if row.TenantID != scopeTenant {
		t.Errorf("Create left the row's tenant at %d", row.TenantID)
	}
	if got := drv.LastSQL(); !strings.Contains(got, "`tenantId`") {
		t.Errorf("Create went out unstamped: %s", got)
	}

	// A bare db.Select().From(the entity's table) is scoped by the same
	// declaration, which is the difference between an axis on the table
	// and a predicate the entity methods injected.
	if _, _, err := db.Select().From(tbl).ToSQLCtx(context.Background()); !errors.Is(err, mysql.ErrTenantMissing) {
		t.Errorf("a raw SELECT over the entity's table = %v, want ErrTenantMissing", err)
	}
}

// A struct carrying somebody else's tenant is refused rather than
// written, on every path that writes a row.
func TestEntityWritesRefuseAForeignTenant(t *testing.T) {
	ent, _ := scopedEntity(t)
	db := mysql.New(dropstest.New())
	ctx := scopeCtx()

	foreign := scopedRow{TenantID: 999, Title: "x"}
	if err := ent.Create(db, ctx, &foreign); !errors.Is(err, mysql.ErrTenantMismatch) {
		t.Errorf("Create = %v, want ErrTenantMismatch", err)
	}
	if _, err := ent.CreateMany(db, ctx, []scopedRow{{TenantID: 999}}); !errors.Is(err, mysql.ErrTenantMismatch) {
		t.Errorf("CreateMany = %v, want ErrTenantMismatch", err)
	}
	if _, err := ent.UpsertMany(db, ctx, []scopedRow{{TenantID: 999}}); !errors.Is(err, mysql.ErrTenantMismatch) {
		t.Errorf("UpsertMany = %v, want ErrTenantMismatch", err)
	}
	withKey := scopedRow{ID: 1, TenantID: 999, Title: "x"}
	if err := ent.Update(db, ctx, &withKey); !errors.Is(err, mysql.ErrTenantMismatch) {
		t.Errorf("Update = %v, want ErrTenantMismatch", err)
	}
}

// Entity.Update writes every non-key column, and on a scoped entity the
// tenant column is one of them: an unstamped struct would write its
// zero over a row it is otherwise allowed to touch and hand it to no
// tenant at all.
func TestEntityUpdateStampsTheTenantItWrites(t *testing.T) {
	ent, _ := scopedEntity(t)
	drv := dropstest.New()
	db := mysql.New(drv)

	row := scopedRow{ID: 7, Title: "x"}
	if err := ent.Update(db, scopeCtx(), &row); err != nil {
		t.Fatalf("Update: %v", err)
	}
	st := drv.Last()
	for _, a := range st.Args {
		if a == int64(0) {
			t.Fatalf("the UPDATE bound a zero tenant: %s %v", st.SQL, st.Args)
		}
	}
	if !strings.Contains(st.SQL, "`rows`.`tenantId` = ?") {
		t.Errorf("the UPDATE went out unscoped: %s", st.SQL)
	}
}

// UpsertMany is the method whose collision is broadest here — any
// unique index, not the primary key alone — so its branch is the one
// that most needs the gate.
func TestEntityUpsertManyIsGated(t *testing.T) {
	ent, _ := scopedEntity(t)
	drv := dropstest.New()
	db := mysql.New(drv)

	if _, err := ent.UpsertMany(db, scopeCtx(), []scopedRow{{ID: 1, Title: "x"}}); err != nil {
		t.Fatalf("UpsertMany: %v", err)
	}
	got := drv.LastSQL()
	if !strings.Contains(got, "`title` = IF(`tenantId` = VALUES(`tenantId`), VALUES(`title`), `title`)") {
		t.Errorf("the upsert branch is not gated: %s", got)
	}
	// No assignment whose left-hand side is the tenant column: the
	// gate's own reading of it, inside an IF, is not one.
	if strings.Contains(got, "UPDATE `tenantId` = ") || strings.Contains(got, ", `tenantId` = ") {
		t.Errorf("the upsert can rewrite the row's owner: %s", got)
	}
}
