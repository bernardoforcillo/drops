package sqlite_test

import (
	"errors"
	"testing"

	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/sqlite"
)

// sameTenant refuses a string on one side and a non-string on the
// other before it looks at any conversion, and what that guard is FOR
// was described wrongly for the whole of this phase.
//
// It was said to be what stops a numeric tenant owning a text column's
// row: Go converts an integer to a string as a rune, so 65 becomes
// "A". It is not. No conversion goes back from a string to an integer,
// and the comparison is a round trip in both directions, so 65 and "A"
// are already refused before the guard is consulted — removing it
// leaves that pair refused.
//
// What the guard actually rules out is []byte and []rune. Those DO
// convert onto a string and back losing nothing, so without it
// []byte("acme") and "acme" are the same tenant, and a schema holding
// its tenant as bytes on one table and as text on another would have
// the two read as one tenant rather than as the type confusion it is.
//
// Nothing asserted any of that, in any dialect, which is how the wrong
// description survived eleven rounds. These tests ask both halves.

// textAxisTable declares the tenant axis as TEXT.
func textAxisTable(name string) (*sqlite.Table, *sqlite.Col[string], *sqlite.Col[string]) {
	t := sqlite.NewTable(name)
	tenant := sqlite.Add(t, sqlite.Text("tenantId").NotNull())
	title := sqlite.Add(t, sqlite.Text("title").NotNull())
	t.ContextFilter(sqlite.TenantFilter(tenant)).ScopeWritesByTenant(tenant)
	return t, tenant, title
}

// blobAxisTable declares the same axis as bytes, which is the pairing
// the guard exists for: []byte and string convert onto each other
// losing nothing.
func blobAxisTable(name string) (*sqlite.Table, *sqlite.Col[[]byte], *sqlite.Col[string]) {
	t := sqlite.NewTable(name)
	tenant := sqlite.Add(t, sqlite.Blob("tenantId").NotNull())
	title := sqlite.Add(t, sqlite.Text("title").NotNull())
	t.ContextFilter(sqlite.TenantFilter(tenant)).ScopeWritesByTenant(tenant)
	return t, tenant, title
}

// tenantName is the shape a caller's own tenant type takes. Its KIND
// is string, which is what the guard asks, so it is the same tenant as
// the string it is defined over.
type tenantName string

func TestBytesAndTextAreNotTheSameTenant(t *testing.T) {
	t.Run("bytes bound, text on ctx", func(t *testing.T) {
		tbl, tenant, title := blobAxisTable("guard_blob_rows")
		drv := dropstest.New()
		db := sqlite.New(drv)

		_, err := db.Insert(tbl).Values(title.Val("x"), tenant.Val([]byte("acme"))).
			Exec(typeCtx("acme"))
		if !errors.Is(err, sqlite.ErrTenantMismatch) {
			t.Errorf("Exec = %v, want ErrTenantMismatch", err)
		}
		if got := drv.Statements(); len(got) != 0 {
			t.Errorf("a refused INSERT still sent %d statement(s): %v", len(got), got)
		}
	})
	t.Run("text bound, bytes on ctx", func(t *testing.T) {
		tbl, tenant, title := textAxisTable("guard_text_rows")
		drv := dropstest.New()
		db := sqlite.New(drv)

		_, err := db.Insert(tbl).Values(title.Val("x"), tenant.Val("acme")).
			Exec(typeCtx([]byte("acme")))
		if !errors.Is(err, sqlite.ErrTenantMismatch) {
			t.Errorf("Exec = %v, want ErrTenantMismatch", err)
		}
		if got := drv.Statements(); len(got) != 0 {
			t.Errorf("a refused INSERT still sent %d statement(s): %v", len(got), got)
		}
	})
}

// The rune pairing the guard was said to be for. It is refused, and it
// is refused by the round trip: a string does not convert to an
// integer, so the pair never reaches the guard at all.
func TestARuneConversionIsRefusedByTheRoundTrip(t *testing.T) {
	tbl, tenant, title := textAxisTable("guard_rune_rows")
	drv := dropstest.New()
	db := sqlite.New(drv)

	_, err := db.Insert(tbl).Values(title.Val("x"), tenant.Val("A")).Exec(typeCtx(65))
	if !errors.Is(err, sqlite.ErrTenantMismatch) {
		t.Errorf("Exec = %v, want ErrTenantMismatch", err)
	}
	if got := drv.Statements(); len(got) != 0 {
		t.Errorf("a refused INSERT still sent %d statement(s): %v", len(got), got)
	}
}

// The guard asks about the KIND, not the type, so a caller's own named
// string type names the same tenant as the string a column binds. A
// guard written as a type comparison would refuse every schema that
// gives its tenant id a name.
func TestANamedStringTypeIsTheSameTenant(t *testing.T) {
	tbl, tenant, title := textAxisTable("guard_named_rows")
	drv := dropstest.New()
	db := sqlite.New(drv)

	if _, err := db.Insert(tbl).Values(title.Val("x"), tenant.Val("acme")).
		Exec(typeCtx(tenantName("acme"))); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	st := drv.Last()
	wantSQL := `INSERT INTO "guard_named_rows" ("title", "tenantId") VALUES (?, ?)`
	if st.SQL != wantSQL {
		t.Errorf("rendered SQL:\n got = %v\nwant = %v", st.SQL, wantSQL)
	}
	if !sameArgs(st.Args, []any{"x", "acme"}) {
		t.Errorf("args:\n got = %#v\nwant = %#v", st.Args, []any{"x", "acme"})
	}
}
