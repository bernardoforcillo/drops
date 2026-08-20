package qdrant

import "encoding/json"

// Filter is Qdrant's predicate object: a tree of Must / Should /
// MustNot blocks containing Condition leaves.
type Filter struct {
	Must    []Condition `json:"must,omitempty"`
	Should  []Condition `json:"should,omitempty"`
	MustNot []Condition `json:"must_not,omitempty"`
}

// Condition is a single filter clause — either a key/value test, a
// range, an ID membership test, or a nested Filter.
type Condition struct {
	Key     string            `json:"key,omitempty"`
	Match   *MatchCondition   `json:"match,omitempty"`
	Range   *RangeCondition   `json:"range,omitempty"`
	HasID   []any             `json:"has_id,omitempty"`
	IsEmpty *IsEmptyCondition `json:"is_empty,omitempty"`
	IsNull  *IsNullCondition  `json:"is_null,omitempty"`
	Geo     *GeoBoundingBox   `json:"geo_bounding_box,omitempty"`
	Nested  *Filter           `json:"-"`
}

// MarshalJSON writes a sub-filter as the bare filter object Qdrant's
// grammar asks for.
//
// A condition is an untagged union and one of its variants is a Filter
// itself, so a sub-filter goes into the enclosing must / should /
// must_not array written out plainly — `{"must_not":[{"must":[…]}]}`.
// There is no key to put one under, and Filter is declared with
// serde's deny_unknown_fields, so a condition wrapped in a "filter"
// key matches no variant of the union and the whole request comes back
// 400.
func (c Condition) MarshalJSON() ([]byte, error) {
	if c.Nested != nil {
		return json.Marshal(c.Nested)
	}
	type plain Condition
	return json.Marshal(plain(c))
}

// UnmarshalJSON is the inverse: an object carrying any of the block
// keys is a sub-filter, anything else is a leaf.
func (c *Condition) UnmarshalJSON(data []byte) error {
	var blocks struct {
		Must    json.RawMessage `json:"must"`
		Should  json.RawMessage `json:"should"`
		MustNot json.RawMessage `json:"must_not"`
	}
	if err := json.Unmarshal(data, &blocks); err != nil {
		return err
	}
	if blocks.Must != nil || blocks.Should != nil || blocks.MustNot != nil {
		var f Filter
		if err := json.Unmarshal(data, &f); err != nil {
			return err
		}
		*c = Condition{Nested: &f}
		return nil
	}
	type plain Condition
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	*c = Condition(p)
	return nil
}

// MatchCondition expresses payload value equality / set membership /
// text-search. Exactly one of Value / Any / Text / Except should be
// non-zero.
type MatchCondition struct {
	Value  any    `json:"value,omitempty"`
	Any    []any  `json:"any,omitempty"`
	Except []any  `json:"except,omitempty"`
	Text   string `json:"text,omitempty"`
}

// RangeCondition expresses numeric range tests. All bounds are
// optional; nil-pointer means "open end".
type RangeCondition struct {
	Lt  *float64 `json:"lt,omitempty"`
	Lte *float64 `json:"lte,omitempty"`
	Gt  *float64 `json:"gt,omitempty"`
	Gte *float64 `json:"gte,omitempty"`
}

// IsEmptyCondition / IsNullCondition test for missing / null payloads.
type IsEmptyCondition struct {
	Key string `json:"key"`
}
type IsNullCondition struct {
	Key string `json:"key"`
}

// GeoBoundingBox is the rectangle-shape geo filter.
type GeoBoundingBox struct {
	TopLeft     GeoPoint `json:"top_left"`
	BottomRight GeoPoint `json:"bottom_right"`
}

type GeoPoint struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// Block constructors -----------------------------------------------

// Must builds a Filter where every condition is required.
func Must(conds ...Condition) *Filter { return &Filter{Must: conds} }

// Should builds a Filter where at least one condition must match.
func Should(conds ...Condition) *Filter { return &Filter{Should: conds} }

// MustNot builds a Filter where no condition may match.
func MustNot(conds ...Condition) *Filter { return &Filter{MustNot: conds} }

// All / Any are aliases that read naturally at call sites.
var All = Must
var Any = Should

// Condition constructors -------------------------------------------

// Eq tests payload.<field> == value.
func Eq(field string, value any) Condition {
	return Condition{Key: field, Match: &MatchCondition{Value: value}}
}

// In tests payload.<field> ∈ values.
//
// With no values it matches nothing, which is what membership of the
// empty set means. Qdrant cannot be asked that directly — every
// variant of its match union has a required key, so an empty match
// object is a 400 rather than a predicate — so it compiles to the
// must_not form instead.
func In(field string, values ...any) Condition {
	if len(values) == 0 {
		return matchNothing()
	}
	return Condition{Key: field, Match: &MatchCondition{Any: values}}
}

// NotIn tests payload.<field> ∉ values.
//
// With no values it matches everything, for the same reason In matches
// nothing: no point is excluded by the empty set.
func NotIn(field string, values ...any) Condition {
	if len(values) == 0 {
		return matchEverything()
	}
	return Condition{Key: field, Match: &MatchCondition{Except: values}}
}

// MatchText runs a tokenised full-text match.
func MatchText(field, text string) Condition {
	return Condition{Key: field, Match: &MatchCondition{Text: text}}
}

// RangeOpts is the argument bag for Range — pointer fields express
// "absent" cleanly.
type RangeOpts struct {
	Lt  *float64
	Lte *float64
	Gt  *float64
	Gte *float64
}

// Range tests payload.<field> against a numeric range.
//
//	qdrant.Range("created_at", qdrant.RangeOpts{Gte: qdrant.F(1700000000)})
func Range(field string, opts RangeOpts) Condition {
	return Condition{
		Key: field,
		Range: &RangeCondition{
			Lt: opts.Lt, Lte: opts.Lte,
			Gt: opts.Gt, Gte: opts.Gte,
		},
	}
}

// F is a helper that returns a pointer to v — useful when filling
// RangeOpts' pointer fields inline.
func F(v float64) *float64 { return &v }

// HasID tests that the point's ID is in the given set.
//
// With no ids it matches nothing, and getting that right matters more
// here than anywhere else in this file. Condition.HasID carries
// `omitempty`, so an empty set is dropped from the object entirely and
// the condition renders as `{}` — which in Qdrant's grammar is a
// condition that constrains nothing, and therefore one that matches
// every point in the collection. Passed to DeleteByFilter, the
// obvious use of a lookup that came back empty, that deletes the
// collection.
func HasID(ids ...any) Condition {
	if len(ids) == 0 {
		return matchNothing()
	}
	return Condition{HasID: ids}
}

// Qdrant's grammar has no literal true or false. A filter with no
// clauses constrains nothing, so it is the term that matches every
// point, and a must_not over it is the term that matches none.
func matchEverything() Condition { return Condition{Nested: &Filter{}} }

func matchNothing() Condition {
	return Condition{Nested: &Filter{MustNot: []Condition{matchEverything()}}}
}

// IsEmpty tests that payload.<field> is missing or empty.
func IsEmpty(field string) Condition {
	return Condition{IsEmpty: &IsEmptyCondition{Key: field}}
}

// IsNull tests that payload.<field> is null.
func IsNull(field string) Condition {
	return Condition{IsNull: &IsNullCondition{Key: field}}
}

// GeoIn tests that payload.<field> falls inside the bounding box.
func GeoIn(field string, topLeft, bottomRight GeoPoint) Condition {
	return Condition{
		Key: field,
		Geo: &GeoBoundingBox{TopLeft: topLeft, BottomRight: bottomRight},
	}
}

// Nest wraps a Filter as a Condition so it can sit inside another
// block — useful for OR-of-ANDs and similar shapes.
func Nest(f *Filter) Condition { return Condition{Nested: f} }
