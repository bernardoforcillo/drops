package pg_test

import (
	"testing"

	"github.com/bernardoforcillo/drops/pg"
)

// A filter that depends on a request parameter is built by
// accumulating predicates:
//
//	var preds []drops.Expression
//	if q != "" {
//	    preds = append(preds, userName.Like(q))
//	}
//	db.Select().From(users).Where(preds...)
//
// The slice and the if exist only because a nil predicate used to be a
// nil dereference or a dangling AND. A nil predicate means "no
// restriction" — which is exactly what the caller meant — so it is
// dropped, and the accumulator collapses to a single expression that
// is nil when the parameter was absent.

func TestAndOrDropNilPredicates(t *testing.T) {
	x := userAge.Gte(18)
	y := userName.Ne("")

	checkExpr(t, pg.And(nil, x, nil), `("users"."age" >= $1)`, int32(18))
	checkExpr(t, pg.Or(nil, x, nil), `("users"."age" >= $1)`, int32(18))
	checkExpr(t, pg.And(nil, x, nil, y),
		`(("users"."age" >= $1) AND ("users"."name" <> $2))`, int32(18), "")
	checkExpr(t, pg.Or(x, nil, y),
		`(("users"."age" >= $1) OR ("users"."name" <> $2))`, int32(18), "")

	// All-nil is the same as none: the identity of each connective,
	// so a fully-absent filter still renders a legal predicate.
	checkExpr(t, pg.And(nil, nil), `TRUE`)
	checkExpr(t, pg.Or(nil, nil), `FALSE`)
	checkExpr(t, pg.And(), `TRUE`)
	checkExpr(t, pg.Or(), `FALSE`)

	// Nested, because that is how a real filter is shaped.
	checkExpr(t, pg.And(nil, x, pg.Or(nil, y, nil)),
		`(("users"."age" >= $1) AND ("users"."name" <> $2))`, int32(18), "")
}

func TestNotOfNilIsTheEmptyConjunctionNegated(t *testing.T) {
	checkExpr(t, pg.Not(nil), `(NOT TRUE)`)
}

func TestWhereDropsNilPredicates(t *testing.T) {
	db := pg.New(nil)

	// Every predicate absent: no WHERE at all, not "WHERE " with
	// nothing after it.
	check(t, db.Select().From(users).Where(nil), `SELECT * FROM "users"`)
	check(t, db.Select().From(users).Where(nil, nil), `SELECT * FROM "users"`)

	check(t,
		db.Select().From(users).Where(nil, userAge.Gte(18), nil),
		`SELECT * FROM "users" WHERE ("users"."age" >= $1)`, int32(18))

	check(t,
		db.Select().From(users).Where(nil, userAge.Gte(18)).Where(nil).Where(userName.Ne("")),
		`SELECT * FROM "users" WHERE ("users"."age" >= $1) AND ("users"."name" <> $2)`,
		int32(18), "")
}

func TestHavingDropsNilPredicates(t *testing.T) {
	db := pg.New(nil)
	check(t,
		db.Select(userName).From(users).GroupBy(userName).Having(nil),
		`SELECT "users"."name" FROM "users" GROUP BY "users"."name"`)
	check(t,
		db.Select(userName).From(users).GroupBy(userName).Having(nil, pg.Gt(pg.CountAll(), 1)),
		`SELECT "users"."name" FROM "users" GROUP BY "users"."name" HAVING (count(*) > $1)`, 1)
}

func TestUpdateAndDeleteWhereDropNilPredicates(t *testing.T) {
	db := pg.New(nil)
	check(t,
		db.Update(users).Set(userName.Val("x")).Where(nil, userAge.Gte(18), nil),
		`UPDATE "users" SET "name" = $1 WHERE ("users"."age" >= $2)`, "x", int32(18))
	check(t,
		db.Delete(users).Where(nil, userAge.Gte(18), nil),
		`DELETE FROM "users" WHERE ("users"."age" >= $1)`, int32(18))
}

// A join with no condition is the same empty conjunction, rendered
// where the grammar demands one. Before, it emitted "ON" followed by
// nothing — SQL no server will parse.
func TestJoinOnNilRendersTheIdentity(t *testing.T) {
	db := pg.New(nil)
	check(t,
		db.Select(userID).From(users).LeftJoin(posts, nil),
		`SELECT "users"."id" FROM "users" LEFT JOIN "posts" ON TRUE`)
}
