package pg_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// flagRow exercises the four ways a zero value can reach an INSERT:
// a bool on a column marked AlwaysInsert, a bool on a plain DEFAULT
// column (where the skip rule still applies), a pointer field, and a
// string the caller may or may not name in CreateCols.
type flagRow struct {
	ID     int64  `drop:"id"`
	Name   string `drop:"name"`
	Active bool   `drop:"active"`
	Admin  bool   `drop:"admin"`
	Notify *bool  `drop:"notify"`
	Tier   string `drop:"tier"`
}

type flagCols struct {
	name *pg.Col[string]
	tier *pg.Col[string]
}

// flagSchema declares "active" with AlwaysInsert and "admin" without,
// so one table shows both sides of the rule.
func flagSchema(t *testing.T) (*pg.Entity[flagRow], flagCols) {
	t.Helper()
	tbl := pg.NewTable("rows")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	name := pg.Add(tbl, pg.Text("name").NotNull())
	pg.Add(tbl, pg.Boolean("active").NotNull().Default("true").AlwaysInsert())
	pg.Add(tbl, pg.Boolean("admin").NotNull().Default("true"))
	pg.Add(tbl, pg.Boolean("notify").Default("true"))
	tier := pg.Add(tbl, pg.Text("tier").Default("'free'"))
	return pg.NewEntity[flagRow](tbl), flagCols{name: name, tier: tier}
}

// flagDriver answers the INSERT's RETURNING with a full row. notify
// comes back as a *bool because that is the field's type — the
// reflection scanner assigns what the driver hands it.
func flagDriver() *fakeDriver {
	notify := true
	return &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"id", "name", "active", "admin", "notify", "tier"},
			data: [][]any{{int64(1), "a", true, true, &notify, "free"}},
		}, nil
	}}
}

// insertCols returns the column names an INSERT writes to — the
// section before VALUES, which is the only place the skip rule is
// visible from outside.
func insertCols(sql string) []string {
	head, _, _ := strings.Cut(sql, ") VALUES")
	_, cols, _ := strings.Cut(head, "(")
	parts := strings.Split(cols, ", ")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.Trim(strings.TrimSpace(p), `"`))
	}
	return out
}

func hasCol(cols []string, want string) bool {
	for _, c := range cols {
		if c == want {
			return true
		}
	}
	return false
}

// TestCreateBindsZeroValueOnAlwaysInsertColumn is the regression for
// the complaint: Active = false on a column declared DEFAULT true was
// dropped from the INSERT, and the row came back true. The marker is
// what says the zero value is a value.
func TestCreateBindsZeroValueOnAlwaysInsertColumn(t *testing.T) {
	ent, _ := flagSchema(t)
	fd := flagDriver()
	r := flagRow{Name: "a"} // Active, Admin, Tier all zero, Notify nil
	if err := ent.Create(pg.New(fd), context.Background(), &r); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cols := insertCols(fd.queries[0])
	if !hasCol(cols, "active") {
		t.Errorf("AlwaysInsert column missing from INSERT: got %v, want it to contain %v", cols, "active")
	}
	var bound bool
	for _, a := range fd.args[0] {
		if b, ok := a.(bool); ok && !b {
			bound = true
		}
	}
	if !bound {
		t.Errorf("false was not bound as an argument: got %v, want it to contain %v", fd.args[0], false)
	}
	// The columns without the marker keep the old behaviour: a zero
	// value on a DEFAULT column is left to the server.
	for _, c := range []string{"admin", "notify", "tier"} {
		if hasCol(cols, c) {
			t.Errorf("unmarked DEFAULT column %q should be omitted at the zero value: got %v", c, cols)
		}
	}
}

// TestCreatePointerFieldSeparatesUnsetFromZero documents the second
// escape hatch, which needs no marker and no API: a nil pointer is
// the zero value and is skipped, a non-nil one is not — so a *bool
// pointing at false is written. It is the convention encoding/json
// established, and it was an undocumented accident here until now.
func TestCreatePointerFieldSeparatesUnsetFromZero(t *testing.T) {
	no, yes := false, true
	cases := []struct {
		name   string
		notify *bool
		want   bool
	}{
		{"nil is unset", nil, false},
		{"pointer to false is a value", &no, true},
		{"pointer to true is a value", &yes, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ent, _ := flagSchema(t)
			fd := flagDriver()
			r := flagRow{Name: "a", Notify: tc.notify}
			if err := ent.Create(pg.New(fd), context.Background(), &r); err != nil {
				t.Fatalf("Create: %v", err)
			}
			got := hasCol(insertCols(fd.queries[0]), "notify")
			if got != tc.want {
				t.Errorf("notify bound = %v, want %v (cols %v)", got, tc.want, insertCols(fd.queries[0]))
			}
		})
	}
}

// TestCreateColsBindsExactlyTheNamedColumns covers the per-call form:
// the named columns are written whatever they hold, and nothing else
// is written at all.
func TestCreateColsBindsExactlyTheNamedColumns(t *testing.T) {
	ent, c := flagSchema(t)
	fd := flagDriver()
	r := flagRow{Name: "a"} // Tier is "" and its column has a DEFAULT
	if err := ent.CreateCols(pg.New(fd), context.Background(), &r, c.name, c.tier); err != nil {
		t.Fatalf("CreateCols: %v", err)
	}
	got := insertCols(fd.queries[0])
	want := []string{"name", "tier"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("INSERT columns = %v, want %v", got, want)
	}
	if r.ID != 1 {
		t.Errorf("RETURNING did not refresh the row: got %v, want %v", r.ID, 1)
	}
}

// TestCreateColsRejectsColumnsItCannotBind checks the errors. Each
// one names a column the caller asked for and this row cannot supply,
// and writing the remaining columns instead would store a row nobody
// asked for.
func TestCreateColsRejectsColumnsItCannotBind(t *testing.T) {
	other := pg.NewTable("others")
	foreign := pg.Add(other, pg.Text("name"))

	unmappedTbl := pg.NewTable("rows")
	pg.Add(unmappedTbl, pg.BigSerial("id").PrimaryKey())
	pg.Add(unmappedTbl, pg.Text("name").NotNull())
	pg.Add(unmappedTbl, pg.Boolean("active").NotNull())
	pg.Add(unmappedTbl, pg.Boolean("admin").NotNull())
	pg.Add(unmappedTbl, pg.Boolean("notify"))
	pg.Add(unmappedTbl, pg.Text("tier"))
	tsv := pg.Add(unmappedTbl, pg.Text("search"))
	unmapped := pg.NewEntity[flagRow](unmappedTbl, pg.AllowUnmappedColumns("search"))

	ent, c := flagSchema(t)
	cases := []struct {
		name string
		run  func(*pg.DB) error
		want string
	}{
		{"no columns", func(db *pg.DB) error {
			return ent.CreateCols(db, context.Background(), &flagRow{})
		}, "requires at least one column"},
		{"column of another table", func(db *pg.DB) error {
			return ent.CreateCols(db, context.Background(), &flagRow{}, c.name, foreign)
		}, `belongs to table "others"`},
		{"column with no struct field", func(db *pg.DB) error {
			return unmapped.CreateCols(db, context.Background(), &flagRow{}, tsv)
		}, `no struct field bound to column "search"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fd := flagDriver()
			err := tc.run(pg.New(fd))
			if err == nil {
				t.Fatalf("CreateCols must fail, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
			if len(fd.queries) != 0 {
				t.Errorf("no statement should be issued, got %v", fd.queries)
			}
		})
	}
}

// TestCreateColsRequiresTheTenantColumn: a scoped entity stamps the
// tenant onto the row, and a column list that leaves the tenant
// column out would write the row anyway — outside every tenant, so
// visible to none of them. Naming the column is the only way to say
// which tenant this write belongs to.
func TestCreateColsRequiresTheTenantColumn(t *testing.T) {
	ent := tenantSchema(t)
	ctx := pg.WithTenant(context.Background(), int64(42))

	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{cols: []string{"id", "tenantId", "name"}, data: [][]any{{int64(1), int64(42), "Alice"}}}, nil
	}}
	db := pg.New(fd)
	u := tenantUser{Name: "Alice"}
	err := ent.CreateCols(db, ctx, &u, ent.Table().Col("name"))
	if err == nil || !strings.Contains(err.Error(), `tenant column "tenantId" is missing`) {
		t.Fatalf("error = %v, want it to name the missing tenant column", err)
	}
	if len(fd.queries) != 0 {
		t.Errorf("no statement should be issued, got %v", fd.queries)
	}

	if err := ent.CreateCols(db, ctx, &u, ent.Table().Col("name"), ent.Table().Col("tenantId")); err != nil {
		t.Fatalf("CreateCols with the tenant column: %v", err)
	}
	cols := insertCols(fd.queries[0])
	if !hasCol(cols, "tenantId") {
		t.Errorf("INSERT columns = %v, want them to contain %v", cols, "tenantId")
	}
}

// TestAutoTableCarriesAlwaysInsert: a table derived from struct tags
// is the only declaration its columns get, so the marker has to be
// reachable from the tag — otherwise `default=true` on a bool in an
// AutoTable model is a value that can never be set to false.
func TestAutoTableCarriesAlwaysInsert(t *testing.T) {
	type autoFlagRow struct {
		ID     int64 `drop:"id,primaryKey,autoIncrement"`
		Active bool  `drop:"active,notNull,default=true,alwaysInsert"`
		Admin  bool  `drop:"admin,notNull,default=true"`
	}
	tbl := pg.AutoTable[autoFlagRow]("rows")
	if !tbl.Col("active").IsAlwaysInsert() {
		t.Errorf("active IsAlwaysInsert = %v, want %v", false, true)
	}
	if tbl.Col("admin").IsAlwaysInsert() {
		t.Errorf("admin IsAlwaysInsert = %v, want %v", true, false)
	}

	ent := pg.NewEntity[autoFlagRow](tbl)
	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{cols: []string{"id", "active", "admin"}, data: [][]any{{int64(1), false, true}}}, nil
	}}
	r := autoFlagRow{}
	if err := ent.Create(pg.New(fd), context.Background(), &r); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cols := insertCols(fd.queries[0])
	if !hasCol(cols, "active") || hasCol(cols, "admin") {
		t.Errorf("INSERT columns = %v, want active bound and admin omitted", cols)
	}
}
