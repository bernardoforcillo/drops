package pg_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/pg"
	left "github.com/bernardoforcillo/drops/pg/internal/rowscope/left"
	right "github.com/bernardoforcillo/drops/pg/internal/rowscope/right"
)

// A guard and a tenant axis are context filters on the table, keyed so
// that an entity rebuilt per request replaces its own filter instead of
// stacking one per request. The key was the row type's SHORT name, so
// two User types from two packages whose last path element matches —
// which is every monorepo with an internal/app and a services/app —
// shared one key, and registering under a key that is taken replaces
// what is there. One of the two guards stopped applying, silently, on a
// table both of them were meant to protect.
//
// The fixture is two packages both called "app", both holding a User:
// reflect.Type.String prints "app.User" for each.
func TestRowScopeKeysSeparateLikeNamedPackages(t *testing.T) {
	tbl := pg.AutoTable[left.User]("users")
	owner, name := tbl.Col("ownerId"), tbl.Col("name")

	pg.NewEntity[left.User](tbl).AuthorizeWith(pg.OwnerGuard{Owner: owner})
	pg.NewEntity[right.User](tbl).AuthorizeWith(pg.CustomGuard(
		func(context.Context) (drops.Expression, error) { return pg.Eq(name, "visible"), nil }))

	ctx := pg.WithSubject(context.Background(), int64(42))
	sql, _, err := pg.New(dropstest.New()).Select().From(tbl).ToSQLCtx(ctx)
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	for _, want := range []string{`"ownerId" = $`, `"name" = $`} {
		if !strings.Contains(sql, want) {
			t.Errorf("rendered SQL %q is missing %q: a guard registered on this table is not being applied", sql, want)
		}
	}
}

// The other half of the same rule, and the half [pg.Entity.AuthorizeWith]
// now documents rather than denies: two entities of the SAME row type
// hold one key, so the second registration replaces the first. The test
// is here to pin the behaviour the doc comment describes — a reader who
// believed the two guards were AND-ed would be reading a narrowing that
// is not there.
func TestRowScopeKeyReplacesWithinOneRowType(t *testing.T) {
	tbl := pg.AutoTable[left.User]("users")
	owner, name := tbl.Col("ownerId"), tbl.Col("name")

	pg.NewEntity[left.User](tbl).AuthorizeWith(pg.OwnerGuard{Owner: owner})
	pg.NewEntity[left.User](tbl).AuthorizeWith(pg.CustomGuard(
		func(context.Context) (drops.Expression, error) { return pg.Eq(name, "visible"), nil }))

	ctx := pg.WithSubject(context.Background(), int64(42))
	sql, _, err := pg.New(dropstest.New()).Select().From(tbl).ToSQLCtx(ctx)
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	if strings.Contains(sql, `"ownerId" = $`) {
		t.Errorf("rendered SQL %q still carries the replaced guard", sql)
	}
	if !strings.Contains(sql, `"name" = $`) {
		t.Errorf("rendered SQL %q is missing the guard that replaced it", sql)
	}
}
