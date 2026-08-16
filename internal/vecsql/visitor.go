// Package vecsql compiles portable vector filters into SQL
// expressions.
//
// It exists because drops/pg and drops/clickhouse need exactly the
// same traversal — resolve a field name to an expression, pick the
// operator, thread the negations — and differ only in which operator
// functions they call and how they spell a text match. Parameterising
// those differences is cheaper than maintaining the walk twice, and it
// guarantees the two backends agree on the semantics of every portable
// filter.
//
// It is internal: the portable surface is
// [github.com/bernardoforcillo/drops/vector], and a third SQL backend
// wiring itself up here is an implementation detail of this
// repository, not API.
package vecsql

import (
	"fmt"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/vector"
)

// Ops is the dialect's operator set. Every field is the function the
// dialect package already exports — pg.Eq, clickhouse.In, and so on —
// so wiring a backend up is naming functions, not writing them.
type Ops struct {
	Eq  func(left, right any) drops.Expression
	Ne  func(left, right any) drops.Expression
	Lt  func(left, right any) drops.Expression
	Lte func(left, right any) drops.Expression
	Gt  func(left, right any) drops.Expression
	Gte func(left, right any) drops.Expression

	In    func(left any, values ...any) drops.Expression
	NotIn func(left any, values ...any) drops.Expression

	And func(preds ...drops.Expression) drops.Expression
	Or  func(preds ...drops.Expression) drops.Expression
	Not func(pred drops.Expression) drops.Expression

	IsNull    func(e any) drops.Expression
	IsNotNull func(e any) drops.Expression
}

// Visitor compiles [vector.Filter] nodes into drops expressions.
type Visitor struct {
	// Ops is the dialect's operator set.
	Ops Ops

	// Resolve maps a portable filter field name onto an expression.
	// sample is the value the field is about to be compared against,
	// which lets a backend choose a cast for schemaless payload
	// accessors.
	Resolve func(field string, sample any) (drops.Expression, error)

	// IDColumn is the record-ID expression [vector.HasID] tests.
	IDColumn drops.Expression

	// TextMatcher renders a case-insensitive containment test. The
	// dialects spell this differently enough (ILIKE with concatenated
	// wildcards vs. a position function) that it stays a hook.
	TextMatcher func(field drops.Expression, text string) drops.Expression

	// GeoCompiler compiles a bounding-box test. Kept a hook because a
	// backend may have a real spatial type to defer to.
	GeoCompiler func(field string, box vector.GeoBox) (drops.Expression, error)

	// NullTest renders "this field holds no usable value". Optional:
	// when nil it is Ops.IsNull on the resolved expression, which is
	// right wherever a missing value really is SQL NULL.
	//
	// It is a hook because that is not universal. ClickHouse's JSON
	// accessors return an empty string for a key that isn't there, so
	// a store reading its payload out of a String column has to ask
	// JSONHas instead — otherwise IsNull would never be true and Ne
	// would silently drop every record missing the field.
	NullTest func(field string, col drops.Expression) drops.Expression
}

// nullOf renders the "no usable value" test for a resolved field.
func (v Visitor) nullOf(field string, col drops.Expression) drops.Expression {
	if v.NullTest != nil {
		return v.NullTest(field, col)
	}
	return v.Ops.IsNull(col)
}

// Compile walks f and returns the SQL predicate, or ok=false when f is
// the zero filter and no predicate should be emitted at all.
func (v Visitor) Compile(f vector.Filter) (drops.Expression, bool, error) {
	return vector.Compile[drops.Expression](f, v)
}

func (v Visitor) And(parts []drops.Expression) (drops.Expression, error) {
	return v.Ops.And(parts...), nil
}

func (v Visitor) Or(parts []drops.Expression) (drops.Expression, error) {
	return v.Ops.Or(parts...), nil
}

func (v Visitor) Not(part drops.Expression) (drops.Expression, error) {
	return v.Ops.Not(part), nil
}

func (v Visitor) Compare(op vector.Op, field string, value any) (drops.Expression, error) {
	col, err := v.Resolve(field, value)
	if err != nil {
		return nil, err
	}
	switch op {
	case vector.OpEq:
		return v.Ops.Eq(col, value), nil
	case vector.OpNe:
		// NULL is not "different from" anything in SQL's three-valued
		// logic, but a portable Ne is asking "does this record fail
		// the equality test", and a record with no value for the
		// field does. Qdrant answers it that way — a must_not over
		// equality keeps records missing the key — so the SQL
		// backends spell out the same semantics rather than quietly
		// dropping those rows.
		return v.Ops.Or(v.Ops.Ne(col, value), v.nullOf(field, col)), nil
	case vector.OpLt:
		return v.Ops.Lt(col, value), nil
	case vector.OpLte:
		return v.Ops.Lte(col, value), nil
	case vector.OpGt:
		return v.Ops.Gt(col, value), nil
	case vector.OpGte:
		return v.Ops.Gte(col, value), nil
	default:
		return nil, fmt.Errorf("%w: sql compare %q", vector.ErrUnsupportedOp, op)
	}
}

func (v Visitor) Set(op vector.Op, field string, values []any) (drops.Expression, error) {
	var sample any
	if len(values) > 0 {
		sample = values[0]
	}
	col, err := v.Resolve(field, sample)
	if err != nil {
		return nil, err
	}
	switch op {
	case vector.OpIn:
		return v.Ops.In(col, values...), nil
	case vector.OpNotIn:
		// Same three-valued-logic correction as Ne: a record with no
		// value for the field is not in the set.
		return v.Ops.Or(v.Ops.NotIn(col, values...), v.nullOf(field, col)), nil
	default:
		return nil, fmt.Errorf("%w: sql set %q", vector.ErrUnsupportedOp, op)
	}
}

func (v Visitor) Null(op vector.Op, field string) (drops.Expression, error) {
	col, err := v.Resolve(field, nil)
	if err != nil {
		return nil, err
	}
	switch op {
	case vector.OpIsNull:
		return v.nullOf(field, col), nil
	case vector.OpIsNotNull:
		return v.Ops.Not(v.nullOf(field, col)), nil
	default:
		return nil, fmt.Errorf("%w: sql null %q", vector.ErrUnsupportedOp, op)
	}
}

func (v Visitor) MatchText(field, text string) (drops.Expression, error) {
	col, err := v.Resolve(field, text)
	if err != nil {
		return nil, err
	}
	return v.TextMatcher(col, text), nil
}

func (v Visitor) HasID(ids []any) (drops.Expression, error) {
	return v.Ops.In(v.IDColumn, ids...), nil
}

func (v Visitor) GeoWithin(field string, box vector.GeoBox) (drops.Expression, error) {
	return v.GeoCompiler(field, box)
}

var _ vector.Visitor[drops.Expression] = Visitor{}

// IsNumeric reports whether v is a Go number. Backends use it to
// decide the cast on a schemaless payload accessor: a numeric operand
// wants a numeric comparison, anything else is compared as text.
func IsNumeric(v any) bool {
	switch v.(type) {
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return true
	default:
		return false
	}
}
