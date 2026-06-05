package pg

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/bernardoforcillo/drops"
)

// ErrNoRows is returned by One when the query produced no rows.
var ErrNoRows = errors.New("drops/pg: no rows in result set")

// scanAll consumes rows into dest, which must be a pointer to a slice
// of structs or pointer-to-structs. Mapping rules:
//   - struct field tag `drop:"col"` is honoured (use `drop:"-"` to skip)
//   - otherwise, both the field name and its snake_case form match
//   - embedded structs are walked
//   - unmatched columns are scanned into a discard sink
func scanAll(rows drops.Rows, dest any) error {
	defer rows.Close()

	rv := reflect.ValueOf(dest)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return fmt.Errorf("drops/pg: All requires a non-nil pointer to slice, got %T", dest)
	}
	slice := rv.Elem()
	if slice.Kind() != reflect.Slice {
		return fmt.Errorf("drops/pg: All requires a pointer to slice, got *%s", slice.Kind())
	}
	elemType := slice.Type().Elem()
	isPtr := elemType.Kind() == reflect.Ptr
	structType := elemType
	if isPtr {
		structType = elemType.Elem()
	}
	if structType.Kind() != reflect.Struct {
		return fmt.Errorf("drops/pg: slice element must be struct or *struct, got %s", structType.Kind())
	}

	cols, err := rows.Columns()
	if err != nil {
		return err
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

// scanOne consumes the first row into dest, a pointer to a struct.
// It returns ErrNoRows if no row is available.
func scanOne(rows drops.Rows, dest any) error {
	defer rows.Close()

	rv := reflect.ValueOf(dest)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return fmt.Errorf("drops/pg: One requires a non-nil pointer to struct, got %T", dest)
	}
	elem := rv.Elem()
	if elem.Kind() != reflect.Struct {
		return fmt.Errorf("drops/pg: One requires a pointer to struct, got *%s", elem.Kind())
	}

	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return ErrNoRows
	}
	if err := scanRowInto(rows, elem, cols, fieldMap(elem.Type())); err != nil {
		return err
	}
	return rows.Err()
}

func scanRowInto(rows drops.Rows, structVal reflect.Value, cols []string, fields map[string][]int) error {
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

// fieldMap is cached because reflection over a struct type is the same
// for every row of the same query.
var fieldMapCache sync.Map // map[reflect.Type]map[string][]int

func fieldMap(t reflect.Type) map[string][]int {
	if v, ok := fieldMapCache.Load(t); ok {
		return v.(map[string][]int)
	}
	m := map[string][]int{}
	var walk func(reflect.Type, []int)
	walk = func(t reflect.Type, prefix []int) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			idx := append(append([]int(nil), prefix...), i)
			tag := f.Tag.Get("drop")
			if tag == "-" {
				continue
			}
			if tag != "" {
				// Honour the comma-separated form
				// (`drop:"col,opt,opt=val"`) used by AutoTable so the
				// scanner and the auto-declared schema agree on the
				// column name.
				name := tag
				if j := strings.IndexByte(tag, ','); j >= 0 {
					name = tag[:j]
				}
				m[name] = idx
				continue
			}
			if f.Anonymous && f.Type.Kind() == reflect.Struct {
				walk(f.Type, idx)
				continue
			}
			m[f.Name] = idx
			m[camelCase(f.Name)] = idx
		}
	}
	walk(t, nil)
	fieldMapCache.Store(t, m)
	return m
}

// camelCase converts PascalCase to camelCase. It treats runs of
// capitals as a single word so the second word's first letter is
// the boundary marker:
//
//	"UserID"     → "userId"
//	"HTTPStatus" → "httpStatus"
//	"Name"       → "name"
//
// Used as the fallback column-name match for untagged struct fields.
func camelCase(s string) string {
	if s == "" {
		return ""
	}
	// First pass: identify word boundaries via the snake_case logic,
	// then re-stitch with the second-word-onwards title-cased.
	type word struct{ start, end int }
	var words []word
	startW := 0
	for i := 1; i < len(s); i++ {
		c := s[i]
		isUpper := c >= 'A' && c <= 'Z'
		if isUpper {
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
			// Lowercase the entire first word.
			for i := w.start; i < w.end; i++ {
				c := s[i]
				if c >= 'A' && c <= 'Z' {
					c += 'a' - 'A'
				}
				b.WriteByte(c)
			}
			continue
		}
		// Capitalise the first letter, lowercase the rest.
		first := s[w.start]
		b.WriteByte(first) // already uppercase by construction
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
