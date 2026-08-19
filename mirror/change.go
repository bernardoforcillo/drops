package mirror

import (
	"errors"
	"time"
)

// Op is the kind of mutation a [Change] carries.
type Op string

const (
	OpInsert Op = "insert"
	OpUpdate Op = "update"
	OpDelete Op = "delete"
)

// Change is one row mutation on its way from the source of truth to a
// mirror. It is deliberately untyped: a change crosses package
// boundaries between three stores that agree on nothing except column
// names, and a map is the only shape all three can consume without
// the mirror knowing the row's Go type.
type Change struct {
	// Op is the mutation kind.
	Op Op

	// Key is the source row's primary key, coerced to text so
	// int, uuid and text keys travel the same path. It is what a
	// sink addresses the mirrored row by.
	Key string

	// Row holds the row's column values by column name. Nil for
	// [OpDelete] — the row is gone at the source, and a sink only
	// needs the key to tombstone its copy.
	Row map[string]any

	// Version orders competing writes to the same Key. Sinks that
	// deduplicate — ClickHouse's ReplacingMergeTree — keep the
	// highest. It must be monotonic per key; an outbox event id or
	// a commit timestamp in nanoseconds both qualify.
	//
	// Zero means "unset", and [Change.Normalized] fills it from At.
	Version uint64

	// At is when the mutation happened at the source.
	At time.Time
}

// ErrNoKey is returned when a change carries no primary key, which no
// sink can address.
var ErrNoKey = errors.New("drops/mirror: change has no key")

// Validate reports whether the change can be applied at all.
func (c Change) Validate() error {
	if c.Key == "" {
		return ErrNoKey
	}
	switch c.Op {
	case OpInsert, OpUpdate, OpDelete:
	default:
		return errors.New("drops/mirror: unknown op " + string(c.Op))
	}
	return nil
}

// Normalized fills in the defaults a sink relies on: At becomes the
// supplied clock reading when unset, and Version falls back to At in
// nanoseconds so that changes still order correctly when the source
// has no sequence number of its own.
//
// The clock is a parameter rather than a call to time.Now so that a
// batch shares one reading and tests stay deterministic.
func (c Change) Normalized(now time.Time) Change {
	if c.At.IsZero() {
		c.At = now
	}
	if c.Version == 0 {
		c.Version = uint64(c.At.UnixNano())
	}
	return c
}

// IsDelete reports whether the change tombstones the row.
func (c Change) IsDelete() bool { return c.Op == OpDelete }
