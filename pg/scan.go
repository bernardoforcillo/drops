package pg

import (
	"errors"
	"reflect"

	"github.com/bernardoforcillo/drops"
)

// ErrNoRows is returned by One when the query produced no rows. The
// scanner underneath is the root one, which raises [drops.ErrNoRows];
// the sentinel is translated back at this boundary so the pg spelling
// stays the one pg callers already test for. sqlite/select.go does the
// same translation for the same reason.
var ErrNoRows = errors.New("drops/pg: no rows in result set")

// ScanOne consumes the first row from rows into dest (a pointer to a
// struct, or to a scalar for a single-column result), returning
// ErrNoRows when the cursor is empty. Exported for code generators
// (cmd/dropsgen sql) that want the reflection scanner without going
// through a builder.
//
// The column→field mapping is [drops.ScanOne]'s, and the delegation is
// the point rather than an implementation detail. pg used to carry its
// own copy of the reflection walk and the copy had drifted from the
// original in four ways, each of them silent:
//
//   - it skipped unexported fields before it considered anonymous ones,
//     so a row type that factors its bookkeeping columns into an
//     embedded, unexported `audit` struct — the shape [Audit] hands out
//     — lost every promoted field. No error, no unmapped-column
//     complaint, just a zero CreatedAt on every row;
//   - it walked into an embedded time.Time looking for fields instead of
//     handing it the column, so an embedded timestamp never scanned;
//   - it resolved a name claimed twice by whichever field the walk
//     reached last, which put the outcome at the mercy of declaration
//     order: an outer ID declared above an embedded one handed its
//     column to the embedded field, and a `drop` tag lost to another
//     field's camelCase form;
//   - it refused a slice of scalars, so All[int64] over SELECT id was a
//     type error in pg and ordinary everywhere else.
//
// Neither scanner errors on a mismatch between struct and result set —
// an unmapped column goes to a discard sink either way — so switching
// takes nothing away from a caller selecting more columns than the
// destination has fields.
func ScanOne(rows drops.Rows, dest any) error { return scanOne(rows, dest) }

// ScanAll consumes every row from rows into dest (pointer to a slice of
// struct, *struct or scalar). Exported for the same reason as ScanOne,
// and mapping rows the same way; see [drops.ScanAll].
func ScanAll(rows drops.Rows, dest any) error { return scanAll(rows, dest) }

func scanAll(rows drops.Rows, dest any) error { return drops.ScanAll(rows, dest) }

func scanOne(rows drops.Rows, dest any) error {
	err := drops.ScanOne(rows, dest)
	if errors.Is(err, drops.ErrNoRows) {
		return ErrNoRows
	}
	return err
}

// fieldMap is the column→field index map the entity layer binds primary
// keys and relation keys with. It is the scanner's map, deliberately:
// when a column resolves to one field for a SELECT and to another for a
// key lookup, the entity writes back to a row it did not read, and no
// test that scans and asserts in the same shape can see it.
func fieldMap(t reflect.Type) map[string][]int { return drops.StructFields(t) }

// scanRowInto binds one row's columns to fields of structVal and scans.
// Columns with no field go to a shared discard sink — a result set is
// allowed to be wider than the destination, which is what lets one
// query text serve a full row type and a narrow projection.
//
// It stays here rather than moving to the root package because the
// entity fast path scans row by row into a value it already holds,
// which neither ScanOne nor ScanAll can express.
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
