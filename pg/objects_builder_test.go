package pg_test

import (
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/pg"
)

func TestViewBuilderFromSelectInlinesLiterals(t *testing.T) {
	users := pg.NewTable("users")
	id := pg.Add(users, pg.BigSerial("id").PrimaryKey())
	active := pg.Add(users, pg.Boolean("active").NotNull())

	v := pg.View("activeUsers").As(
		pg.New(noopDriver{}).Select(id).From(users).Where(active.Eq(true)),
	)

	if v.IsMaterialized() {
		t.Error("View(...) must default to non-materialized")
	}
	if v.Name() != "activeUsers" {
		t.Errorf("Name: %q", v.Name())
	}
	def := v.Definition()
	if !strings.Contains(def, "SELECT") || !strings.Contains(def, "FROM") {
		t.Errorf("definition missing SELECT/FROM: %q", def)
	}
	if !strings.Contains(def, "= true") {
		t.Errorf("literal not inlined: %q", def)
	}
	if strings.Contains(def, "$1") {
		t.Errorf("placeholder still present: %q", def)
	}
}

func TestMaterializedViewBuilderFlagsMaterialized(t *testing.T) {
	t1 := pg.NewTable("players")
	id := pg.Add(t1, pg.BigSerial("id").PrimaryKey())

	v := pg.MaterializedView("playerStats").As(
		pg.New(noopDriver{}).Select(id).From(t1),
	)
	if !v.IsMaterialized() {
		t.Error("MaterializedView must set materialized = true")
	}
}

func TestViewBuilderMaterializedChainFlipsKind(t *testing.T) {
	v := pg.View("v").AsSQL("SELECT 1").Materialized()
	if !v.IsMaterialized() {
		t.Error("Materialized() must flip the flag")
	}
}

func TestViewBuilderAsSQLPassesDefinitionThrough(t *testing.T) {
	body := "SELECT id, name FROM users WHERE created_at > now() - INTERVAL '7 days'"
	v := pg.View("recent").AsSQL(body)
	if v.Definition() != body {
		t.Errorf("AsSQL must preserve text verbatim: got %q", v.Definition())
	}
}

func TestViewBuilderInlinesMultipleParamTypes(t *testing.T) {
	users := pg.NewTable("users")
	id := pg.Add(users, pg.BigSerial("id").PrimaryKey())
	name := pg.Add(users, pg.Text("name").NotNull())
	level := pg.Add(users, pg.Integer("level"))
	since := pg.Add(users, pg.Timestamp("since", true))

	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	v := pg.View("filtered").As(
		pg.New(noopDriver{}).Select(id).From(users).
			Where(name.Eq("alice")).
			Where(level.Gt(int32(50))).
			Where(since.Gte(cutoff)),
	)
	def := v.Definition()
	if !strings.Contains(def, "'alice'") {
		t.Errorf("string literal not inlined: %q", def)
	}
	if !strings.Contains(def, "> 50") {
		t.Errorf("int literal not inlined: %q", def)
	}
	if !strings.Contains(def, "::timestamptz") {
		t.Errorf("time literal not inlined with cast: %q", def)
	}
	if strings.Contains(def, "$") {
		t.Errorf("placeholders should all be substituted: %q", def)
	}
}

func TestViewBuilderEscapesEmbeddedQuotes(t *testing.T) {
	users := pg.NewTable("users")
	id := pg.Add(users, pg.BigSerial("id").PrimaryKey())
	name := pg.Add(users, pg.Text("name").NotNull())

	v := pg.View("vips").As(
		pg.New(noopDriver{}).Select(id).From(users).
			Where(name.Eq("O'Brien")),
	)
	def := v.Definition()
	if !strings.Contains(def, "'O''Brien'") {
		t.Errorf("single quote not doubled: %q", def)
	}
}

func TestViewBuilderPanicsOnUnsupportedParamType(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for unsupported parameter type")
		}
	}()
	users := pg.NewTable("users")
	id := pg.Add(users, pg.BigSerial("id").PrimaryKey())
	name := pg.Add(users, pg.Text("name"))

	// chan can't be SQL-literalised — must panic. Use the
	// untyped pg.Eq overload so the compile-time generic doesn't
	// reject the bad value.
	pg.View("bad").As(
		pg.New(noopDriver{}).Select(id).From(users).
			Where(pg.Eq(name, make(chan int))),
	)
}

func TestViewBuilderIntegratesWithDiff(t *testing.T) {
	// Ensure the builder-produced PgView round-trips through the
	// snapshot / diff layer the same as the legacy string-based
	// constructor.
	users := pg.NewTable("users")
	id := pg.Add(users, pg.BigSerial("id").PrimaryKey())
	active := pg.Add(users, pg.Boolean("active").NotNull())

	v := pg.View("activeUsers").As(
		pg.New(noopDriver{}).Select(id).From(users).Where(active.Eq(true)),
	)
	cur := pg.BuildSnapshot(pg.NewSchema(users).AddView(v))
	stmts := pg.Diff(pg.EmptySnapshot(), cur)

	sawCreate := false
	for _, s := range stmts {
		if strings.HasPrefix(s, `CREATE VIEW "activeUsers"`) && strings.Contains(s, "= true") {
			sawCreate = true
		}
	}
	if !sawCreate {
		t.Errorf("expected CREATE VIEW with inlined literal, got:\n%s", strings.Join(stmts, "\n"))
	}
}
