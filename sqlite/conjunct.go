package sqlite

import "github.com/bernardoforcillo/drops"

// How a conjunction is written, and why it is not just " AND ".
//
// Every clause in this package that AND-s predicates together —a WHERE,
// an ON, [And] itself— composes a predicate the caller wrote with
// predicates drops added on the statement's own account: a
// DefaultFilter, a resolved ContextFilter, a tenant axis. That
// composition is only sound if each conjunct binds to itself.

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
//     bracketed group — Eq gives ("users"."id" = ?) — so it would double
//     the parentheses in every statement drops has ever generated, and
//     make every query log worse forever, to defend against the one
//     predicate shape that does not self-parenthesise.
//   - Documenting that a raw predicate must parenthesise itself puts the
//     burden on the caller least able to know about it, and is the
//     default that produced this defect.
//
// So the conjunct is rendered once to see what it looks like and
// bracketed only if its shape can reach outside itself. That costs one
// extra render of each predicate of a multi-predicate clause — the
// price of a guarantee that does not depend on the caller.
func writeAnd(b *drops.Builder, preds []drops.Expression) {
	// A lone conjunct is the whole clause: there is nothing on either
	// side of it to reassociate, so it is written as the caller wrote it.
	bracket := len(preds) > 1
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
// Three shapes can, and all three end with a scoping predicate that is
// present in the SQL and binding nothing:
//
//   - a top-level OR, which binds looser than AND and so takes the
//     conjuncts on either side with it;
//   - a line or block comment, which swallows whatever follows it —
//     "a = 1 --" drops every later conjunct, tenant guard included;
//   - a statement terminator, which ends the statement early and leaves
//     the guard in a second one nobody executes.
//
// The check is on p's rendered text because that is the only thing that
// can be checked: a predicate arrives as an opaque Expression, and the
// ones that need bracketing are precisely the ones drops did not
// build — a [drops.Raw], a caller's own [drops.ExprFunc].
//
// It renders through the SQLite dialect rather than through
// drops.String so that the text inspected is the text the statement
// will carry: identifier quoting decides where a literal starts and
// ends, and a check that read a different quoting than the renderer
// emits would be reasoning about a string nobody sends.
//
// A fragment whose parentheses do not balance is the one shape reported
// as safe without being read, on purpose. It is not an expression, so
// bracketing cannot repair it — and for the shape that matters,
// "a = 1) OR (1 = 1", bracketing would close the injected parenthesis
// and turn a statement SQLite refuses into a working, unscoped OR. Left
// alone it stays a syntax error, which is the fail-closed answer.
func escapesConjunct(p drops.Expression) bool {
	if p == nil {
		return false
	}
	sql, _ := ToSQL(p)
	depth := 0
	loose := false
	for i := 0; i < len(sql); i++ {
		switch c := sql[i]; c {
		case '\'', '"', '`', '[':
			// A string literal or a quoted identifier: its content is
			// text, so a parenthesis or an OR inside it is neither
			// structure nor an operator. SQLite admits four quoting
			// forms — '…' for strings, "…" and `…` for identifiers, and
			// the MS-Access-compatible […] — and all four are legal in
			// a fragment a caller wrote, so all four are skipped. The
			// quote character doubled inside the literal escapes
			// itself; the bracket form has no escape at all, which is
			// SQLite's rule and not this function's choice.
			closer := c
			if c == '[' {
				closer = ']'
			}
			for i++; i < len(sql); i++ {
				if sql[i] != closer {
					continue
				}
				if closer != ']' && i+1 < len(sql) && sql[i+1] == closer {
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
		case ';':
			return true
		case 'o', 'O':
			if depth == 0 && wordAt(sql, i, "OR") {
				loose = true
				i++
			}
		}
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
