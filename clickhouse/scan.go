package clickhouse

import (
	"errors"
	"reflect"

	"github.com/bernardoforcillo/drops"
)

// scanAll consumes rows into dest (a pointer to a slice of struct,
// *struct or scalar), and scanOne the first row into a pointer to a
// struct or scalar. Both are [drops.ScanAll] and [drops.ScanOne]: this
// package used to carry a copy of the reflection walk, and a copy of a
// mapping rule is a rule that drifts. It had — like the pg copy it was
// derived from — stopped walking anonymous fields of unexported struct
// types, so a row type embedding an unexported struct lost every
// promoted field with no error to show for it, and it resolved a name
// claimed by two fields by whichever the walk reached last, making the
// destination's declaration order part of the query's meaning.
//
// Unmatched columns still scan into a discard sink; the root scanner is
// no stricter about the width of a result set than this one was.
func scanAll(rows drops.Rows, dest any) error { return drops.ScanAll(rows, dest) }

// scanOne returns ErrNoRows — the clickhouse spelling — where the root
// scanner returns [drops.ErrNoRows], so callers that test the package
// sentinel keep working.
func scanOne(rows drops.Rows, dest any) error {
	err := drops.ScanOne(rows, dest)
	if errors.Is(err, drops.ErrNoRows) {
		return ErrNoRows
	}
	return err
}

// fieldMap is the column→field index map, shared with the scanner so
// that the entity layer binds its keys to the same field a SELECT would
// scan into. Divergence here is invisible to any test that writes and
// reads through the same struct.
func fieldMap(t reflect.Type) map[string][]int { return drops.StructFields(t) }
