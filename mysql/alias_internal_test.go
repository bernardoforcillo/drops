package mysql

import "testing"

// isKeyColumn decides whether bindings skips a column on UPDATE, and
// nothing in the package hands it a column from another instance of
// the table today — Entity builds both its key list and its column
// list off the one *Table. That is a property of the current callers,
// not of the function: Entity.PK and Entity.PKs hand those pointers
// out, so the first caller to route a caller-supplied column here
// turns the pointer comparison live, and the symptom is an UPDATE
// that reassigns the primary key. The guard costs nothing.
func TestIsKeyColumnAcceptsEitherHandle(t *testing.T) {
	type row struct {
		ID   int64  `drop:"id"`
		Name string `drop:"name"`
	}
	tbl := NewTable("users")
	Add(tbl, BigSerial("id").PrimaryKey())
	Add(tbl, Varchar("name", 255).NotNull())
	e := NewEntity[row](tbl)

	u := tbl.As("u")
	if !e.isKeyColumn(u.Col("id")) {
		t.Error("the alias's handle on the key column is the key column")
	}
	if e.isKeyColumn(u.Col("name")) {
		t.Error("a non-key column must not become one by being aliased")
	}
	// A same-named column of a different table is a different column.
	other := NewTable("users")
	Add(other, BigSerial("id").PrimaryKey())
	if e.isKeyColumn(other.Col("id")) {
		t.Error("another table's key column is not this entity's")
	}
}
