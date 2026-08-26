package integration_test

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	"github.com/bernardoforcillo/drops/pg"
	"github.com/bernardoforcillo/drops/sqlite"
	"github.com/bernardoforcillo/drops/stdlib"
)

// The tenant-scoping phase closed one question three times: a handle
// names a column two ways at once, drops asked by IDENTITY and the
// renderer answered with a NAME. "The same name" turned out to be the
// SERVER's question rather than Go's, and each dialect now answers it
// in its own identKey.
//
// These tests ask the same question one level over, about VALUES rather
// than about names:
//
//	every place drops decides whether two VALUES are the same in Go,
//	while the statement it renders leaves that decision to the
//	column's type and collation, is a candidate.
//
// sameTenant is that decision for the tenant axis. It compares the ctx
// tenant with a bound value by Go equality and a lossless round trip,
// and tenant.go's section 1 states it as THE definition of the same
// tenant. The predicate drops renders — "tenantId" = $1 — makes no such
// comparison: it hands both sides to the server, which compares them by
// the column's collation and coerces the parameter to the column's
// type. Where those two disagree, drops has two tenants and the server
// has one, and the rows of one are the rows of the other.
//
// Neither of these is asserted as correct. They are what happens today,
// pinned so that the entries in each dialect's "Where the automatic
// scoping stops" list are checkable rather than remembered, and so that
// closing the gap shows up here as a diff rather than as a surprise.
// The failure mode is silent in both directions: a read returns another
// tenant's rows, and a write lands under another tenant's value.

// The SQLite half runs in-process, for the reason
// sqlite_tenant_axis_test.go gives: a leak the whole suite is about has
// to be catchable on every CI run rather than on the ones with
// containers. COLLATE NOCASE is SQLite's own case-insensitive
// comparison, and it is a column property the server applies to every
// comparison against that column — including the one drops renders.
func TestSQLiteCaseInsensitiveAxisFoldsTwoTenants(t *testing.T) {
	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sqlDB.Close()
	db := sqlite.New(stdlib.New(sqlDB))
	ctx := context.Background()

	if _, err := db.Exec(ctx, `CREATE TABLE fold_posts (
		id INTEGER PRIMARY KEY,
		"tenantId" TEXT COLLATE NOCASE NOT NULL,
		title TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	tbl := sqlite.NewTable("fold_posts")
	sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
	tenantCol := sqlite.Add(tbl, sqlite.Text("tenantId").NotNull())
	title := sqlite.Add(tbl, sqlite.Text("title"))
	tbl.ContextFilter(sqlite.TenantFilter(tenantCol)).ScopeWritesByTenant(tenantCol)

	lower := sqlite.WithTenant(ctx, "acme")
	upper := sqlite.WithTenant(ctx, "ACME")

	// drops calls these two different tenants: sameTenant compares
	// strings by Go equality, and neither converts onto the other
	// losing nothing.
	if _, err := db.Insert(tbl).Values(title.Val("written by acme")).Exec(lower); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// The statement the other tenant sends is correctly scoped, and
	// still reads the row.
	sqlText, args, err := db.Select(title).From(tbl).ToSQLCtx(upper)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	const wantSQL = `SELECT "fold_posts"."title" FROM "fold_posts" WHERE ("fold_posts"."tenantId" = ?)`
	if sqlText != wantSQL {
		t.Errorf("sql = %v, want %v", sqlText, wantSQL)
	}
	if len(args) != 1 || args[0] != "ACME" {
		t.Fatalf("args = %v, want [ACME]", args)
	}
	rows, err := db.Query(upper, sqlText, args...)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(got) != 1 || got[0] != "written by acme" {
		t.Fatalf("tenant \"ACME\" read %v; the entry in sqlite's \"Where the automatic scoping stops\" "+
			"list says it reads acme's row, so either the server changed or the gap is closed and "+
			"the entry has to go", got)
	}
	t.Logf("tenant %q read %q, written by tenant %q, through %s",
		"ACME", got[0], "acme", wantSQL)
}

// The PostgreSQL half needs a nondeterministic collation, which is
// where the same shape lives in a dialect that compares byte for byte
// by default. It is skipped rather than failed where the server cannot
// make one: what is being demonstrated is a property of the COLUMN, and
// a server with no ICU has no such column to declare.
func TestPGNondeterministicAxisFoldsTwoTenants(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()

	if _, err := db.Exec(ctx, `CREATE COLLATION IF NOT EXISTS drops_fold_ci `+
		`(provider = icu, locale = 'und-u-ks-level2', deterministic = false)`); err != nil {
		t.Skipf("this server cannot declare a nondeterministic collation: %v", err)
	}
	name := "fold_posts"
	if _, err := db.Exec(ctx, `DROP TABLE IF EXISTS `+name); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := db.Exec(ctx, `CREATE TABLE `+name+` (
		id bigserial PRIMARY KEY,
		"tenantId" text COLLATE drops_fold_ci NOT NULL,
		title text)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(context.Background(), `DROP TABLE IF EXISTS `+name) })

	tbl := pg.NewTable(name)
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	tenantCol := pg.Add(tbl, pg.Text("tenantId").NotNull())
	title := pg.Add(tbl, pg.Text("title"))
	tbl.ContextFilter(pg.TenantFilter(tenantCol)).ScopeWritesByTenant(tenantCol)

	lower := pg.WithTenant(ctx, "acme")
	upper := pg.WithTenant(ctx, "ACME")

	if _, err := db.Insert(tbl).Row(title.Val("written by acme")).Exec(lower); err != nil {
		t.Fatalf("insert: %v", err)
	}
	sqlText, args, err := db.Select(title).From(tbl).ToSQLCtx(upper)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	const wantSQL = `SELECT "fold_posts"."title" FROM "fold_posts" WHERE ("fold_posts"."tenantId" = $1)`
	if sqlText != wantSQL {
		t.Errorf("sql = %v, want %v", sqlText, wantSQL)
	}
	if len(args) != 1 || args[0] != "ACME" {
		t.Fatalf("args = %v, want [ACME]", args)
	}
	rows, err := db.Query(upper, sqlText, args...)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(got) != 1 || got[0] != "written by acme" {
		t.Fatalf("tenant \"ACME\" read %v; pg's \"Where the automatic scoping stops\" list says it "+
			"reads acme's row on a nondeterministic collation", got)
	}
	t.Logf("tenant %q read %q, written by tenant %q, through %s",
		"ACME", got[0], "acme", wantSQL)
}

// The other half of the same question, with no collation in it: the
// server also decides the TYPE the parameter is read as. A ctx tenant
// of "01" against a bigint axis is the pairing tenant.go's section 1
// calls a type confusion and refuses outright — when what it is
// comparing is a BINDING, in Go. As the ctx tenant it is bound into the
// predicate and into the stamp, where PostgreSQL parses it as the
// column's type, and "01" and int64(1) become one tenant.
func TestPGNumericAxisFoldsAStringTenant(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()

	name := "fold_num_posts"
	if _, err := db.Exec(ctx, `DROP TABLE IF EXISTS `+name); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := db.Exec(ctx, `CREATE TABLE `+name+` (
		id bigserial PRIMARY KEY,
		"tenantId" bigint NOT NULL,
		title text)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(context.Background(), `DROP TABLE IF EXISTS `+name) })

	tbl := pg.NewTable(name)
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	tenantCol := pg.Add(tbl, pg.BigInt("tenantId").NotNull())
	title := pg.Add(tbl, pg.Text("title"))
	tbl.ContextFilter(pg.TenantFilter(tenantCol)).ScopeWritesByTenant(tenantCol)

	one := pg.WithTenant(ctx, int64(1))
	spelled := pg.WithTenant(ctx, "01")

	if _, err := db.Insert(tbl).Row(title.Val("written by 1")).Exec(one); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// The read.
	sqlText, args, err := db.Select(title).From(tbl).ToSQLCtx(spelled)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(args) != 1 || args[0] != "01" {
		t.Fatalf("args = %v, want [01]", args)
	}
	rows, err := db.Query(spelled, sqlText, args...)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	var got []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		got = append(got, s)
	}
	rows.Close()
	if len(got) != 1 || got[0] != "written by 1" {
		t.Fatalf("tenant \"01\" read %v; pg's \"Where the automatic scoping stops\" list says it reads "+
			"tenant 1's rows", got)
	}
	// And the write: the stamp binds the ctx value, and the server
	// stores it as the column's type, so the row lands under 1.
	if _, err := db.Insert(tbl).Row(title.Val("written by 01")).Exec(spelled); err != nil {
		t.Fatalf("insert: %v", err)
	}
	stored, err := db.Query(ctx, `SELECT title, "tenantId" FROM `+name+` ORDER BY id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer stored.Close()
	var landed []string
	for stored.Next() {
		var s string
		var tid int64
		if err := stored.Scan(&s, &tid); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if tid != 1 {
			t.Fatalf("row %q stored under tenantId %d, want 1", s, tid)
		}
		landed = append(landed, s)
	}
	if err := stored.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(landed) != 2 {
		t.Fatalf("rows under tenantId 1 = %v, want both", landed)
	}
	t.Logf("both %v are stored under tenantId 1, written by ctx tenants int64(1) and %q", landed, "01")
}
