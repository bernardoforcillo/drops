package vector

import "fmt"

// Op identifies a node in a Filter tree — either a logical connective
// (And / Or / Not) or a leaf predicate.
type Op string

// Logical connectives.
const (
	OpAnd Op = "and"
	OpOr  Op = "or"
	OpNot Op = "not"
)

// Leaf predicates.
const (
	OpEq        Op = "eq"
	OpNe        Op = "ne"
	OpIn        Op = "in"
	OpNotIn     Op = "not_in"
	OpLt        Op = "lt"
	OpLte       Op = "lte"
	OpGt        Op = "gt"
	OpGte       Op = "gte"
	OpIsNull    Op = "is_null"
	OpIsNotNull Op = "is_not_null"
	OpMatchText Op = "match_text"
	OpHasID     Op = "has_id"
	OpGeoWithin Op = "geo_within"
)

// Filter is a portable predicate over a record's payload — the one
// filter vocabulary the pgvector, ClickHouse and Qdrant backends all
// compile from.
//
// It is a plain struct rather than an interface so a Filter is
// comparable-by-inspection, trivially serialisable, and exhaustively
// switchable by a backend that wants to bypass [Compile].
//
// Build one with the constructors — [And], [Or], [Not], [Eq], [In],
// [Gte] and friends — rather than by hand: they are what keeps the
// field/value/children triple consistent per Op.
type Filter struct {
	// Op is the node kind. Always set.
	Op Op

	// Field is the payload key a leaf predicate tests. Empty for
	// connectives, for OpHasID (which tests the record ID) and for
	// nothing else.
	Field string

	// Value holds the single operand of Eq / Ne / Lt / Lte / Gt / Gte
	// / MatchText.
	Value any

	// Values holds the operand list of In / NotIn / HasID.
	Values []any

	// Children holds the operands of And / Or / Not. Not always has
	// exactly one.
	Children []Filter

	// Box is the rectangle of a GeoWithin test.
	Box *GeoBox
}

// GeoPoint is a latitude/longitude pair in degrees.
type GeoPoint struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// GeoBox is an axis-aligned geographic rectangle.
type GeoBox struct {
	TopLeft     GeoPoint `json:"top_left"`
	BottomRight GeoPoint `json:"bottom_right"`
}

// IsZero reports whether f is the zero Filter — the shape a caller
// gets from `var f vector.Filter` and the one [Query.Filter] uses to
// mean "no filter".
func (f Filter) IsZero() bool { return f.Op == "" }

// Connectives -------------------------------------------------------

// And requires every sub-filter to match.
//
// Zero sub-filters — including the case where every argument was the
// zero Filter — yield the zero Filter, i.e. no constraint at all. That
// is what makes And the natural way to assemble optional predicates:
//
//	f := vector.And(base, maybeLangFilter(req), maybeDateFilter(req))
//
// A single sub-filter collapses to that sub-filter, so And never adds
// a redundant layer of nesting to the compiled output.
func And(subs ...Filter) Filter { return connective(OpAnd, subs) }

// Or requires at least one sub-filter to match. Like [And], zero
// sub-filters yield the zero Filter — no constraint. Neither
// constructor can express an unsatisfiable predicate; to return
// nothing, don't run the query.
func Or(subs ...Filter) Filter { return connective(OpOr, subs) }

func connective(op Op, subs []Filter) Filter {
	kept := make([]Filter, 0, len(subs))
	for _, s := range subs {
		if !s.IsZero() {
			kept = append(kept, s)
		}
	}
	switch len(kept) {
	case 0:
		return Filter{}
	case 1:
		return kept[0]
	}
	return Filter{Op: op, Children: kept}
}

// Not negates a filter. Negating the zero Filter yields the zero
// Filter — "not nothing" stays "no constraint" rather than becoming
// an unsatisfiable one.
func Not(sub Filter) Filter {
	if sub.IsZero() {
		return Filter{}
	}
	return Filter{Op: OpNot, Children: []Filter{sub}}
}

// Leaves ------------------------------------------------------------

// Eq tests payload[field] == value.
func Eq(field string, value any) Filter {
	return Filter{Op: OpEq, Field: field, Value: value}
}

// Ne tests payload[field] != value.
func Ne(field string, value any) Filter {
	return Filter{Op: OpNe, Field: field, Value: value}
}

// In tests payload[field] ∈ values. An empty list matches nothing.
func In(field string, values ...any) Filter {
	return Filter{Op: OpIn, Field: field, Values: expandSlice(values)}
}

// NotIn tests payload[field] ∉ values. An empty list matches
// everything.
func NotIn(field string, values ...any) Filter {
	return Filter{Op: OpNotIn, Field: field, Values: expandSlice(values)}
}

// Lt / Lte / Gt / Gte test payload[field] against a numeric bound.
func Lt(field string, value any) Filter  { return Filter{Op: OpLt, Field: field, Value: value} }
func Lte(field string, value any) Filter { return Filter{Op: OpLte, Field: field, Value: value} }
func Gt(field string, value any) Filter  { return Filter{Op: OpGt, Field: field, Value: value} }
func Gte(field string, value any) Filter { return Filter{Op: OpGte, Field: field, Value: value} }

// Between tests low <= payload[field] <= high. It expands to an And of
// two bounds so every backend gets it for free.
func Between(field string, low, high any) Filter {
	return And(Gte(field, low), Lte(field, high))
}

// IsNull tests that payload[field] is absent or null.
func IsNull(field string) Filter { return Filter{Op: OpIsNull, Field: field} }

// IsNotNull tests that payload[field] is present and non-null.
func IsNotNull(field string) Filter { return Filter{Op: OpIsNotNull, Field: field} }

// MatchText runs a substring / tokenised text match on payload[field].
// The exact semantics are the backend's: Qdrant tokenises through the
// field's text index, the SQL backends use a case-insensitive LIKE.
func MatchText(field, text string) Filter {
	return Filter{Op: OpMatchText, Field: field, Value: text}
}

// HasID tests that the record's own ID is in the given set — the one
// predicate that addresses the record rather than its payload. An
// empty list matches nothing.
func HasID(ids ...any) Filter {
	return Filter{Op: OpHasID, Values: expandSlice(ids)}
}

// GeoWithin tests that the lat/lon in payload[field] falls inside box.
func GeoWithin(field string, box GeoBox) Filter {
	return Filter{Op: OpGeoWithin, Field: field, Box: &box}
}

// expandSlice unwraps a lone slice argument into its elements so
// In("id", ids) reads the same as In("id", ids...) — matching the
// convenience pg.In and clickhouse.In already offer.
func expandSlice(values []any) []any {
	if len(values) != 1 {
		return values
	}
	switch v := values[0].(type) {
	case []any:
		return v
	case []string:
		return toAnySlice(v)
	case []int:
		return toAnySlice(v)
	case []int64:
		return toAnySlice(v)
	case []float64:
		return toAnySlice(v)
	default:
		return values
	}
}

func toAnySlice[T any](in []T) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

// Compilation -------------------------------------------------------

// Visitor turns Filter nodes into a backend's own representation R —
// a qdrant.Condition, a drops.Expression, whatever the store speaks.
//
// [Compile] drives it: a Visitor never walks the tree itself, so the
// traversal order, the child recursion and the degenerate cases are
// decided once here instead of once per backend.
type Visitor[R any] interface {
	// And / Or combine already-compiled children. Compile always
	// passes at least two parts: empty connectives collapse to the
	// zero Filter and single-child ones collapse to that child, both
	// before the visitor is ever reached.
	And(parts []R) (R, error)
	Or(parts []R) (R, error)
	// Not negates an already-compiled child.
	Not(part R) (R, error)

	// Compare handles Eq / Ne / Lt / Lte / Gt / Gte.
	Compare(op Op, field string, value any) (R, error)
	// Set handles In / NotIn.
	Set(op Op, field string, values []any) (R, error)
	// Null handles IsNull / IsNotNull.
	Null(op Op, field string) (R, error)
	// MatchText handles a text match on a payload field.
	MatchText(field, text string) (R, error)
	// HasID handles an ID membership test.
	HasID(ids []any) (R, error)
	// GeoWithin handles a bounding-box test.
	GeoWithin(field string, box GeoBox) (R, error)
}

// Compile walks f depth-first and hands each node to v, returning the
// backend representation of the whole tree.
//
// The zero Filter compiles to the zero R and ok=false, which is how a
// backend distinguishes "no predicate at all" (emit no WHERE clause,
// send no filter object) from "a predicate that happens to match
// everything".
func Compile[R any](f Filter, v Visitor[R]) (out R, ok bool, err error) {
	if f.IsZero() {
		return out, false, nil
	}
	out, err = compileNode(f, v)
	if err != nil {
		return out, false, err
	}
	return out, true, nil
}

func compileNode[R any](f Filter, v Visitor[R]) (zero R, err error) {
	switch f.Op {
	case OpAnd, OpOr:
		parts := make([]R, 0, len(f.Children))
		for _, c := range f.Children {
			p, cErr := compileNode(c, v)
			if cErr != nil {
				return zero, cErr
			}
			parts = append(parts, p)
		}
		if len(parts) == 1 {
			return parts[0], nil
		}
		if f.Op == OpAnd {
			return v.And(parts)
		}
		return v.Or(parts)

	case OpNot:
		if len(f.Children) != 1 {
			return zero, fmt.Errorf("drops/vector: Not expects exactly 1 child, got %d", len(f.Children))
		}
		part, cErr := compileNode(f.Children[0], v)
		if cErr != nil {
			return zero, cErr
		}
		return v.Not(part)

	case OpEq, OpNe, OpLt, OpLte, OpGt, OpGte:
		return v.Compare(f.Op, f.Field, f.Value)

	case OpIn, OpNotIn:
		return v.Set(f.Op, f.Field, f.Values)

	case OpIsNull, OpIsNotNull:
		return v.Null(f.Op, f.Field)

	case OpMatchText:
		text, _ := f.Value.(string)
		return v.MatchText(f.Field, text)

	case OpHasID:
		return v.HasID(f.Values)

	case OpGeoWithin:
		if f.Box == nil {
			return zero, fmt.Errorf("drops/vector: GeoWithin on %q has no bounding box", f.Field)
		}
		return v.GeoWithin(f.Field, *f.Box)

	default:
		return zero, fmt.Errorf("drops/vector: unknown filter op %q", f.Op)
	}
}
