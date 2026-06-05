package pg_test

import (
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

func TestTimestampsTemplate(t *testing.T) {
	table := pg.NewTable("widgets")
	pg.Add(table, pg.BigSerial("id").PrimaryKey())
	ts := pg.Timestamps(table)

	if ts.CreatedAt == nil || ts.UpdatedAt == nil {
		t.Fatalf("Timestamps must return non-nil column handles")
	}
	if ts.CreatedAt.Name() != "createdAt" || ts.UpdatedAt.Name() != "updatedAt" {
		t.Fatalf("unexpected column names: %q, %q", ts.CreatedAt.Name(), ts.UpdatedAt.Name())
	}
	if !ts.CreatedAt.Column.IsNotNull() || !ts.UpdatedAt.Column.IsNotNull() {
		t.Fatalf("timestamps columns must be NOT NULL")
	}
	if !ts.CreatedAt.HasDefault() || ts.CreatedAt.DefaultSQL() != "now()" {
		t.Fatalf("createdAt default: got %q", ts.CreatedAt.DefaultSQL())
	}

	want := "CREATE TABLE \"widgets\" (\n" +
		"  \"id\" bigserial PRIMARY KEY,\n" +
		"  \"createdAt\" timestamptz NOT NULL DEFAULT now(),\n" +
		"  \"updatedAt\" timestamptz NOT NULL DEFAULT now()\n" +
		")"
	got, _ := drops.String(pg.CreateTable(table))
	if got != want {
		t.Errorf("DDL mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestSoftDeleteTemplate(t *testing.T) {
	table := pg.NewTable("widgets")
	pg.Add(table, pg.BigSerial("id").PrimaryKey())
	sd := pg.SoftDelete(table)

	if sd.DeletedAt == nil {
		t.Fatalf("SoftDelete must return a non-nil column handle")
	}
	if sd.DeletedAt.Name() != "deletedAt" {
		t.Fatalf("unexpected column name: %q", sd.DeletedAt.Name())
	}
	if sd.DeletedAt.Column.IsNotNull() {
		t.Fatalf("deletedAt must be nullable")
	}

	// Typed handle is usable in a query filter.
	sql, _ := drops.String(sd.DeletedAt.IsNull())
	if sql != "(\"widgets\".\"deletedAt\" IS NULL)" {
		t.Errorf("filter mismatch: %s", sql)
	}
}

func TestAuditTemplateBigSerial(t *testing.T) {
	users := pg.NewTable("users")
	uid := pg.Add(users, pg.BigSerial("id").PrimaryKey())

	posts := pg.NewTable("posts")
	pg.Add(posts, pg.BigSerial("id").PrimaryKey())
	ac := pg.Audit(posts, uid)

	if ac.CreatedBy == nil || ac.UpdatedBy == nil {
		t.Fatalf("Audit must return non-nil column handles")
	}
	if ac.CreatedBy.Type().TypeSQL() != "bigint" {
		t.Fatalf("createdBy type: got %q, want bigint", ac.CreatedBy.Type().TypeSQL())
	}
	if ac.CreatedBy.ForeignKey() == nil || ac.CreatedBy.ForeignKey().Target != uid.Column {
		t.Fatalf("createdBy FK must target users.id")
	}

	want := "CREATE TABLE \"posts\" (\n" +
		"  \"id\" bigserial PRIMARY KEY,\n" +
		"  \"createdBy\" bigint REFERENCES \"users\" (\"id\"),\n" +
		"  \"updatedBy\" bigint REFERENCES \"users\" (\"id\")\n" +
		")"
	got, _ := drops.String(pg.CreateTable(posts))
	if got != want {
		t.Errorf("DDL mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestAuditTemplateUUID(t *testing.T) {
	tenants := pg.NewTable("tenants")
	tid := pg.Add(tenants, pg.UUID("id").PrimaryKey())

	docs := pg.NewTable("docs")
	pg.Add(docs, pg.UUID("id").PrimaryKey())
	ac := pg.Audit(docs, tid)

	if ac.CreatedBy.Type().TypeSQL() != "uuid" {
		t.Fatalf("createdBy type: got %q, want uuid", ac.CreatedBy.Type().TypeSQL())
	}
}

func TestUUIDPrimaryKeyTemplate(t *testing.T) {
	table := pg.NewTable("widgets")
	pk := pg.UUIDPrimaryKey(table)
	pg.Add(table, pg.Text("name").NotNull())

	if pk.ID == nil {
		t.Fatalf("UUIDPrimaryKey must return a non-nil column handle")
	}
	if !pk.ID.IsPrimaryKey() {
		t.Fatalf("id must be PRIMARY KEY")
	}
	if pk.ID.DefaultSQL() != "gen_random_uuid()" {
		t.Fatalf("id default: got %q", pk.ID.DefaultSQL())
	}

	want := "CREATE TABLE \"widgets\" (\n" +
		"  \"id\" uuid PRIMARY KEY DEFAULT gen_random_uuid(),\n" +
		"  \"name\" text NOT NULL\n" +
		")"
	got, _ := drops.String(pg.CreateTable(table))
	if got != want {
		t.Errorf("DDL mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestCustomTemplate verifies that the same recipe used by built-in
// templates works for application-defined ones — proving the pattern is
// open to extension.
func TestCustomTemplate(t *testing.T) {
	type LocalisedCols struct {
		Locale *pg.Col[string]
	}
	Localised := func(t *pg.Table) LocalisedCols {
		return LocalisedCols{
			Locale: pg.Add(t, pg.Varchar("locale", 8).NotNull().Default("'en'")),
		}
	}

	table := pg.NewTable("widgets")
	pg.Add(table, pg.BigSerial("id").PrimaryKey())
	loc := Localised(table)

	if loc.Locale == nil || loc.Locale.Name() != "locale" {
		t.Fatalf("custom template returned unexpected handle: %+v", loc.Locale)
	}

	sql, args := drops.String(loc.Locale.Eq("it"))
	if sql != "(\"widgets\".\"locale\" = $1)" || len(args) != 1 || args[0] != "it" {
		t.Errorf("typed handle from custom template: sql=%s args=%v", sql, args)
	}
}

// TestCombinedTemplates verifies that multiple templates can be applied
// to the same table without collisions and produce stable DDL.
func TestCombinedTemplates(t *testing.T) {
	users := pg.NewTable("users")
	uid := pg.Add(users, pg.BigSerial("id").PrimaryKey())

	docs := pg.NewTable("docs")
	pg.UUIDPrimaryKey(docs)
	pg.Add(docs, pg.Text("title").NotNull())
	pg.Timestamps(docs)
	pg.SoftDelete(docs)
	pg.Audit(docs, uid)

	want := "CREATE TABLE \"docs\" (\n" +
		"  \"id\" uuid PRIMARY KEY DEFAULT gen_random_uuid(),\n" +
		"  \"title\" text NOT NULL,\n" +
		"  \"createdAt\" timestamptz NOT NULL DEFAULT now(),\n" +
		"  \"updatedAt\" timestamptz NOT NULL DEFAULT now(),\n" +
		"  \"deletedAt\" timestamptz,\n" +
		"  \"createdBy\" bigint REFERENCES \"users\" (\"id\"),\n" +
		"  \"updatedBy\" bigint REFERENCES \"users\" (\"id\")\n" +
		")"
	got, _ := drops.String(pg.CreateTable(docs))
	if got != want {
		t.Errorf("combined DDL mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
