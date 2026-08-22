package sqlite_test

import (
	"errors"
	"testing"

	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/sqlite"
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

func nilAxisEntity(name string) *sqlite.Entity[nilAxisRow] {
	tbl := sqlite.NewTable(name)
	sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
	tenant := sqlite.Add(tbl, sqlite.Text("tenantId"))
	sqlite.Add(tbl, sqlite.Text("title"))
	return sqlite.NewEntity[nilAxisRow](tbl).ScopeByTenant(tenant)
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
			db := sqlite.New(drv)

			row := nilAxisRow{ID: 1, Title: "x"}
			err := ent.Create(db, typeCtx(tt.tenant), &row)
			if !errors.Is(err, sqlite.ErrTenantMissing) {
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
			posts, _, _ := scopedTable("nil_read_posts")
			drv := dropstest.New()
			db := sqlite.New(drv)

			var out []struct{}
			err := db.Select().From(posts).All(typeCtx(tt.tenant), &out)
			if !errors.Is(err, sqlite.ErrTenantMissing) {
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
	tbl := sqlite.NewTable("empty_tenant_rows")
	id := sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
	tenant := sqlite.Add(tbl, sqlite.Text("tenantId").NotNull())
	tbl.ContextFilter(sqlite.TenantFilter(tenant)).ScopeWritesByTenant(tenant)
	drv := dropstest.New()
	db := sqlite.New(drv)

	sql, args := renderCtx(t, db.Select().From(tbl).Where(sqlite.Eq(id, int64(3))), typeCtx(""))
	checkSQL(t, sql,
		`SELECT * FROM "empty_tenant_rows" WHERE ("empty_tenant_rows"."id" = ?) `+
			`AND ("empty_tenant_rows"."tenantId" = ?)`,
		args, []any{int64(3), ""})
}
