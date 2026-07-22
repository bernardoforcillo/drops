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

	// ErrBusy / ErrLocked are the retryable-contention sentinels
	// matching SQLITE_BUSY and SQLITE_LOCKED. A RetryPolicy listing
	// them (as DefaultRetryPolicy does) retries a transaction when the
	// underlying driver reports either — matched by errors.Is or by the
	// driver error's message (drivers rarely wrap a comparable
	// sentinel).
	ErrBusy   = errors.New("drops/sqlite: database is busy (SQLITE_BUSY)")
	ErrLocked = errors.New("drops/sqlite: database is locked (SQLITE_LOCKED)")
)
