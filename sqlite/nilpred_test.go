package sqlite_test

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/sqlite"
)

// A nil predicate is how a conditional filter says "no restriction".
// The same tests exist in pg, mysql and clickhouse: an answer that
// differs per dialect is no answer at all, because the boilerplate it
// removes is written by callers who use more than one.

func TestAndOrDropNilPredicates(t *testing.T) {
	c := col("a")
	x := sqlite.Gt(c, 1)
	y := sqlite.Lt(c, 9)

	wantSQL(t, sqlite.And(nil, x, nil), `("t"."a" > ?)`, 1)
	wantSQL(t, sqlite.Or(nil, x, nil), `("t"."a" > ?)`, 1)
	wantSQL(t, sqlite.And(nil, x, nil, y), `(("t"."a" > ?) AND ("t"."a" < ?))`, 2)
	wantSQL(t, sqlite.Or(x, nil, y), `(("t"."a" > ?) OR ("t"."a" < ?))`, 2)

	// Nothing left to join is the connective's identity — not "()",
	// which is what the empty case used to render and which no
	// engine will parse.
	wantSQL(t, sqlite.And(nil, nil), `TRUE`, 0)
	wantSQL(t, sqlite.Or(nil, nil), `FALSE`, 0)
	wantSQL(t, sqlite.And(), `TRUE`, 0)
	wantSQL(t, sqlite.Or(), `FALSE`, 0)

	wantSQL(t, sqlite.Not(nil), `(NOT TRUE)`, 0)
}

func TestWhereDropsNilPredicates(t *testing.T) {
	users, orders := mkSchema()
	db := sqlite.New(&fakeDriver{})
	age := users.Col("tenantId")

	got, _ := db.Select(users.Col("id")).From(users).Where(nil).ToSQL()
	if strings.Contains(got, "WHERE") {
		t.Errorf("an all-nil Where left a clause behind: %s", got)
	}

	got, _ = db.Select(users.Col("id")).From(users).Where(nil, sqlite.Eq(age, 1), nil).ToSQL()
	want := `SELECT "users"."id" FROM "users" WHERE ("users"."tenantId" = ?)`
	if got != want {
		t.Errorf("sql\n  got:  %s\n  want: %s", got, want)
	}

	got, _ = db.Delete(orders).Where(nil, sqlite.Eq(orders.Col("tenantId"), 1)).ToSQL()
	if got != `DELETE FROM "orders" WHERE ("orders"."tenantId" = ?)` {
		t.Errorf("sql = %s", got)
	}
}

func TestJoinOnNilRendersTheIdentity(t *testing.T) {
	users, orders := mkSchema()
	db := sqlite.New(&fakeDriver{})
	got, _ := db.Select(users.Col("id")).From(users).LeftJoin(orders, nil).ToSQL()
	if !strings.HasSuffix(got, `LEFT JOIN "orders" ON TRUE`) {
		t.Errorf("a nil join condition rendered: %s", got)
	}
}
