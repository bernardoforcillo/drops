package clickhouse

import (
	"fmt"
	"reflect"

	"github.com/bernardoforcillo/drops"
)

// scanAll consumes rows into dest (pointer to a slice of struct,
// *struct or — for a single-column result — scalar, as
// [drops.ScanAll] accepts). The struct-to-column mapping is the one
// [drops.ScanOne]
// documents — a `drop:"col"` tag names the column, an untagged field
// matches its own name and its camelCase form, embedded structs are
// walked whether or not the embedded type is exported, and an
// unmatched column goes to a discard sink.
func scanAll(rows drops.Rows, dest any) error {
	defer rows.Close()
	rv := reflect.ValueOf(dest)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return fmt.Errorf("drops/clickhouse: All requires a non-nil pointer to slice, got %T", dest)
	}
	slice := rv.Elem()
	if slice.Kind() != reflect.Slice {
		return fmt.Errorf("drops/clickhouse: All requires a pointer to slice, got *%s", slice.Kind())
	}
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	elemType := slice.Type().Elem()
	// A single-column result into a slice of scalars, the shape SELECT
	// id produces. It has to be decided before the struct unwrap below,
	// because some scalars are structs: a []time.Time taken apart
	// field-by-field matches no column at all and comes back as a slice
	// of zero timestamps with a nil error, which is the same query
	// through the same rows answering differently depending on whether
	// the caller reached [drops.ScanAll] or this one.
	if drops.IsScalarDest(elemType) {
		if len(cols) > 1 {
			return fmt.Errorf("drops/clickhouse: All into *[]%s needs a single-column result, got %d columns %v",
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
		return fmt.Errorf("drops/clickhouse: slice element must be struct or *struct, got %s", structType.Kind())
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

// scanOne consumes the first row into dest (pointer to struct).
func scanOne(rows drops.Rows, dest any) error {
	defer rows.Close()
	rv := reflect.ValueOf(dest)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return fmt.Errorf("drops/clickhouse: One requires a non-nil pointer to struct, got %T", dest)
	}
	elem := rv.Elem()
	scalar := drops.IsScalarDest(elem.Type())
	if !scalar && elem.Kind() != reflect.Struct {
		return fmt.Errorf("drops/clickhouse: One requires a pointer to struct or to a scalar, got *%s", elem.Kind())
	}
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	if scalar && len(cols) > 1 {
		return fmt.Errorf("drops/clickhouse: One into *%s needs a single-column result, got %d columns %v",
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
// It is the root scanner's map, not a second one: a struct scanned
// through this package and the same struct scanned through
// [drops.ScanOne] must bind the same way, or which entry point a caller
// reached decides which columns arrive. This package used to keep a
// fork of the walk, and the fork got two things wrong — it skipped an
// unexported embedded struct instead of promoting its exported fields,
// so a type that factors its timestamps into an unexported audit lost
// them silently, and it walked into an embedded time.Time instead of
// handing it a column. The root memoises per type and documents the
// rules.
func fieldMap(t reflect.Type) map[string][]int { return drops.StructFields(t) }
