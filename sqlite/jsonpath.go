package sqlite

import (
	"strings"

	"github.com/bernardoforcillo/drops"
)

// JSONPath is a typed accessor inside a JSON (TEXT) column. The type
// parameter T fixes the Go type of the leaf value at declaration time,
// keeping comparisons type-safe:
//
//	type Settings struct {
//	    Theme string `json:"theme"`
//	    Beta  bool   `json:"beta"`
//	}
//
//	var (
//	    Users    = sqlite.NewTable("users")
//	    UserID   = sqlite.Add(Users, sqlite.BigInt("id").PrimaryKey())
//	    UserMeta = sqlite.Add(Users, sqlite.JSON("meta"))
//	)
//
//	beta := sqlite.JSONField[bool](UserMeta, "settings", "beta")
//	db.Select(UserID).From(Users).Where(beta.Eq(true))
//	// SELECT "users"."id" FROM "users"
//	// WHERE (json_extract("users"."meta", '$.settings.beta') = ?)
//
// SQLite's json_extract already returns a value with the right storage
// class (INTEGER / REAL / TEXT) for the JSON leaf, so — unlike the
// PostgreSQL accessor — no explicit cast is emitted.
type JSONPath[T any] struct {
	col  *Column
	path []string
}

// JSONField builds a typed JSONPath. col is the JSON column, path the
// keys to walk. An empty path targets the document root ('$').
func JSONField[T any](col ColRef, path ...string) *JSONPath[T] {
	return &JSONPath[T]{col: col.col(), path: path}
}

// Column returns the source JSON column.
func (j *JSONPath[T]) Column() *Column { return j.col }

// Path returns a copy of the path segments.
func (j *JSONPath[T]) Path() []string {
	out := make([]string, len(j.path))
	copy(out, j.path)
	return out
}

// jsonPathString builds the SQLite JSON path expression: "$" for the
// root, "$.a.b" for nested keys. Each segment is emitted as a
// bracketed, quote-escaped key so names containing dots or spaces stay
// unambiguous.
func (j *JSONPath[T]) jsonPathString() string {
	if len(j.path) == 0 {
		return "$"
	}
	var sb strings.Builder
	sb.WriteByte('$')
	for _, seg := range j.path {
		sb.WriteString(`."`)
		sb.WriteString(strings.ReplaceAll(seg, `"`, `""`))
		sb.WriteByte('"')
	}
	return sb.String()
}

// writeExpr renders json_extract(<col>, '<path>').
func (j *JSONPath[T]) writeExpr(b *drops.Builder) {
	b.WriteString("json_extract(")
	j.col.WriteSQL(b)
	b.WriteString(", ")
	b.AddArg(j.jsonPathString())
	b.WriteByte(')')
}

// WriteSQL makes JSONPath usable as a SELECT projection or generic
// Expression. Implements drops.Expression.
func (j *JSONPath[T]) WriteSQL(b *drops.Builder) { j.writeExpr(b) }

// Operators ------------------------------------------------------------

func (j *JSONPath[T]) cmp(op string, v T) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		j.writeExpr(b)
		b.WriteByte(' ')
		b.WriteString(op)
		b.WriteByte(' ')
		b.AddArg(v)
		b.WriteByte(')')
	})
}

func (j *JSONPath[T]) Eq(v T) drops.Expression  { return j.cmp("=", v) }
func (j *JSONPath[T]) Ne(v T) drops.Expression  { return j.cmp("<>", v) }
func (j *JSONPath[T]) Gt(v T) drops.Expression  { return j.cmp(">", v) }
func (j *JSONPath[T]) Gte(v T) drops.Expression { return j.cmp(">=", v) }
func (j *JSONPath[T]) Lt(v T) drops.Expression  { return j.cmp("<", v) }
func (j *JSONPath[T]) Lte(v T) drops.Expression { return j.cmp("<=", v) }

// In tests whether the path's value is one of values.
func (j *JSONPath[T]) In(values ...T) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		if len(values) == 0 {
			b.WriteString("(0)")
			return
		}
		b.WriteByte('(')
		j.writeExpr(b)
		b.WriteString(" IN (")
		for i, v := range values {
			if i > 0 {
				b.WriteString(", ")
			}
			b.AddArg(v)
		}
		b.WriteString("))")
	})
}

// IsNull renders "(path) IS NULL" — true when the key is absent or the
// JSON value is null.
func (j *JSONPath[T]) IsNull() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		j.writeExpr(b)
		b.WriteString(" IS NULL)")
	})
}

// IsNotNull renders "(path) IS NOT NULL".
func (j *JSONPath[T]) IsNotNull() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		j.writeExpr(b)
		b.WriteString(" IS NOT NULL)")
	})
}

// Like applies the SQL LIKE operator (meaningful when T is a string).
func (j *JSONPath[T]) Like(pattern string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		j.writeExpr(b)
		b.WriteString(" LIKE ")
		b.AddArg(pattern)
		b.WriteByte(')')
	})
}

// Containment / existence ----------------------------------------------

// JSONHasKey renders "json_type(<col>, '<path>') IS NOT NULL" — true
// when the key exists in the document (even if its value is JSON null,
// json_type returns 'null', which is still non-NULL SQL).
func JSONHasKey(col ColRef, path ...string) drops.Expression {
	jp := JSONField[any](col, path...)
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("(json_type(")
		jp.col.WriteSQL(b)
		b.WriteString(", ")
		b.AddArg(jp.jsonPathString())
		b.WriteString(") IS NOT NULL)")
	})
}
