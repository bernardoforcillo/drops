package pg

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/bernardoforcillo/drops"
)

// Keyset (cursor) pagination — the only correct way to page through
// large result sets. OFFSET-based pagination scans every row up to
// the offset, so the 1000th page costs 1000× more than the first;
// a keyset cursor anchors the next page to a strict inequality
// against the last row of the previous page, so every page costs
// the same regardless of depth.
//
//	// Declare the cursor shape once — typically per page-able endpoint.
//	spec := pg.NewCursorSpec(
//	    pg.OrderKey{Col: Posts.CreatedAt, Desc: true},
//	    pg.OrderKey{Col: Posts.ID,        Desc: true}, // tiebreaker
//	)
//
//	// First page — no cursor.
//	var rows []Post
//	err := db.Select(/* cols */).From(Posts).
//	    OrderByCursor(spec).
//	    Limit(50).
//	    Iter(ctx, /* scan into &rows */ )
//
//	// Build the cursor from the last row and pass it to the next page.
//	last := rows[len(rows)-1]
//	cur, _ := pg.EncodeCursor(spec, last.CreatedAt, last.ID)
//	err = db.Select(/* cols */).From(Posts).
//	    OrderByCursor(spec).
//	    AfterCursor(spec, cur).
//	    Limit(50).
//	    Iter(ctx, /* scan into &rows */)
//
// The cursor is opaque — base64 URL-safe encoding of a typed JSON
// payload — so it can be passed through query strings without
// escaping. Use BeforeCursor for backward paging.

// Cursor is an opaque page marker — obtain one from EncodeCursor and
// pass it to SelectBuilder.AfterCursor / BeforeCursor.
type Cursor string

// OrderKey is one (column, direction) pair making up a cursor shape.
// Combine multiple OrderKey values in a CursorSpec to form a stable
// ordering — the typical pattern is (sort_column DESC, primary_key
// DESC) so rows with identical sort values still produce a unique
// page boundary.
type OrderKey struct {
	// Col is the column referenced by both the ORDER BY clause
	// and the keyset WHERE comparison.
	Col ColRef

	// Desc selects descending order. Defaults to ascending.
	Desc bool

	// Nulls picks the NULLS FIRST / NULLS LAST clause. Leaving
	// it empty inherits PG's default ("NULLS LAST" for ASC,
	// "NULLS FIRST" for DESC) — PostgreSQL sorts NULL as the
	// largest value, so the default puts it at whichever end
	// "largest" falls on.
	//
	// The keyset guard is built against whatever this says, so a
	// nullable key column paginates correctly either way; see
	// [keysetStrict]. Set it explicitly when the ORDER BY is also
	// written by hand somewhere, so the two cannot drift apart.
	Nulls NullsOrdering
}

// NullsOrdering is the NULLS FIRST / NULLS LAST modifier on an
// ORDER BY clause.
type NullsOrdering string

const (
	// NullsDefault inherits PostgreSQL's per-direction default.
	NullsDefault NullsOrdering = ""
	// NullsFirst pushes NULL values to the start of the order.
	NullsFirst NullsOrdering = "FIRST"
	// NullsLast pushes NULL values to the end of the order.
	NullsLast NullsOrdering = "LAST"
)

// CursorSpec is the ordered list of keys that defines the cursor
// shape — used by OrderByCursor and AfterCursor so the same shape
// drives both the ORDER BY and the keyset WHERE.
type CursorSpec struct {
	Keys []OrderKey
}

// NewCursorSpec returns a CursorSpec carrying the supplied keys in
// declaration order. The last key should be a primary key (or
// otherwise unique column) so equal leading values still resolve
// to a deterministic page boundary.
func NewCursorSpec(keys ...OrderKey) CursorSpec {
	return CursorSpec{Keys: append([]OrderKey(nil), keys...)}
}

// EncodeCursor builds a cursor from values matching the spec, one
// per key in declaration order. Returns an error when values is the
// wrong length or contains an unsupported type.
//
// A nil — or a nil pointer, which is how a nullable column arrives in
// Go — is a value like any other here: the row it came from has a
// place in the ORDER BY, so it has to be usable as a page boundary.
// See [keysetStrict] for the comparison it produces.
func EncodeCursor(spec CursorSpec, values ...any) (Cursor, error) {
	if len(values) != len(spec.Keys) {
		return "", fmt.Errorf("drops/pg: EncodeCursor: %d values for %d keys", len(values), len(spec.Keys))
	}
	w := cursorWire{V: make([]cursorVal, len(values))}
	for i, v := range values {
		cv, err := encodeCursorValue(v)
		if err != nil {
			return "", err
		}
		w.V[i] = cv
	}
	body, err := json.Marshal(w)
	if err != nil {
		return "", err
	}
	return Cursor(base64.RawURLEncoding.EncodeToString(body)), nil
}

// Decode returns the values held inside the cursor, in spec order.
// Unknown / corrupt cursors return an error so callers can treat
// the request as "first page" (or reject the input outright).
func (c Cursor) Decode() ([]any, error) {
	if c == "" {
		return nil, errors.New("drops/pg: empty cursor")
	}
	body, err := base64.RawURLEncoding.DecodeString(string(c))
	if err != nil {
		return nil, fmt.Errorf("drops/pg: cursor base64: %w", err)
	}
	var w cursorWire
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("drops/pg: cursor JSON: %w", err)
	}
	out := make([]any, len(w.V))
	for i, cv := range w.V {
		v, err := decodeCursorValue(cv)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// cursorWire is the on-the-wire shape of a cursor — a list of
// typed values so decoding round-trips int64 / time.Time / []byte
// to their original Go types rather than to JSON's lossy float64 /
// string defaults.
type cursorWire struct {
	V []cursorVal `json:"v"`
}

type cursorVal struct {
	T string          `json:"t"`
	V json.RawMessage `json:"v"`
}

const (
	cTypeInt    = "i"
	cTypeUint   = "u"
	cTypeFloat  = "f"
	cTypeString = "s"
	cTypeBool   = "b"
	cTypeTime   = "t"
	cTypeBytes  = "x"
	cTypeNull   = "n"
)

func encodeCursorValue(v any) (cursorVal, error) {
	switch x := v.(type) {
	case nil:
		return cursorVal{T: cTypeNull, V: json.RawMessage("null")}, nil
	case int:
		b, _ := json.Marshal(int64(x))
		return cursorVal{T: cTypeInt, V: b}, nil
	case int8:
		b, _ := json.Marshal(int64(x))
		return cursorVal{T: cTypeInt, V: b}, nil
	case int16:
		b, _ := json.Marshal(int64(x))
		return cursorVal{T: cTypeInt, V: b}, nil
	case int32:
		b, _ := json.Marshal(int64(x))
		return cursorVal{T: cTypeInt, V: b}, nil
	case int64:
		b, _ := json.Marshal(x)
		return cursorVal{T: cTypeInt, V: b}, nil
	case uint:
		b, _ := json.Marshal(uint64(x))
		return cursorVal{T: cTypeUint, V: b}, nil
	case uint8:
		b, _ := json.Marshal(uint64(x))
		return cursorVal{T: cTypeUint, V: b}, nil
	case uint16:
		b, _ := json.Marshal(uint64(x))
		return cursorVal{T: cTypeUint, V: b}, nil
	case uint32:
		b, _ := json.Marshal(uint64(x))
		return cursorVal{T: cTypeUint, V: b}, nil
	case uint64:
		b, _ := json.Marshal(x)
		return cursorVal{T: cTypeUint, V: b}, nil
	case float32:
		b, _ := json.Marshal(float64(x))
		return cursorVal{T: cTypeFloat, V: b}, nil
	case float64:
		b, _ := json.Marshal(x)
		return cursorVal{T: cTypeFloat, V: b}, nil
	case string:
		b, _ := json.Marshal(x)
		return cursorVal{T: cTypeString, V: b}, nil
	case bool:
		b, _ := json.Marshal(x)
		return cursorVal{T: cTypeBool, V: b}, nil
	case time.Time:
		b, _ := json.Marshal(x.UTC().Format(time.RFC3339Nano))
		return cursorVal{T: cTypeTime, V: b}, nil
	case []byte:
		return cursorVal{T: cTypeBytes, V: json.RawMessage(`"` + base64.RawURLEncoding.EncodeToString(x) + `"`)}, nil
	default:
		// A nullable column is a pointer field on the row struct, so
		// the value handed back from the last row of a page is a
		// *string or a *time.Time rather than the bare type. Follow
		// it — a nil one is the NULL the keyset guard knows how to
		// page past.
		if rv := reflect.ValueOf(v); rv.Kind() == reflect.Ptr {
			if rv.IsNil() {
				return cursorVal{T: cTypeNull, V: json.RawMessage("null")}, nil
			}
			return encodeCursorValue(rv.Elem().Interface())
		}
		return cursorVal{}, fmt.Errorf("drops/pg: cursor value type %T not supported", v)
	}
}

func decodeCursorValue(cv cursorVal) (any, error) {
	switch cv.T {
	case cTypeNull:
		return nil, nil
	case cTypeInt:
		var x int64
		if err := json.Unmarshal(cv.V, &x); err != nil {
			return nil, err
		}
		return x, nil
	case cTypeUint:
		var x uint64
		if err := json.Unmarshal(cv.V, &x); err != nil {
			return nil, err
		}
		return x, nil
	case cTypeFloat:
		var x float64
		if err := json.Unmarshal(cv.V, &x); err != nil {
			return nil, err
		}
		return x, nil
	case cTypeString:
		var x string
		if err := json.Unmarshal(cv.V, &x); err != nil {
			return nil, err
		}
		return x, nil
	case cTypeBool:
		var x bool
		if err := json.Unmarshal(cv.V, &x); err != nil {
			return nil, err
		}
		return x, nil
	case cTypeTime:
		var s string
		if err := json.Unmarshal(cv.V, &s); err != nil {
			return nil, err
		}
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return nil, err
		}
		return t, nil
	case cTypeBytes:
		var s string
		if err := json.Unmarshal(cv.V, &s); err != nil {
			return nil, err
		}
		return base64.RawURLEncoding.DecodeString(s)
	default:
		return nil, fmt.Errorf("drops/pg: cursor value type %q unknown", cv.T)
	}
}

// OrderByCursor sets ORDER BY according to spec. The same spec must
// be passed to AfterCursor / BeforeCursor on subsequent pages so
// the keyset WHERE matches.
func (s *SelectBuilder) OrderByCursor(spec CursorSpec) *SelectBuilder {
	for _, k := range spec.Keys {
		s.orderBys = append(s.orderBys, orderKeyExpr(k))
	}
	return s
}

// AfterCursor appends the keyset WHERE clause for forward paging
// past cursor c. Decoding errors are stored on the builder and
// returned by Rows / All / One so callers don't have to guard every
// chained call.
func (s *SelectBuilder) AfterCursor(spec CursorSpec, c Cursor) *SelectBuilder {
	if c == "" {
		return s
	}
	values, err := c.Decode()
	if err != nil {
		s.err = err
		s.wheres = append(s.wheres, falseExpr)
		return s
	}
	if len(values) != len(spec.Keys) {
		s.err = fmt.Errorf("drops/pg: cursor has %d values, spec wants %d", len(values), len(spec.Keys))
		s.wheres = append(s.wheres, falseExpr)
		return s
	}
	s.wheres = append(s.wheres, keysetWhere(spec, values, true))
	return s
}

// BeforeCursor is the reverse — appends WHERE constraints that page
// backward past c. Combine with a reversed ORDER BY (or reverse the
// returned slice on the caller side) to present the page in the
// expected order.
func (s *SelectBuilder) BeforeCursor(spec CursorSpec, c Cursor) *SelectBuilder {
	if c == "" {
		return s
	}
	values, err := c.Decode()
	if err != nil {
		s.err = err
		s.wheres = append(s.wheres, falseExpr)
		return s
	}
	if len(values) != len(spec.Keys) {
		s.err = fmt.Errorf("drops/pg: cursor has %d values, spec wants %d", len(values), len(spec.Keys))
		s.wheres = append(s.wheres, falseExpr)
		return s
	}
	s.wheres = append(s.wheres, keysetWhere(spec, values, false))
	return s
}

// orderKeyExpr renders one ORDER BY fragment honouring direction
// and NULLS placement.
func orderKeyExpr(k OrderKey) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		k.Col.WriteSQL(b)
		if k.Desc {
			b.WriteString(" DESC")
		} else {
			b.WriteString(" ASC")
		}
		switch k.Nulls {
		case NullsFirst:
			b.WriteString(" NULLS FIRST")
		case NullsLast:
			b.WriteString(" NULLS LAST")
		}
	})
}

// keysetWhere produces the row-wise expansion of the lexicographic
// "(k1, k2, ..., kn) > (v1, v2, ..., vn)" comparison:
//
//	   (k1 > v1)
//	OR (k1 = v1 AND k2 > v2)
//	OR (k1 = v1 AND k2 = v2 AND k3 > v3)
//	...
//
// The strict comparator on the i-th key flips per OrderKey.Desc and
// per forward / backward paging direction. Both it and the equality on
// the leading keys are NULL-aware: a cursor value of NULL compared
// with = or > yields NULL rather than true, so a plain comparison
// would drop the very row the walk is standing on and every row after
// it — an empty page that reads exactly like the end of the result
// set, on every page from then on.
func keysetWhere(spec CursorSpec, values []any, forward bool) drops.Expression {
	ors := make([]drops.Expression, 0, len(spec.Keys))
	for i := range spec.Keys {
		strict := keysetStrict(spec.Keys[i], values[i], forward)
		if strict == nil {
			// Nothing sorts past this key's value in this direction.
			// The disjunct is absent rather than false, and the later
			// keys still contribute theirs.
			continue
		}
		ands := make([]drops.Expression, 0, i+1)
		for j := 0; j < i; j++ {
			ands = append(ands, keysetEq(spec.Keys[j], values[j]))
		}
		ands = append(ands, strict)
		if len(ands) == 1 {
			ors = append(ors, ands[0])
		} else {
			ors = append(ors, And(ands...))
		}
	}
	if len(ors) == 1 {
		return ors[0]
	}
	// Or() of nothing renders FALSE, which is the right answer: the
	// cursor is sitting on the last row the ordering can reach.
	return Or(ors...)
}

// keysetEq is the equality term for a leading key. NULL = NULL is
// NULL, so a NULL cursor value has to be matched with IS NULL.
func keysetEq(k OrderKey, v any) drops.Expression {
	c := k.Col.col()
	if v == nil {
		return IsNull(c)
	}
	return Eq(c, v)
}

// keysetStrict is the "strictly past this value" term for one key, or
// nil when nothing can be past it.
//
// Unlike MySQL, PostgreSQL lets the ORDER BY say where the NULLs go,
// so the guard is written against the placement the spec asks for —
// [nullsFirst] resolves it — rather than against a fixed one. For a
// key ordered ascending with NULLS LAST, which is PostgreSQL's default:
//
//	value NULL     → nothing follows        (nil)
//	value non-NULL → col > v OR col IS NULL (the NULLs are still ahead)
//
// and with NULLS FIRST the two swap round: past a NULL lies every
// non-NULL row, and past a value lie only larger values.
//
// Paging backward reverses the order, which reverses the direction and
// the NULL placement together — "before v under (ASC, NULLS LAST)" is
// "after v under (DESC, NULLS FIRST)".
//
// The IS NULL disjunct is emitted only for a column the schema
// declares nullable: it is one more range for the planner to merge,
// and on a NOT NULL column it can never match.
func keysetStrict(k OrderKey, v any, forward bool) drops.Expression {
	c := k.Col.col()
	asc := !k.Desc
	first := nullsFirst(k)
	if !forward {
		asc = !asc
		first = !first
	}
	if v == nil {
		// Past a NULL lies everything else, but only when the NULLs
		// come first in this direction.
		if first {
			return IsNotNull(c)
		}
		return nil
	}
	var strict drops.Expression
	if asc {
		strict = Gt(c, v)
	} else {
		strict = Lt(c, v)
	}
	if !first && c.IsNullable() {
		return Or(strict, IsNull(c))
	}
	return strict
}

// nullsFirst resolves a key's NULL placement to a plain "do the NULLs
// come first", applying PostgreSQL's default — NULL sorts as the
// largest value, so it is last under ASC and first under DESC — when
// the spec does not say.
func nullsFirst(k OrderKey) bool {
	switch k.Nulls {
	case NullsFirst:
		return true
	case NullsLast:
		return false
	default:
		return k.Desc
	}
}

// falseExpr is a guaranteed-false predicate emitted when a cursor
// decode error is stored on the builder. It contains no dynamic
// content so malformed cursor payloads cannot inject SQL.
var falseExpr = drops.ExprFunc(func(b *drops.Builder) {
	b.WriteString("FALSE")
})
