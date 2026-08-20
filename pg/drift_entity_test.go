package pg_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/pg"
)

// The bug this check exists for. The struct misspells one field, so
// the column binds to nothing: it vanishes from every INSERT and
// UPDATE while the schema, the tests and the compiler all stay happy.
type driftRow struct {
	ID    int64
	Name  string
	Emial string // the column is "email"
}

func driftTable() *pg.Table {
	t := pg.NewTable("people")
	pg.Add(t, pg.BigSerial("id").PrimaryKey())
	pg.Add(t, pg.Text("name").NotNull())
	pg.Add(t, pg.Text("email").NotNull())
	return t
}

func mustPanic(t *testing.T, fn func()) (msg string) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected a panic")
		}
		msg, _ = r.(string)
	}()
	fn()
	return ""
}

func TestNewEntityRejectsUnmappedColumn(t *testing.T) {
	msg := mustPanic(t, func() { pg.NewEntity[driftRow](driftTable()) })

	for _, want := range []string{`"email"`, "silently dropped"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message should contain %q, got: %s", want, msg)
		}
	}
	// The unbound field is the likely culprit, so naming it turns
	// the panic into a fix rather than a puzzle.
	if !strings.Contains(msg, "Emial") {
		t.Errorf("message should point at the unbound field, got: %s", msg)
	}
	// And it should say how to accept the situation deliberately.
	if !strings.Contains(msg, "AllowUnmappedColumns") {
		t.Errorf("message should name the escape hatch, got: %s", msg)
	}
}

func TestAllowUnmappedColumnsExemptsNamedColumn(t *testing.T) {
	ent := pg.NewEntity[driftRow](driftTable(), pg.AllowUnmappedColumns("email"))
	if ent == nil {
		t.Fatal("expected the entity to build")
	}
}

// An exemption covers only what it names.
func TestAllowUnmappedColumnsIsNotBlanket(t *testing.T) {
	tbl := driftTable()
	pg.Add(tbl, pg.Text("nickname"))
	msg := mustPanic(t, func() {
		pg.NewEntity[driftRow](tbl, pg.AllowUnmappedColumns("email"))
	})
	if !strings.Contains(msg, "nickname") {
		t.Errorf("the unexempted column should still be reported, got: %s", msg)
	}
	if strings.Contains(msg, `"email"`) {
		t.Errorf("the exempted column should not be reported, got: %s", msg)
	}
}

func TestAllowAnyUnmappedColumn(t *testing.T) {
	if pg.NewEntity[driftRow](driftTable(), pg.AllowAnyUnmappedColumn()) == nil {
		t.Fatal("expected the entity to build")
	}
}

// Columns drops itself writes — the soft-delete marker, the
// timestamps a mixin keeps current — are not the application's to
// map, so they must not trip the check.
func TestManagedColumnsAreExempt(t *testing.T) {
	tbl := pg.NewTable("docs")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	pg.Add(tbl, pg.Text("title").NotNull())
	pg.SoftDelete(tbl)
	pg.Timestamps(tbl)

	type doc struct {
		ID    int64
		Title string
	}
	if pg.NewEntity[doc](tbl) == nil {
		t.Fatal("managed columns should not require a struct field")
	}
}

// A field holding an eager-loaded relation is bound to no column by
// design, so it must not be blamed for a real gap elsewhere... but it
// also must not be reported as a suspect.
func TestRelationFieldsAreNotSuspects(t *testing.T) {
	type post struct{ ID int64 }
	type user struct {
		ID    int64
		Name  string
		Email string
		Posts []post `dropRel:"posts"`
	}
	tbl := pg.NewTable("users")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	pg.Add(tbl, pg.Text("name"))
	pg.Add(tbl, pg.Text("email"))
	pg.Add(tbl, pg.Text("bio"))

	msg := mustPanic(t, func() { pg.NewEntity[user](tbl) })
	if !strings.Contains(msg, "bio") {
		t.Errorf("the genuinely unmapped column should be reported: %s", msg)
	}
	if strings.Contains(msg, "Posts") {
		t.Errorf("a dropRel field is not a suspect: %s", msg)
	}
}

// A `drop:"-"` field is opted out of column binding by the author, so
// it is not a suspect either.
func TestSkippedFieldsAreNotSuspects(t *testing.T) {
	type row struct {
		ID    int64
		Name  string
		Cache string `drop:"-"`
	}
	tbl := pg.NewTable("t")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	pg.Add(tbl, pg.Text("name"))
	pg.Add(tbl, pg.Text("extra"))

	msg := mustPanic(t, func() { pg.NewEntity[row](tbl) })
	if strings.Contains(msg, "Cache") {
		t.Errorf(`a drop:"-" field is not a suspect: %s`, msg)
	}
}

// A correctly-bound entity still builds, tags included.
func TestWellMappedEntityBuilds(t *testing.T) {
	type row struct {
		ID       int64
		FullName string `drop:"name"`
		Email    string
	}
	if pg.NewEntity[row](driftTable()) == nil {
		t.Fatal("expected the entity to build")
	}
}

// The end-to-end consequence: with the check in place, the INSERT for
// a well-mapped entity carries every column.
func TestMappedEntityWritesEveryColumn(t *testing.T) {
	type row struct {
		ID    int64
		Name  string
		Email string
	}
	ent := pg.NewEntity[row](driftTable())
	fd := &fakeDriver{}
	r := row{Name: "Ada", Email: "ada@example.com"}
	_ = ent.Create(pg.New(fd), context.Background(), &r)

	if len(fd.queries) == 0 {
		t.Fatal("no statement issued")
	}
	for _, col := range []string{`"name"`, `"email"`} {
		if !strings.Contains(fd.queries[0], col) {
			t.Errorf("INSERT is missing %s: %s", col, fd.queries[0])
		}
	}
}
