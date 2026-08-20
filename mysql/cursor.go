package mysql

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/bernardoforcillo/drops"
)

// Keyset (cursor) pagination — the only correct way to page through
// large result sets. OFFSET-based pagination reads and discards every
// row up to the offset, so the 1000th page costs 1000× more than the
// first; a keyset cursor anchors the next page to a strict inequality
// against the last row of the previous page, so every page costs the
// same regardless of depth.
//
//	// Declare the cursor shape once — typically per page-able endpoint.
//	spec := mysql.NewCursorSpec(
//	    mysql.OrderKey{Col: Posts.CreatedAt, Desc: true},
//	    mysql.OrderKey{Col: Posts.ID,        Desc: true}, // tiebreaker
//	)
//
//	// First page — no cursor.
//	var rows []Post
//	err := db.Select(cols...).From(Posts).
//	    OrderByCursor(spec).
//	    Limit(50).
//	    All(ctx, &rows)
//
//	// Build the cursor from the last row and pass it to the next page.
//	last := rows[len(rows)-1]
//	cur, _ := mysql.EncodeCursor(spec, last.CreatedAt, last.ID)
//	err = db.Select(cols...).From(Posts).
//	    OrderByCursor(spec).
//	    AfterCursor(spec, cur).
//	    Limit(50).
//	    All(ctx, &rows)
//
// The cursor is opaque — base64 URL-safe encoding of a typed JSON
// payload — so it can travel through a query string without escaping.
// Use BeforeCursor for backward paging.
//
// # What MySQL makes different
//
// Three things, and each one changes the emitted SQL rather than just
// the documentation.
//
// The guard is never written as a row comparison. The lexicographic
// keyset condition has a compact spelling — (a, b) > (?, ?) — and
// MySQL's optimiser has turned that into a range scan since 5.7.3.
// MariaDB's does not: on 10.11 the row-constructor form plans as
// `type: index, possible_keys: NULL` and reads the index from the
// beginning on every page. Measured on the live server, a 50-row page
// at offset 4800 of a 5000-row table cost 4849 index reads that way
// against 49 for the expanded form below. drops targets both servers,
// so it emits only the expansion, which both plan as a range.
//
// There is no NULLS FIRST / NULLS LAST. Both servers reject the
// clause outright (MariaDB 10.11 answers a 1064 syntax error), and
// both sort NULL as the smallest value: first under ASC, last under
// DESC, with no way to ask for anything else. That placement is
// therefore what the guard is built against — see keysetStrict — and
// [OrderKey] has no Nulls field for PostgreSQL parity to hang on.
//
// The tiebreaker is checked rather than merely recommended. Both
// servers default a text column to a case- and accent-insensitive
// collation — utf8mb4_general_ci on MariaDB 10.11, utf8mb4_0900_ai_ci
// on MySQL 8 — under which 'alice', 'Alice' and 'ALICE' are one value,
// as are 'cafe' and 'café'. A walk whose last key is such a column and
// nothing else does not just risk skipping duplicates in theory — a
// strict `name > 'alice'` steps over every casing of the name at
// once. So a spec whose final key is not backed by a NOT NULL unique
// constraint is refused; see [CursorSpec.AllowNonUniqueKey] to
// override that, and the doc on [NewCursorSpec] for the fix.
//
// Trailing space is where the two servers part: utf8mb4_general_ci is
// PAD SPACE, so 'a' = 'a ' on MariaDB, while MySQL 8's default is NO
// PAD and the two differ. That widens or narrows the group by one more
// case; it does not change the conclusion, because case alone already
// makes the group bigger than one row.

// Cursor is an opaque page marker — obtain one from EncodeCursor and
// pass it to SelectBuilder.AfterCursor / BeforeCursor.
type Cursor string

// OrderKey is one (column, direction) pair making up a cursor shape.
// Combine multiple OrderKey values in a CursorSpec to form a stable
// ordering — the typical pattern is (sort_column DESC, primary_key
// DESC) so rows with identical sort values still produce a unique
// page boundary.
//
// PostgreSQL's OrderKey carries a Nulls field. This one does not:
// neither MySQL nor MariaDB accepts NULLS FIRST / NULLS LAST, and both
// place NULL first under ASC and last under DESC. The keyset guard is
// written against that fixed placement, so a nullable key column
// paginates correctly without a knob — the cost is that you cannot ask
// for the other placement at all.
type OrderKey struct {
	// Col is the column referenced by both the ORDER BY clause and
	// the keyset WHERE comparison.
	Col ColRef

	// Desc selects descending order. Defaults to ascending.
	Desc bool
}

// CursorSpec is the ordered list of keys that defines the cursor
// shape — used by OrderByCursor and AfterCursor so the same shape
// drives both the ORDER BY and the keyset WHERE.
type CursorSpec struct {
	Keys []OrderKey

	// allowNonUnique disables the tiebreaker check. Unexported so
	// the only way to reach it is the named method, which documents
	// what is being given up.
	allowNonUnique bool
}

// NewCursorSpec returns a CursorSpec carrying the supplied keys in
// declaration order.
//
// The last key must be a NOT NULL column the schema declares unique —
// a primary key, a UNIQUE column, or (with every one of its columns
// among the keys) a UNIQUE index. Without one a page boundary is not a
// point but a group, and the strict comparison that opens the next
// page steps over the whole group: the walk silently loses rows. That
// is a real hazard on MySQL rather than a pedantic one, because its
// default collation makes 'alice', 'Alice' and 'ALICE' one group.
//
// The usual fix is to append the primary key:
//
//	spec := mysql.NewCursorSpec(
//	    mysql.OrderKey{Col: Posts.Title},
//	    mysql.OrderKey{Col: Posts.ID}, // tiebreaker
//	)
//
// A spec that fails the check still builds — the refusal lands when
// the spec is used, as a guaranteed-false predicate carrying the
// reason, because a paginator that returns nothing is a bug report and
// one that skips rows is a data-loss incident. [CursorSpec.AllowNonUniqueKey]
// opts out.
func NewCursorSpec(keys ...OrderKey) CursorSpec {
	return CursorSpec{Keys: append([]OrderKey(nil), keys...)}
}

// AllowNonUniqueKey returns a copy of the spec with the tiebreaker
// check disabled, for the caller who knows the sort key is unique in
// a way the schema does not say — a column made unique by an index
// drops did not declare, or a table written once and never updated.
// The walk may skip rows if that is not true.
func (s CursorSpec) AllowNonUniqueKey() CursorSpec {
	s.allowNonUnique = true
	return s
}

// EncodeCursor builds a cursor from values matching the spec, one per
// key in declaration order. Returns an error when values is the wrong
// length or contains an unsupported type.
func EncodeCursor(spec CursorSpec, values ...any) (Cursor, error) {
	if len(values) != len(spec.Keys) {
		return "", fmt.Errorf("drops/mysql: EncodeCursor: %d values for %d keys", len(values), len(spec.Keys))
	}
	w := cursorWire{D: cursorDialect, K: spec.fingerprint(), V: make([]cursorVal, len(values))}
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
// Unknown / corrupt cursors return an error so callers can treat the
// request as "first page" (or reject the input outright).
//
// A cursor issued by another dialect's package is rejected here rather
// than silently decoded: the payload carries the backend that stamped
// it, and a drops/pg cursor reaching a MySQL store means the caller
// has crossed two stores' page tokens, which no comparison can repair.
func (c Cursor) Decode() ([]any, error) {
	w, err := c.decodeWire()
	if err != nil {
		return nil, err
	}
	return w.values()
}

// decodeFor is Decode plus the spec check: the cursor must have been
// stamped by the same ordering it is about to be compared against.
// Handing page 2 of "newest first" to a query ordered "oldest first"
// otherwise returns rows that look plausible and are wrong.
func (c Cursor) decodeFor(spec CursorSpec) ([]any, error) {
	w, err := c.decodeWire()
	if err != nil {
		return nil, err
	}
	if w.K != "" && w.K != spec.fingerprint() {
		return nil, errors.New("drops/mysql: cursor was issued for a different ORDER BY")
	}
	vals, err := w.values()
	if err != nil {
		return nil, err
	}
	if len(vals) != len(spec.Keys) {
		return nil, fmt.Errorf("drops/mysql: cursor has %d values, spec wants %d", len(vals), len(spec.Keys))
	}
	return vals, nil
}

func (c Cursor) decodeWire() (cursorWire, error) {
	var w cursorWire
	if c == "" {
		return w, errors.New("drops/mysql: empty cursor")
	}
	body, err := base64.RawURLEncoding.DecodeString(string(c))
	if err != nil {
		return w, fmt.Errorf("drops/mysql: cursor base64: %w", err)
	}
	if err := json.Unmarshal(body, &w); err != nil {
		return w, fmt.Errorf("drops/mysql: cursor JSON: %w", err)
	}
	if w.D != cursorDialect {
		return w, fmt.Errorf("drops/mysql: cursor was stamped %q, not %q", w.D, cursorDialect)
	}
	return w, nil
}

// cursorWire is the on-the-wire shape of a cursor: the stamp of the
// backend that issued it, the fingerprint of the ordering it was taken
// against, and a list of typed values so decoding round-trips int64 /
// time.Time / []byte to their original Go types rather than to JSON's
// lossy float64 / string defaults.
type cursorWire struct {
	D string      `json:"d"`
	K string      `json:"k"`
	V []cursorVal `json:"v"`
}

func (w cursorWire) values() ([]any, error) {
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

type cursorVal struct {
	T string          `json:"t"`
	V json.RawMessage `json:"v"`
}

// cursorDialect stamps every cursor this package issues. It is the
// dialect name rather than a version: the payload shape is what the
// stamp guards, and it is shared with no other backend.
const cursorDialect = "mysql"

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
		// A nullable column is usually scanned into a pointer, so a
		// cursor over one arrives here as *string or *time.Time. The
		// pointer is the Go spelling of "this may be NULL" and not part
		// of the value, so it is unwrapped: nil becomes the NULL the
		// guard knows how to walk past, and anything else becomes the
		// value it points at.
		rv := reflect.ValueOf(v)
		if rv.Kind() == reflect.Ptr {
			if rv.IsNil() {
				return cursorVal{T: cTypeNull, V: json.RawMessage("null")}, nil
			}
			return encodeCursorValue(rv.Elem().Interface())
		}
		return cursorVal{}, fmt.Errorf("drops/mysql: cursor value type %T not supported", v)
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
		return nil, fmt.Errorf("drops/mysql: cursor value type %q unknown", cv.T)
	}
}

// OrderByCursor sets ORDER BY according to spec. The same spec must be
// passed to AfterCursor / BeforeCursor on subsequent pages so the
// keyset WHERE matches.
//
// A spec that cannot be paginated safely poisons the query here, on
// the first page, rather than waiting for the second: the check does
// not depend on the cursor, so there is no reason to let a walk start
// that cannot finish.
func (s *SelectBuilder) OrderByCursor(spec CursorSpec) *SelectBuilder {
	if err := spec.validate(); err != nil {
		s.wheres = append(s.wheres, cursorErrExpr(err))
		return s
	}
	for _, k := range spec.Keys {
		s.orderBys = append(s.orderBys, s.deferredOrderKey(k))
	}
	return s
}

// AfterCursor appends the keyset WHERE clause for forward paging past
// cursor c. Decoding errors fall through as a guaranteed-false
// predicate carrying the reason, so callers do not have to guard every
// chained call — an empty page with the reason in the statement text
// beats a page of wrong rows.
func (s *SelectBuilder) AfterCursor(spec CursorSpec, c Cursor) *SelectBuilder {
	return s.cursorWhere(spec, c, true)
}

// BeforeCursor is the reverse — appends WHERE constraints that page
// backward past c. Combine with a reversed ORDER BY (or reverse the
// returned slice on the caller side) to present the page in the
// expected order.
func (s *SelectBuilder) BeforeCursor(spec CursorSpec, c Cursor) *SelectBuilder {
	return s.cursorWhere(spec, c, false)
}

func (s *SelectBuilder) cursorWhere(spec CursorSpec, c Cursor, forward bool) *SelectBuilder {
	if c == "" {
		return s
	}
	if err := spec.validate(); err != nil {
		s.wheres = append(s.wheres, cursorErrExpr(err))
		return s
	}
	values, err := c.decodeFor(spec)
	if err != nil {
		s.wheres = append(s.wheres, cursorErrExpr(err))
		return s
	}
	s.wheres = append(s.wheres, drops.ExprFunc(func(b *drops.Builder) {
		b.Append(keysetWhere(s.rebindSpec(spec), values, forward))
	}))
	return s
}

// deferredOrderKey renders one ORDER BY fragment, restating the key
// against the builder's FROM table first.
//
// The restatement cannot happen when OrderByCursor is called:
// [OrderKey.Col] is whichever handle the caller held, and the FROM
// table it has to agree with may still be several chained calls away.
// So the expression closes over the builder and asks at WriteSQL time,
// by which point From has run.
func (s *SelectBuilder) deferredOrderKey(k OrderKey) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.Append(orderKeyExpr(s.rebindSpec(CursorSpec{Keys: []OrderKey{k}}).Keys[0]))
	})
}

// rebindSpec restates each key with the handle the builder's FROM
// table hands out for that column, so the reference qualifies with the
// relation the statement names rather than with another instance of
// the same table — which is error 1054 in either direction.
//
// It moves a key only when the statement names one instance of the
// table. A self-join names two on purpose and the spec picks between
// them; restating there would erase the distinction the alias exists
// to draw. A key on some other table's column is left alone for the
// same reason: it is a deliberate reference, not a second handle.
//
// Counting the instances is only possible over the relations drops
// renders itself — the FROM table and the joins. A source added with
// FromExpr is an expression drops re-emits without reading, and it can
// perfectly well be a second instance of the same table: a comma join
// on the alias, or a derived table carrying the alias's name. Moving a
// key then picks the wrong instance, and picks it silently — both
// relations exist, so the ORDER BY and the keyset guard are valid SQL
// over the wrong rows. So any FromExpr at all makes the count unknown
// and the spec is left as it came.
func (s *SelectBuilder) rebindSpec(spec CursorSpec) CursorSpec {
	if s.from == nil || len(s.fromExprs) > 0 {
		return spec
	}
	for _, j := range s.joins {
		if j.table != nil && j.table.key() == s.from.key() {
			return spec
		}
	}
	keys := make([]OrderKey, len(spec.Keys))
	for i, k := range spec.Keys {
		keys[i] = k
		if k.Col == nil || k.Col.col() == nil {
			continue
		}
		c := k.Col.col()
		own := s.from.byName[c.name]
		if own != nil && own != c && own.key() == c.key() {
			keys[i].Col = own
		}
	}
	spec.Keys = keys
	return spec
}

// orderKeyExpr renders one ORDER BY fragment. There is no NULLS
// placement to render: see the package-level note on [OrderKey].
func orderKeyExpr(k OrderKey) drops.Expression {
	if k.Desc {
		return k.Col.col().Desc()
	}
	return k.Col.col().Asc()
}

// keysetWhere produces the row-wise expansion of the lexicographic
// "(k1, k2, ..., kn) > (v1, v2, ..., vn)" comparison:
//
//	   (k1 > v1)
//	OR (k1 = v1 AND k2 > v2)
//	OR (k1 = v1 AND k2 = v2 AND k3 > v3)
//	...
//
// It is written out rather than handed to MySQL as a row comparison
// because MariaDB's optimiser does not treat a row constructor as a
// range — the package doc has the measurement. The strict comparator
// on the i-th key flips per OrderKey.Desc and per forward / backward
// paging direction; both it and the equality on the leading keys are
// NULL-aware, because a NULL cursor value compared with = or > yields
// NULL and would drop the row that the walk is standing on.
func keysetWhere(spec CursorSpec, values []any, forward bool) drops.Expression {
	ors := make([]drops.Expression, 0, len(spec.Keys))
	for i := range spec.Keys {
		strict := keysetStrict(spec.Keys[i], values[i], forward)
		if strict == nil {
			// Nothing sorts past this key's value in this
			// direction — the disjunct is empty rather than false,
			// and later keys still contribute their own.
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
	// cursor sits on the last row the ordering can reach.
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
// Both servers sort NULL as the smallest value — first under ASC, last
// under DESC — with no clause to change it, so the walk treats NULL as
// a real position in the order rather than as an absence:
//
//	ascending,  value NULL     → col IS NOT NULL   (every non-NULL row follows)
//	ascending,  value non-NULL → col > v           (NULL rows are behind, and drop out)
//	descending, value NULL     → nothing follows   (nil)
//	descending, value non-NULL → col < v OR col IS NULL
//
// The trailing OR is emitted only for a column the schema declares
// nullable. It is not free — an unnecessary disjunct is one more range
// for the optimiser to merge — and on a NOT NULL column it can never
// match anything.
func keysetStrict(k OrderKey, v any, forward bool) drops.Expression {
	c := k.Col.col()
	asc := !k.Desc
	if !forward {
		asc = !asc
	}
	if asc {
		if v == nil {
			return IsNotNull(c)
		}
		return Gt(c, v)
	}
	if v == nil {
		return nil
	}
	if c.notNull {
		return Lt(c, v)
	}
	return Or(Lt(c, v), IsNull(c))
}

// validate reports why a spec cannot drive a keyset walk.
func (s CursorSpec) validate() error {
	if len(s.Keys) == 0 {
		return errors.New("drops/mysql: cursor spec has no keys")
	}
	for i, k := range s.Keys {
		if k.Col == nil || k.Col.col() == nil {
			return fmt.Errorf("drops/mysql: cursor spec key %d has no column", i)
		}
	}
	if s.allowNonUnique {
		return nil
	}
	if s.hasUniqueTail() {
		return nil
	}
	last := s.Keys[len(s.Keys)-1].Col.col()
	return fmt.Errorf("drops/mysql: cursor spec ends on %s, which the schema does not declare unique and NOT NULL; "+
		"add the primary key as a final OrderKey, or call AllowNonUniqueKey to page anyway",
		cursorColKey(last))
}

// hasUniqueTail reports whether the spec's keys pin a row down to one.
//
// Uniqueness has to come from the schema rather than from the last
// key alone: a two-column primary key is a valid tiebreaker only when
// both of its columns are keys. A nullable unique column is not one at
// all — MySQL allows any number of NULLs in a UNIQUE index, so the
// rows holding them tie with each other.
func (s CursorSpec) hasUniqueTail() bool {
	keys := make(map[string]bool, len(s.Keys))
	for _, k := range s.Keys {
		keys[cursorColKey(k.Col.col())] = true
	}
	last := s.Keys[len(s.Keys)-1].Col.col()
	if last.unique && last.notNull {
		return true
	}
	t := last.table
	if t == nil {
		return false
	}
	if pks := t.PrimaryKeyColumns(); len(pks) > 0 && coversColumns(keys, pks) {
		return true
	}
	for _, idx := range t.Indexes() {
		if !idx.unique {
			continue
		}
		cols := make([]*Column, len(idx.cols))
		for i, c := range idx.cols {
			cols[i] = c.col()
		}
		if coversColumns(keys, cols) {
			return true
		}
	}
	return false
}

// coversColumns reports whether every column is among the spec's keys
// and NOT NULL.
func coversColumns(keys map[string]bool, cols []*Column) bool {
	if len(cols) == 0 {
		return false
	}
	for _, c := range cols {
		if !c.notNull || !keys[cursorColKey(c)] {
			return false
		}
	}
	return true
}

// fingerprint is a short, stable digest of the ordering: the columns
// and their directions. It rides along in the cursor so a token taken
// under one ORDER BY is rejected by another instead of quietly
// producing a page from the wrong end of the table.
func (s CursorSpec) fingerprint() string {
	h := fnv.New64a()
	for _, k := range s.Keys {
		if k.Col == nil || k.Col.col() == nil {
			continue
		}
		_, _ = h.Write([]byte(cursorColKey(k.Col.col())))
		if k.Desc {
			_, _ = h.Write([]byte(":d|"))
		} else {
			_, _ = h.Write([]byte(":a|"))
		}
	}
	return strconv.FormatUint(h.Sum64(), 36)
}

// cursorColKey identifies a column by name rather than by pointer, so
// a handle reached through an aliased table matches the one it was
// copied from.
func cursorColKey(c *Column) string {
	if c.table == nil {
		return c.name
	}
	if c.table.database != "" {
		return c.table.database + "." + c.table.name + "." + c.name
	}
	return c.table.name + "." + c.name
}

// cursorErrExpr renders as a guaranteed-false predicate that also
// carries an error tag, so a malformed cursor or an unpaginatable spec
// shows up in the statement text — and therefore in the query hook,
// the slow log and EXPLAIN — instead of vanishing.
func cursorErrExpr(err error) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("FALSE /* drops cursor: ")
		b.WriteString(defuseComment(err.Error()))
		b.WriteString(" */")
	})
}

// defuseComment blunts the two sequences that can turn the tag back
// into SQL.
//
// A cursor is designed to travel through a query string, so its bytes
// come from the client and the decode error quotes them back: an
// unknown type tag is reported as `cursor value type "..." unknown`. A
// tag of `*/ OR 1=1 --` would close the comment and leave the rest
// standing as a predicate that matched every row. MySQL adds a second
// escape the other dialects do not have: a block comment opening with
// `/*!` is not a comment at all but a version-gated fragment the
// server executes.
func defuseComment(s string) string {
	s = strings.ReplaceAll(s, "*/", "* /")
	return strings.ReplaceAll(s, "/*", "/ *")
}
