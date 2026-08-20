package pg_test

import (
	"context"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// objectFake answers the catalogue queries Introspect runs for the
// schema-level objects, for an imagined database holding one table
// with row-level security on and forced, one policy, one enum, two
// sequences, a view and a materialised view.
func objectFake() *fakeDriver {
	return &fakeDriver{handler: func(q string, _ []any) (drops.Rows, error) {
		switch {
		case strings.Contains(q, "c.relrowsecurity"):
			return &fakeRows{
				cols: []string{"nspname", "relname", "relrowsecurity", "relforcerowsecurity"},
				data: [][]any{{"public", "docs", true, true}},
			}, nil
		case strings.Contains(q, "pg_policy"):
			return &fakeRows{
				cols: []string{"nspname", "relname", "polname", "polcmd", "polpermissive", "roles", "using", "withcheck"},
				data: [][]any{
					{"public", "docs", "docsRead", "r", false, "alice,bob", `(owner = CURRENT_USER)`, ``},
					{"public", "docs", "docsAll", "*", true, "", `(tenant = 1)`, `(tenant = 1)`},
				},
			}, nil
		case strings.Contains(q, "pg_enum"):
			return &fakeRows{
				cols: []string{"nspname", "typname", "enumlabel"},
				data: [][]any{
					{"public", "docStatus", "draft"},
					{"public", "docStatus", "review"},
					{"public", "docStatus", "published"},
				},
			}, nil
		case strings.Contains(q, "pg_sequence"):
			return &fakeRows{
				cols: []string{"nspname", "relname", "start", "increment", "min", "max", "cache", "cycle"},
				data: [][]any{
					// Everything left to the server.
					{"public", "plain", int64(1), int64(1), int64(1), int64(math.MaxInt64), int64(1), false},
					// Everything named by the declaration.
					{"public", "custom", int64(10), int64(5), int64(2), int64(900), int64(4), true},
				},
			}, nil
		case strings.Contains(q, "pg_get_viewdef"):
			return &fakeRows{
				cols: []string{"nspname", "relname", "relkind", "definition"},
				data: [][]any{
					{"public", "activeDocs", "v", " SELECT id\n   FROM docs;"},
					{"public", "docCounts", "m", " SELECT count(*) AS n\n   FROM docs;"},
				},
			}, nil
		}
		return &fakeRows{}, nil
	}}
}

// Everything the schema layer can declare has to reach the snapshot in
// the shape BuildSnapshot gives it, or Diff reads "not looked at" as
// "removed".
func TestIntrospectReadsSchemaLevelObjects(t *testing.T) {
	snap, err := pg.Introspect(context.Background(), pg.New(objectFake()))
	if err != nil {
		t.Fatal(err)
	}

	enum := snap.Enums["docStatus"]
	if enum == nil {
		t.Fatal("the enum type was not read back")
	}
	// Order, not membership: two enums with the same labels in a
	// different order are different types.
	if !reflect.DeepEqual(enum.Values, []string{"draft", "review", "published"}) {
		t.Errorf("enum labels = %v, want them in catalogue order", enum.Values)
	}

	docs := snap.Tables["public.docs"]
	if docs == nil {
		t.Fatal("the table was not read back")
	}
	if !docs.IsRLSEnabled || !docs.IsRLSForced {
		t.Errorf("RLS read back as enabled=%v forced=%v, want both true", docs.IsRLSEnabled, docs.IsRLSForced)
	}
	read := docs.Policies["docsRead"]
	if read == nil {
		t.Fatal("the policy was not read back")
	}
	if read.As != "RESTRICTIVE" || read.For != "SELECT" {
		t.Errorf("policy as=%q for=%q, want RESTRICTIVE/SELECT", read.As, read.For)
	}
	if !reflect.DeepEqual(read.To, []string{"alice", "bob"}) {
		t.Errorf("policy roles = %v", read.To)
	}
	if read.Using != `(owner = CURRENT_USER)` || read.WithCheck != "" {
		t.Errorf("policy expressions using=%q withCheck=%q", read.Using, read.WithCheck)
	}
	// A policy the catalogue reports for PUBLIC carries no roles, which
	// is how a Policy declared without To spells it.
	if all := docs.Policies["docsAll"]; all == nil || len(all.To) != 0 || all.For != "ALL" {
		t.Errorf("PUBLIC policy read back as %+v", all)
	}

	view := snap.Views["activeDocs"]
	if view == nil || view.Materialized {
		t.Fatalf("view read back as %+v", view)
	}
	// pg_get_viewdef's trailing semicolon is not part of the body:
	// createViewSQL appends its own, and a snapshot holding one would
	// never equal a declared definition.
	if view.Definition != "SELECT id\n   FROM docs" {
		t.Errorf("view body = %q", view.Definition)
	}
	if mat := snap.Views["docCounts"]; mat == nil || !mat.Materialized {
		t.Errorf("materialised view read back as %+v", mat)
	}
}

// A sequence attribute the declaration never named is read back unset,
// not as the value PostgreSQL chose for it. Recording the server's
// answer would leave NewSequence("plain") — which names none of them —
// unequal to what the database holds for every sequence anybody
// declares plainly.
func TestIntrospectLeavesDefaultedSequenceAttributesUnset(t *testing.T) {
	snap, err := pg.Introspect(context.Background(), pg.New(objectFake()))
	if err != nil {
		t.Fatal(err)
	}
	plain := snap.Sequences["plain"]
	if plain == nil {
		t.Fatal("the sequence was not read back")
	}
	declared := pg.BuildSnapshot(pg.NewSchema().AddSequence(pg.NewSequence("plain"))).Sequences["plain"]
	plain.Schema = declared.Schema
	if !reflect.DeepEqual(plain, declared) {
		t.Errorf("a plainly declared sequence reads back as %+v, declared %+v", plain, declared)
	}

	custom := snap.Sequences["custom"]
	if custom == nil {
		t.Fatal("the configured sequence was not read back")
	}
	for _, tc := range []struct {
		field string
		got   *int64
		want  int64
	}{
		{"startWith", custom.Start, 10},
		{"incrementBy", custom.Increment, 5},
		{"minValue", custom.MinValue, 2},
		{"maxValue", custom.MaxValue, 900},
		{"cacheSize", custom.Cache, 4},
	} {
		if tc.got == nil || *tc.got != tc.want {
			t.Errorf("%s read back as %v, want %d", tc.field, tc.got, tc.want)
		}
	}
	if !custom.Cycle {
		t.Error("CYCLE was not read back")
	}
}

// A type a column names has to exist before the CREATE TABLE that
// names it. Emitting the top-level creates after the table DDL meant a
// schema with a single enum column produced a migration PostgreSQL
// refused on its first statement.
func TestDiffCreatesAnEnumBeforeTheTableThatUsesIt(t *testing.T) {
	status := pg.NewEnum("doc_status", "draft", "published")
	docs := pg.NewTable("docs")
	pg.Add(docs, pg.BigSerial("id").PrimaryKey())
	pg.Add(docs, status.Col("status").NotNull())

	stmts := pg.Diff(pg.EmptySnapshot(), pg.BuildSnapshot(pg.NewSchema(docs).AddEnum(status)))
	createType, createTable := -1, -1
	for i, s := range stmts {
		switch {
		case strings.HasPrefix(s, "CREATE TYPE"):
			createType = i
		case strings.HasPrefix(s, `CREATE TABLE "docs"`):
			createTable = i
		}
	}
	if createType < 0 || createTable < 0 {
		t.Fatalf("expected a CREATE TYPE and a CREATE TABLE, got:\n%s", strings.Join(stmts, "\n"))
	}
	if createType > createTable {
		t.Errorf("CREATE TYPE is emitted after the CREATE TABLE that names it:\n%s", strings.Join(stmts, "\n"))
	}
}

// FORCE is a flag of its own in PostgreSQL and has to be one here: a
// diff that read it off IsRLSEnabled would either never apply a
// declared ForceRLS or emit it again on every push.
func TestDiffEmitsForceRowLevelSecurity(t *testing.T) {
	build := func(force bool) *pg.Snapshot {
		docs := pg.NewTable("docs")
		pg.Add(docs, pg.BigSerial("id").PrimaryKey())
		docs.EnableRLS()
		if force {
			docs.ForceRLS()
		}
		return pg.BuildSnapshot(pg.NewSchema(docs))
	}
	on := pg.Diff(build(false), build(true))
	if len(on) != 1 || on[0] != `ALTER TABLE "docs" FORCE ROW LEVEL SECURITY;` {
		t.Errorf("declaring ForceRLS produced %v", on)
	}
	off := pg.Diff(build(true), build(false))
	if len(off) != 1 || off[0] != `ALTER TABLE "docs" NO FORCE ROW LEVEL SECURITY;` {
		t.Errorf("withdrawing ForceRLS produced %v", off)
	}
	if same := pg.Diff(build(true), build(true)); len(same) != 0 {
		t.Errorf("an unchanged ForceRLS produced %v", same)
	}
}

// A policy's roles are a set. pg_policy stores them as an oid array
// with no ordering contract, so two declarations naming the same roles
// in a different order describe one policy and have to compare equal —
// otherwise a push drops and recreates a policy that has not changed.
func TestDiffIgnoresPolicyRoleOrder(t *testing.T) {
	build := func(roles ...string) *pg.Snapshot {
		docs := pg.NewTable("docs")
		pg.Add(docs, pg.BigSerial("id").PrimaryKey())
		docs.EnableRLS()
		docs.AddPolicy(pg.NewPolicy("docsRead").To(roles...).Using("true"))
		return pg.BuildSnapshot(pg.NewSchema(docs))
	}
	if stmts := pg.Diff(build("bob", "alice"), build("alice", "bob")); len(stmts) != 0 {
		t.Errorf("reordering a policy's roles produced %v", stmts)
	}
	if stmts := pg.Diff(build("alice"), build("alice", "bob")); len(stmts) == 0 {
		t.Error("adding a role to a policy produced no statement")
	}
}

// A user-defined type name is written into DDL as the catalogue
// reports it, which for a camelCase enum means it has to be quoted:
// unquoted, PostgreSQL folds docKind to dockind and refuses the
// statement, so a schema declaring an enum in the case the rest of
// drops uses could not be pushed at all. Built-in spellings must not
// be quoted, and none of them is caught.
func TestDiffQuotesAUserDefinedColumnType(t *testing.T) {
	kind := pg.NewEnum("docKind", "note", "memo")
	build := func(withKind bool) *pg.Snapshot {
		docs := pg.NewTable("docs")
		pg.Add(docs, pg.BigSerial("id").PrimaryKey())
		pg.Add(docs, pg.Varchar("title", 255))
		pg.Add(docs, pg.DoublePrecision("weight"))
		if withKind {
			pg.Add(docs, kind.Col("kind"))
		}
		return pg.BuildSnapshot(pg.NewSchema(docs).AddEnum(kind))
	}
	create := strings.Join(pg.Diff(pg.EmptySnapshot(), build(true)), "\n")
	if !strings.Contains(create, `"kind" "docKind"`) {
		t.Errorf("the enum column type is not quoted:\n%s", create)
	}
	for _, builtin := range []string{`"title" varchar(255)`, `"weight" double precision`} {
		if !strings.Contains(create, builtin) {
			t.Errorf("a built-in type was rewritten; wanted %s in:\n%s", builtin, create)
		}
	}
	add := strings.Join(pg.Diff(build(false), build(true)), "\n")
	if !strings.Contains(add, `ADD COLUMN "kind" "docKind"`) {
		t.Errorf("ADD COLUMN does not quote the type:\n%s", add)
	}
}
