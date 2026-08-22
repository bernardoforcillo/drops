package clickhouse_test

import (
	"errors"
	"testing"

	"github.com/bernardoforcillo/drops/clickhouse"
)

// Stamping asked "is the tenant value right?" and never "is this
// handle even ours?".
//
// It located the tenant column with Column.key, which collapses alias
// copies onto the column they were declared as and therefore calls a
// handle for the same-named column of a DIFFERENT table object a
// stranger — so a row that plainly named the tenant column was read as
// naming no tenant at all, and the ctx stamp was APPENDED ALONGSIDE
// the caller's binding:
//
//	INSERT INTO "hits" ("id", "tenantId", "tenantId") VALUES (?, ?, ?)
//	args: {1, "globex", "acme"}
//
// What a server does with a duplicate column varies — PostgreSQL
// refuses, SQLite keeps the first occurrence and stored the caller's
// value under the ctx tenant's name — so this dialect does not get to
// rely on the answer either. A batch is the normal size of a write
// here, which makes it the wrong place to be finding out.
//
// So the axis is matched by the name the statement RENDERS, and every
// occurrence is checked rather than the first.

// scForeignTen is a handle for a "tenantId" column of ANOTHER table.
// Two tables with a "tenantId" is what any multi-tenant schema looks
// like, and in a codegen'd one every table exports its own
// <Table>Cols.TenantID.
var (
	scForeign    = clickhouse.NewTable("widgets")
	scForeignTen = clickhouse.Add(scForeign, clickhouse.String("tenantId"))
)

func TestInsertReadsAForeignHandleAsTheAxisItRenders(t *testing.T) {
	t.Run("bound to another tenant through another table's handle", func(t *testing.T) {
		db := clickhouse.New(nil)
		sql, _, err := db.Insert(scHits).
			Row(scHitID.Val(1), scForeignTen.Val("globex")).
			ToSQLCtx(tenantCtx("acme"))
		if !errors.Is(err, clickhouse.ErrTenantMismatch) {
			t.Fatalf("ToSQLCtx = %v, want ErrTenantMismatch", err)
		}
		if sql != "" {
			t.Errorf("a refused INSERT still rendered: %s", sql)
		}
	})

	// The same handle carrying the right value is not an error — it
	// names the column it renders as, and the statement that goes out
	// binds the tenant ONCE.
	t.Run("bound to the ctx tenant through another table's handle", func(t *testing.T) {
		db := clickhouse.New(nil)
		sql, args, err := db.Insert(scHits).
			Row(scHitID.Val(1), scForeignTen.Val("acme")).
			ToSQLCtx(tenantCtx("acme"))
		if err != nil {
			t.Fatalf("ToSQLCtx: %v", err)
		}
		checkSQL(t, sql, `INSERT INTO "hits" ("id", "tenantId") VALUES (?, ?)`)
		checkArgs(t, args, uint64(1), "acme")
	})

	// Two bindings for one rendered column, the second of them wrong.
	// A check that stopped at the first match passed this.
	t.Run("the column bound twice, the second binding another tenant's", func(t *testing.T) {
		db := clickhouse.New(nil)
		_, _, err := db.Insert(scHits).
			Row(scHitTen.Val("acme"), scHitID.Val(1), scForeignTen.Val("globex")).
			ToSQLCtx(tenantCtx("acme"))
		if !errors.Is(err, clickhouse.ErrTenantMismatch) {
			t.Fatalf("ToSQLCtx = %v, want ErrTenantMismatch", err)
		}
	})

	// And per row, not per statement: the batch's first row is
	// impeccable, and a batch is the normal size of a write here.
	t.Run("a later row of a batch carrying another tenant", func(t *testing.T) {
		db := clickhouse.New(nil)
		_, _, err := db.Insert(scHits).
			Row(scHitID.Val(1), scForeignTen.Val("acme")).
			Row(scHitID.Val(2), scForeignTen.Val("globex")).
			ToSQLCtx(tenantCtx("acme"))
		if !errors.Is(err, clickhouse.ErrTenantMismatch) {
			t.Fatalf("ToSQLCtx = %v, want ErrTenantMismatch", err)
		}
	})
}
