package sqlite_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/sqlite"
)

type entDriver struct {
	queries []string
	args    [][]any
	rows    *entRows
}

func (d *entDriver) Exec(_ context.Context, sql string, args ...any) (drops.Result, error) {
	d.queries = append(d.queries, sql)
	d.args = append(d.args, args)
	return entResult{}, nil
}
func (d *entDriver) Query(_ context.Context, sql string, args ...any) (drops.Rows, error) {
	d.queries = append(d.queries, sql)
	d.args = append(d.args, args)
	if d.rows != nil {
		return d.rows, nil
	}
	return &entRows{}, nil
}
func (d *entDriver) Begin(_ context.Context) (drops.Tx, error) { return &entTx{d}, nil }

type entTx struct{ *entDriver }

func (*entTx) Commit(_ context.Context) error   { return nil }
func (*entTx) Rollback(_ context.Context) error { return nil }

type entResult struct{}

func (entResult) RowsAffected() (int64, error) { return 1, nil }

type entRows struct {
	cols []string
	data [][]any
	pos  int
}

func (r *entRows) Next() bool {
	if r.pos >= len(r.data) {
		return false
	}
	r.pos++
	return true
}
func (r *entRows) Scan(dest ...any) error {
	row := r.data[r.pos-1]
	for i, d := range dest {
		switch p := d.(type) {
		case *int64:
			*p = row[i].(int64)
		case *string:
			*p = row[i].(string)
		}
	}
	return nil
}
func (r *entRows) Columns() ([]string, error) { return r.cols, nil }
func (r *entRows) Close() error               { return nil }
func (r *entRows) Err() error                 { return nil }

type entUser struct {
	ID   int64  `drop:"id"`
	Name string `drop:"name"`
}

func entSchema() *sqlite.Table {
	t := sqlite.NewTable("users")
	sqlite.Add(t, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(t, sqlite.Text("name").NotNull())
	return t
}

func TestEntityCreateAndGet(t *testing.T) {
	users := entSchema()
	ent := sqlite.NewEntity[entUser](users)

	drv := &entDriver{}
	db := sqlite.New(drv)

	if err := ent.Create(db, context.Background(), &entUser{ID: 1, Name: "alice"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(drv.queries) != 1 || !strings.HasPrefix(drv.queries[0], `INSERT INTO "users"`) {
		t.Errorf("expected INSERT, got %v", drv.queries)
	}

	// Get: return one canned row.
	drv.rows = &entRows{cols: []string{"id", "name"}, data: [][]any{{int64(7), "bob"}}}
	got, err := ent.Get(db, context.Background(), 7)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != 7 || got.Name != "bob" {
		t.Errorf("Get returned %+v", got)
	}
	last := drv.queries[len(drv.queries)-1]
	if !strings.Contains(last, `WHERE ("users"."id" = ?)`) {
		t.Errorf("Get query missing PK predicate: %s", last)
	}
}

func TestEntityUpdateAndDelete(t *testing.T) {
	users := entSchema()
	ent := sqlite.NewEntity[entUser](users)
	drv := &entDriver{}
	db := sqlite.New(drv)

	if err := ent.Update(db, context.Background(), &entUser{ID: 3, Name: "carol"}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	upd := drv.queries[len(drv.queries)-1]
	// The PK column must not be in the SET list, only in WHERE.
	if !strings.HasPrefix(upd, `UPDATE "users" SET "name" = ?`) || !strings.Contains(upd, `WHERE ("users"."id" = ?)`) {
		t.Errorf("update sql: %s", upd)
	}

	if _, err := ent.Delete(db, context.Background(), 3); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	del := drv.queries[len(drv.queries)-1]
	if del != `DELETE FROM "users" WHERE ("users"."id" = ?)` {
		t.Errorf("delete sql: %s", del)
	}
}

func TestEntityQuery(t *testing.T) {
	users := entSchema()
	ent := sqlite.NewEntity[entUser](users)
	drv := &entDriver{rows: &entRows{
		cols: []string{"id", "name"},
		data: [][]any{{int64(1), "a"}, {int64(2), "b"}},
	}}
	db := sqlite.New(drv)

	id := users.Col("id")
	out, err := ent.Query(db).Where(sqlite.And()).OrderBy(id).Limit(10).All(context.Background())
	if err != nil {
		t.Fatalf("Query.All: %v", err)
	}
	if len(out) != 2 || out[1].Name != "b" {
		t.Errorf("query result: %+v", out)
	}
}

// ----------------------------------------------------------------------
// The tenant column is an axis, never an assignment
// ----------------------------------------------------------------------

// tenant.go opens by promising that a stray background job cannot
// silently write into the wrong tenant. Update was the write that could:
// it puts every non-key column in the SET list, the tenant column is one
// of them, and it took its value from the struct. So a handler that
// built its row from a decoded request body — a shape where the tenant
// field is whatever the payload did or did not carry — either wrote a
// zero over a row it was otherwise allowed to touch, orphaning it to a
// tenant no predicate matches and therefore to nobody, or wrote 99 and
// handed the row to tenant 99. Both were reported as a successful
// update.
//
// The rule the four dialects hold in common: the tenant column is an
// axis, never an assignment. Create stamps it, Update stamps it, both
// refuse a mismatch, and neither ever takes the value from the struct as
// an instruction.
//
// Every assertion here is on RENDERED SQL and ARGS, because a round trip
// cannot see any of this: a fixture with one tenant in it stores the
// stamped value and the struct's value at the same column, reads the row
// back through a predicate that matches either, and passes while doing
// all of it wrong.

type axisRow struct {
	ID       int64  `drop:"id"`
	TenantID int64  `drop:"tenantId"`
	Title    string `drop:"title"`
}

// axisEntity declares the axis through ScopeByTenant, which is the
// spelling the readme shows: it registers the read-side predicate on the
// table and names the column a write stamps, in one call.
func axisEntity() *sqlite.Entity[axisRow] {
	tbl := sqlite.NewTable("axis_rows")
	sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
	tenant := sqlite.Add(tbl, sqlite.BigInt("tenantId").NotNull())
	sqlite.Add(tbl, sqlite.Text("title").NotNull())
	return sqlite.NewEntity[axisRow](tbl).ScopeByTenant(tenant)
}

// A struct whose tenant field nobody filled in is the ordinary shape —
// the one a form or a decoded body produces. The SET list must carry the
// ctx tenant for it, and the struct must come back stamped so the caller
// holds the row as it was written.
func TestUpdateStampsTheTenantItWrites(t *testing.T) {
	ent := axisEntity()
	drv := dropstest.New()
	db := sqlite.New(drv)

	row := axisRow{ID: 7, Title: "x"}
	if err := ent.Update(db, scopeCtx(), &row); err != nil {
		t.Fatalf("Update: %v", err)
	}
	st := drv.Last()
	want := `UPDATE "axis_rows" SET "tenantId" = ?, "title" = ? ` +
		`WHERE ("axis_rows"."id" = ?) AND ("axis_rows"."tenantId" = ?)`
	if st.SQL != want {
		t.Errorf("rendered SQL:\n got = %s\nwant = %s", st.SQL, want)
	}
	wantArgs := []any{scopeTenant, "x", int64(7), scopeTenant}
	if !reflect.DeepEqual(st.Args, wantArgs) {
		t.Errorf("bound arguments: got = %v, want %v", st.Args, wantArgs)
	}
	if row.TenantID != scopeTenant {
		t.Errorf("Update left the row's tenant at %d, want %d", row.TenantID, scopeTenant)
	}
}

// The struct is not an instruction. A row carrying somebody else's
// tenant is refused with ErrTenantMismatch and NO statement is sent —
// not an UPDATE that happens to match nothing, which would report zero
// rows affected and leave the caller to guess why.
func TestUpdateRefusesToHandTheRowToAnotherTenant(t *testing.T) {
	ent := axisEntity()
	drv := dropstest.New()
	db := sqlite.New(drv)

	row := axisRow{ID: 7, TenantID: 99, Title: "x"}
	if err := ent.Update(db, scopeCtx(), &row); !errors.Is(err, sqlite.ErrTenantMismatch) {
		t.Errorf("Update = %v, want ErrTenantMismatch", err)
	}
	if got := drv.Statements(); len(got) != 0 {
		t.Errorf("a refused Update still sent %d statement(s): %v", len(got), got)
	}
}

// A bare ctx cannot say which tenant the write is for, so the write does
// not happen. The read-side filter already refused a bare ctx; the stamp
// refuses it one step earlier, before any statement is built, which is
// what makes the two halves of the axis fail closed in the same way.
func TestUpdateRefusesABareCtx(t *testing.T) {
	ent := axisEntity()
	drv := dropstest.New()
	db := sqlite.New(drv)

	row := axisRow{ID: 7, Title: "x"}
	if err := ent.Update(db, context.Background(), &row); !errors.Is(err, sqlite.ErrTenantMissing) {
		t.Errorf("Update = %v, want ErrTenantMissing", err)
	}
	if got := drv.Statements(); len(got) != 0 {
		t.Errorf("an Update with no tenant on ctx still sent %d statement(s): %v", len(got), got)
	}
	if row.TenantID != 0 {
		t.Errorf("a refused Update stamped the row anyway: %d", row.TenantID)
	}
}
