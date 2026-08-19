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

	// Version orders competing writes to the same Key. A sink that
	// deduplicates — ClickHouse's ReplacingMergeTree — keeps the
	// highest and discards the rest, for good, so this single number
	// decides what the mirror ends up holding.
	//
	// It is not a free-form counter. Every version this package
	// produces comes from exactly one of three functions, and which
	// one it is determines which band of the uint64 line the value
	// falls in:
	//
	//	[SeedVersionAt]  a reseed pass       [1, MaxSeedVersion]
	//	[ClockVersion]   a wall clock        (MaxSeedVersion, LiveVersionBase)
	//	[LiveVersion]    a source sequence   [LiveVersionBase, 1<<64)
	//
	// The bands are the ordering: a seeded row loses to anything the
	// change stream carries, and a clock-stamped change loses to a
	// sequenced one. The package doc's "Versions" section says why
	// the line is cut this way and what it costs.
	//
	// Zero means "unset". [OutboxSource] fills it from the event id
	// and [Change.Normalized] falls back to the clock band, so a
	// change that reaches a sink with a zero version has come from a
	// source that went round both.
	Version uint64

	// At is when the mutation happened at the source.
	At time.Time
}

const (
	// MaxSeedVersion is the top of the seed band. Versions in
	// [1, MaxSeedVersion] are reserved for rows a reseed fills in,
	// and nothing that travels the change stream can land in it: it
	// is the room below the live space that makes a fill safe, which
	// a version space anchored on the outbox id alone would not have
	// (ids start at 1, and there is nothing below 1).
	//
	// The value is a millisecond count, so a seed version is simply
	// the time its pass started — see [SeedVersionAt] — and the band
	// runs out in the year 2527.
	MaxSeedVersion uint64 = 1<<44 - 1

	// LiveVersionBase is the floor of the live band: a change
	// numbered by its source carries LiveVersionBase plus that
	// number, via [LiveVersion].
	//
	// It is 1<<63 for two reasons that both matter. Below it lies
	// the whole of the int64 nanosecond space, so every version a
	// clock can produce — including every version this package
	// stamped before the bands existed, and every row a running
	// mirror already holds because of it — is below every sequenced
	// change, and an upgrade cannot leave a stale row winning
	// forever. Above it lie exactly as many values as an int64
	// sequence has, so mapping an outbox id into the band is total
	// and cannot overflow into another key's version.
	LiveVersionBase uint64 = 1 << 63
)

// LiveVersion maps a source sequence number into the live band.
//
// seq is whatever the source of truth numbers its committed changes
// with. For [OutboxSource] it is the outbox event id: one BigSerial
// in one database, so it is skew-free and totally ordered, and
// because a second writer of a row blocks on that row's lock until
// the first commits, it hands out ids in source commit order per key
// — which is the only order a mirror has to get right.
//
// A negative seq is not a sequence number; it clamps to the band
// floor rather than wrapping around into some other change's
// version.
func LiveVersion(seq int64) uint64 {
	if seq < 0 {
		return LiveVersionBase
	}
	return LiveVersionBase + uint64(seq)
}

// ClockVersion maps a wall-clock reading into the clock band, which
// is where a change whose source has no sequence number of its own
// ends up.
//
// It is the weakest of the three orderings and the package doc says
// so at length: two hosts a few milliseconds apart can stamp two
// changes to one key in the wrong order, and the mirror will keep
// the older value. What the band buys is that the damage stops
// there. A clock-stamped change cannot outrank a sequenced one, so
// wiring a durable [OutboxSource] alongside a clock-stamped feed
// resolves every disagreement in favour of the durable one.
//
// Readings from the first hours of 1970 or earlier — a zero
// [time.Time], a row dated 1900, anything that has been through a
// broken parse, never a real change time — are lifted to the bottom
// of the band rather than converted. Below the epoch the conversion
// is worse than useless: uint64(negative nanoseconds) is a number
// near the top of the live band, and would outrank everything the
// source ever emits.
func ClockVersion(t time.Time) uint64 {
	ns := t.UnixNano()
	if ns <= int64(MaxSeedVersion) {
		return MaxSeedVersion + 1
	}
	return uint64(ns)
}

// SeedVersionAt is the version a reseed pass that started at t
// stamps on every row it fills in.
//
// Milliseconds since the epoch, which gives the two properties a
// seed version needs. It cannot leave the seed band, and both other
// bands are above that band by construction — [ClockVersion] lifts a
// reading that would fall short — so a filled row loses every race it
// is in and can only populate keys nothing else has written. And it
// increases from pass to pass, so re-running a fill
// that read a row before it changed overwrites its own earlier
// answer instead of tying with it — a tie between two rows carrying
// different values is the one thing ReplacingMergeTree cannot
// resolve, and would make a FINAL read stop being a function of the
// key.
//
// Ordering passes by the clock is the concession this design makes:
// a machine whose clock jumps backwards between two passes runs the
// second one at a version the first already used or beat, so the
// second pass does not take. That is a fill declining to overwrite
// a fill, never a seed overwriting live data — the band boundary is
// structural and no clock reading can cross it.
func SeedVersionAt(t time.Time) uint64 {
	ms := t.UnixMilli()
	if ms < 1 {
		return 1
	}
	if uint64(ms) > MaxSeedVersion {
		return MaxSeedVersion
	}
	return uint64(ms)
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
// supplied clock reading when unset, and Version falls back to
// [ClockVersion] of At so that changes still order somehow when the
// source has no sequence number to offer.
//
// The fallback is a last resort, not the intended path. A source
// that can number its changes should stamp [LiveVersion] itself —
// [OutboxSource] does — because the clock band orders changes only
// as well as the clocks of the hosts that wrote them agree, and two
// hosts never agree exactly. What the fallback guarantees is the
// part a mirror cannot do without: a change that arrives unversioned
// is not left at zero, where it would tie with every other
// unversioned change and let a sink resolve the tie arbitrarily.
//
// The clock is a parameter rather than a call to time.Now so that a
// batch shares one reading and tests stay deterministic.
func (c Change) Normalized(now time.Time) Change {
	if c.At.IsZero() {
		c.At = now
	}
	if c.Version == 0 {
		c.Version = ClockVersion(c.At)
	}
	return c
}

// IsDelete reports whether the change tombstones the row.
func (c Change) IsDelete() bool { return c.Op == OpDelete }
