package clickhouse_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bernardoforcillo/drops/clickhouse"
)

// [InsertBuilder.Unscoped] now says in writing that it stops at the
// edge of the statement it was said on. It has always behaved that
// way, but the sentence was in pg's and sqlite's doc and not in this
// one, so the boundary here was something a reviewer had to go and
// assert for themselves rather than read — and an escape hatch whose
// blast radius is undocumented is one that gets assumed wider or
// narrower than it is.
//
// The wider assumption is the dangerous one, and it is worse in this
// dialect than in the other three. An INSERT here is how a
// materialised view's source is fed and how a backfill replays a
// month, so the subquery bound as a value is doing arithmetic over
// somebody's data, at a row count where nobody eyeballs the result. A
// caller who believes Unscoped on the INSERT widens the subquery too
// will read this statement as spanning tenants; it spans one, and the
// rows it writes are computed from a slice of what the author thought.

// TestInsertUnscopedLeavesInnerStatementsScoped pins the boundary of
// the escape hatch. The outer INSERT gives up its stamp — there is no
// tenantId in its column list — and the subquery bound as its value
// keeps its own predicate in the same statement.
//
// The SQL is compared exactly rather than searched for a substring,
// because the defect this is about is a predicate that is present and
// powerless, and "contains tenantId" cannot tell that apart from one
// in the wrong clause.
func TestInsertUnscopedLeavesInnerStatementsScoped(t *testing.T) {
	db := clickhouse.New(nil)
	sub := db.Select(scSessionID).From(scSessions).Limit(1)

	sql, args, err := db.Insert(scHits).
		Row(scHitID.Expr(clickhouse.Subquery(sub))).
		Unscoped().ToSQLCtx(tenantCtx("acme"))
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}

	const want = `INSERT INTO "hits" ("id") VALUES ` +
		`((SELECT "sessions"."id" FROM "sessions" WHERE ("sessions"."tenantId" = ?) LIMIT ?))`
	if sql != want {
		t.Errorf("got  %s\nwant %s", sql, want)
	}
	checkArgs(t, args, "acme", int64(1))
}

// And the refusal travels with the predicate. An inner statement that
// cannot say which tenant it means stops the whole INSERT, rather than
// the outer Unscoped standing in as an answer for it: the caller gave
// up the axis on the statement they were writing, not on the one they
// were reading from.
func TestInsertUnscopedDoesNotAnswerForAnInnerStatement(t *testing.T) {
	db := clickhouse.New(nil)
	sub := db.Select(scSessionID).From(scSessions).Limit(1)

	_, _, err := db.Insert(scHits).
		Row(scHitID.Expr(clickhouse.Subquery(sub))).
		Unscoped().ToSQLCtx(context.Background())

	if !errors.Is(err, clickhouse.ErrTenantMissing) {
		t.Fatalf("err = %v, want ErrTenantMissing", err)
	}
}
