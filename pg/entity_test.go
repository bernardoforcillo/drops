package pg_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/pg"
)

// User is the entity fixture used across these tests.
type entUser struct {
	ID    int64  `drop:"id"`
	Name  string `drop:"name"`
	Email string `drop:"email"`
}

func entUsersSchema() (*pg.Table, *pg.Entity[entUser]) {
	tbl := pg.NewTable("users")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	pg.Add(tbl, pg.Text("name").NotNull())
	pg.Add(tbl, pg.Text("email").NotNull().Unique())
	return tbl, pg.NewEntity[entUser](tbl)
}

// TestNewEntityPanicsWithoutPK verifies that a table missing PRIMARY KEY
// is rejected at declaration time.
func TestNewEntityPanicsWithoutPK(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on table without PRIMARY KEY")
		}
	}()
	tbl := pg.NewTable("no_pk")
	pg.Add(tbl, pg.Text("name").NotNull())
	pg.NewEntity[entUser](tbl)
}

// TestNewEntityPanicsWithoutPKField verifies that a struct missing the
// PK field is rejected.
func TestNewEntityPanicsWithoutPKField(t *testing.T) {
	type noIDUser struct {
		Name string `drop:"name"`
	}
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic when struct has no field bound to the PK column")
		}
	}()
	tbl := pg.NewTable("users")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	pg.Add(tbl, pg.Text("name").NotNull())
	pg.NewEntity[noIDUser](tbl)
}

// TestEntityCreateInsertsAndScans verifies that Create binds fields,
// emits an INSERT with RETURNING all columns, and refreshes the struct
// from the returned row.
func TestEntityCreateInsertsAndScans(t *testing.T) {
	_, ent := entUsersSchema()

	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"id", "name", "email"},
			data: [][]any{{int64(7), "Alice", "alice@x"}},
		}, nil
	}}
	db := pg.New(fd)
	u := entUser{Name: "Alice", Email: "alice@x"}
	if err := ent.Create(db, context.Background(), &u); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if u.ID != 7 {
		t.Errorf("RETURNING did not refresh ID: %+v", u)
	}
	sql := fd.queries[0]
	if !strings.HasPrefix(sql, "INSERT INTO") {
		t.Errorf("expected INSERT, got: %s", sql)
	}
	// PK is zero → omitted; non-PK fields included.
	if strings.Contains(sql, `"id"`) && !strings.Contains(sql, "RETURNING") {
		// "id" should appear only inside RETURNING, not the column list.
		t.Errorf("PK should be omitted from INSERT cols: %s", sql)
	}
	if !strings.Contains(sql, `"name"`) || !strings.Contains(sql, `"email"`) {
		t.Errorf("non-PK columns must be in INSERT: %s", sql)
	}
	if !strings.Contains(sql, "RETURNING") {
		t.Errorf("INSERT must use RETURNING: %s", sql)
	}
}

// TestEntityCreateWithExplicitPK verifies that a non-zero PK is
// included in the INSERT.
func TestEntityCreateWithExplicitPK(t *testing.T) {
	_, ent := entUsersSchema()

	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"id", "name", "email"},
			data: [][]any{{int64(99), "Bob", "b@x"}},
		}, nil
	}}
	db := pg.New(fd)
	u := entUser{ID: 99, Name: "Bob", Email: "b@x"}
	if err := ent.Create(db, context.Background(), &u); err != nil {
		t.Fatalf("Create: %v", err)
	}
	sql := fd.queries[0]
	// Find the column list section: between "(" and ") VALUES"
	header, _, _ := strings.Cut(sql, " VALUES")
	if !strings.Contains(header, `"id"`) {
		t.Errorf("explicit PK should be in INSERT cols: %s", sql)
	}
}

// TestEntityGet verifies SELECT ... WHERE pk = id.
func TestEntityGet(t *testing.T) {
	_, ent := entUsersSchema()

	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"id", "name", "email"},
			data: [][]any{{int64(42), "Carol", "c@x"}},
		}, nil
	}}
	db := pg.New(fd)
	u, err := ent.Get(db, context.Background(), int64(42))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if u.ID != 42 || u.Name != "Carol" {
		t.Errorf("unexpected user: %+v", u)
	}
	sql := fd.queries[0]
	if !strings.Contains(sql, `WHERE ("users"."id" = $1)`) {
		t.Errorf("Get must filter by PK: %s", sql)
	}
	if fd.args[0][0] != int64(42) {
		t.Errorf("Get must bind id param: %v", fd.args[0])
	}
}

// TestEntityUpdate verifies UPDATE ... SET ... WHERE pk = $.
func TestEntityUpdate(t *testing.T) {
	_, ent := entUsersSchema()

	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"id", "name", "email"},
			data: [][]any{{int64(42), "Carol-new", "c-new@x"}},
		}, nil
	}}
	db := pg.New(fd)
	u := entUser{ID: 42, Name: "Carol-new", Email: "c-new@x"}
	if err := ent.Update(db, context.Background(), &u); err != nil {
		t.Fatalf("Update: %v", err)
	}
	sql := fd.queries[0]
	if !strings.HasPrefix(sql, "UPDATE ") {
		t.Errorf("expected UPDATE: %s", sql)
	}
	if !strings.Contains(sql, `WHERE ("users"."id" = $`) {
		t.Errorf("UPDATE must filter by PK: %s", sql)
	}
	setList, _, _ := strings.Cut(sql, " WHERE")
	if strings.Contains(setList, `"id" =`) {
		t.Errorf("PK must not be in SET list: %s", sql)
	}
}

func TestEntityUpdateZeroPKReturnsError(t *testing.T) {
	_, ent := entUsersSchema()
	db := pg.New(&fakeDriver{})
	u := entUser{Name: "Eve"}
	err := ent.Update(db, context.Background(), &u)
	if err == nil {
		t.Fatal("expected error when updating with zero PK")
	}
	if err != pg.ErrPKNotSet {
		t.Errorf("expected ErrPKNotSet, got: %v", err)
	}
}

// TestEntitySaveBranchesOnPK verifies that Save Inserts when PK is
// zero and Updates otherwise.
func TestEntitySaveBranchesOnPK(t *testing.T) {
	_, ent := entUsersSchema()

	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"id", "name", "email"},
			data: [][]any{{int64(1), "X", "x@x"}},
		}, nil
	}}
	db := pg.New(fd)

	// Zero PK → INSERT.
	u := entUser{Name: "X", Email: "x@x"}
	if err := ent.Save(db, context.Background(), &u); err != nil {
		t.Fatalf("Save(insert): %v", err)
	}
	if !strings.HasPrefix(fd.queries[0], "INSERT ") {
		t.Errorf("Save with zero PK should INSERT: %s", fd.queries[0])
	}

	// Non-zero PK → UPDATE.
	u2 := entUser{ID: 1, Name: "X2", Email: "x@x"}
	if err := ent.Save(db, context.Background(), &u2); err != nil {
		t.Fatalf("Save(update): %v", err)
	}
	if !strings.HasPrefix(fd.queries[1], "UPDATE ") {
		t.Errorf("Save with non-zero PK should UPDATE: %s", fd.queries[1])
	}
}

// TestEntityDelete verifies DELETE FROM ... WHERE pk = $.
func TestEntityDelete(t *testing.T) {
	_, ent := entUsersSchema()

	fd := &fakeDriver{}
	db := pg.New(fd)
	if _, err := ent.Delete(db, context.Background(), int64(42)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	sql := fd.queries[0]
	if !strings.HasPrefix(sql, "DELETE FROM") {
		t.Errorf("expected DELETE: %s", sql)
	}
	if !strings.Contains(sql, `WHERE ("users"."id" = $1)`) {
		t.Errorf("Delete must filter by PK: %s", sql)
	}
}

// TestEntityComposesWithHooks verifies that the Phase-1 hooks fire
// when CRUD operations go through Entity — specifically that
// SoftDeleteMixin rewrites the Delete and that the default scope
// guards Get.
func TestEntityComposesWithHooks(t *testing.T) {
	type doc struct {
		ID    int64  `drop:"id"`
		Title string `drop:"title"`
	}
	tbl := pg.NewTable("docs")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	pg.Add(tbl, pg.Text("title").NotNull())
	sd := &pg.SoftDeleteMixin{}
	pg.ApplyMixins(tbl, sd)
	ent := pg.NewEntity[doc](tbl)

	fd := &fakeDriver{}
	db := pg.New(fd)

	if _, err := ent.Delete(db, context.Background(), int64(1)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	sql := fd.queries[0]
	if !strings.HasPrefix(sql, "UPDATE ") {
		t.Errorf("SoftDelete should rewrite Delete to UPDATE: %s", sql)
	}

	// Get also picks up the default scope.
	fd.queries = nil
	fd.handler = func(string, []any) (drops.Rows, error) {
		return &fakeRows{cols: []string{"id", "title"}}, nil
	}
	_, _ = ent.Get(db, context.Background(), int64(1))
	sel := fd.queries[0]
	if !strings.Contains(sel, "deletedAt") {
		t.Errorf("Get should pick up default scope: %s", sel)
	}
}

// TestEntityQueryReturnsTypedSlice exercises EntityQuery.All / .One.
func TestEntityQueryReturnsTypedSlice(t *testing.T) {
	_, ent := entUsersSchema()

	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"id", "name", "email"},
			data: [][]any{
				{int64(1), "A", "a@x"},
				{int64(2), "B", "b@x"},
			},
		}, nil
	}}
	db := pg.New(fd)
	users, err := ent.Query(db).Where(drops.Raw(`"users"."name" IS NOT NULL`)).All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(users) != 2 || users[0].Name != "A" || users[1].Name != "B" {
		t.Errorf("unexpected users: %+v", users)
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

// axisTenant is the tenant these tests put on ctx. It is distinctive so
// a SET list that bound the wrong value cannot pass by coincidence.
const axisTenant = int64(77)

type axisRow struct {
	ID       int64  `drop:"id"`
	TenantID int64  `drop:"tenantId"`
	Title    string `drop:"title"`
}

// axisEntity declares the axis through ScopeByTenant, which is the
// spelling the readme shows: it registers the read-side predicate on the
// table and names the column a write stamps, in one call.
func axisEntity() *pg.Entity[axisRow] {
	tbl := pg.NewTable("axis_rows")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	tenant := pg.Add(tbl, pg.BigInt("tenantId").NotNull())
	pg.Add(tbl, pg.Text("title").NotNull())
	return pg.NewEntity[axisRow](tbl).ScopeByTenant(tenant)
}

// A struct whose tenant field nobody filled in is the ordinary shape —
// the one a form or a decoded body produces. The SET list must carry the
// ctx tenant for it. The assertion is on the bound argument rather than
// on the struct afterwards, because pg refreshes r from RETURNING and
// the server's echo would hide an unstamped write.
func TestUpdateStampsTheTenantItWrites(t *testing.T) {
	ent := axisEntity()
	drv := dropstest.New().AlwaysRows(
		[]string{"id", "tenantId", "title"}, []any{int64(7), axisTenant, "x"})
	db := pg.New(drv)

	row := axisRow{ID: 7, Title: "x"}
	if err := ent.Update(db, pg.WithTenant(context.Background(), axisTenant), &row); err != nil {
		t.Fatalf("Update: %v", err)
	}
	st := drv.Last()
	want := `UPDATE "axis_rows" SET "tenantId" = $1, "title" = $2 ` +
		`WHERE ("axis_rows"."id" = $3) AND ("axis_rows"."tenantId" = $4) ` +
		`RETURNING "axis_rows"."id", "axis_rows"."tenantId", "axis_rows"."title"`
	if st.SQL != want {
		t.Errorf("rendered SQL:\n got = %s\nwant = %s", st.SQL, want)
	}
	wantArgs := []any{axisTenant, "x", int64(7), axisTenant}
	if !reflect.DeepEqual(st.Args, wantArgs) {
		t.Errorf("bound arguments: got = %v, want %v", st.Args, wantArgs)
	}
}

// The struct is not an instruction. A row carrying somebody else's
// tenant is refused with ErrTenantMismatch and NO statement is sent —
// not an UPDATE that happens to match nothing, which would report zero
// rows affected and leave the caller to guess why.
func TestUpdateRefusesToHandTheRowToAnotherTenant(t *testing.T) {
	ent := axisEntity()
	drv := dropstest.New()
	db := pg.New(drv)

	row := axisRow{ID: 7, TenantID: 99, Title: "x"}
	err := ent.Update(db, pg.WithTenant(context.Background(), axisTenant), &row)
	if !errors.Is(err, pg.ErrTenantMismatch) {
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
	db := pg.New(drv)

	row := axisRow{ID: 7, Title: "x"}
	if err := ent.Update(db, context.Background(), &row); !errors.Is(err, pg.ErrTenantMissing) {
		t.Errorf("Update = %v, want ErrTenantMissing", err)
	}
	if got := drv.Statements(); len(got) != 0 {
		t.Errorf("an Update with no tenant on ctx still sent %d statement(s): %v", len(got), got)
	}
	if row.TenantID != 0 {
		t.Errorf("a refused Update stamped the row anyway: %d", row.TenantID)
	}
}

// Save is Update with a branch in front of it, so the rule has to hold
// through it too: a keyed row routed to Update carries the axis, and a
// foreign tenant is refused on that path as well.
func TestSaveRoutesThroughTheSameAxisRule(t *testing.T) {
	ent := axisEntity()
	drv := dropstest.New()
	db := pg.New(drv)

	row := axisRow{ID: 7, TenantID: 99, Title: "x"}
	if err := ent.Save(db, pg.WithTenant(context.Background(), axisTenant), &row); !errors.Is(err, pg.ErrTenantMismatch) {
		t.Errorf("Save = %v, want ErrTenantMismatch", err)
	}
	if got := drv.Statements(); len(got) != 0 {
		t.Errorf("a refused Save still sent %d statement(s): %v", len(got), got)
	}
}
