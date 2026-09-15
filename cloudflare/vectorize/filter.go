package vectorize

import (
	"fmt"

	"github.com/bernardoforcillo/drops/vector"
)

// Filter is Vectorize's metadata predicate: a flat object mapping a
// property to a comparison.
//
//	{"lang": {"$eq": "it"}, "published_at": {"$gte": 1700000000}}
//
// Listing two properties means AND. There is no other connective —
// no $or, no $not — which is the constraint the whole of
// [CompileFilter] is organised around.
type Filter map[string]map[string]any

// The Vectorize comparison operators.
const (
	OpEq  = "$eq"
	OpNe  = "$ne"
	OpIn  = "$in"
	OpNin = "$nin"
	OpLt  = "$lt"
	OpLte = "$lte"
	OpGt  = "$gt"
	OpGte = "$gte"
)

// CompileFilter turns a portable
// [github.com/bernardoforcillo/drops/vector.Filter] into the
// Vectorize filter object. The zero Filter compiles to nil, so the
// query goes out with no filter key at all.
//
// What it refuses, and why it refuses rather than approximates:
//
//   - Or has no equivalent. Emulating it would mean running one query
//     per branch and merging, which changes what topK means and what
//     the scores rank against — a different query wearing the same
//     name.
//   - Not has no equivalent either. A negated leaf with a direct
//     opposite is rewritten (Not(Eq) becomes $ne, Not(In) becomes
//     $nin, and the range operators invert), because that is a
//     rewrite rather than an approximation. Negating a conjunction
//     is not: De Morgan turns it into a disjunction, and there is
//     no disjunction.
//   - IsNull, MatchText, HasID and GeoWithin have no operator at all.
//
// Each refusal is
// [github.com/bernardoforcillo/drops/vector.ErrUnsupportedOp], named
// so the message says which operator and which field.
func CompileFilter(f vector.Filter) (Filter, error) {
	out, ok, err := vector.Compile[Filter](f, visitor{})
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return out, nil
}

// visitor compiles portable filter nodes into Vectorize filters.
type visitor struct{}

// And merges the parts into one object. Two branches constraining the
// same property with the same operator are a contradiction Vectorize
// cannot express — {"a": {"$eq": 1}} and {"a": {"$eq": 2}} would
// collapse to whichever came last — so it is reported rather than
// silently resolved.
func (visitor) And(parts []Filter) (Filter, error) {
	out := Filter{}
	for _, p := range parts {
		for field, cmp := range p {
			if out[field] == nil {
				out[field] = map[string]any{}
			}
			for op, val := range cmp {
				if existing, clash := out[field][op]; clash {
					return nil, fmt.Errorf(
						"drops/cloudflare/vectorize: %q is constrained by %s twice (%v and %v); Vectorize's filter is one comparison per operator per field",
						field, op, existing, val)
				}
				out[field][op] = val
			}
		}
	}
	return out, nil
}

func (visitor) Or([]Filter) (Filter, error) {
	return nil, fmt.Errorf("%w: Vectorize's metadata filter has no $or", vector.ErrUnsupportedOp)
}

// Not rewrites a single negated leaf into its opposite operator and
// refuses everything else. A one-field, one-operator filter is
// exactly what a negated leaf compiles to, so the shape check is also
// the "is this a leaf" check.
func (visitor) Not(part Filter) (Filter, error) {
	if len(part) != 1 {
		return nil, fmt.Errorf("%w: Vectorize has no $not, and negating a conjunction needs the $or it also lacks", vector.ErrUnsupportedOp)
	}
	for field, cmp := range part {
		if len(cmp) != 1 {
			return nil, fmt.Errorf("%w: Vectorize has no $not, and %q carries more than one comparison to invert", vector.ErrUnsupportedOp, field)
		}
		for op, val := range cmp {
			opposite, ok := invert(op)
			if !ok {
				return nil, fmt.Errorf("%w: Vectorize has no $not and no opposite of %s", vector.ErrUnsupportedOp, op)
			}
			return Filter{field: {opposite: val}}, nil
		}
	}
	return nil, fmt.Errorf("%w: cannot negate an empty filter", vector.ErrUnsupportedOp)
}

// invert returns the operator meaning the opposite of op.
func invert(op string) (string, bool) {
	switch op {
	case OpEq:
		return OpNe, true
	case OpNe:
		return OpEq, true
	case OpIn:
		return OpNin, true
	case OpNin:
		return OpIn, true
	case OpLt:
		return OpGte, true
	case OpGte:
		return OpLt, true
	case OpLte:
		return OpGt, true
	case OpGt:
		return OpLte, true
	default:
		return "", false
	}
}

func (visitor) Compare(op vector.Op, field string, value any) (Filter, error) {
	if field == "" {
		return nil, fmt.Errorf("drops/cloudflare/vectorize: %s has no field", op)
	}
	var name string
	switch op {
	case vector.OpEq:
		name = OpEq
	case vector.OpNe:
		name = OpNe
	case vector.OpLt:
		name = OpLt
	case vector.OpLte:
		name = OpLte
	case vector.OpGt:
		name = OpGt
	case vector.OpGte:
		name = OpGte
	default:
		return nil, fmt.Errorf("%w: vectorize compare %q", vector.ErrUnsupportedOp, op)
	}
	// The range operators compare numerically; a string bound would
	// be accepted by the API and then match nothing.
	switch op {
	case vector.OpLt, vector.OpLte, vector.OpGt, vector.OpGte:
		if !isNumeric(value) {
			return nil, fmt.Errorf("%w: %s on %q needs a number, got %T",
				vector.ErrNotNumeric, op, field, value)
		}
	}
	return Filter{field: {name: value}}, nil
}

func (visitor) Set(op vector.Op, field string, values []any) (Filter, error) {
	if field == "" {
		return nil, fmt.Errorf("drops/cloudflare/vectorize: %s has no field", op)
	}
	// An empty list is what an unset request parameter compiles to.
	// The portable contract fixes its meaning — In matches nothing,
	// NotIn matches everything — and Vectorize can express neither
	// with an empty array, so say so rather than send a filter that
	// means the opposite.
	if len(values) == 0 {
		switch op {
		case vector.OpIn:
			return nil, fmt.Errorf("%w: an empty In matches nothing, and Vectorize's $in has no empty form to say so", vector.ErrUnsupportedOp)
		case vector.OpNotIn:
			// Matches everything: no constraint at all is exactly
			// right, and costs nothing.
			return Filter{}, nil
		}
	}
	switch op {
	case vector.OpIn:
		return Filter{field: {OpIn: values}}, nil
	case vector.OpNotIn:
		return Filter{field: {OpNin: values}}, nil
	default:
		return nil, fmt.Errorf("%w: vectorize set %q", vector.ErrUnsupportedOp, op)
	}
}

func (visitor) Null(op vector.Op, field string) (Filter, error) {
	return nil, fmt.Errorf("%w: Vectorize cannot test %q for null", vector.ErrUnsupportedOp, field)
}

func (visitor) MatchText(field, _ string) (Filter, error) {
	return nil, fmt.Errorf("%w: Vectorize has no text match on %q; index the token as its own metadata property and use Eq", vector.ErrUnsupportedOp, field)
}

func (visitor) HasID([]any) (Filter, error) {
	return nil, fmt.Errorf("%w: Vectorize filters metadata only; fetch by ID with Index.GetByIDs", vector.ErrUnsupportedOp)
}

func (visitor) GeoWithin(field string, _ vector.GeoBox) (Filter, error) {
	return nil, fmt.Errorf("%w: Vectorize has no geo filter on %q; store lat and lon as numeric metadata and use two ranges", vector.ErrUnsupportedOp, field)
}

func isNumeric(v any) bool {
	switch v.(type) {
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return true
	default:
		return false
	}
}
