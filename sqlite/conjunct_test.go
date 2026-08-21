package sqlite_test

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/sqlite"
)

// Conjunctions bracket any conjunct that can reach outside itself.
//
// The failure this defends against is the nastiest one in the package,
// because the guard is present in the SQL: OR binds looser than AND, so
// a caller's raw predicate whose top level is an OR reassociates the
// whole clause and
//
//	(D AND a = 1) OR (b = 2 AND tenantId = ?)
//
// returns every row matching the first half, for every tenant. A review
// that looks for the tenant predicate finds it and passes.

// A raw predicate with a depth-0 OR is bracketed, so the guards on
// either side of it keep binding what they were written to bind.
func TestARawOrIsBracketedInAWhereClause(t *testing.T) {
	posts, _, _ := scopedTable("cj_posts")
	db := sqlite.New(nil)

	sel := db.Select().From(posts).Where(drops.Raw("a = 1 OR b = 2"))
	sql, args := renderCtx(t, sel, scopeCtx())
	checkSQL(t, sql,
		`SELECT * FROM "cj_posts" WHERE (a = 1 OR b = 2) AND ("cj_posts"."tenantId" = ?)`,
		args, []any{scopeTenant})
}

// The same rule inside a connective: And joined its operands with a
// bare separator, so And(raw-or, guard) reassociated exactly as the
// WHERE clause did.
func TestARawOrIsBracketedInsideAndAndOr(t *testing.T) {
	guard := drops.Raw("tenantId = 1")
	tests := []struct {
		name string
		expr drops.Expression
		want string
	}{
		{"And", sqlite.And(drops.Raw("a = 1 OR b = 2"), guard),
			`((a = 1 OR b = 2) AND tenantId = 1)`},
		{"Or", sqlite.Or(drops.Raw("a = 1 OR b = 2"), guard),
			`((a = 1 OR b = 2) OR tenantId = 1)`},
		{"Not", sqlite.Not(drops.Raw("a = 1 OR b = 2")),
			`(NOT (a = 1 OR b = 2))`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := sqlite.ToSQL(tc.expr); got != tc.want {
				t.Errorf("got = %v, want %v", got, tc.want)
			}
		})
	}
}

// A comment opener swallows whatever follows it, so a conjunct
// containing one has to be bracketed too — otherwise "a = 1 --" drops
// every later conjunct, tenant guard included.
func TestACommentBearingConjunctIsBracketed(t *testing.T) {
	posts, _, _ := scopedTable("cc_posts")
	db := sqlite.New(nil)

	for _, raw := range []string{"a = 1 --", "a = 1 /* x", "a = 1; DROP TABLE t"} {
		sel := db.Select().From(posts).Where(drops.Raw(raw))
		sql, _ := renderCtx(t, sel, scopeCtx())
		if !strings.Contains(sql, "("+raw+")") {
			t.Errorf("%q was not bracketed:\n%s", raw, sql)
		}
	}
}

// A lone conjunct is the whole clause: there is nothing on either side
// to reassociate, so it renders as the caller wrote it. Bracketing it
// would change every single-predicate statement drops has ever
// generated for no gain at all.
func TestALoneConjunctIsNotBracketed(t *testing.T) {
	plain, _ := plainTable("cl_plain")
	db := sqlite.New(nil)

	sql, _ := db.Select().From(plain).Where(drops.Raw("a = 1 OR b = 2")).ToSQL()
	if got, want := sql, `SELECT * FROM "cl_plain" WHERE a = 1 OR b = 2`; got != want {
		t.Errorf("got = %v, want %v", got, want)
	}
}

// An OR inside a string literal or a quoted identifier is text, not an
// operator, and an OR inside parentheses cannot reach out of them.
// Bracketing either would be noise in every query log that has one.
func TestAnEnclosedOrIsNotBracketed(t *testing.T) {
	posts, _, _ := scopedTable("ce_posts")
	db := sqlite.New(nil)

	for _, raw := range []string{
		`a = 'x OR y'`,
		`"a OR b" = 1`,
		"`a OR b` = 1",
		`[a OR b] = 1`,
		`(a = 1 OR b = 2)`,
		`a = 1 AND b FOR c`,
	} {
		sel := db.Select().From(posts).Where(drops.Raw(raw))
		sql, _ := renderCtx(t, sel, scopeCtx())
		if strings.Contains(sql, "("+raw+")") {
			t.Errorf("%q was bracketed and did not need to be:\n%s", raw, sql)
		}
	}
}

// A fragment whose parentheses do not balance is left alone, on
// purpose. Bracketing "a = 1) OR (1 = 1" would CLOSE the injected
// parenthesis and turn a statement SQLite refuses into a working,
// unscoped OR. Left alone it stays a syntax error, which is the
// fail-closed answer.
func TestAnUnbalancedFragmentIsLeftAsASyntaxError(t *testing.T) {
	posts, _, _ := scopedTable("cu_posts")
	db := sqlite.New(nil)

	raw := `a = 1) OR (1 = 1`
	sel := db.Select().From(posts).Where(drops.Raw(raw))
	sql, _ := renderCtx(t, sel, scopeCtx())
	if strings.Contains(sql, "("+raw+")") {
		t.Errorf("the unbalanced fragment was repaired into a working OR:\n%s", sql)
	}
}

// An ON clause is a WHERE clause in a different place, and a join
// condition whose top level is an OR reassociates the filters that
// follow it there just the same.
func TestARawOrInAnOnClauseIsBracketed(t *testing.T) {
	posts, _, _ := scopedTable("co_posts")
	plain, plainID := plainTable("co_plain")
	db := sqlite.New(nil)

	sel := db.Select(plainID).From(plain).
		LeftJoin(posts, drops.Raw("a = 1 OR b = 2"))
	sql, args := renderCtx(t, sel, scopeCtx())
	checkSQL(t, sql,
		`SELECT "co_plain"."id" FROM "co_plain" LEFT JOIN "co_posts" `+
			`ON ((a = 1 OR b = 2) AND ("co_posts"."tenantId" = ?))`,
		args, []any{scopeTenant})
}

// SoftDeleteMixin registers its guard once, not twice. It used to
// register one in SoftDelete and a second of its own, so every
// statement against a mixin-soft-deleted table carried the predicate
// twice.
func TestTheSoftDeleteMixinRegistersItsGuardOnce(t *testing.T) {
	posts := sqlite.NewTable("sm_posts")
	sqlite.Add(posts, sqlite.BigInt("id").PrimaryKey())
	sqlite.ApplyMixins(posts, &sqlite.SoftDeleteMixin{})

	db := sqlite.New(nil)
	sql, _ := db.Select().From(posts).ToSQL()
	if n := strings.Count(sql, `"sm_posts"."deletedAt" IS NULL`); n != 1 {
		t.Errorf("the guard appears %d times: %s", n, sql)
	}
	if got, want := len(posts.DefaultFilters()), 1; got != want {
		t.Errorf("default filters = %d, want %d", got, want)
	}
}
