package clickhouse

import "github.com/bernardoforcillo/drops"

// How a conjunction is written, and why it is not just " AND ".
//
// Every clause in this package that AND-s predicates together — a
// PREWHERE, a WHERE, a HAVING, a join's ON, [And] itself — composes
// predicates the caller wrote with predicates drops added on the
// statement's own account: a DefaultFilter, a resolved ContextFilter.
// That composition is only sound if each conjunct binds to itself.

// writeAnd writes a list of predicates joined by AND, without the outer
// parentheses And / Or would emit when used as a sub-expression.
//
// A conjunct is parenthesised when, and only when, leaving it bare
// would let it bind something other than itself — see escapesConjunct.
// The alternative designs are worth writing down, because the obvious
// one is wrong:
//
//   - Bare " AND " between every predicate, which is what this package
//     did, is fail-open in the one feature sold as fail-closed. OR binds
//     looser than AND, so a predicate whose own top level is an OR
//     reassociates the whole clause:
//     "(deletedAt IS NULL) AND a = 1 OR b = 2 AND (tenantId = ?)"
//     parses as "(D AND a) OR (b AND T)" and returns every row matching
//     the first half, for every tenant. The guard is in the statement,
//     which is exactly why a review that looks for the predicate finds
//     it and passes.
//   - Parenthesising every conjunct unconditionally is correct, but
//     nearly every predicate this package builds already renders as one
//     bracketed group — Eq gives ("events"."userId" = ?) — so it would
//     double the parentheses in every statement drops has ever
//     generated, and make every query log worse forever, to defend
//     against the one predicate shape that does not self-parenthesise.
//   - Documenting that a raw predicate must parenthesise itself puts the
//     burden on the caller least able to know about it, and is the
//     default that produced this defect.
//
// So the conjunct is rendered once to see what it looks like and
// bracketed only if its shape can reach outside itself. That costs one
// extra render of each predicate of a multi-predicate clause — the
// price of a guarantee that does not depend on the caller.
//
// A lone conjunct is written as the caller wrote it: there is nothing
// on either side of it to reassociate. That is true of every clause in
// this package except one, and the exception is a ClickHouse clause no
// other dialect here has — see writeLeadingAnd.
func writeAnd(b *drops.Builder, preds []drops.Expression) {
	writeConjuncts(b, preds, len(preds) > 1)
}

// writeLeadingAnd is writeAnd for a clause that stands IN FRONT of the
// clause drops writes its own guards into: PREWHERE.
//
// ClickHouse is the only dialect here with two predicate clauses in
// sequence, and the ordering is what makes the difference. In a WHERE
// clause a lone conjunct is the whole clause, and the automatic
// predicates — which are prepended, never appended — are already in
// front of it, so nothing of drops' is left for it to swallow. A
// PREWHERE conjunct is in front of the WHERE clause, so a lone one that
// opens a comment takes the tenant guard, the GROUP BY and the rest of
// the statement with it, and the query runs, returning every tenant's
// rows through a statement whose text contains the guard.
//
// So a PREWHERE conjunct is bracketed on its own shape rather than on
// how many of them there are. Bracketing does not repair a comment
// opener and is not meant to: "(a = 1 --)" leaves the parenthesis
// unclosed and the server refuses the statement, which is the
// fail-closed answer. What it does is stop the swallow from being
// silent.
func writeLeadingAnd(b *drops.Builder, preds []drops.Expression) {
	writeConjuncts(b, preds, true)
}

func writeConjuncts(b *drops.Builder, preds []drops.Expression, bracket bool) {
	for i, p := range preds {
		if i > 0 {
			b.WriteString(" AND ")
		}
		if bracket && escapesConjunct(p) {
			b.WriteByte('(')
			b.Append(p)
			b.WriteByte(')')
			continue
		}
		b.Append(p)
	}
}

// bracketConjunct returns p parenthesised when leaving it bare in a
// conjunction would let it reach past its own position, and p itself
// otherwise. It is writeAnd's decision, taken once when the expression
// is built rather than each time it is rendered.
//
// [And], [Or] and [Not] take it, because a connective is a clause in
// miniature and used to join its operands with a bare separator:
// And(drops.Raw("a OR b"), guard) reassociated to "a OR (b AND guard)"
// precisely as the WHERE clause did, which left the tenant guard in the
// statement binding nothing. Not was worse — NOT binds tighter than
// every connective a caller can put inside it, so
// Not(drops.Raw("a OR b")) rendered "(NOT a OR b)", which negates half
// of what the caller wrote.
//
// Deciding at construction rather than at render is what keeps a
// predicate tree affordable. The check reads the operand's rendered
// text, so a nested tree checked at render time would render each
// subtree once per level, per render, and a predicate built from user
// input a dozen combinators deep would cost more to bracket than to
// execute. The answer cannot change afterwards: it depends on the
// operand's shape, and the only thing resolution substitutes is a
// scoped copy of a statement — which adds bracketed conjuncts inside a
// SELECT and so can neither introduce a top-level OR nor unbalance a
// parenthesis.
func bracketConjunct(p drops.Expression) drops.Expression {
	if escapesConjunct(p) {
		return parens(p)
	}
	return p
}

// bracketConjuncts applies bracketConjunct to a whole list, into a new
// slice so the caller's variadic backing array is not written through.
func bracketConjuncts(preds []drops.Expression) []drops.Expression {
	out := make([]drops.Expression, len(preds))
	for i, p := range preds {
		out[i] = bracketConjunct(p)
	}
	return out
}

// escapesConjunct reports whether p, written bare between two " AND "s,
// could reach past its own position in the clause.
//
// Four shapes can here, and all four end with a scoping predicate that
// is present in the SQL and binding nothing:
//
//   - a top-level OR, which binds looser than AND and so takes the
//     conjuncts on either side with it;
//   - a top-level conditional "a ? b : c", which in ClickHouse binds
//     looser still — looser than OR — so "D AND c ? x : y AND T" is
//     "(D AND c) ? x : (y AND T)", a clause whose value on the false
//     branch is decided by whatever the caller wrote and on the true
//     branch by nothing at all;
//   - a comment opener, which swallows whatever follows it —
//     "a = 1 --" drops every later conjunct, tenant guard included;
//   - a statement terminator, which ends the statement early and leaves
//     the guard in a second one nobody executes.
//
// Which tokens those are is a dialect question, and ClickHouse's answer
// differs from PostgreSQL's, SQLite's and MySQL's in three places:
//
//   - "#" starts a comment running to the end of the line, as does
//     "#!". ClickHouse accepts the shell spelling so a query file can
//     carry a shebang; the effect inside one conjunct of a WHERE clause
//     is that everything after it is a comment.
//   - The conditional operator has no counterpart in the dialects this
//     check was first written for, and it is the loosest-binding
//     operator ClickHouse has.
//   - "||" is string concatenation here, not a synonym for OR as it is
//     in MySQL's default mode, and binds tighter than a comparison. It
//     needs nothing.
//
// Finding the conditional is the awkward part, because "?" is also this
// dialect's parameter placeholder: a check that called every "?" an
// operator would bracket every predicate drops has ever built. The two
// are indistinguishable in the text, so they are told apart by count
// instead — an expression drops built renders exactly one "?" per bound
// argument, so a fragment carrying more "?" outside its string literals
// than it carries arguments is carrying at least one that is not a
// placeholder. That is only reachable through text drops did not build,
// which is the same set of fragments every other arm of this function
// is about.
//
// The check is on p's rendered text because that is the only thing that
// can be checked: a predicate arrives as an opaque Expression, and the
// ones that need bracketing are precisely the ones drops did not
// build — a [drops.Raw], a caller's own [drops.ExprFunc].
//
// It renders through this package's own [ToSQL] rather than through
// drops.String so that the text inspected is the text the statement
// will carry: identifier quoting decides where a literal starts and
// ends, and a check that read a different quoting than the renderer
// emits would be reasoning about a string nobody sends.
//
// A fragment whose parentheses do not balance is the one shape reported
// as safe without being read, on purpose. It is not an expression, so
// bracketing cannot repair it. There are two ways such a fragment can
// be written, and only one of them is answered by design; both are
// stated here because the second reads as safe and is not, quite.
//
// The first escapes SIDEWAYS: "a = 1) OR (1 = 1" closes the conjunct's
// own parenthesis, ORs past it, and opens another so the whole clause
// balances again. Bracketing that would close the injected parenthesis
// and turn a statement ClickHouse refuses into a working, unscoped OR.
// Left alone it stays a syntax error, which is the fail-closed answer,
// and it is the reason this shape is not bracketed.
//
// The second escapes UPWARDS: "1) --" written into a conjunct of a
// subquery closes the subquery and comments out everything after it,
// the tenant guard included. Bracketing would not repair that one
// either — the parenthesis it opens is closed again before the comment
// begins — so the fragment is rendered as written. What stops it is not
// this check: the arguments bound for the commented-out tail have no
// placeholders left to fill, and the driver rejects the statement on
// the argument count. That is fail-closed BY ACCIDENT rather than by
// design, and an injection with no arguments after it would not be
// caught by it at all.
//
// Both live entirely inside [drops.Raw], which every dialect documents
// as the caller's to scope: drops re-emits its text and does not parse
// it. Do not build one from request data.
func escapesConjunct(p drops.Expression) bool {
	if p == nil {
		return false
	}
	sql, args := ToSQL(p)
	depth := 0
	loose := false
	marks := 0
	for i := 0; i < len(sql); i++ {
		switch c := sql[i]; c {
		case '\'', '"', '`':
			// A string literal or a quoted identifier: its content is
			// text, so a parenthesis or an OR inside it is neither
			// structure nor an operator. ClickHouse admits three
			// quoting forms — '…' for strings, "…" and `…` for
			// identifiers — and all three are legal in a fragment a
			// caller wrote, so all three are skipped. In all three the
			// delimiter doubled escapes itself AND a backslash escapes
			// the next byte: ClickHouse runs one lexer over quoted
			// tokens, which is also why this package's own quoteIdent
			// doubles backslashes before doubling the delimiter.
			closer := c
			for i++; i < len(sql); i++ {
				if sql[i] == '\\' {
					i++
					continue
				}
				if sql[i] != closer {
					continue
				}
				if i+1 < len(sql) && sql[i+1] == closer {
					i++
					continue
				}
				break
			}
			if i >= len(sql) {
				// The fragment ends inside the literal, so it could not
				// be read as an expression and nothing about it has
				// been verified. Bracketing is the safe answer: the
				// statement is a syntax error either way, and the
				// alternative is to trust text nobody parsed.
				return true
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return false
			}
		case '-':
			if i+1 < len(sql) && sql[i+1] == '-' {
				return true
			}
		case '/':
			if i+1 < len(sql) && sql[i+1] == '*' {
				return true
			}
		case '#', ';':
			return true
		case '?':
			marks++
		case 'o', 'O':
			if depth == 0 && wordAt(sql, i, "OR") {
				loose = true
				i++
			}
		}
	}
	if marks > len(args) {
		// At least one "?" is an operator rather than a placeholder,
		// and the conditional it opens binds looser than everything
		// around it. Which "?" it was cannot be recovered from the
		// text, so the whole fragment is bracketed.
		return true
	}
	return loose && depth == 0
}

// wordAt reports whether the ASCII keyword word (given upper-case)
// starts at sql[i] and stands alone, rather than being part of a longer
// identifier — "OR" is an operator, the OR in "FOR" and in "ORDER" is
// not.
func wordAt(sql string, i int, word string) bool {
	if i+len(word) > len(sql) {
		return false
	}
	for j := 0; j < len(word); j++ {
		c := sql[i+j]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		if c != word[j] {
			return false
		}
	}
	return !identByte(sql, i-1) && !identByte(sql, i+len(word))
}

// identByte reports whether sql[i] can appear inside an SQL identifier.
// An index outside the string is not one, which makes a keyword at
// either end of the fragment stand alone.
func identByte(sql string, i int) bool {
	if i < 0 || i >= len(sql) {
		return false
	}
	c := sql[i]
	return c == '_' || c == '$' || (c >= '0' && c <= '9') ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

// andWith returns on AND every predicate in extra, for an ON clause
// that has to carry a table's automatic filters. A nil on — a join
// written with no condition at all — yields the filters alone rather
// than a dangling AND.
//
// It renders the conjunction through writeAnd rather than through And
// because an ON clause is a WHERE clause in a different place: a
// caller-written join condition whose top level is an OR reassociates
// the guards that follow it exactly as it would in a WHERE clause, and
// And joins its operands with a bare " AND ". The bracketing is
// otherwise identical to And's, so an ON clause that was correct before
// renders unchanged.
func andWith(on drops.Expression, extra []drops.Expression) drops.Expression {
	if len(extra) == 0 {
		return on
	}
	preds := make([]drops.Expression, 0, len(extra)+1)
	if on != nil {
		preds = append(preds, on)
	}
	preds = append(preds, extra...)
	if len(preds) == 1 {
		return preds[0]
	}
	return drops.ExprFunc(func(b *drops.Builder) { writeAnd(b, preds) })
}
