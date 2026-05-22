package qdrant

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
	Key    string          `json:"key,omitempty"`
	Match  *MatchCondition `json:"match,omitempty"`
	Range  *RangeCondition `json:"range,omitempty"`
	HasID  []any           `json:"has_id,omitempty"`
	IsEmpty *IsEmptyCondition `json:"is_empty,omitempty"`
	IsNull *IsNullCondition `json:"is_null,omitempty"`
	Geo    *GeoBoundingBox `json:"geo_bounding_box,omitempty"`
	Nested *Filter         `json:"filter,omitempty"`
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
func In(field string, values ...any) Condition {
	return Condition{Key: field, Match: &MatchCondition{Any: values}}
}

// NotIn tests payload.<field> ∉ values.
func NotIn(field string, values ...any) Condition {
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
func HasID(ids ...any) Condition { return Condition{HasID: ids} }

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
