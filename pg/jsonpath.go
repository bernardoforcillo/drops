package pg

import (
	"reflect"
	"time"

	"github.com/bernardoforcillo/drops"
)

// JSONPath is a typed accessor inside a jsonb column. The type
// parameter T fixes the Go type of the leaf value, which in turn
// drives the SQL cast emitted at the comparison site so the
// resulting predicate stays index-friendly and type-safe at
// declaration time:
//
//	type Settings struct {
//	    Theme    string `json:"theme"`
//	    LangCode string `json:"lang"`
//	    Beta     bool   `json:"beta"`
//	}
//
//	var (
//	    Users    = pg.NewTable("users")
//	    UserID   = pg.Add(Users, pg.BigSerial("id").PrimaryKey())
//	    UserMeta = pg.Add(Users, pg.JSONB("meta"))
//	)
//
//	beta := pg.JSONField[bool](UserMeta, "settings", "beta")
//	db.Select(UserID).From(Users).Where(beta.Eq(true))
//	// SELECT "users"."id" FROM "users"
//	// WHERE (("users"."meta" -> 'settings' ->> 'beta')::boolean = $1)
//
// JSONField stitches arbitrary-length path segments together. The
// final accessor uses `->>` (text) so the cast lands on a scalar;
// the intermediate segments use `->` so they stay jsonb until the
// last step.
//
// Containment / existence operators live alongside as
// JSONContains / JSONHasKey — they don't need a typed leaf.
type JSONPath[T any] struct {
	col  *Column
	path []string
}

// JSONField builds a typed JSONPath. col is the jsonb column, path
// the keys to walk. An empty path targets the column itself (useful
// with JSONContains).
func JSONField[T any](col ColRef, path ...string) *JSONPath[T] {
	return &JSONPath[T]{col: col.col(), path: path}
}

// Column returns the source jsonb column.
func (j *JSONPath[T]) Column() *Column { return j.col }

// Path returns a copy of the path segments.
func (j *JSONPath[T]) Path() []string {
	out := make([]string, len(j.path))
	copy(out, j.path)
	return out
}

// writeExpr renders the path expression with the appropriate cast
// for T. Without a path it renders the bare column reference (used
// by JSONContains / JSONHasKey).
func (j *JSONPath[T]) writeExpr(b *drops.Builder) {
	if len(j.path) == 0 {
		j.col.WriteSQL(b)
		return
	}
	b.WriteByte('(')
	j.col.WriteSQL(b)
	// Intermediate segments stay jsonb.
	for i := 0; i < len(j.path)-1; i++ {
		b.WriteString(" -> ")
		writeJSONLiteral(b, j.path[i])
	}
	// Final segment with ->> so the cast applies to text.
	b.WriteString(" ->> ")
	writeJSONLiteral(b, j.path[len(j.path)-1])
	b.WriteString(")::")
	b.WriteString(jsonCastFor[T]())
}

// writeJSONLiteral writes a JSON-path key as a single-quoted string,
// escaping embedded quotes.
func writeJSONLiteral(b *drops.Builder, s string) {
	b.WriteByte('\'')
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			b.WriteByte('\'')
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('\'')
}

// jsonCastFor returns the SQL type to cast the text accessor to,
// based on the Go type parameter T.
func jsonCastFor[T any]() string {
	var zero T
	switch any(zero).(type) {
	case string:
		return "text"
	case int, int32:
		return "integer"
	case int16:
		return "smallint"
	case int64:
		return "bigint"
	case float32:
		return "real"
	case float64:
		return "double precision"
	case bool:
		return "boolean"
	case time.Time:
		return "timestamptz"
	}
	// Fall back to text — drivers can handle conversion on the Go
	// side. Avoids surprising users with a panic for niche T's.
	return "text"
}

// WriteSQL makes JSONPath usable as a SELECT projection or as a
// generic Expression. Implements drops.Expression.
func (j *JSONPath[T]) WriteSQL(b *drops.Builder) { j.writeExpr(b) }

// ----------------------------------------------------------------------
// Operators
// ----------------------------------------------------------------------

// cmp builds "(<path> <op> $n)".
//
// The accessor is held as an operand rather than closed over — the
// JSONPath is itself an Expression — for the reason opExpr gives, even
// though neither side of the comparison can carry a statement here: the
// leaf is a T, a value the accessor's cast is generated from, and it is
// bound as a parameter. Holding it costs nothing and leaves no closure
// for a later helper to be copied out of.
func (j *JSONPath[T]) cmp(op string, v T) drops.Expression {
	return &opExpr{
		parts:    []string{"(", " " + op + " ", ")"},
		operands: []drops.Expression{j, drops.Param{Value: v}},
	}
}

func (j *JSONPath[T]) Eq(v T) drops.Expression  { return j.cmp("=", v) }
func (j *JSONPath[T]) Ne(v T) drops.Expression  { return j.cmp("<>", v) }
func (j *JSONPath[T]) Gt(v T) drops.Expression  { return j.cmp(">", v) }
func (j *JSONPath[T]) Gte(v T) drops.Expression { return j.cmp(">=", v) }
func (j *JSONPath[T]) Lt(v T) drops.Expression  { return j.cmp("<", v) }
func (j *JSONPath[T]) Lte(v T) drops.Expression { return j.cmp("<=", v) }

// In tests whether the path's value is one of values.
func (j *JSONPath[T]) In(values ...T) drops.Expression {
	var o opBuilder
	o.text("(")
	o.operand(j)
	o.text(" IN (")
	for i, v := range values {
		if i > 0 {
			o.text(", ")
		}
		o.operand(drops.Param{Value: v})
	}
	o.text("))")
	return o.done()
}

// IsNull renders "(path) IS NULL". Useful to filter rows where the
// key is absent from the json structure entirely.
func (j *JSONPath[T]) IsNull() drops.Expression {
	return &opExpr{parts: []string{"(", " IS NULL)"}, operands: []drops.Expression{j}}
}

// IsNotNull renders "(path) IS NOT NULL".
func (j *JSONPath[T]) IsNotNull() drops.Expression {
	return &opExpr{parts: []string{"(", " IS NOT NULL)"}, operands: []drops.Expression{j}}
}

// Like applies the SQL LIKE operator. Only meaningful when T is a
// string; the call compiles for any T but won't be useful elsewhere.
func (j *JSONPath[T]) Like(pattern string) drops.Expression {
	return &opExpr{
		parts:    []string{"(", " LIKE ", ")"},
		operands: []drops.Expression{j, drops.Param{Value: pattern}},
	}
}

// ----------------------------------------------------------------------
// Containment / existence helpers (operate on the raw jsonb)
// ----------------------------------------------------------------------

// JSONContains renders "col @> $1" — the jsonb containment
// operator. Accepts anything that drivers can serialise as jsonb
// (json.RawMessage, []byte, or a marshaled struct value).
//
// A drops.Expression given as the value is rendered as SQL and held as
// an operand rather than bound, so a statement there is scoped as one —
// which is also the only way it could have worked, a builder handed to
// a driver as an argument being nothing the driver can encode.
func JSONContains(col ColRef, value any) drops.Expression {
	if reflect.TypeOf(value) == nil {
		// nil would render as NULL and the operator returns NULL,
		// which is almost certainly not what the caller intended.
		// Surface the mistake via an explicit message rather than
		// silently emitting AND NULL.
		return drops.Raw("/* drops/pg: JSONContains called with nil value */ FALSE")
	}
	return &opExpr{
		parts:    []string{"(", " @> ", ")"},
		operands: []drops.Expression{col.col(), operandExpr(value)},
	}
}

// JSONHasKey renders "col ? $1" — the jsonb key-existence operator.
func JSONHasKey(col ColRef, key string) drops.Expression {
	return &opExpr{
		parts:    []string{"(", " ? ", ")"},
		operands: []drops.Expression{col.col(), drops.Param{Value: key}},
	}
}

// JSONHasAnyKey renders "col ?| $1" — true when any of keys is
// present at the jsonb top level.
func JSONHasAnyKey(col ColRef, keys []string) drops.Expression {
	return &opExpr{
		parts:    []string{"(", " ?| ", ")"},
		operands: []drops.Expression{col.col(), drops.Param{Value: keys}},
	}
}

// JSONHasAllKeys renders "col ?& $1" — true only when every key in
// keys is present at the top level.
func JSONHasAllKeys(col ColRef, keys []string) drops.Expression {
	return &opExpr{
		parts:    []string{"(", " ?& ", ")"},
		operands: []drops.Expression{col.col(), drops.Param{Value: keys}},
	}
}
