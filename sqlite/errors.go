package sqlite

import "errors"

// Sentinel errors for assertable failure modes. They mirror
// drops/pg's error surface so code that switches dialects keeps the
// same errors.Is checks.
var (
	// ErrNoRows is returned by ScanOne when the result set is empty.
	ErrNoRows = errors.New("drops/sqlite: no rows in result set")

	// ErrNoRowsToInsert is returned when an INSERT has no rows to write.
	ErrNoRowsToInsert = errors.New("drops/sqlite: INSERT with no rows")

	// ErrInvalidIdentifier is returned (or panicked, at declaration
	// time) for an identifier that cannot be safely quoted.
	ErrInvalidIdentifier = errors.New("drops/sqlite: invalid identifier")
)
