package pg_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// The shapes in which the scoping predicate was present and powerless.
//
// scopereach_test.go covers the statements the tenant predicate never
// reached at all. These are the ones where it reached the SQL and then
// did not restrict anything: an operator precedence that reassociated
// it away, a subquery that carried none of its own, a join kind that
// dropped the preserved side's rows instead of the other tenant's, and
// a set operation that lost half of what it was asked to render.
//
// Every assertion is on rendered SQL. A round trip against a
// single-tenant fixture passes through all four.

// ----------------------------------------------------------------------
// OPEN-5: a predicate that binds looser than AND
// ----------------------------------------------------------------------

// The conjuncts of a WHERE clause used to be joined with a bare " AND ",
// so a caller predicate whose own top level is an OR reassociated the
// whole clause: "(deleted IS NULL) AND a = 1 OR b = 2 AND (tenant = $1)"
// parses as "((deleted IS NULL) AND a = 1) OR (b = 2 AND (tenant = $1))"
// and every row matching the first half comes back for every tenant.
// The guard is in the statement, which is why counting predicates never
// caught it.
func TestRawPredicateCannotReassociateTheGuard(t *testing.T) {
	users, posts := reachTable("pr_users"), reachTable("pr_posts")
	db := pg.New(nil)

	tests := []struct {
		name string
		sel  func() *pg.SelectBuilder
		want string
	}{
		{
			// The one that leaks: without the parentheses the tenant
			// predicate binds to "b = 2" alone.
			name: "top-level OR in the WHERE clause",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).Where(drops.Raw(`a = 1 OR b = 2`))
			},
			want: `SELECT * FROM "pr_users" WHERE ("pr_users"."deletedAt" IS NULL) ` +
				`AND (a = 1 OR b = 2) AND ("pr_users"."tenantId" = $1)`,
		},
		{
			// A predicate that cannot reassociate anything is written
			// exactly as it always was: the parentheses are added where
			// they change the parse, not everywhere.
			name: "a predicate that binds tighter is left alone",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).Where(drops.Raw(`a = 1`), pg.Exists(drops.Raw(`SELECT 1 FROM t WHERE x OR y`)))
			},
			want: `SELECT * FROM "pr_users" WHERE ("pr_users"."deletedAt" IS NULL) ` +
				`AND a = 1 AND EXISTS (SELECT 1 FROM t WHERE x OR y) AND ("pr_users"."tenantId" = $1)`,
		},
		{
			name: "top-level OR in the HAVING clause",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).
					GroupBy(users.Col("id")).
					Having(drops.Raw(`count(*) > 1 OR TRUE`), drops.Raw(`count(*) < 9`))
			},
			want: `SELECT * FROM "pr_users" WHERE ("pr_users"."deletedAt" IS NULL) ` +
				`AND ("pr_users"."tenantId" = $1) GROUP BY "pr_users"."id" ` +
				`HAVING (count(*) > 1 OR TRUE) AND count(*) < 9`,
		},
		{
			// The ON clause carries the joined table's guard for a LEFT
			// JOIN, so it reassociates the same way the WHERE clause did.
			name: "top-level OR in an ON clause",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).LeftJoin(posts, drops.Raw(`x = 1 OR y = 2`))
			},
			want: `SELECT * FROM "pr_users" LEFT JOIN "pr_posts" ` +
				`ON ((x = 1 OR y = 2) AND ("pr_posts"."deletedAt" IS NULL) AND ("pr_posts"."tenantId" = $1)) ` +
				`WHERE ("pr_users"."deletedAt" IS NULL) AND ("pr_users"."tenantId" = $2)`,
		},
		{
			// A line comment ends the conjunction just as an OR
			// reassociates it — everything after it, tenant guard
			// included, stops being part of the statement. Parenthesised,
			// the comment eats the closing paren instead and PostgreSQL
			// rejects the statement.
			name: "a line comment cannot swallow the guard",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).Where(drops.Raw(`a = 1 --`))
			},
			want: `SELECT * FROM "pr_users" WHERE ("pr_users"."deletedAt" IS NULL) ` +
				`AND (a = 1 --) AND ("pr_users"."tenantId" = $1)`,
		},
		{
			// A fragment whose parentheses do not balance is not an
			// expression, and wrapping it would close the injected paren
			// and turn a syntax error into a working OR. It is left as
			// written, so PostgreSQL refuses the statement.
			name: "an unbalanced fragment is not repaired",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).Where(drops.Raw(`a = 1) OR (1 = 1`))
			},
			want: `SELECT * FROM "pr_users" WHERE ("pr_users"."deletedAt" IS NULL) ` +
				`AND a = 1) OR (1 = 1 AND ("pr_users"."tenantId" = $1)`,
		},
		{
			// Parentheses inside a string literal are text, not
			// structure, and an OR inside one is not an operator.
			name: "a literal is scanned as text",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).Where(drops.Raw(`a = ') OR ('`))
			},
			want: `SELECT * FROM "pr_users" WHERE ("pr_users"."deletedAt" IS NULL) ` +
				`AND a = ') OR (' AND ("pr_users"."tenantId" = $1)`,
		},
		{
			// A fragment that ends inside a string literal cannot be
			// read as an expression, and an unread fragment is
			// bracketed rather than trusted. Here the literal is an
			// E-string whose backslash escapes the closing quote, which
			// the scan deliberately does not try to interpret.
			name: "an unreadable literal is bracketed rather than trusted",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).Where(drops.Raw(`a = E'\''`))
			},
			want: `SELECT * FROM "pr_users" WHERE ("pr_users"."deletedAt" IS NULL) ` +
				`AND (a = E'\'') AND ("pr_users"."tenantId" = $1)`,
		},
		{
			// Nothing to reassociate with: a lone predicate is the whole
			// clause and keeps the shape the caller wrote.
			name: "a lone predicate is left alone",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).Unscoped().Where(drops.Raw(`a = 1 OR b = 2`))
			},
			want: `SELECT * FROM "pr_users" WHERE a = 1 OR b = 2`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := tc.sel().ToSQLCtx(reachCtx())
			if err != nil {
				t.Fatalf("ToSQLCtx: %v", err)
			}
			if got != tc.want {
				t.Errorf("got = %v, want %v", got, tc.want)
			}
		})
	}
}

// ----------------------------------------------------------------------
// OPEN-4: a subquery is a statement and is scoped like one
// ----------------------------------------------------------------------

// A subquery reached through EXISTS, a scalar subquery, ANY/ALL or
// AsSubquery used to render through WriteSQL, which has no ctx: the
// body carried its DefaultFilters and none of its context filters, so
// EXISTS (SELECT ... FROM posts) saw every tenant's posts while the
// outer statement was scoped. It was documented, and a documented
// fail-open in the one feature sold as fail-closed is still a leak.
func TestSubqueryBodyCarriesTheTenantAxis(t *testing.T) {
	users, posts := reachTable("sq_users"), reachTable("sq_posts")
	db := pg.New(nil)
	sub := func() *pg.SelectBuilder { return db.Select(posts.Col("id")).From(posts) }
	subSQL := `SELECT "sq_posts"."id" FROM "sq_posts" ` +
		`WHERE ("sq_posts"."deletedAt" IS NULL) AND ("sq_posts"."tenantId" = $1)`

	tests := []struct {
		name string
		sel  func() *pg.SelectBuilder
		want string
	}{
		{
			name: "EXISTS",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).Where(pg.Exists(sub()))
			},
			want: `SELECT * FROM "sq_users" WHERE ("sq_users"."deletedAt" IS NULL) ` +
				`AND EXISTS (` + subSQL + `) AND ("sq_users"."tenantId" = $2)`,
		},
		{
			name: "NOT EXISTS",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).Where(pg.NotExists(sub()))
			},
			want: `SELECT * FROM "sq_users" WHERE ("sq_users"."deletedAt" IS NULL) ` +
				`AND NOT EXISTS (` + subSQL + `) AND ("sq_users"."tenantId" = $2)`,
		},
		{
			name: "ANY",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).Where(pg.AnySub(users.Col("id"), sub()))
			},
			want: `SELECT * FROM "sq_users" WHERE ("sq_users"."deletedAt" IS NULL) ` +
				`AND ("sq_users"."id" = ANY (` + subSQL + `)) AND ("sq_users"."tenantId" = $2)`,
		},
		{
			name: "scalar subquery in the select list",
			sel: func() *pg.SelectBuilder {
				return db.Select(pg.Subquery(sub())).From(users)
			},
			want: `SELECT (` + subSQL + `) FROM "sq_users" ` +
				`WHERE ("sq_users"."deletedAt" IS NULL) AND ("sq_users"."tenantId" = $2)`,
		},
		{
			name: "AsSubquery in the FROM clause",
			sel: func() *pg.SelectBuilder {
				return db.Select().FromExpr(sub().AsSubquery("p"))
			},
			want: `SELECT * FROM (` + subSQL + `) AS "p"`,
		},
		{
			name: "a subquery in an ON clause",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).Unscoped().
					Join(posts, pg.Exists(sub()))
			},
			want: `SELECT * FROM "sq_users" INNER JOIN "sq_posts" ON EXISTS (` + subSQL + `)`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := tc.sel().ToSQLCtx(reachCtx())
			if err != nil {
				t.Fatalf("ToSQLCtx: %v", err)
			}
			if got != tc.want {
				t.Errorf("got = %v, want %v", got, tc.want)
			}
		})
	}
}

// A subquery body that cannot name a tenant fails the whole statement,
// for the reason a CTE body does: the alternative is an inner SELECT
// that spans every tenant feeding an outer one that does not.
func TestSubqueryBodyWithNoTenantRefusesTheStatement(t *testing.T) {
	users, posts := reachTable("sn_users"), reachTable("sn_posts")
	db := pg.New(nil)

	sel := db.Select().From(users).Unscoped().
		Where(pg.Exists(db.Select(posts.Col("id")).From(posts)))
	if _, _, err := sel.ToSQLCtx(context.Background()); !errors.Is(err, pg.ErrTenantMissing) {
		t.Errorf("got = %v, want %v", err, pg.ErrTenantMissing)
	}
}

// Resolving must not double-scope a body the caller already resolved —
// the per-parent-limit rewrite in find.go hands one in deliberately.
// Two tenant predicates bind the same value twice, so the rows are
// right and only the argument list shows it.
func TestAlreadyResolvedSubqueryIsNotScopedTwice(t *testing.T) {
	posts := reachTable("sd_posts")
	db := pg.New(nil)

	inner := db.Select().From(posts)
	outer := db.Select().FromExpr(inner.AsSubquery("p"))
	got, args, err := outer.ToSQLCtx(reachCtx())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	want := `SELECT * FROM (SELECT * FROM "sd_posts" ` +
		`WHERE ("sd_posts"."deletedAt" IS NULL) AND ("sd_posts"."tenantId" = $1)) AS "p"`
	if got != want {
		t.Errorf("got = %v, want %v", got, want)
	}
	if len(args) != 1 {
		t.Errorf("got = %v, want one bound tenant", args)
	}

	// The same builder, executed again: the resolution must not have
	// been written back into it.
	again, _, err := outer.ToSQLCtx(reachCtx())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	if again != want {
		t.Errorf("got = %v, want %v", again, want)
	}
}

// ----------------------------------------------------------------------
// OPEN-3: the FROM table is the nullable side of a RIGHT JOIN
// ----------------------------------------------------------------------

// The joined table's placement was worked out per join kind; the FROM
// table's predicates always landed in the WHERE clause. Under a RIGHT
// JOIN the FROM table is the nullable side, so its tenant guard is
// false for every unmatched row of the preserved table and the join
// degenerates to an INNER JOIN — the exact failure the LEFT JOIN half
// exists to prevent, mirrored.
func TestFromTableFiltersFollowTheJoinKind(t *testing.T) {
	users, posts := reachTable("fj_users"), reachTable("fj_posts")
	plain := pg.NewTable("fj_plain")
	pg.Add(plain, pg.BigSerial("id").PrimaryKey())
	db := pg.New(nil)
	on := pg.Eq(users.Col("id"), posts.Col("id"))
	onPlain := pg.Eq(users.Col("id"), plain.Col("id"))

	tests := []struct {
		name string
		sel  func() *pg.SelectBuilder
		want string
	}{
		{
			// Nothing preserved on the right: the WHERE clause is
			// where the FROM table's predicates have always belonged.
			name: "inner join keeps them in the WHERE clause",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).Join(plain, onPlain)
			},
			want: `SELECT * FROM "fj_users" INNER JOIN "fj_plain" ON ("fj_users"."id" = "fj_plain"."id") ` +
				`WHERE ("fj_users"."deletedAt" IS NULL) AND ("fj_users"."tenantId" = $1)`,
		},
		{
			name: "right join moves them into the ON clause",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).RightJoin(plain, onPlain)
			},
			want: `SELECT * FROM "fj_users" RIGHT JOIN "fj_plain" ` +
				`ON (("fj_users"."id" = "fj_plain"."id") AND ("fj_users"."deletedAt" IS NULL) ` +
				`AND ("fj_users"."tenantId" = $1))`,
		},
		{
			// The FROM table enters the join tree at the first RIGHT
			// JOIN: filtering it there restricts it before the
			// preserved side is added, and every later join sees the
			// restricted relation.
			name: "the first right join is the one that carries them",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).
					LeftJoin(plain, onPlain).
					RightJoin(posts, on)
			},
			want: `SELECT * FROM "fj_users" LEFT JOIN "fj_plain" ON ("fj_users"."id" = "fj_plain"."id") ` +
				`RIGHT JOIN "fj_posts" ON (("fj_users"."id" = "fj_posts"."id") ` +
				`AND ("fj_users"."deletedAt" IS NULL) AND ("fj_users"."tenantId" = $1)) ` +
				`WHERE ("fj_posts"."deletedAt" IS NULL) AND ("fj_posts"."tenantId" = $2)`,
		},
		{
			// Unscoped is statement-wide, so there is nothing to place.
			name: "unscoped right join carries nothing",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).RightJoin(plain, onPlain).Unscoped()
			},
			want: `SELECT * FROM "fj_users" RIGHT JOIN "fj_plain" ON ("fj_users"."id" = "fj_plain"."id")`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := tc.sel().ToSQLCtx(reachCtx())
			if err != nil {
				t.Fatalf("ToSQLCtx: %v", err)
			}
			if got != tc.want {
				t.Errorf("got = %v, want %v", got, tc.want)
			}
		})
	}
}

// A FULL JOIN preserves both sides, so the FROM table's own context
// filters have nowhere correct to go either: the WHERE clause drops the
// full-joined table's unmatched rows, the ON clause lets the FROM
// table's unmatched rows through unfiltered. Same refusal as for the
// joined side.
func TestFullJoinWithScopedFromTableIsRefused(t *testing.T) {
	users := reachTable("ff_users")
	plain := pg.NewTable("ff_plain")
	pg.Add(plain, pg.BigSerial("id").PrimaryKey())
	db := pg.New(nil)

	_, _, err := db.Select().From(users).
		FullJoin(plain, pg.Eq(users.Col("id"), plain.Col("id"))).
		ToSQLCtx(reachCtx())
	if !errors.Is(err, pg.ErrFullJoinScoped) {
		t.Errorf("got = %v, want %v", err, pg.ErrFullJoinScoped)
	}
}

// ----------------------------------------------------------------------
// OPEN-6: a set operation's right operand is a statement too
// ----------------------------------------------------------------------

// WriteSQL rendered the right operand through writeCore, which writes
// neither its own set operations nor its CTEs. A.Union(B.Union(C))
// therefore sent "A UNION B" — C's rows silently missing — and a CTE on
// the right operand vanished, leaving the operand referring to a
// relation the statement no longer declared.
func TestSetOpRightOperandKeepsItsOwnStatement(t *testing.T) {
	first, second, third := reachTable("so_a"), reachTable("so_b"), reachTable("so_c")
	db := pg.New(nil)
	core := func(tbl *pg.Table, arg string) string {
		return `SELECT * FROM "` + tbl.Name() + `" WHERE ("` + tbl.Name() + `"."deletedAt" IS NULL) ` +
			`AND ("` + tbl.Name() + `"."tenantId" = ` + arg + `)`
	}

	tests := []struct {
		name string
		sel  func() *pg.SelectBuilder
		want string
	}{
		{
			// Parenthesised, not flattened: "A EXCEPT B UNION C" is
			// left-associative and means something else entirely.
			name: "nested set operations are rendered",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(first).Union(db.Select().From(second).Union(db.Select().From(third)))
			},
			want: core(first, "$1") + ` UNION (` + core(second, "$2") + ` UNION ` + core(third, "$3") + `)`,
		},
		{
			name: "a nested EXCEPT keeps its grouping",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(first).Except(db.Select().From(second).Union(db.Select().From(third)))
			},
			want: core(first, "$1") + ` EXCEPT (` + core(second, "$2") + ` UNION ` + core(third, "$3") + `)`,
		},
		{
			// A trailing LIMIT belongs to the whole set operation, so
			// an operand written with one of its own has to keep it
			// inside parentheses or the union takes it over: five rows
			// of A ∪ B instead of all of A and five of B.
			name: "an operand's row bounds stay its own",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(first).Union(db.Select().From(second).Limit(5))
			},
			want: core(first, "$1") + ` UNION (` + core(second, "$2") + ` LIMIT $3)`,
		},
		{
			// One WITH clause per statement, so the operand's CTE is
			// hoisted to the front rather than dropped.
			name: "an operand's CTE is hoisted to the leading WITH",
			sel: func() *pg.SelectBuilder {
				right := db.Select().With(pg.CTEDef("r", db.Select().From(third))).FromExpr(drops.Raw(`"r"`))
				return db.Select().From(first).Union(right)
			},
			want: `WITH "r" AS (` + core(third, "$1") + `) ` + core(first, "$2") + ` UNION SELECT * FROM "r"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := tc.sel().ToSQLCtx(reachCtx())
			if err != nil {
				t.Fatalf("ToSQLCtx: %v", err)
			}
			if got != tc.want {
				t.Errorf("got = %v, want %v", got, tc.want)
			}
		})
	}
}

// Two different CTEs with one name cannot both be hoisted into the
// single WITH clause the statement is allowed. The statement is refused
// rather than sent with a duplicate declaration.
func TestSetOpCTENameConflictIsRefused(t *testing.T) {
	tb, tc := reachTable("sc_b"), reachTable("sc_c")
	db := pg.New(nil)

	left := db.Select().With(pg.CTEDef("dup", db.Select().From(tb))).FromExpr(drops.Raw(`"dup"`))
	right := db.Select().With(pg.CTEDef("dup", db.Select().From(tc))).FromExpr(drops.Raw(`"dup"`))

	_, _, err := left.Union(right).ToSQLCtx(reachCtx())
	if !errors.Is(err, pg.ErrCTENameConflict) {
		t.Errorf("got = %v, want %v", err, pg.ErrCTENameConflict)
	}
}

// The same CTE value attached to both operands is one declaration, not
// a conflict: hoisting it twice would declare a name twice for no
// reason.
func TestSetOpSharedCTEIsHoistedOnce(t *testing.T) {
	tc := reachTable("sh_c")
	db := pg.New(nil)
	shared := pg.CTEDef("shared", db.Select().From(tc))

	left := db.Select().With(shared).FromExpr(drops.Raw(`"shared"`))
	right := db.Select().With(shared).FromExpr(drops.Raw(`"shared"`))

	got, _, err := left.Union(right).ToSQLCtx(reachCtx())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	want := `WITH "shared" AS (SELECT * FROM "sh_c" WHERE ("sh_c"."deletedAt" IS NULL) ` +
		`AND ("sh_c"."tenantId" = $1)) SELECT * FROM "shared" UNION SELECT * FROM "shared"`
	if got != want {
		t.Errorf("got = %v, want %v", got, want)
	}
}
