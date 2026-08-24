package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/examples/schemagen"
	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/pg"
)

// The generated row and insert structs, against a live server.
//
// examples/schemagen checks what the structs *are*: every column
// bound, the nullable ones pointers, pg.NewEntity accepting the row
// with no exemptions. What no unit test can check is the claim the
// insert struct makes by omitting a column — that the database is the
// one that fills it. Whether a bigserial hands back a key, whether
// DEFAULT now() lands, and whether a nil pointer arrives as NULL and
// comes back nil, are facts about PostgreSQL and pgx.
//
// The table is declared here rather than reused from the example
// because the suite shares one database and "users" is not a name to
// claim in it. mustMirror asserts this declaration is the example's
// table column for column, so the fixture cannot drift away from the
// structs under test.

type genrowsCols struct {
	Table *pg.Table
	Email *pg.Col[string]
	Name  *pg.Col[string]
	Age   *pg.Col[int32]
}

func genrowsTable(t *testing.T) genrowsCols {
	t.Helper()
	tbl := pg.NewTable(integration.UniqueName(t, "genrows"))
	cols := genrowsCols{Table: tbl}
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	cols.Email = pg.Add(tbl, pg.Text("email").NotNull().Unique())
	cols.Name = pg.Add(tbl, pg.Text("name").NotNull())
	cols.Age = pg.Add(tbl, pg.Integer("age").Nullable())
	pg.Add(tbl, pg.Timestamp("createdAt", true).NotNull().Default("now()"))
	mustMirror(t, tbl, schemagen.Users)
	return cols
}

// mustMirror fails unless got declares the same columns as want, in
// the same order, with the same type, nullability, key and default.
func mustMirror(t *testing.T, got, want *pg.Table) {
	t.Helper()
	g, w := got.Columns(), want.Columns()
	if len(g) != len(w) {
		t.Fatalf("fixture has %d columns, %s has %d", len(g), want.Name(), len(w))
	}
	for i := range w {
		switch {
		case g[i].Name() != w[i].Name():
			t.Errorf("column %d is %q, want %q", i, g[i].Name(), w[i].Name())
		case g[i].Type().TypeSQL() != w[i].Type().TypeSQL():
			t.Errorf("column %q is %s, want %s", w[i].Name(), g[i].Type().TypeSQL(), w[i].Type().TypeSQL())
		case g[i].IsNullable() != w[i].IsNullable(),
			g[i].IsPrimaryKey() != w[i].IsPrimaryKey(),
			g[i].HasDefault() != w[i].HasDefault():
			t.Errorf("column %q disagrees with %s about null / key / default", w[i].Name(), want.Name())
		}
	}
}

// What the insert struct omits, the database supplies; what its
// pointer field leaves nil, the database stores as NULL and hands
// back as nil.
func TestPGGeneratedInsertLetsTheDatabaseFillWhatItOmits(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	cols := genrowsTable(t)
	dropPG(t, db, cols.Table)
	execPG(t, db, pg.CreateTable(cols.Table))

	// The row struct binds every column of the table, so this is the
	// entity's own drift and nullability check running against a
	// table the server is about to hand real rows from.
	ent := pg.NewEntity[schemagen.UsersRow](cols.Table)

	// A caller supplies exactly the insert struct's fields — no id,
	// no createdAt, because those are not theirs to supply — and a
	// nil Age is the column the caller has nothing to say about.
	age := int32(41)
	for _, ins := range []schemagen.UsersInsert{
		{Email: "unset@example.com", Name: "Unset"},
		{Email: "set@example.com", Name: "Set", Age: &age},
	} {
		stmt := db.Insert(cols.Table).Row(
			cols.Email.Val(ins.Email),
			cols.Name.Val(ins.Name),
			cols.Age.ValPtr(ins.Age),
		)
		// What the insert struct's doc promises about a nil pointer.
		// ValPtr does not drop the column from the statement — it
		// binds NULL — and the difference is the whole of what a
		// reader would do with a column that has a DEFAULT, which is
		// why such a column is not in the struct at all.
		if text, _ := stmt.ToSQL(); !strings.Contains(text, `"age"`) {
			t.Errorf("a nil pointer left the column out of the INSERT: %s", text)
		}
		if _, err := stmt.Exec(ctx); err != nil {
			t.Fatalf("insert %s: %v", ins.Email, err)
		}
	}

	// And the server agrees it stored a NULL there rather than a zero.
	nulls, err := db.Query(ctx,
		`SELECT count(*) FROM "`+cols.Table.Name()+`" WHERE age IS NULL`)
	if err != nil {
		t.Fatalf("count nulls: %v", err)
	}
	var nullCount int
	for nulls.Next() {
		if err := nulls.Scan(&nullCount); err != nil {
			t.Fatalf("scan count: %v", err)
		}
	}
	nulls.Close()
	if nullCount != 1 {
		t.Errorf("server holds %d NULL ages, want 1", nullCount)
	}

	rows, err := ent.Query(db).OrderBy(cols.Email.Asc()).All(ctx)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("read %d rows, want 2", len(rows))
	}
	set, unset := rows[0], rows[1]

	for _, r := range rows {
		// The bigserial the insert struct omits.
		if r.ID == 0 {
			t.Errorf("%s came back with no key; the serial did not fill", r.Email)
		}
		// The DEFAULT now() it omits too.
		if r.CreatedAt.IsZero() {
			t.Errorf("%s came back with a zero createdAt; the DEFAULT did not fill", r.Email)
		}
	}
	if set.ID == unset.ID {
		t.Errorf("both rows got key %d", set.ID)
	}
	// The pointer is the whole reason the row struct is generated: a
	// plain int32 here would have failed pg.NewEntity above, and had
	// it somehow got past, this scan is where it would have died.
	if unset.Age != nil {
		t.Errorf("unset age came back as %v, want nil", *unset.Age)
	}
	if set.Age == nil || *set.Age != age {
		t.Errorf("set age came back as %v, want %d", set.Age, age)
	}
}
