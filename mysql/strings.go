package mysql

import "github.com/bernardoforcillo/drops"

// MySQL string functions, ported from drops/pg's strings.go. Arguments
// may be a drops.Expression or a Go value, which binds as a parameter.
//
// This is the file where a blind port does the most damage, because
// several of these have a PostgreSQL function of the same name that
// answers something else. Each such case is named at its own helper;
// the three that bite hardest:
//
//   - CONCAT is null-propagating. PostgreSQL's concat() treats a NULL
//     argument as the empty string; MySQL's returns NULL for the whole
//     expression. See [Concat].
//   - LENGTH counts bytes, not characters. PostgreSQL's length()
//     counts characters. [Length] therefore renders CHAR_LENGTH, and
//     the byte count is [OctetLength].
//   - The || operator is *OR*, not concatenation — unless the session
//     runs under the PIPES_AS_CONCAT sql_mode, which is not the
//     default. So pg's ConcatOp has no counterpart here at all;
//     'a' || 'b' evaluates to 0 on a stock server rather than failing,
//     which is the worst way for a difference to show up. Use [Concat].
//
// Left out for want of anything that means the same thing:
//
//   - pg's Initcap. No MySQL function title-cases a string.
//   - pg's Format. MySQL has a FORMAT(), but it renders a number with
//     thousands separators — it is not printf, and exposing it under
//     pg's name would be the worst kind of false friend.
//   - pg's ToChar. Date formatting is [DateFormat] in datetime.go, with
//     MySQL's own pattern language; there is no general to_char.
//   - pg's Encode / Decode. The MySQL equivalents are per-encoding
//     functions: [Hex], [Unhex], [ToBase64], [FromBase64].
//   - pg's RegexpMatch, which returns text[]. MySQL has no array type;
//     [RegexpSubstr] returns the matching substring instead.

// Concat renders concat(<args...>).
//
// Beware the null rule, which is the opposite of PostgreSQL's: MySQL
// returns NULL if *any* argument is NULL, where pg's concat() skips
// NULLs and returns the rest. [ConcatWS] skips them on both servers
// and is the closer match for ported code.
func Concat(args ...any) drops.Expression { return funcCall("concat", args) }

// ConcatWS renders concat_ws(<sep>, <args...>), which skips NULL
// arguments — as PostgreSQL's concat_ws does. A NULL *separator* still
// makes the whole call NULL, on both servers.
func ConcatWS(sep any, args ...any) drops.Expression {
	return funcCall("concat_ws", append([]any{sep}, args...))
}

// Length renders char_length(<e>) — the number of characters, which is
// what PostgreSQL's length() counts.
//
// MySQL's own LENGTH() counts bytes, so under utf8mb4 it answers 2 for
// "é" and pg answers 1. That is [OctetLength] here, spelled apart so
// the difference is a choice rather than an accident.
func Length(e any) drops.Expression { return funcCall("char_length", []any{e}) }

// CharLength renders char_length(<e>) — the same thing [Length] does,
// under the name the SQL standard gives it.
func CharLength(e any) drops.Expression { return funcCall("char_length", []any{e}) }

// OctetLength renders length(<e>): the size in bytes, which depends on
// the string's character set.
func OctetLength(e any) drops.Expression { return funcCall("length", []any{e}) }

// Substring renders substring(<e> FROM <from> FOR <count>), 1-based as
// in PostgreSQL. count may be nil to omit the FOR clause and take the
// rest of the string.
func Substring(e, from, count any) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("substring(")
		writeOperand(b, e)
		b.WriteString(" FROM ")
		writeOperand(b, from)
		if count != nil {
			b.WriteString(" FOR ")
			writeOperand(b, count)
		}
		b.WriteByte(')')
	})
}

// Left / Right take a prefix or suffix of n characters. No pg
// counterpart is needed — pg has left() and right() too — but MySQL
// counts characters here even though LENGTH counts bytes.
func Left(e, n any) drops.Expression  { return funcCall("left", []any{e, n}) }
func Right(e, n any) drops.Expression { return funcCall("right", []any{e, n}) }

// Trim renders trim(<e>), removing spaces from both ends.
//
// MySQL's TRIM takes only the one-argument form here: TRIM(str) and
// the TRIM(BOTH x FROM str) keyword form. The two-argument
// LTRIM(str, chars) that PostgreSQL and Oracle-mode MariaDB accept is
// rejected by both servers in their default mode — error 1582, wrong
// parameter count — so [LTrim] and [RTrim] take one argument only, and
// [TrimChars] is the keyword form.
func Trim(e any) drops.Expression  { return funcCall("trim", []any{e}) }
func LTrim(e any) drops.Expression { return funcCall("ltrim", []any{e}) }
func RTrim(e any) drops.Expression { return funcCall("rtrim", []any{e}) }

// TrimChars renders trim(<where> <chars> FROM <e>) with where being
// "BOTH", "LEADING" or "TRAILING" — the only way to trim something
// other than spaces on both servers.
func TrimChars(where string, chars, e any) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("trim(")
		b.WriteString(where)
		b.WriteByte(' ')
		writeOperand(b, chars)
		b.WriteString(" FROM ")
		writeOperand(b, e)
		b.WriteByte(')')
	})
}

// LPad / RPad pad e to n characters with pad.
func LPad(e, n, pad any) drops.Expression { return funcCall("lpad", []any{e, n, pad}) }
func RPad(e, n, pad any) drops.Expression { return funcCall("rpad", []any{e, n, pad}) }

// Repeat / Reverse.
func Repeat(e, n any) drops.Expression { return funcCall("repeat", []any{e, n}) }
func Reverse(e any) drops.Expression   { return funcCall("reverse", []any{e}) }

// Replace renders replace(<e>, <from>, <to>).
func Replace(e, from, to any) drops.Expression { return funcCall("replace", []any{e, from, to}) }

// RegexpReplace renders regexp_replace(<e>, <pattern>, <replacement>).
//
// pg's helper takes a trailing flags string; this one does not,
// because the argument that would carry it is not portable. MySQL 8.0
// accepts REGEXP_REPLACE(expr, pat, repl, pos, occurrence, match_type)
// and MariaDB 10.11 rejects anything past the third argument outright
// — error 1582. Put the flag in the pattern instead: "(?i)" is
// understood by both servers' regex engines.
func RegexpReplace(e, pattern, replacement any) drops.Expression {
	return funcCall("regexp_replace", []any{e, pattern, replacement})
}

// RegexpSubstr renders regexp_substr(<e>, <pattern>) — the matching
// substring. Stands in for pg's RegexpMatch, which returns an array
// MySQL has no way to hold.
//
// The two servers disagree about a miss: MySQL 8 answers NULL, MariaDB
// 10.11 answers the empty string. Test for "no match" with
// [RegexpInstr] = 0, or with [RegexpLike], rather than with an IS NULL
// that is only right on one of them.
func RegexpSubstr(e, pattern any) drops.Expression {
	return funcCall("regexp_substr", []any{e, pattern})
}

// RegexpInstr renders regexp_instr(<e>, <pattern>) — the 1-based
// position of the match, or 0.
func RegexpInstr(e, pattern any) drops.Expression {
	return funcCall("regexp_instr", []any{e, pattern})
}

// RegexpLike renders (<e> REGEXP <pattern>) — the operator rather than
// the function, because MySQL 8.0's REGEXP_LIKE() does not exist on
// MariaDB (error 1305) while the operator has worked on both for
// decades.
//
// Case sensitivity follows the operands' collation, exactly as [Like]
// does; "(?i)" at the head of the pattern forces it.
func RegexpLike(e, pattern any) drops.Expression { return binOp(e, "REGEXP", pattern) }

// Position renders position(<substring> IN <string>) — the 1-based
// index of substring, or 0. Same spelling and same answer as
// PostgreSQL's.
func Position(substring, str any) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("position(")
		writeOperand(b, substring)
		b.WriteString(" IN ")
		writeOperand(b, str)
		b.WriteByte(')')
	})
}

// StrPos keeps PostgreSQL's strpos(<string>, <substring>) argument
// order and renders MySQL's locate(<substring>, <string>), which takes
// them the other way round.
//
// The swap is the point. Both functions are two arguments of the same
// type, so a port that left the order alone would compile, run, and
// answer 0 for every row that does not happen to contain itself.
func StrPos(str, substring any) drops.Expression {
	return funcCall("locate", []any{substring, str})
}

// Locate renders locate(<substring>, <string> [, <from>]) in MySQL's
// own argument order, with the optional starting position pg has no
// form for.
func Locate(substring, str any, from ...any) drops.Expression {
	return funcCall("locate", append([]any{substring, str}, from...))
}

// SubstringIndex renders substring_index(<e>, <delim>, <count>): the
// text before the count-th occurrence of delim, or after it when count
// is negative. MySQL-only; pg reaches for split_part.
func SubstringIndex(e, delim, count any) drops.Expression {
	return funcCall("substring_index", []any{e, delim, count})
}

// Md5 renders md5(<e>).
func Md5(e any) drops.Expression { return funcCall("md5", []any{e}) }

// Sha1 renders sha1(<e>); Sha2 renders sha2(<e>, <bits>) with bits one
// of 224, 256, 384, 512.
func Sha1(e any) drops.Expression       { return funcCall("sha1", []any{e}) }
func Sha2(e, bits any) drops.Expression { return funcCall("sha2", []any{e, bits}) }

// Hex / Unhex and ToBase64 / FromBase64 are what pg's encode/decode
// become: one function per encoding rather than a format argument.
func Hex(e any) drops.Expression        { return funcCall("hex", []any{e}) }
func Unhex(e any) drops.Expression      { return funcCall("unhex", []any{e}) }
func ToBase64(e any) drops.Expression   { return funcCall("to_base64", []any{e}) }
func FromBase64(e any) drops.Expression { return funcCall("from_base64", []any{e}) }

// Collate renders (<e> COLLATE <collation>) — the way to force a
// comparison's case sensitivity in a dialect where it is otherwise a
// property of the schema. The collation name is written verbatim and
// must be one the server knows, e.g. "utf8mb4_bin".
func Collate(e any, collation string) drops.Expression {
	mustIdent("collation", collation)
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		writeOperand(b, e)
		b.WriteString(" COLLATE ")
		b.WriteString(collation)
		b.WriteByte(')')
	})
}
