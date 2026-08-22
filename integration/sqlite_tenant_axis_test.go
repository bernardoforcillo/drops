package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bernardoforcillo/drops/sqlite"
)

// The tenant leak this suite exists to catch: not a statement that
// looks wrong, but one that renders perfectly and stores a row under
// somebody else's tenant.
//
// Stamping located the tenant column by handle identity, so a binding
// made through a handle for the same-named column of a DIFFERENT table
// object was read as naming no tenant at all and the ctx stamp was
// appended alongside it:
//
//	INSERT INTO "zzi" ("title", "tenantId", "tenantId") VALUES (?, ?, ?)
//	args: ["x", "evil", "acme"]
//
// PostgreSQL answers 42701 and fails closed. SQLite ACCEPTS a
// duplicate column and keeps the FIRST occurrence, so this is the
// dialect where the row lands: written under ctx tenant "acme",
// stored as "evil", no error — in the dialect whose own tenant.go says
// the predicates are the whole boundary because there is no row-level
// security underneath them.
//
// A unit test can only assert the rendering. Which of two duplicate
// columns a server keeps is the server's answer, so this asks the
// server: it runs in-process through modernc.org/sqlite and needs no
// DSN, which is the point — the regression has to be catchable on
// every CI run rather than on the ones with containers.

type axisRow struct {
	ID       int64  `drop:"id"`
	Title    string `drop:"title"`
	TenantID string `drop:"tenantId"`
}

func TestForeignTenantHandleCannotWriteARowToAnotherTenant(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	ours := sqlite.NewTable("axis_rows")
	sqlite.Add(ours, sqlite.BigInt("id").PrimaryKey().AutoIncrement())
	title := sqlite.Add(ours, sqlite.Text("title").NotNull())
	tenant := sqlite.Add(ours, sqlite.Text("tenantId").NotNull())
	sqlite.NewEntity[axisRow](ours).ScopeByTenant(tenant)
	exec(t, db, sqlite.CreateTable(ours))

	// The second table an application declares, exporting its own
	// handle for a column of the same name. In a codegen'd schema this
	// is one character away from the right one.
	other := sqlite.NewTable("other_axis_rows")
	sqlite.Add(other, sqlite.BigInt("id").PrimaryKey())
	foreign := sqlite.Add(other, sqlite.Text("tenantId").NotNull())

	acme := sqlite.WithTenant(ctx, "acme")
	_, err := db.Insert(ours).Values(title.Val("x"), foreign.Val("evil")).Exec(acme)
	if !errors.Is(err, sqlite.ErrTenantMismatch) {
		t.Fatalf("INSERT through another table's tenant handle = %v, want ErrTenantMismatch", err)
	}

	// Nothing reached the table. The assertion is on the server rather
	// than on the rendering, because the rendering was fine: it was
	// the server's choice of which duplicate column to keep that
	// decided who owned the row.
	var landed []axisRow
	if err := db.Select(title, tenant).From(ours).Unscoped().All(ctx, &landed); err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if len(landed) != 0 {
		t.Fatalf("a refused INSERT still wrote %d row(s): %+v", len(landed), landed)
	}

	// And the row the caller was entitled to write still writes,
	// through the same foreign handle, because naming the right tenant
	// through the wrong handle is not the defect — the statement it
	// renders is the one it always was.
	if _, err := db.Insert(ours).Values(title.Val("y"), foreign.Val("acme")).Exec(acme); err != nil {
		t.Fatalf("INSERT naming the ctx tenant: %v", err)
	}
	if err := db.Select(title, tenant).From(ours).All(acme, &landed); err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if len(landed) != 1 || landed[0].TenantID != "acme" || landed[0].Title != "y" {
		t.Fatalf("row landed as %+v, want one row {y acme}", landed)
	}
}
