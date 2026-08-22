package clickhouse_test

import (
	"testing"

	"github.com/bernardoforcillo/drops/clickhouse"
	"github.com/bernardoforcillo/drops/dropstest"
)

// Section 2 of the normative policy block says the axis stamp runs
// before the validators, so that a validator reads the row as it will
// be WRITTEN rather than as the caller happened to BUILD it.
//
// It did not here. Create and CreateMany ran the validators first, so
// every validator in this dialect saw the tenant field as the caller
// left it — zero on the ordinary path, since a struct decoded from a
// request body does not carry a tenant. A validator that checks the
// row's tenant therefore checked nothing, and one that requires the
// field to be set rejected every legitimate Create.
//
// pg has stamped first since the axis moved onto the table; this
// dialect is the only other one with validators at all, and it had the
// order the other way round. The rule is what the block says, and the
// test is here so the two orders cannot drift apart again.

type validatedRow struct {
	ID       uint64 `drop:"id"`
	TenantID string `drop:"tenantId"`
	Path     string `drop:"path"`
}

// validatedEntity records what each validator saw, in call order.
func validatedEntity(name string, seen *[]string) *clickhouse.Entity[validatedRow] {
	tbl := clickhouse.NewTable(name)
	id := clickhouse.Add(tbl, clickhouse.UInt64("id"))
	tenant := clickhouse.Add(tbl, clickhouse.String("tenantId"))
	clickhouse.Add(tbl, clickhouse.String("path"))
	tbl.Engine(clickhouse.MergeTree()).OrderBy(tenant, id)
	return clickhouse.NewEntity[validatedRow](tbl).
		ScopeByTenant(tenant).
		Validate(func(r *validatedRow) error {
			*seen = append(*seen, r.TenantID)
			return nil
		})
}

func TestCreateStampsBeforeTheValidatorsRun(t *testing.T) {
	var seen []string
	ent := validatedEntity("val_rows", &seen)
	drv := dropstest.New()
	db := clickhouse.New(drv)

	row := validatedRow{ID: 1, Path: "/"}
	if _, err := ent.Create(db, tenantCtx("acme"), &row); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(seen) != 1 || seen[0] != "acme" {
		t.Errorf("the validator saw tenant %q, want it to read the row as it will be written (%q)",
			seen, "acme")
	}
	st := drv.Last()
	checkSQL(t, st.SQL, `INSERT INTO "val_rows" ("id", "tenantId", "path") VALUES (?, ?, ?)`)
	checkArgs(t, st.Args, uint64(1), "acme", "/")
}

// The batch path is the ordinary write in this dialect, so it is asked
// the same question. Both rows are stamped before either is validated
// only in the sense that each row is stamped before its own validators
// run; what matters is that no validator ever sees an unstamped row.
func TestCreateManyStampsBeforeTheValidatorsRun(t *testing.T) {
	var seen []string
	ent := validatedEntity("val_many_rows", &seen)
	drv := dropstest.New()
	db := clickhouse.New(drv)

	rows := []validatedRow{{ID: 1, Path: "/a"}, {ID: 2, Path: "/b"}}
	if _, err := ent.CreateMany(db, tenantCtx("acme"), rows); err != nil {
		t.Fatalf("CreateMany: %v", err)
	}
	for i, got := range seen {
		if got != "acme" {
			t.Errorf("validator %d saw tenant %q, want %q", i, got, "acme")
		}
	}
	if len(seen) != 2 {
		t.Errorf("validators ran %d times, want 2", len(seen))
	}
	st := drv.Last()
	checkSQL(t, st.SQL, `INSERT INTO "val_many_rows" ("id", "tenantId", "path") VALUES (?, ?, ?), (?, ?, ?)`)
	checkArgs(t, st.Args, uint64(1), "acme", "/a", uint64(2), "acme", "/b")
}
