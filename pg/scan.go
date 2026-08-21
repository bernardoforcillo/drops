package pg

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/bernardoforcillo/drops"
)

// ErrNoRows is returned by One when the query produced no rows.
var ErrNoRows = errors.New("drops/pg: no rows in result set")

// ScanOne consumes the first row from rows into dest (pointer to
// struct), returning ErrNoRows when the cursor is empty. Exported
// for code generators (cmd/dropsgen sql) that want the
// reflection scanner without going through a builder.
func ScanOne(rows drops.Rows, dest any) error { return scanOne(rows, dest) }

// ScanAll consumes every row from rows into dest (pointer to
// slice of struct or *struct). Exported for the same reason as
// ScanOne.
func ScanAll(rows drops.Rows, dest any) error { return scanAll(rows, dest) }

// scanAll consumes rows into dest, which must be a pointer to a slice
// of structs or pointer-to-structs. The struct-to-column mapping is
// the one [drops.ScanOne] documents — a `drop:"col"` tag names the
// column, an untagged field matches its own name and its camelCase
// form, embedded structs are walked whether or not the embedded type
// is exported, and an unmatched column goes to a discard sink.
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
	scalar := drops.IsScalarDest(elem.Type())
	if !scalar && elem.Kind() != reflect.Struct {
		return fmt.Errorf("drops/pg: One requires a pointer to struct or to a scalar, got *%s", elem.Kind())
	}

	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	if scalar && len(cols) > 1 {
		return fmt.Errorf("drops/pg: One into *%s needs a single-column result, got %d columns %v",
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

// fieldMap resolves a struct type to its column → field-index map.
//
// It is the root scanner's map, not a second one: pg's builders and
// the root's [drops.ScanOne] scan the same structs, so a struct that
// bound differently depending on which entry point a caller reached
// would be a trap rather than a dialect difference. The root memoises
// the result per type and documents the matching rules, including the
// two this package used to get wrong — an embedded unexported struct
// lends its exported fields instead of being skipped, and a name
// reachable at two depths belongs to the shallower field.
func fieldMap(t reflect.Type) map[string][]int { return drops.StructFields(t) }
