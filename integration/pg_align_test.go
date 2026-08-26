package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/pg"
)

// The one assertion in this phase that exercises its headline failure
// against a real server. Everything else about the alignment is pinned on
// rendered SQL, which is what let the defect survive three rounds: the
// statement looked right and the row did not.
//
// The shape section 4 of the policy block opens with: a NULLABLE tenant
// axis, a batch whose second row names the axis through ANOTHER table
// object's handle, and Unscoped so nothing stamps over the gap. Before
// alignRow was reindexed by rendered name the binding was DISCARDED,
// the column fell to DEFAULT, and on a nullable axis with no DEFAULT
// clause PostgreSQL stored NULL — a row belonging to nobody, reported
// as written. The check has to be the stored data, not the SQL.
func TestPGNullableAxisUnderUnscopedStoresTheBoundTenant(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	tbl := pg.NewTable(integration.UniqueName(t, "vfy_align"))
	dropPG(t, db, tbl)

	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	tenant := pg.Add(tbl, pg.BigInt("tenantId")) // NULLABLE on purpose
	name := pg.Add(tbl, pg.Text("name").NotNull())
	tbl.ContextFilter(pg.TenantFilter(tenant)).ScopeWritesByTenant(tenant)
	execPG(t, db, pg.CreateTable(tbl))

	// A handle for a "tenantId" column of a DIFFERENT table object:
	// same rendered name, different Column.key.
	other := pg.NewTable(integration.UniqueName(t, "vfy_other"))
	pg.Add(other, pg.BigSerial("id").PrimaryKey())
	foreign := pg.Add(other, pg.BigInt("tenantId"))

	if _, err := db.Insert(tbl).
		Row(tenant.Val(3), name.Val("a")).
		Row(foreign.Val(3), name.Val("b")).
		Unscoped().
		Exec(ctx); err != nil {
		t.Fatalf("insert: %v", err)
	}

	rows, err := db.Query(ctx, `SELECT "name", "tenantId" FROM `+pgQuote(tbl.Name())+` ORDER BY "id"`)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	defer rows.Close()
	type got struct {
		name   string
		tenant sql.NullInt64
	}
	var out []got
	for rows.Next() {
		var g got
		if err := rows.Scan(&g.name, &g.tenant); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("stored %d rows, want 2: %+v", len(out), out)
	}
	for _, g := range out {
		if !g.tenant.Valid {
			t.Errorf("row %q stored a NULL tenant: it belongs to nobody, which is the failure "+
				"section 1 names and section 4 reaches", g.name)
			continue
		}
		if g.tenant.Int64 != 3 {
			t.Errorf("row %q stored tenant %d, want 3", g.name, g.tenant.Int64)
		}
	}
	_ = id

	// And the row really is reachable under tenant 3 afterwards — a row
	// that stored the right value but that the tenant predicate cannot
	// see would be the same outage by another route.
	var seen []struct {
		Name string `drop:"name"`
	}
	if err := db.Select(name).From(tbl).All(pg.WithTenant(ctx, int64(3)), &seen); err != nil {
		t.Fatalf("scoped read back: %v", err)
	}
	if len(seen) != 2 {
		t.Errorf("tenant 3 sees %d of its 2 rows: %+v", len(seen), seen)
	}
}

// The scoped half of the same shape: a later row assigning ANOTHER
// tenant through a foreign handle must be refused, not silently
// rewritten to the ctx tenant.
func TestVerifyPGScopedForeignHandleAssigningAnotherTenantIsRefused(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	tbl := pg.NewTable(integration.UniqueName(t, "vfy_scoped"))
	dropPG(t, db, tbl)

	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	tenant := pg.Add(tbl, pg.BigInt("tenantId"))
	name := pg.Add(tbl, pg.Text("name").NotNull())
	tbl.ContextFilter(pg.TenantFilter(tenant)).ScopeWritesByTenant(tenant)
	execPG(t, db, pg.CreateTable(tbl))

	other := pg.NewTable(integration.UniqueName(t, "vfy_other2"))
	pg.Add(other, pg.BigSerial("id").PrimaryKey())
	foreign := pg.Add(other, pg.BigInt("tenantId"))

	_, err := db.Insert(tbl).
		Row(tenant.Val(3), name.Val("a")).
		Row(foreign.Val(9), name.Val("b")).
		Exec(pg.WithTenant(ctx, int64(3)))
	if !errors.Is(err, pg.ErrTenantMismatch) {
		t.Fatalf("Exec = %v, want ErrTenantMismatch", err)
	}

	// Nothing was written: a refused batch is refused whole.
	cnt, err := db.Query(ctx, `SELECT count(*) FROM `+pgQuote(tbl.Name()))
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	defer cnt.Close()
	var n int
	if cnt.Next() {
		if err := cnt.Scan(&n); err != nil {
			t.Fatalf("scan count: %v", err)
		}
	}
	if n != 0 {
		t.Errorf("a refused INSERT wrote %d rows", n)
	}
}

func pgQuote(s string) string { return `"` + s + `"` }
