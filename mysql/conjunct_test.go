package mysql_test

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/mysql"
)

// A conjunct that can reach past its own position, and the tenant guard
// that ends up in the statement binding nothing.
//
// Every case here is a predicate the caller wrote, AND-ed with a guard
// drops added on the statement's own account. The bug is not that the
// guard is missing — it is present in every one of these strings — but
// that without bracketing it applies to half the clause, or to none of
// it.

// The canonical shape: OR binds looser than AND, so a raw predicate
// whose top level is an OR reassociates the whole clause and
// "(D AND a) OR (b AND T)" returns every tenant's rows matching the
// first half.
func TestARawOrIsBracketedInTheWhereClause(t *testing.T) {
	posts, _, _ := scopedTable("posts")
	db := mysql.New(dropstest.New())

	sql, _ := mustCtxSQL(t, db.Select().From(posts).Where(drops.Raw("a = 1 OR b = 2")))
	wantText(t, sql, "SELECT * FROM `posts` WHERE (a = 1 OR b = 2) AND (`posts`.`tenantId` = ?)")
}

// The same shape in an ON clause, which is a WHERE clause in a
// different place — and the place a LEFT JOIN's tenant guard lands.
func TestARawOrIsBracketedInTheOnClause(t *testing.T) {
	comments, _ := plainTable("comments")
	posts, _, _ := scopedTable("posts")
	db := mysql.New(dropstest.New())

	sql, _ := mustCtxSQL(t, db.Select().From(comments).
		LeftJoin(posts, drops.Raw("a = 1 OR b = 2")))
	if !strings.Contains(sql, "ON ((a = 1 OR b = 2) AND (`posts`.`tenantId` = ?))") {
		t.Errorf("the ON clause reassociated: %s", sql)
	}
}

// And in the connectives, which are clauses in miniature: And used to
// join its operands with a bare separator, so the guard landed inside
// the caller's OR.
func TestAndBracketsAConjunctThatEscapes(t *testing.T) {
	guard := drops.Raw("tenant = 1")
	got, _ := render(mysql.And(drops.Raw("a = 1 OR b = 2"), guard))
	wantText(t, got, "((a = 1 OR b = 2) AND tenant = 1)")
}

// NOT binds tighter than every connective a caller can put inside it,
// so an unbracketed operand is negated in half and left standing in the
// other.
func TestNotBracketsAnOperandThatEscapes(t *testing.T) {
	got, _ := render(mysql.Not(drops.Raw("a = 1 OR b = 2")))
	wantText(t, got, "(NOT (a = 1 OR b = 2))")
}

// The tokens that can reach outside a conjunct are a dialect question,
// and MySQL's list is longer than PostgreSQL's or SQLite's: "||" is OR
// outside ANSI mode, XOR sits between OR and AND in the precedence
// table, and "#" opens a comment that swallows the rest of the line.
func TestMySQLsOwnEscapingTokensAreBracketed(t *testing.T) {
	posts, _, _ := scopedTable("posts")
	db := mysql.New(dropstest.New())

	escapes := []string{
		"a = 1 OR b = 2",
		"a = 1 or b = 2",
		"a = 1 || b = 2",
		"a = 1 XOR b = 2",
		"a = 1 #",
		"a = 1 --",
		"a = 1 /*",
		"a = 1;",
	}
	for _, raw := range escapes {
		sql, _ := mustCtxSQL(t, db.Select().From(posts).Where(drops.Raw(raw)))
		if !strings.Contains(sql, "("+raw+")") {
			t.Errorf("%q was not bracketed: %s", raw, sql)
		}
	}
}

// And the ones that cannot: a predicate that already brackets itself,
// an operator that binds tighter than AND, a keyword that only looks
// like one. Bracketing these would double the parentheses in every
// statement drops has ever generated.
func TestPredicatesThatBindThemselvesAreLeftAlone(t *testing.T) {
	posts, _, _ := scopedTable("posts")
	db := mysql.New(dropstest.New())

	safe := []string{
		"(a = 1 OR b = 2)",
		"a = 1 && b = 2",
		"a | b = 2",
		"a = 1 AND b = 2",
		"ORDER = 1",
		"a = 'x OR y'",
		"a = `or`",
		"a = 'it''s OR not'",
		"a = 'back\\\\slash OR x'",
	}
	for _, raw := range safe {
		sql, _ := mustCtxSQL(t, db.Select().From(posts).Where(drops.Raw(raw)))
		if !strings.Contains(sql, "WHERE "+raw+" AND ") {
			t.Errorf("%q was bracketed and should not have been: %s", raw, sql)
		}
	}
}

// A fragment whose parentheses do not balance is reported safe on
// purpose. Bracketing "a = 1) OR (1 = 1" would CLOSE the injected
// parenthesis and turn a statement the server refuses into a working,
// unscoped OR.
func TestAnUnbalancedFragmentIsNotRepaired(t *testing.T) {
	posts, _, _ := scopedTable("posts")
	db := mysql.New(dropstest.New())

	sql, _ := mustCtxSQL(t, db.Select().From(posts).Where(drops.Raw("a = 1) OR (1 = 1")))
	if strings.Contains(sql, "(a = 1) OR (1 = 1)") {
		t.Errorf("bracketing repaired an injection into valid SQL: %s", sql)
	}
}

// A lone conjunct is the whole clause: there is nothing on either side
// of it to reassociate, so it is written as the caller wrote it and a
// query log does not change.
func TestALoneConjunctIsNotBracketed(t *testing.T) {
	plain, _ := plainTable("plain")
	db := mysql.New(dropstest.New())

	sql, _ := db.Select().From(plain).Where(drops.Raw("a = 1 OR b = 2")).ToSQL()
	wantText(t, sql, "SELECT * FROM `plain` WHERE a = 1 OR b = 2")
}
