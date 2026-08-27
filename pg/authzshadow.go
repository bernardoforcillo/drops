package pg

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Proving that a pushed-down authorisation rule filters the same rows
// the rule means.
//
// [Entity.AuthorizeWith] turns a [Guard] into a WHERE predicate and
// AND-s it into every read. That is the right implementation — the
// database does the filtering, one query, no rows crossing the wire
// that the caller may not see — and it has a failure mode that
// nothing catches: the predicate can be *wrong*. A join written the
// wrong way round, a NULL that makes a comparison neither true nor
// false, an OR that widens instead of narrowing. The query returns
// rows, the test asserts it returns rows, and the rows are the wrong
// ones.
//
// InstantDB runs the same query twice for this — once with its
// permission rules pushed into SQL and once with them evaluated
// row by row — and compares. drops has no second evaluator to compare
// against, because a [Guard] *is* the SQL. So the second opinion has
// to come from the test: state the rule again, in Go, over the
// unguarded rows.
//
//	diff, err := pg.CompareGuard(ctx, db, Invoices,
//	    func(inv Invoice) bool {
//	        return inv.CreatedBy == userID || memberOf(userID, inv.OrgID)
//	    },
//	    func(q *pg.EntityQuery[Invoice]) *pg.EntityQuery[Invoice] { return q },
//	)
//	if !diff.Agrees() {
//	    t.Fatalf("guard and rule disagree:\n%s", diff)
//	}
//
// Writing the rule twice is the point rather than a cost. The two
// statements are independent — one in SQL that the database
// optimises, one in Go that a reader can check by eye — and a
// disagreement means one of them is wrong, which is exactly the
// signal that is otherwise missing. Run it over a fixture with the
// awkward rows in it: the NULL org, the row owned by nobody, the
// membership that was revoked.
//
// It reads twice and compares in memory, so it belongs in a test or
// behind a sampling flag, never on a request path.

// GuardDiff is the outcome of a [CompareGuard] run.
type GuardDiff[T any] struct {
	// Leaked are rows the guard's SQL predicate returned that the Go
	// rule denies. This is the direction that matters: each one is a
	// row a user can see and should not.
	Leaked []T

	// Hidden are rows the Go rule allows that the guard's predicate
	// filtered out. Less dangerous and still a bug — usually a NULL
	// comparison, since NULL = anything is NULL and a WHERE drops
	// what it cannot call true.
	Hidden []T

	// Rows is how many rows the unguarded query returned, i.e. how
	// many the comparison actually covered. A run over an empty
	// fixture agrees perfectly and proves nothing, so a test should
	// assert on this too.
	Rows int

	// Allowed is how many of them the Go rule permits.
	Allowed int

	// GuardedDuration and UnguardedDuration are what each read cost.
	// The gap is what the pushdown buys, and a guarded read that is
	// dramatically slower usually means the predicate defeated an
	// index.
	GuardedDuration   time.Duration
	UnguardedDuration time.Duration
}

// Agrees reports whether the pushed-down predicate and the Go rule
// selected exactly the same rows.
func (d GuardDiff[T]) Agrees() bool { return len(d.Leaked) == 0 && len(d.Hidden) == 0 }

// String renders the disagreement for a test failure message.
func (d GuardDiff[T]) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d rows compared, %d allowed by the rule", d.Rows, d.Allowed)
	if len(d.Leaked) > 0 {
		fmt.Fprintf(&b, "\n  %d row(s) the guard returned that the rule denies:", len(d.Leaked))
		for _, r := range d.Leaked {
			fmt.Fprintf(&b, "\n    %+v", r)
		}
	}
	if len(d.Hidden) > 0 {
		fmt.Fprintf(&b, "\n  %d row(s) the rule allows that the guard hid:", len(d.Hidden))
		for _, r := range d.Hidden {
			fmt.Fprintf(&b, "\n    %+v", r)
		}
	}
	if d.Agrees() {
		b.WriteString("\n  no disagreement")
	}
	return b.String()
}

// CompareGuard runs a query twice — once with the entity's [Guard]
// pushed into SQL, once without it — and reports where the predicate
// and the supplied rule disagree.
//
// allows states the authorisation rule over a materialised row. It
// must be a second, independent statement of the same intent: writing
// it in terms of the guard would compare the predicate with itself.
//
// build shapes the query both runs share — the ordering, the extra
// filters, the limit. Pass the identity function to compare every
// row. A LIMIT is a trap here and the harness cannot stop you setting
// one: the two runs would take different rows and disagree for a
// reason that is not a bug.
//
// The subject must already be on ctx (see [WithSubject]); the guarded
// run needs it and fails closed without it, which is the behaviour
// under test.
//
// Rows are matched by primary key, so T must be the entity's own row
// type and the key columns must be populated — which they are for
// anything the entity scanned.
func CompareGuard[T any](
	ctx context.Context,
	db *DB,
	e *Entity[T],
	allows func(T) bool,
	build func(*EntityQuery[T]) *EntityQuery[T],
) (GuardDiff[T], error) {
	var diff GuardDiff[T]
	if e == nil {
		return diff, fmt.Errorf("drops/pg: CompareGuard needs an entity")
	}
	if allows == nil {
		return diff, fmt.Errorf("drops/pg: CompareGuard needs a rule to compare against")
	}
	if e.guard == nil {
		return diff, fmt.Errorf("drops/pg: CompareGuard: entity %q has no guard to compare", e.table.Name())
	}
	if build == nil {
		build = func(q *EntityQuery[T]) *EntityQuery[T] { return q }
	}

	start := time.Now()
	guarded, err := build(e.Query(db)).All(ctx)
	if err != nil {
		return diff, fmt.Errorf("drops/pg: CompareGuard: guarded read: %w", err)
	}
	diff.GuardedDuration = time.Since(start)

	// The unguarded entity is a copy with the guard removed rather
	// than a raw SELECT, so both runs go through the same scanning,
	// tenanting and default-filter machinery. Only the one thing
	// under test differs.
	unguarded := *e
	unguarded.guard = nil
	// The cache would serve one run the other's rows: the cache key
	// is built from the rendered SQL, and the two statements differ,
	// but a stale entry from an earlier subject would still be
	// eligible. A comparison has to read the database.
	unguarded.cache = nil

	start = time.Now()
	all, err := build(unguarded.Query(db)).All(ctx)
	if err != nil {
		return diff, fmt.Errorf("drops/pg: CompareGuard: unguarded read: %w", err)
	}
	diff.UnguardedDuration = time.Since(start)
	diff.Rows = len(all)

	guardedKeys := make(map[string]struct{}, len(guarded))
	for i := range guarded {
		guardedKeys[entityKeyString(e, &guarded[i])] = struct{}{}
	}

	for i := range all {
		row := all[i]
		key := entityKeyString(e, &all[i])
		_, inGuarded := guardedKeys[key]
		permitted := allows(row)
		if permitted {
			diff.Allowed++
		}
		switch {
		case inGuarded && !permitted:
			diff.Leaked = append(diff.Leaked, row)
		case !inGuarded && permitted:
			diff.Hidden = append(diff.Hidden, row)
		}
	}
	return diff, nil
}

// entityKeyString renders a row's primary key as a comparable string.
// The separator is the same unit separator the cache keys use, so a
// composite key ("ab","c") cannot collide with ("a","bc").
func entityKeyString[T any](e *Entity[T], row *T) string {
	var b strings.Builder
	for i, v := range e.pkValuesOf(row) {
		if i > 0 {
			b.WriteByte(0x1f)
		}
		fmt.Fprintf(&b, "%v", v)
	}
	return b.String()
}
