package pg_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// jsonbSchema returns a table with a single jsonb column ready for
// the path tests.
func jsonbSchema() (*pg.Table, *pg.Col[int64], *pg.Col[any]) {
	tbl := pg.NewTable("users")
	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	meta := pg.Add(tbl, pg.Custom[any]("meta", "jsonb"))
	return tbl, id, meta
}

func TestJSONFieldEqRendersCast(t *testing.T) {
	_, _, meta := jsonbSchema()
	theme := pg.JSONField[string](meta, "settings", "theme")
	sql, args := drops.String(theme.Eq("dark"))
	want := `(("users"."meta" -> 'settings' ->> 'theme')::text = $1)`
	if sql != want {
		t.Errorf("Eq sql mismatch\n got:  %s\n want: %s", sql, want)
	}
	if len(args) != 1 || args[0] != "dark" {
		t.Errorf("Eq args: %v", args)
	}
}

func TestJSONFieldIntCast(t *testing.T) {
	_, _, meta := jsonbSchema()
	count := pg.JSONField[int64](meta, "stats", "loginCount")
	sql, args := drops.String(count.Gt(int64(10)))
	if !strings.Contains(sql, "::bigint") {
		t.Errorf("int64 should cast to ::bigint, got: %s", sql)
	}
	if !strings.Contains(sql, " > $1") {
		t.Errorf("expected > $1, got: %s", sql)
	}
	if len(args) != 1 || args[0] != int64(10) {
		t.Errorf("args: %v", args)
	}
}

func TestJSONFieldBoolCast(t *testing.T) {
	_, _, meta := jsonbSchema()
	beta := pg.JSONField[bool](meta, "flags", "beta")
	sql, _ := drops.String(beta.Eq(true))
	if !strings.Contains(sql, "::boolean") {
		t.Errorf("bool should cast to ::boolean, got: %s", sql)
	}
}

func TestJSONFieldInWalksValues(t *testing.T) {
	_, _, meta := jsonbSchema()
	role := pg.JSONField[string](meta, "role")
	sql, args := drops.String(role.In("admin", "owner", "member"))
	if !strings.Contains(sql, " IN ($1, $2, $3)") {
		t.Errorf("expected IN ($1, $2, $3), got: %s", sql)
	}
	if len(args) != 3 {
		t.Errorf("args: %v", args)
	}
}

func TestJSONFieldIsNull(t *testing.T) {
	_, _, meta := jsonbSchema()
	missing := pg.JSONField[string](meta, "nope")
	sql, _ := drops.String(missing.IsNull())
	if !strings.Contains(sql, "IS NULL") {
		t.Errorf("expected IS NULL, got: %s", sql)
	}
}

func TestJSONContainsRendersContainmentOperator(t *testing.T) {
	_, _, meta := jsonbSchema()
	sql, args := drops.String(pg.JSONContains(meta, []byte(`{"role":"admin"}`)))
	if !strings.Contains(sql, " @> ") {
		t.Errorf("expected @> operator, got: %s", sql)
	}
	if len(args) != 1 {
		t.Errorf("args: %v", args)
	}
}

func TestJSONHasKeyRendersExistenceOperator(t *testing.T) {
	_, _, meta := jsonbSchema()
	sql, args := drops.String(pg.JSONHasKey(meta, "role"))
	if !strings.Contains(sql, " ? ") {
		t.Errorf("expected ? operator, got: %s", sql)
	}
	if len(args) != 1 || args[0] != "role" {
		t.Errorf("args: %v", args)
	}
}

func TestJSONFieldUsableInWhere(t *testing.T) {
	tbl, id, meta := jsonbSchema()
	beta := pg.JSONField[bool](meta, "flags", "beta")
	db := pg.New(&fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{}, nil
	}})
	q := db.Select(id).From(tbl).Where(beta.Eq(true))
	sql, _ := q.ToSQL()
	if !strings.Contains(sql, "::boolean") {
		t.Errorf("expected ::boolean cast in SELECT WHERE, got: %s", sql)
	}
	// Smoke: actually run through the Rows path.
	_, _ = q.Rows(context.Background())
}

func TestJSONFieldEscapesQuotesInKey(t *testing.T) {
	_, _, meta := jsonbSchema()
	// Pathological key containing a single quote — must be escaped.
	weird := pg.JSONField[string](meta, "isn't")
	sql, _ := drops.String(weird.Eq("x"))
	if !strings.Contains(sql, "'isn''t'") {
		t.Errorf("single quote should be doubled, got: %s", sql)
	}
}
