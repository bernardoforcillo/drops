package clickhouse_test

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/clickhouse"
)

func TestTimestampsTemplateClickhouse(t *testing.T) {
	table := clickhouse.NewTable("widgets").Engine(clickhouse.MergeTree())
	id := clickhouse.Add(table, clickhouse.UUID("id"))
	ts := clickhouse.Timestamps(table)
	table.OrderBy(id)

	if ts.CreatedAt == nil || ts.UpdatedAt == nil {
		t.Fatalf("Timestamps must return non-nil column handles")
	}
	if ts.CreatedAt.Name() != "created_at" || ts.UpdatedAt.Name() != "updated_at" {
		t.Fatalf("unexpected column names: %q, %q", ts.CreatedAt.Name(), ts.UpdatedAt.Name())
	}
	if !ts.CreatedAt.Column.HasDefault() || ts.CreatedAt.Column.DefaultSQL() != "now()" {
		t.Fatalf("created_at default: got %q", ts.CreatedAt.Column.DefaultSQL())
	}

	got, _ := clickhouse.ToSQL(clickhouse.CreateTable(table))
	for _, want := range []string{
		`"created_at" DateTime DEFAULT now()`,
		`"updated_at" DateTime DEFAULT now()`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in DDL:\n%s", want, got)
		}
	}
}

func TestSoftDeleteTemplateClickhouse(t *testing.T) {
	table := clickhouse.NewTable("widgets").Engine(clickhouse.MergeTree())
	id := clickhouse.Add(table, clickhouse.UUID("id"))
	sd := clickhouse.SoftDelete(table)
	table.OrderBy(id)

	if sd.DeletedAt == nil {
		t.Fatalf("SoftDelete must return a non-nil column handle")
	}
	if !sd.DeletedAt.Column.IsNullable() {
		t.Fatalf("deleted_at must be Nullable")
	}

	got, _ := clickhouse.ToSQL(clickhouse.CreateTable(table))
	if !strings.Contains(got, `"deleted_at" Nullable(DateTime)`) {
		t.Errorf("missing nullable deleted_at in DDL:\n%s", got)
	}
}

func TestAuditTemplateClickhouse(t *testing.T) {
	users := clickhouse.NewTable("users").Engine(clickhouse.MergeTree())
	uid := clickhouse.Add(users, clickhouse.UUID("id"))
	users.OrderBy(uid)

	posts := clickhouse.NewTable("posts").Engine(clickhouse.MergeTree())
	pid := clickhouse.Add(posts, clickhouse.UUID("id"))
	ac := clickhouse.Audit(posts, uid)
	posts.OrderBy(pid)

	if ac.CreatedBy == nil || ac.UpdatedBy == nil {
		t.Fatalf("Audit must return non-nil column handles")
	}
	if ac.CreatedBy.Type().TypeSQL() != "UUID" {
		t.Fatalf("created_by type: got %q, want UUID", ac.CreatedBy.Type().TypeSQL())
	}

	got, _ := clickhouse.ToSQL(clickhouse.CreateTable(posts))
	for _, want := range []string{
		`"created_by" UUID`,
		`"updated_by" UUID`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in DDL:\n%s", want, got)
		}
	}
}

func TestUUIDPrimaryKeyTemplateClickhouse(t *testing.T) {
	table := clickhouse.NewTable("widgets").Engine(clickhouse.MergeTree())
	pk := clickhouse.UUIDPrimaryKey(table)
	clickhouse.Add(table, clickhouse.String("name"))
	table.OrderBy(pk.ID)

	if pk.ID == nil {
		t.Fatalf("UUIDPrimaryKey must return a non-nil column handle")
	}
	if pk.ID.Column.DefaultSQL() != "generateUUIDv4()" {
		t.Fatalf("id default: got %q", pk.ID.Column.DefaultSQL())
	}

	got, _ := clickhouse.ToSQL(clickhouse.CreateTable(table))
	if !strings.Contains(got, `"id" UUID DEFAULT generateUUIDv4()`) {
		t.Errorf("missing id column in DDL:\n%s", got)
	}
}

// TestCustomTemplateClickhouse mirrors the pg test: external code can
// build its own templates following the same recipe.
func TestCustomTemplateClickhouse(t *testing.T) {
	type LocalisedCols struct {
		Locale *clickhouse.Col[string]
	}
	Localised := func(t *clickhouse.Table) LocalisedCols {
		return LocalisedCols{
			Locale: clickhouse.Add(t, clickhouse.String("locale").LowCardinality().Default("'en'")),
		}
	}

	table := clickhouse.NewTable("widgets").Engine(clickhouse.MergeTree())
	id := clickhouse.Add(table, clickhouse.UUID("id"))
	loc := Localised(table)
	table.OrderBy(id)

	if loc.Locale == nil || loc.Locale.Name() != "locale" {
		t.Fatalf("custom template returned unexpected handle: %+v", loc.Locale)
	}

	got, _ := clickhouse.ToSQL(clickhouse.CreateTable(table))
	if !strings.Contains(got, `"locale" LowCardinality(String) DEFAULT 'en'`) {
		t.Errorf("missing locale column in DDL:\n%s", got)
	}
}
