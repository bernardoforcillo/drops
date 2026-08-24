package pg_test

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// The write half. SetNull has to bind a parameter, because the whole
// reason it exists is that splicing the literal NULL turns one
// statement into two — two plan-cache entries, two prepared
// statements — and routes the value around AddArg, where hooks,
// tracers and PII redaction live.
//
// The lazy implementation is `return c.Expr(drops.Raw("NULL"))`,
// which is today's workaround behind a new name. It passes every
// round-trip test and fails this one.
func TestSetNullBindsAParameterNotTheToken(t *testing.T) {
	tbl := pg.NewTable("null_probe")
	id := pg.Add(tbl, pg.BigInt("id").NotNull())
	bio := pg.Add(tbl, pg.Text("bio").Nullable())

	sqlText, args := pg.New(nil).Insert(tbl).Row(id.Val(1), bio.SetNull()).ToSQL()
	if strings.Contains(sqlText, "NULL") {
		t.Errorf("SetNull spliced the literal into the SQL: %s", sqlText)
	}
	if !strings.Contains(sqlText, "VALUES ($1, $2)") {
		t.Errorf("want two placeholders, got: %s", sqlText)
	}
	if len(args) != 2 || args[1] != nil {
		t.Errorf("args = %#v, want [1 <nil>]", args)
	}
}

func TestValPtrAndValNullBindTheUnwrappedValue(t *testing.T) {
	tbl := pg.NewTable("null_probe")
	bio := pg.Add(tbl, pg.Text("bio").Nullable())
	v := "hello"

	cases := []struct {
		name string
		cv   pg.ColumnValue
		want any
	}{
		{"ValPtr nil", bio.ValPtr(nil), nil},
		{"ValPtr value", bio.ValPtr(&v), "hello"},
		{"ValNull invalid", bio.ValNull(sql.Null[string]{}), nil},
		{"ValNull valid", bio.ValNull(sql.Null[string]{V: "hello", Valid: true}), "hello"},
	}
	for _, c := range cases {
		_, args := pg.New(nil).Insert(tbl).Row(c.cv).ToSQL()
		if len(args) != 1 {
			t.Fatalf("%s: args = %#v", c.name, args)
		}
		// The bound argument's Go type must be what Val would have
		// bound, not a wrapper: sql.Null[T].Value did not convert its
		// payload before Go 1.25, so handing the driver the wrapper
		// makes the parameter's fate depend on the toolchain.
		if args[0] != c.want {
			t.Errorf("%s: bound %#v, want %#v", c.name, args[0], c.want)
		}
	}
}

// Nullability must not leak into the operand type. WHERE bio = $1
// compares against a value and never against NULL, so a nullable
// column's comparisons take the same plain T a NOT NULL one does.
//
// This is a compile-time assertion wearing a runtime test's clothes:
// if anyone later "simplifies" Col[T] into Col[sql.Null[T]] or grows
// a NullCol[T] with its own operator set, the calls below stop
// compiling — which is the point.
func TestNullableDoesNotChangeTheOperandType(t *testing.T) {
	tbl := pg.NewTable("operands")
	name := pg.Add(tbl, pg.Text("name").NotNull())
	bio := pg.Add(tbl, pg.Text("bio").Nullable())

	pairs := []struct {
		notNull, nullable drops.Expression
	}{
		{name.Eq("x"), bio.Eq("x")},
		{name.In("a", "b"), bio.In("a", "b")},
		{name.Between("a", "z"), bio.Between("a", "z")},
		{name.Gt("m"), bio.Gt("m")},
	}
	for _, p := range pairs {
		gotN, argsN := drops.String(p.notNull)
		gotB, argsB := drops.String(p.nullable)
		if want := strings.ReplaceAll(gotN, `"name"`, `"bio"`); gotB != want {
			t.Errorf("nullable rendered %s, want %s", gotB, want)
		}
		if len(argsN) != len(argsB) {
			t.Errorf("arg counts differ: %v vs %v", argsN, argsB)
		}
	}
}

// Nullable is a statement about the schema, not a DDL keyword: a
// PostgreSQL column is nullable unless it says otherwise, so saying
// so must not churn the CREATE TABLE or make a spurious migration.
func TestNullableRendersNothingInDDL(t *testing.T) {
	plain := pg.NewTable("t")
	pg.Add(plain, pg.BigSerial("id").PrimaryKey())
	pg.Add(plain, pg.Text("bio"))

	stated := pg.NewTable("t")
	pg.Add(stated, pg.BigSerial("id").PrimaryKey())
	pg.Add(stated, pg.Text("bio").Nullable())

	a, _ := drops.String(pg.CreateTable(plain))
	b, _ := drops.String(pg.CreateTable(stated))
	if a != b {
		t.Errorf("Nullable changed the DDL:\n%s\n%s", a, b)
	}
}

// ----------------------------------------------------------------------
// The read half: NewEntity rejects a column that admits NULL bound to
// a field that cannot receive one.

type nullBadString struct {
	ID  int64  `drop:"id"`
	Bio string `drop:"bio"`
}

type nullBadTime struct {
	ID  int64     `drop:"id"`
	Bio time.Time `drop:"bio"`
}

type nullPtr struct {
	ID  int64   `drop:"id"`
	Bio *string `drop:"bio"`
}

type nullWrapped struct {
	ID  int64            `drop:"id"`
	Bio sql.Null[string] `drop:"bio"`
}

type nullRaw struct {
	ID  int64           `drop:"id"`
	Bio json.RawMessage `drop:"bio"`
}

// bioTable builds a two-column table whose "bio" column is declared
// however the caller says: stated Nullable, stated NotNull, or — the
// case that has been silently accepting NULLs — nothing at all.
func bioTable(state string) *pg.Table {
	tbl := pg.NewTable("bios")
	pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	bio := pg.Text("bio")
	switch state {
	case "nullable":
		bio.Nullable()
	case "notNull":
		bio.NotNull()
	}
	pg.Add(tbl, bio)
	return tbl
}

func TestNewEntityRejectsNullableColumnBoundToUnreceptiveField(t *testing.T) {
	msg := mustPanic(t, func() { pg.NewEntity[nullBadString](bioTable("nullable")) })
	for _, want := range []string{`"bio"`, "field Bio", "string", "*string", "pg.AllowNullableColumns"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q, got: %s", want, msg)
		}
	}
}

func TestNewEntityRejectsNullableColumnBoundToTime(t *testing.T) {
	msg := mustPanic(t, func() { pg.NewEntity[nullBadTime](bioTable("nullable")) })
	if !strings.Contains(msg, "time.Time") {
		t.Errorf("message should name the field type, got: %s", msg)
	}
}

// The case the whole check turns on. A bare pg.Text("bio") states
// nothing, so the database accepts NULL; if the check only questioned
// columns that had already said Nullable it would protect exactly the
// schemas that had already thought about it — nobody.
func TestNewEntityRejectsAnUnstatedColumnToo(t *testing.T) {
	msg := mustPanic(t, func() { pg.NewEntity[nullBadString](bioTable("")) })
	if !strings.Contains(msg, "not declared NOT NULL") {
		t.Errorf("message should say the column never decided, got: %s", msg)
	}
	// An unstated column can legitimately be answered by tightening
	// the column instead of loosening the field, so both fixes go in.
	if !strings.Contains(msg, ".NotNull()") {
		t.Errorf("message should offer .NotNull(), got: %s", msg)
	}
}

// The other half of the check, and the half that proves it is a check
// rather than a tantrum: every shape that can receive a NULL passes,
// and so does the one-directional case — a NOT NULL column bound to a
// pointer field is the legitimate "distinguish unset from zero"
// idiom, not an error.
func TestNewEntityAcceptsEveryReceptiveShape(t *testing.T) {
	t.Run("pointer", func(t *testing.T) {
		pg.NewEntity[nullPtr](bioTable("nullable"))
	})
	t.Run("sql.Null", func(t *testing.T) {
		pg.NewEntity[nullWrapped](bioTable("nullable"))
	})
	t.Run("json.RawMessage", func(t *testing.T) {
		pg.NewEntity[nullRaw](bioTable("nullable"))
	})
	t.Run("not null, plain field", func(t *testing.T) {
		pg.NewEntity[nullBadString](bioTable("notNull"))
	})
	t.Run("not null, pointer field", func(t *testing.T) {
		pg.NewEntity[nullPtr](bioTable("notNull"))
	})
	t.Run("waived", func(t *testing.T) {
		pg.NewEntity[nullBadString](bioTable("nullable"), pg.AllowNullableColumns("bio"))
	})
	t.Run("waived wholesale", func(t *testing.T) {
		pg.NewEntity[nullBadString](bioTable("nullable"), pg.AllowAnyNullableColumn())
	})
}

type nullTwoBad struct {
	ID   int64  `drop:"id"`
	Bio  string `drop:"bio"`
	Note string `drop:"note"`
}

// A waiver implemented as a boolean rather than a set turns the whole
// check off at the first exemption, and every other test here would
// still pass.
func TestAllowNullableColumnsIsNarrow(t *testing.T) {
	tbl := bioTable("nullable")
	pg.Add(tbl, pg.Text("note").Nullable())

	msg := mustPanic(t, func() {
		pg.NewEntity[nullTwoBad](tbl, pg.AllowNullableColumns("bio"))
	})
	if !strings.Contains(msg, `"note"`) {
		t.Errorf("message should name the unexempted column, got: %s", msg)
	}
	if strings.Contains(msg, `"bio"`) {
		t.Errorf("exempted column should not be reported, got: %s", msg)
	}
}

// ----------------------------------------------------------------------
// The loop closes: AutoTable states nullability from the field type,
// so NewAutoEntity cannot trip the check NewEntity runs.

type autoNullRow struct {
	ID      int64            `drop:"id,primaryKey,autoIncrement"`
	Name    string           `drop:"name"`
	Bio     *string          `drop:"bio"`
	Meta    sql.Null[string] `drop:"meta"`
	Blob    []byte           `drop:"blob"`
	Deleted *time.Time       `drop:"deleted"`
}

func TestAutoTableStatesNullabilityFromTheFieldType(t *testing.T) {
	tbl := pg.AutoTable[autoNullRow]("auto_null")
	want := map[string]bool{
		"id": false, "name": false, "bio": true,
		"meta": true,
		// A []byte can receive NULL, but writing one is not the
		// author saying the column is optional.
		"blob": false, "deleted": true,
	}
	for _, c := range tbl.Columns() {
		if got := c.IsNullable(); got != want[c.Name()] {
			t.Errorf("column %q: IsNullable = %v, want %v", c.Name(), got, want[c.Name()])
		}
	}
}

func TestNewAutoEntityPassesItsOwnCheck(t *testing.T) {
	pg.NewAutoEntity[autoNullRow]("auto_null")
}

// The `null` tag is the escape hatch for a field whose type cannot
// state optionality — a named type with its own Scan method.
func TestAutoTableNullTagOverridesTheFieldType(t *testing.T) {
	type row struct {
		ID   int64           `drop:"id,primaryKey"`
		Blob json.RawMessage `drop:"blob,null"`
	}
	tbl := pg.AutoTable[row]("auto_null_tag")
	if !tbl.Col("blob").IsNullable() {
		t.Error("`null` should have made the column nullable")
	}
	pg.NewEntity[row](tbl)
}

func TestAutoTableRejectsContradictoryNullTags(t *testing.T) {
	type row struct {
		ID  int64  `drop:"id,primaryKey"`
		Bio string `drop:"bio,notNull,null"`
	}
	msg := mustPanic(t, func() { pg.AutoTable[row]("contradiction") })
	if !strings.Contains(msg, "contradict") {
		t.Errorf("message should call it a contradiction, got: %s", msg)
	}
}

// A composite PRIMARY KEY declared the only way there is —
// Table.PrimaryKey(cols...) — leaves the columns' notNull flag alone,
// so nothing on the column says NOT NULL even though PostgreSQL makes
// every PK column NOT NULL. The check must read the key, not just the
// flag, or it refuses a correct join table and tells the caller to
// make its key fields pointers.
func TestNewEntityAcceptsACompositeKeyDeclaredOnTheTable(t *testing.T) {
	tbl := pg.NewTable("memberships")
	a := pg.Add(tbl, pg.BigInt("userId"))
	b := pg.Add(tbl, pg.BigInt("groupId"))
	pg.Add(tbl, pg.Text("role").NotNull())
	tbl.PrimaryKey(a.Column, b.Column)

	type membershipRow struct {
		UserID  int64  `drop:"userId"`
		GroupID int64  `drop:"groupId"`
		Role    string `drop:"role"`
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a composite-key entity was refused: %v", r)
		}
	}()
	pg.NewEntity[membershipRow](tbl)
}
