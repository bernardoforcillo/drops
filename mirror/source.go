package mirror

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/bernardoforcillo/drops/pg"
)

// Source hands batches of changes to a [Pump].
//
// Fetch returns the next batch together with the function that marks
// it handled. Nothing is acknowledged until every sink has accepted
// the batch, which is what makes delivery at-least-once rather than
// at-most-once: a crash between apply and commit replays the batch,
// and idempotent sinks absorb the replay.
//
// A Fetch that returns no changes means "nothing right now"; the pump
// waits and asks again.
//
// # What a source owes the version column
//
// A mirror keeps the highest [Change.Version] per key and discards
// everything below it, so the order a source stamps is the order the
// mirror is stuck with. A source that has a sequence number for its
// committed changes — an outbox id, an LSN, a binlog position —
// should map it through [LiveVersion] and hand that over;
// [OutboxSource] does, and that is what makes it the supported
// source.
//
// A source that has no such number cannot offer the guarantee, and
// this package would rather say so than fake it. Leave Version at
// zero and the [Pump] falls back to [ClockVersion], which orders the
// stream only as well as the clocks of the hosts that wrote it agree
// — two updates to one key a millisecond apart from two hosts a few
// milliseconds out of step will be applied in the wrong order, and
// the mirror will keep the older value with nothing to report it.
// Because that band sits below the live one, such a source is safe
// to run alongside a sequenced one (the sequenced change always
// wins) and safe to reseed over, but a mirror fed only by it has
// approximate freshness rather than fidelity. Do not paper over it
// by inventing a version: a number that is not derived from the
// source's own commit order is a guess with a tie-break attached.
type Source interface {
	Fetch(ctx context.Context, max int) (changes []Change, commit func(context.Context) error, err error)
}

// EventKind is the outbox kind [EmitChange] writes and
// [OutboxSource] reads. Events of any other kind are left alone, so a
// mirror can share an outbox table with the rest of the application's
// integration events.
const EventKind = "drops.mirror.change"

// EmitChange records a row mutation in the outbox, inside the caller's
// transaction.
//
// Writing the change in the same transaction as the mutation is the
// whole point: either both land or neither does, so the mirror can
// never be told about a row that was rolled back, nor miss one that
// was committed. That is the guarantee a trigger-and-NOTIFY feed
// cannot give.
//
// The change's version is not the caller's to set, and a non-zero
// [Change.Version] is refused here rather than quietly overridden at
// drain. The row this INSERT is about to write carries the number
// that orders it — the outbox id — and [OutboxSource] stamps it on
// the way out; see the package doc's "Versions" section. A version
// the emitter chose comes from some other space (a clock, a row's
// updatedAt, a counter) and is not comparable with an id, so an
// application that set it on some paths and not others would have
// two spaces racing in one mirror. That is the defect this package
// used to ship: EmitChange stamped the wall clock, the id was never
// reached, and two hosts a few milliseconds apart inverted two
// updates a millisecond apart.
//
//	err := db.InTx(ctx, func(tx *pg.DB) error {
//	    if err := Docs.Update(tx, ctx, &doc); err != nil {
//	        return err
//	    }
//	    return mirror.EmitChange(ob, tx, ctx, mirror.Change{
//	        Op:  mirror.OpUpdate,
//	        Key: strconv.FormatInt(doc.ID, 10),
//	        Row: map[string]any{"id": doc.ID, "title": doc.Title},
//	    })
//	})
func EmitChange(ob *pg.Outbox, tx *pg.DB, ctx context.Context, ch Change) error {
	if ob == nil {
		return fmt.Errorf("drops/mirror: EmitChange needs an outbox")
	}
	if err := ch.Validate(); err != nil {
		return err
	}
	if ch.Version != 0 {
		return fmt.Errorf("drops/mirror: EmitChange: change for key %s carries Version %d, but an outbox change is ordered by its event id — leave Version zero and OutboxSource stamps it at drain",
			ch.Key, ch.Version)
	}
	// At is filled here and the version deliberately is not: this is
	// the emitter's reading of when the mutation happened, which a
	// sink may want, while the ordering comes from the id the INSERT
	// below is about to be assigned.
	if ch.At.IsZero() {
		ch.At = time.Now()
	}
	return ob.EmitWith(tx, ctx, EventKind, ch, pg.EmitOptions{
		// Per-aggregate ordering keeps two changes to the same row
		// in the order they were written, which is what lets a sink
		// trust the version it receives.
		AggregateType: EventKind,
		AggregateID:   ch.Key,
	})
}

// OutboxSource reads changes out of a drops/pg outbox.
//
// This is the supported source. It is durable, ordered per key, and
// acknowledges only after the sinks have accepted a batch — see the
// package doc for why the LISTEN/NOTIFY changefeed is not a
// substitute.
//
// It is also the only source in this package that can version a
// change properly. The outbox id is a BigSerial in the same database
// as the rows being mirrored, so it is skew-free and totally
// ordered, and an emitter that mutates a row and emits in one
// transaction gets ids in commit order per key: a second writer of
// that row waits on its lock until the first commits, so it cannot
// take an id ahead of it. Fetch maps that id into the live band with
// [LiveVersion].
type OutboxSource struct {
	ob *pg.Outbox
}

// NewOutboxSource wraps an outbox as a [Source].
func NewOutboxSource(ob *pg.Outbox) (*OutboxSource, error) {
	if ob == nil {
		return nil, fmt.Errorf("drops/mirror: NewOutboxSource needs an outbox")
	}
	return &OutboxSource{ob: ob}, nil
}

// Fetch drains up to max pending events and decodes the mirror ones.
func (s *OutboxSource) Fetch(ctx context.Context, max int) ([]Change, func(context.Context) error, error) {
	events, err := s.ob.Drain(ctx, max)
	if err != nil {
		return nil, nil, err
	}
	if len(events) == 0 {
		return nil, nil, nil
	}
	changes := make([]Change, 0, len(events))
	ids := make([]int64, 0, len(events))
	for _, e := range events {
		ids = append(ids, e.ID)
		if e.Kind != EventKind {
			// Someone else's event sharing the table. Acknowledging
			// it here would swallow it, so leave it pending: Drain
			// returned it, but only the ids we commit are marked.
			ids = ids[:len(ids)-1]
			continue
		}
		var ch Change
		if err := json.Unmarshal(e.Payload, &ch); err != nil {
			return nil, nil, fmt.Errorf("drops/mirror: outbox event %d: %w", e.ID, err)
		}
		// The event id is the version, unconditionally — see
		// [LiveVersion] for why it is the right number and the
		// package doc for why it is offset into the live band rather
		// than used raw. Any version in the payload predates this
		// rule (or comes from an emitter that ignored EmitChange's
		// refusal) and is in a space that cannot be compared with the
		// ids around it, so it is replaced rather than honoured: two
		// changes to one key must be ordered by the same yardstick or
		// the mirror keeps whichever happened to be measured in the
		// larger unit.
		ch.Version = LiveVersion(e.ID)
		if ch.At.IsZero() {
			ch.At = e.CreatedAt
		}
		changes = append(changes, ch)
	}
	commit := func(ctx context.Context) error { return s.ob.MarkPublished(ctx, ids...) }
	return changes, commit, nil
}
