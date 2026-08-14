package vector

import "errors"

var (
	// ErrNoVector is returned when a Query carries an empty query
	// vector.
	ErrNoVector = errors.New("drops/vector: query vector is empty")

	// ErrInvalidCursor is returned when a cursor string is malformed
	// — not base64, or not the JSON payload a cursor encodes to.
	ErrInvalidCursor = errors.New("drops/vector: invalid cursor")

	// ErrCursorMismatch is returned when a cursor issued by one
	// backend is replayed against another. Cursors are opaque but not
	// portable: a Qdrant offset means nothing to a keyset-paginated
	// SQL store, so replaying one is a caller bug worth surfacing
	// rather than a page worth guessing at.
	ErrCursorMismatch = errors.New("drops/vector: cursor was issued by a different backend")

	// ErrUnsupportedMetric is returned when a backend cannot serve
	// the requested Metric — e.g. Hamming against a ClickHouse
	// Array(Float32) column.
	ErrUnsupportedMetric = errors.New("drops/vector: metric not supported by this backend")

	// ErrUnsupportedOp is returned when a backend cannot express a
	// filter operator.
	ErrUnsupportedOp = errors.New("drops/vector: filter operator not supported by this backend")

	// ErrUnknownField is returned by SQL backends when a filter names
	// a field that maps to no column and the store has no payload
	// column to fall back on.
	ErrUnknownField = errors.New("drops/vector: filter field maps to no column")

	// ErrNotNumeric is returned when a range comparison is handed a
	// value that is not a number.
	ErrNotNumeric = errors.New("drops/vector: range comparison requires a numeric value")
)
