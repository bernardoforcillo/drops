package mysql

import "github.com/bernardoforcillo/drops"

// How a conjunction is written, and why it is not just " AND ".
//
// Every clause in this package that AND-s predicates together — a
// WHERE, a HAVING, an ON, [And] itself — composes a predicate the
// caller wrote with predicates drops added on the statement's own
// account: a DefaultFilter, a resolved ContextFilter, a tenant axis.
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
//     "(`deletedAt` IS NULL) AND a = 1 OR b = 2 AND (`tenantId` = ?)"
//     parses as "(D AND a) OR (b AND T)" and returns every row matching
//     the first half, for every tenant. The guard is in the statement,
//     which is exactly why a review that looks for the predicate finds
//     it and passes.
//   - Parenthesising every conjunct unconditionally is correct, but
//     nearly every predicate this package builds already renders as one
//     bracketed group — Eq gives (`users`.`id` = ?) — so it would double
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
//   - an operator at the top level that binds looser than AND, which
//     takes the conjuncts on either side with it;
//   - a comment opener, which swallows whatever follows it —
//     "a = 1 --" drops every later conjunct, tenant guard included;
//   - a statement terminator, which ends the statement early and leaves
//     the guard in a second one nobody executes.
//
// Which tokens those are is a dialect question, and MySQL's answer is
// longer than PostgreSQL's or SQLite's in both categories:
//
//   - OR is not the only loose connective. "||" is a synonym for OR on
//     both servers in their default sql_mode (PIPES_AS_CONCAT, which
//     ANSI mode sets, is what turns it into concatenation), and XOR sits
//     between OR and AND in the precedence table. Either one at depth 0
//     reassociates the clause exactly as OR does, and neither exists in
//     the dialects this check was first written for. "&&" is a synonym
//     for AND and binds at least as tightly, so it is left alone.
//   - "#" starts a comment that runs to the end of the line — a MySQL
//     and MariaDB extension no other dialect here has, and the shortest
//     way there is to swallow a tenant guard.
//
// "--" is treated as a comment opener although MySQL requires a
// whitespace or control character after it: "1--2" is a subtraction of
// a negative number on these servers and a comment on PostgreSQL. The
// check is deliberately the conservative one, because being wrong in
// this direction costs a pair of parentheses around an arithmetic
// fragment nobody writes, and being wrong in the other costs the guard.
//
// The check is on p's rendered text because that is the only thing that
// can be checked: a predicate arrives as an opaque Expression, and the
// ones that need bracketing are precisely the ones drops did not
// build — a [drops.Raw], a caller's own [drops.ExprFunc].
//
// It renders through the MySQL dialect rather than through drops.String
// so that the text inspected is the text the statement will carry:
// identifier quoting decides where a literal starts and ends, and a
// check that read a different quoting than the renderer emits would be
// reasoning about a string nobody sends.
//
// A fragment whose parentheses do not balance is the one shape reported
// as safe without being read, on purpose. It is not an expression, so
// bracketing cannot repair it — and for the shape that matters,
// "a = 1) OR (1 = 1", bracketing would close the injected parenthesis
// and turn a statement the server refuses into a working, unscoped OR.
// Left alone it stays a syntax error, which is the fail-closed answer.
func escapesConjunct(p drops.Expression) bool {
	if p == nil {
		return false
	}
	sql, _ := render(p)
	depth := 0
	loose := false
	for i := 0; i < len(sql); i++ {
		switch c := sql[i]; c {
		case '\'', '"', '`':
			// A string literal or a quoted identifier: its content is
			// text, so a parenthesis or an OR inside it is neither
			// structure nor an operator. MySQL admits three quoting
			// forms — '…' for strings, `…` for identifiers, and "…" for
			// either, depending on whether ANSI_QUOTES is set — and all
			// three are legal in a fragment a caller wrote, so all
			// three are skipped. The quote character doubled inside the
			// literal escapes itself; a backslash escapes the next byte
			// inside the two string forms, which is MySQL's rule
			// (NO_BACKSLASH_ESCAPES turns it off) and not one the other
			// dialects here have. Inside backticks there is no
			// backslash escape at all.
			closer := c
			for i++; i < len(sql); i++ {
				if sql[i] == '\\' && closer != '`' {
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
		case '|':
			// "||" is OR outside ANSI mode, and a fragment drops did
			// not build is a fragment drops cannot ask the sql_mode
			// about. A single '|' is the bitwise operator, which binds
			// tighter than AND and needs nothing.
			if depth == 0 && i+1 < len(sql) && sql[i+1] == '|' {
				loose = true
				i++
			}
		case 'o', 'O':
			if depth == 0 && wordAt(sql, i, "OR") {
				loose = true
				i++
			}
		case 'x', 'X':
			if depth == 0 && wordAt(sql, i, "XOR") {
				loose = true
				i += 2
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
