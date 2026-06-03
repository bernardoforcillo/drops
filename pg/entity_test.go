package pg_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// User is the entity fixture used across these tests.
type entUser struct {
	ID    int64  `db:"id"`
	Name  string `db:"name"`
	Email string `db:"email"`
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
		Name string `db:"name"`
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
	header := sql[:strings.Index(sql, " VALUES")]
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
	if strings.Contains(sql[:strings.Index(sql, " WHERE")], `"id" =`) {
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
		ID    int64  `db:"id"`
		Title string `db:"title"`
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
	if !strings.Contains(sel, "deleted_at") {
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
