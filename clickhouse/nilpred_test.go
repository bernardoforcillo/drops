package clickhouse_test

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/clickhouse"
)

// A nil predicate is how a conditional filter says "no restriction",
// and it has to mean the same thing here as in every other dialect —
// otherwise the accumulate-into-a-slice boilerplate comes back for
// anyone writing against more than one engine. ClickHouse spells the
// identities in lower case, which is the only difference.

func TestAndOrDropNilPredicates(t *testing.T) {
	x := eventUser.Eq(42)
	y := eventKind.Eq("click")

	checkExpr(t, clickhouse.And(nil, x, nil), `("events"."userId" = ?)`, uint64(42))
	checkExpr(t, clickhouse.Or(nil, x, nil), `("events"."userId" = ?)`, uint64(42))
	checkExpr(t, clickhouse.And(nil, x, nil, y),
		`(("events"."userId" = ?) AND ("events"."kind" = ?))`, uint64(42), "click")
	checkExpr(t, clickhouse.Or(x, nil, y),
		`(("events"."userId" = ?) OR ("events"."kind" = ?))`, uint64(42), "click")

	checkExpr(t, clickhouse.And(nil, nil), `true`)
	checkExpr(t, clickhouse.Or(nil, nil), `false`)
	checkExpr(t, clickhouse.And(), `true`)
	checkExpr(t, clickhouse.Or(), `false`)

	checkExpr(t, clickhouse.Not(nil), `(NOT true)`)
}

func TestWherePrewhereHavingDropNilPredicates(t *testing.T) {
	db := clickhouse.New(nil)

	got, _ := db.Select(eventID).From(events).Where(nil).ToSQL()
	if strings.Contains(got, "WHERE") {
		t.Errorf("an all-nil Where left a clause behind: %s", got)
	}
	got, _ = db.Select(eventID).From(events).Prewhere(nil).ToSQL()
	if strings.Contains(got, "PREWHERE") {
		t.Errorf("an all-nil Prewhere left a clause behind: %s", got)
	}
	got, _ = db.Select(eventKind).From(events).GroupBy(eventKind).Having(nil).ToSQL()
	if strings.Contains(got, "HAVING") {
		t.Errorf("an all-nil Having left a clause behind: %s", got)
	}

	got, _ = db.Select(eventID).From(events).Where(nil, eventUser.Eq(42), nil).ToSQL()
	want := `SELECT "events"."id" FROM "events" WHERE ("events"."userId" = ?)`
	if got != want {
		t.Errorf("sql\n  got:  %s\n  want: %s", got, want)
	}
}

func TestJoinOnNilRendersTheIdentity(t *testing.T) {
	users := clickhouse.NewTable("nilpred_users")
	clickhouse.Add(users, clickhouse.UInt64("id"))

	db := clickhouse.New(nil)
	got, _ := db.Select(eventID).From(events).Join(users, nil).ToSQL()
	if !strings.HasSuffix(got, `JOIN "nilpred_users" ON true`) {
		t.Errorf("a nil join condition rendered: %s", got)
	}
}
