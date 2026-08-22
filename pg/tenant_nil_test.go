package pg_test

import (
	"errors"
	"testing"

	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/pg"
)

// A ctx tenant that is nil is no tenant.
//
// WithTenant takes an `any`, and a nil of some type — a (*string)(nil)
// read out of a request struct, a nil map from a header lookup — sits
// inside an interface that is not itself nil. TenantFrom answered "yes,
// there is a tenant" for it, so Create stamped the nil onto the row and
// wrote tenantId = NULL: a row belonging to nobody, invisible to every
// later request including the one that wrote it, reported as a success.
//
// Reads on the same ctx were already fail-closed — = NULL matches
// nothing — so the pair orphans rows rather than leaking them. It is
// still the "hand it to no tenant at all" outcome the write side
// exists to rule out, and a caller who never sees the row again has no
// way to learn why. So it is refused, on the read side and the write
// side alike, with ErrTenantMissing: a ctx that cannot name a tenant is
// a ctx with no tenant.

// nilAxisRow's tenant column is nullable and its field is a pointer,
// which is what makes the defect reachable: a nil ctx tenant is
// assignable to the field, so the stamp succeeds.
type nilAxisRow struct {
	ID       int64   `drop:"id"`
	TenantID *string `drop:"tenantId"`
	Title    string  `drop:"title"`
}

func nilAxisEntity(name string) *pg.Entity[nilAxisRow] {
	tbl := pg.NewTable(name)
	pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	tenant := pg.Add(tbl, pg.Text("tenantId"))
	pg.Add(tbl, pg.Text("title"))
	return pg.NewEntity[nilAxisRow](tbl).ScopeByTenant(tenant)
}

// nilTenants are the shapes a nil arrives in. Each is a non-nil
// interface holding nothing, which is the whole of why v != nil was
// the wrong question.
func nilTenants() []struct {
	name   string
	tenant any
} {
	return []struct {
		name   string
		tenant any
	}{
		{"a typed nil pointer", (*string)(nil)},
		{"a nil map", map[string]string(nil)},
		{"a nil slice", []byte(nil)},
		{"an untyped nil", nil},
	}
}

func TestCreateRefusesANilCtxTenant(t *testing.T) {
	for _, tt := range nilTenants() {
		t.Run(tt.name, func(t *testing.T) {
			ent := nilAxisEntity("nil_create_rows")
			drv := dropstest.New()
			db := pg.New(drv)

			row := nilAxisRow{ID: 1, Title: "x"}
			err := ent.Create(db, tenantCtx(tt.tenant), &row)
			if !errors.Is(err, pg.ErrTenantMissing) {
				t.Errorf("Create = %v, want ErrTenantMissing", err)
			}
			if got := drv.Statements(); len(got) != 0 {
				t.Errorf("a refused Create still sent %d statement(s): %v", len(got), got)
			}
			if row.TenantID != nil {
				t.Errorf("the refused Create still stamped the row: %v", *row.TenantID)
			}
		})
	}
}

// The read half refuses too, rather than binding NULL into a predicate
// that matches nothing and answering "no such row".
func TestSelectRefusesANilCtxTenant(t *testing.T) {
	for _, tt := range nilTenants() {
		t.Run(tt.name, func(t *testing.T) {
			tbl := pg.NewTable("nil_read_posts")
			pg.Add(tbl, pg.BigInt("id").PrimaryKey())
			tenant := pg.Add(tbl, pg.Text("tenantId").NotNull())
			tbl.ContextFilter(pg.TenantFilter(tenant))
			drv := dropstest.New()
			db := pg.New(drv)

			var out []struct{}
			err := db.Select().From(tbl).All(tenantCtx(tt.tenant), &out)
			if !errors.Is(err, pg.ErrTenantMissing) {
				t.Errorf("All = %v, want ErrTenantMissing", err)
			}
			if got := drv.Statements(); len(got) != 0 {
				t.Errorf("a refused SELECT still sent %d statement(s): %v", len(got), got)
			}
		})
	}
}

// The line is nil, not zero. An empty string is a value the schema can
// store, and it addresses the same rows on the way back out, so it is
// a tenant like any other — self-consistent where a NULL is not.
func TestAnEmptyStringIsATenant(t *testing.T) {
	tbl := pg.NewTable("empty_tenant_rows")
	id := pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	tenant := pg.Add(tbl, pg.Text("tenantId").NotNull())
	tbl.ContextFilter(pg.TenantFilter(tenant)).ScopeWritesByTenant(tenant)
	db := pg.New(dropstest.New())

	sql, args, err := db.Select().From(tbl).Where(pg.Eq(id, int64(3))).ToSQLCtx(tenantCtx(""))
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	wantSQL := `SELECT * FROM "empty_tenant_rows" ` +
		`WHERE ("empty_tenant_rows"."id" = $1) AND ("empty_tenant_rows"."tenantId" = $2)`
	if sql != wantSQL {
		t.Errorf("rendered SQL:\n got = %v\nwant = %v", sql, wantSQL)
	}
	if !sameArgs(args, []any{int64(3), ""}) {
		t.Errorf("args:\n got = %#v\nwant = %#v", args, []any{int64(3), ""})
	}
}
