package drops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"
)

// ErrNoRows is returned by [ScanOne] and [One] when the result set is
// empty. The dialect packages carry their own sentinel of the same name
// for queries that go through their own scanners; a caller that reaches
// the row cursor through [RowSource] gets this one.
var ErrNoRows = errors.New("drops: no rows in result set")

// RowSource is anything that runs itself and hands back a cursor. Every
// dialect's SelectBuilder already has this method, which is what lets
// [All] and [One] be generic over builders the root package cannot
// import.
type RowSource interface {
	Rows(ctx context.Context) (Rows, error)
}

// All runs src and returns its rows as a []T, using the column mapping
// rules described on [ScanOne]. It is the typed form of the ad-hoc
// query: T is usually a projection struct belonging to no table, which
// is the shape a join or an aggregate produces and the shape an Entity
// cannot describe.
//
// T may be a struct, a *struct (each element is allocated), or — for a
// single-column result — a scalar, so All[int64] over SELECT id is
// spelled the way it reads. An empty result is an empty slice and a nil
// error.
//
// The cursor is closed before All returns, whichever way it returns.
func All[T any](ctx context.Context, src RowSource) ([]T, error) {
	rows, err := src.Rows(ctx)
	if err != nil {
		return nil, err
	}
	var out []T
	if err := ScanAll(rows, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// One runs src and returns its first row, or ErrNoRows when there is
// none — the convention [ScanOne] follows and every dialect's
// EntityQuery.One mirrors with its own sentinel, so emptiness stays an
// error rather than a zero value the caller has to recognise.
//
// T may be a struct or a scalar, including a pointer to a scalar:
// One[*string] is how a nullable column comes back. It may not be a
// pointer to a struct — ErrNoRows already says "no row", and a nil *T
// would be a second, quieter way to say the same thing.
//
// The cursor is closed before One returns, whichever way it returns.
func One[T any](ctx context.Context, src RowSource) (T, error) {
	var out T
	rows, err := src.Rows(ctx)
	if err != nil {
		return out, err
	}
	err = ScanOne(rows, &out)
	return out, err
}

// ScanOne consumes the first row from rows into dest (a non-nil pointer
// to a struct), returning ErrNoRows when the cursor is empty. It is the
// shared reflection scanner used by every SQL-like dialect, so the
// struct-to-column mapping rules are identical whichever backend a
// query ran against:
//
//   - a `drop:"col"` field tag names the column (`drop:"-"` skips the
//     field; a comma-separated tag like `drop:"col,opt"` uses the part
//     before the first comma);
//   - otherwise both the exported field name and its camelCase form match;
//   - embedded structs are walked, exported or not, unless the embedded
//     type is itself a scalar destination — a time.Time or an
//     sql.Scanner receives a column rather than lending its fields;
//   - a name reachable at two depths belongs to the shallower field, and
//     at equal depth a tagged name beats a field name, which beats a
//     camelCase form. Two fields left genuinely tied — Href and HRef,
//     say, which share the camelCase form href — resolve to the one
//     declared first; tag one of them to say which you meant.
//
// Neither half of a mismatch between struct and result set is an error:
// a column with no field is scanned into a discard sink, and a field
// with no column keeps its zero value. A single query text can then
// serve destinations of differing width. What is left to the driver is
// the value conversion — notably a NULL into a non-pointer field, which
// database/sql reports as an error rather than a zero value.
//
// dest may also be a pointer to a scalar, for a single-column result;
// see [IsScalarDest] for what counts as one.
func ScanOne(rows Rows, dest any) error {
	defer rows.Close()

	rv := reflect.ValueOf(dest)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return fmt.Errorf("drops: ScanOne requires a non-nil pointer to struct, got %T", dest)
	}
	elem := rv.Elem()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	scalar := isScalarDest(elem.Type())
	if !scalar && elem.Kind() != reflect.Struct {
		return fmt.Errorf("drops: ScanOne requires a pointer to struct or to a scalar, got %T", dest)
	}
	if scalar && len(cols) > 1 {
		return fmt.Errorf("drops: ScanOne into *%s needs a single-column result, got %d columns %v",
			elem.Type(), len(cols), cols)
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return ErrNoRows
	}
	if scalar {
		if err := rows.Scan(dest); err != nil {
			return err
		}
		return rows.Err()
	}
	if err := scanRowInto(rows, elem, cols, fieldMap(elem.Type())); err != nil {
		return err
	}
	return rows.Err()
}

// ScanAll consumes every row from rows into dest, a non-nil pointer to
// a slice of structs, *structs or — for a single-column result —
// scalars. Mapping rules are those of ScanOne.
func ScanAll(rows Rows, dest any) error {
	defer rows.Close()

	rv := reflect.ValueOf(dest)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return fmt.Errorf("drops: ScanAll requires a non-nil pointer to slice, got %T", dest)
	}
	slice := rv.Elem()
	if slice.Kind() != reflect.Slice {
		return fmt.Errorf("drops: ScanAll requires a pointer to slice, got *%s", slice.Kind())
	}
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	elemType := slice.Type().Elem()
	if isScalarDest(elemType) {
		if len(cols) > 1 {
			return fmt.Errorf("drops: ScanAll into *[]%s needs a single-column result, got %d columns %v",
				elemType, len(cols), cols)
		}
		for rows.Next() {
			ptr := reflect.New(elemType)
			if err := rows.Scan(ptr.Interface()); err != nil {
				return err
			}
			slice.Set(reflect.Append(slice, ptr.Elem()))
		}
		return rows.Err()
	}
	isPtr := elemType.Kind() == reflect.Ptr
	structType := elemType
	if isPtr {
		structType = elemType.Elem()
	}
	if structType.Kind() != reflect.Struct {
		return fmt.Errorf("drops: slice element must be struct or *struct, got %s", structType.Kind())
	}
	fields := fieldMap(structType)
	for rows.Next() {
		ptr := reflect.New(structType)
		if err := scanRowInto(rows, ptr.Elem(), cols, fields); err != nil {
			return err
		}
		if isPtr {
			slice.Set(reflect.Append(slice, ptr))
		} else {
			slice.Set(reflect.Append(slice, ptr.Elem()))
		}
	}
	return rows.Err()
}

// StructFields returns the column-name → field-index-path map the
// scanner uses; [ScanOne] documents the rules it follows. Dialect
// entity layers use it to bind struct fields to columns without
// re-implementing the reflection walk. The result is cached per type
// and must not be mutated.
func StructFields(t reflect.Type) map[string][]int { return fieldMap(t) }

func scanRowInto(rows Rows, structVal reflect.Value, cols []string, fields map[string][]int) error {
	targets := make([]any, len(cols))
	var discard any
	for i, c := range cols {
		idx, ok := fields[c]
		if !ok {
			targets[i] = &discard
			continue
		}
		targets[i] = structVal.FieldByIndex(idx).Addr().Interface()
	}
	return rows.Scan(targets...)
}

// fieldMapCache memoises the struct→column index map per type.
var fieldMapCache sync.Map // map[reflect.Type]map[string][]int

// candidate is one field competing for a column name. Names collide
// often enough — through embedding, and through the camelCase alias
// every field carries — that the winner cannot be "whichever the walk
// reached last": that makes the mapping depend on the order fields
// happen to be declared in. Shallower wins, as it does for ordinary Go
// field access; at equal depth the more deliberate spelling wins.
type candidate struct {
	idx      []int
	depth    int
	priority int // 0 tag, 1 field name, 2 camelCase form
}

// beats reports whether c should displace the candidate already holding
// a name. A tie leaves the incumbent, so the first field declared wins.
func (c candidate) beats(cur candidate) bool {
	if c.depth != cur.depth {
		return c.depth < cur.depth
	}
	return c.priority < cur.priority
}

func fieldMap(t reflect.Type) map[string][]int {
	if v, ok := fieldMapCache.Load(t); ok {
		return v.(map[string][]int)
	}
	best := map[string]candidate{}
	claim := func(name string, c candidate) {
		if cur, ok := best[name]; ok && !c.beats(cur) {
			return
		}
		best[name] = c
	}
	var walk func(reflect.Type, []int, int)
	walk = func(t reflect.Type, prefix []int, depth int) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			idx := append(append([]int(nil), prefix...), i)
			tag := f.Tag.Get("drop")
			if tag == "-" {
				continue
			}
			// An embedded struct is walked before the export check:
			// the fields promoted out of an unexported embedded type
			// are themselves exported and settable, and a struct that
			// factors its timestamps into an unexported `audit` is
			// exactly the case that must not silently lose them.
			if tag == "" && f.Anonymous && f.Type.Kind() == reflect.Struct && !isScalarDest(f.Type) {
				walk(f.Type, idx, depth+1)
				continue
			}
			if !f.IsExported() {
				continue
			}
			if tag != "" {
				name := tag
				if j := strings.IndexByte(tag, ','); j >= 0 {
					name = tag[:j]
				}
				claim(name, candidate{idx, depth, 0})
				continue
			}
			claim(f.Name, candidate{idx, depth, 1})
			claim(camelCase(f.Name), candidate{idx, depth, 2})
		}
	}
	walk(t, nil, 0)
	m := make(map[string][]int, len(best))
	for name, c := range best {
		m[name] = c.idx
	}
	fieldMapCache.Store(t, m)
	return m
}

// camelCase converts PascalCase to camelCase, treating runs of capitals
// as a single word ("UserID"→"userId", "HTTPStatus"→"httpStatus").
func camelCase(s string) string {
	if s == "" {
		return ""
	}
	type word struct{ start, end int }
	var words []word
	startW := 0
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			prev := s[i-1]
			prevLower := prev >= 'a' && prev <= 'z'
			nextLower := i+1 < len(s) && s[i+1] >= 'a' && s[i+1] <= 'z'
			if prevLower || nextLower {
				words = append(words, word{startW, i})
				startW = i
			}
		}
	}
	words = append(words, word{startW, len(s)})

	var b strings.Builder
	b.Grow(len(s))
	for wi, w := range words {
		if wi == 0 {
			for i := w.start; i < w.end; i++ {
				c := s[i]
				if c >= 'A' && c <= 'Z' {
					c += 'a' - 'A'
				}
				b.WriteByte(c)
			}
			continue
		}
		b.WriteByte(s[w.start]) // already uppercase by construction
		for i := w.start + 1; i < w.end; i++ {
			c := s[i]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			b.WriteByte(c)
		}
	}
	return b.String()
}

// IsScalarDest reports whether a destination type should be scanned as
// a single column rather than mapped field-by-field. Exported so the
// dialect packages, which carry their own scanners, decide the same way.
//
// The distinction is not simply "is it a struct". A query returning one
// column is the most ordinary thing there is — SELECT count(*) — and
// requiring callers to wrap the result in a throwaway struct made drops
// unable to express it. But some scalars *are* structs: time.Time is
// the obvious one, and any type implementing sql.Scanner has said it
// knows how to receive a column value, which is exactly the claim that
// should stop drops from looking inside it. Pointers are followed, so
// *time.Time is a scalar and *T for a plain struct T is not — the
// latter is a mistake worth naming as one rather than sending down the
// single-column path.
func IsScalarDest(t reflect.Type) bool { return isScalarDest(t) }

func isScalarDest(t reflect.Type) bool {
	for t.Kind() == reflect.Ptr {
		if reflect.PointerTo(t).Implements(scannerType) {
			return true
		}
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return true
	}
	if t == timeType {
		return true
	}
	return reflect.PointerTo(t).Implements(scannerType)
}

var (
	timeType    = reflect.TypeOf(time.Time{})
	scannerType = reflect.TypeOf((*sql.Scanner)(nil)).Elem()
)
