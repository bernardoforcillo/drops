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
	ch = ch.Normalized(time.Now())
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
		if ch.Version == 0 {
			// The outbox id is monotonic, which is exactly the
			// ordering guarantee a version column needs.
			ch.Version = uint64(e.ID)
		}
		if ch.At.IsZero() {
			ch.At = e.CreatedAt
		}
		changes = append(changes, ch)
	}
	commit := func(ctx context.Context) error { return s.ob.MarkPublished(ctx, ids...) }
	return changes, commit, nil
}
