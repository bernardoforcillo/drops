package pg

import (
	"context"
	"encoding/json"
	"errors"
)

// PostgreSQL has a built-in pub/sub channel via LISTEN/NOTIFY that
// nobody exposes well in Go. drops makes it first-class:
//
//	// Producer (typically right after a write, often via outbox)
//	pg.Notify(db, ctx, "invoice_paid", InvoicePaidEvent{ID: 7})
//
//	// Consumer
//	ch, err := pg.Listen[InvoicePaidEvent](db, ctx, "invoice_paid")
//	for ev := range ch {
//	    // push WebSocket / refresh dashboard
//	}
//
// Notify uses pg_notify(channel, payload) so the channel is
// parameterised and the payload is treated as data — no string
// concatenation, no injection surface. Payloads are JSON-encoded
// on the way out and decoded into T on the way in; malformed
// rows are dropped (they'd indicate a misalignment between
// producer and consumer types, fixable on either side).
//
// LISTEN requires a sticky connection to the server, so drops
// dispatches via the Listener interface — same duck-typing
// pattern as the Copier path. Drivers that don't expose listen
// (lib/pq does, pgx does via PgConn.WaitForNotification, others
// don't) return ErrListenNotSupported.

// Listener is the driver contract drops uses to subscribe to a
// channel. Implementations should keep their own goroutine
// pumping notifications onto the returned channel and close it
// when ctx is done.
type Listener interface {
	Listen(ctx context.Context, channel string) (<-chan Notification, error)
}

// Notification is one delivery from PG's NOTIFY mechanism.
type Notification struct {
	Channel string
	Payload string
	PID     int
}

// ErrListenNotSupported is returned by Listen when the driver
// does not satisfy Listener — fall back to polling or wire pgx /
// lib/pq's listener APIs into an adapter.
var ErrListenNotSupported = errors.New("drops/pg: driver does not implement Listener")

// Listen subscribes to channel and returns a typed channel that
// receives decoded T values. Payloads that fail JSON decoding
// are silently dropped — a misaligned producer is a deployment
// bug, not a per-message error.
//
// The returned channel closes when ctx is done or the driver's
// underlying subscription terminates. Spawn one goroutine per
// channel; combine with select to multiplex many channels.
func Listen[T any](db *DB, ctx context.Context, channel string) (<-chan T, error) {
	l, ok := db.Driver().(Listener)
	if !ok {
		return nil, ErrListenNotSupported
	}
	raw, err := l.Listen(ctx, channel)
	if err != nil {
		return nil, err
	}
	out := make(chan T, 16)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case n, ok := <-raw:
				if !ok {
					return
				}
				var v T
				if err := json.Unmarshal([]byte(n.Payload), &v); err != nil {
					continue
				}
				select {
				case out <- v:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// Notify publishes payload on channel. The payload is JSON-encoded
// before being handed to pg_notify, so any encodable value works
// — string, struct, json.RawMessage. Empty channel names error
// at the call site rather than at the database.
//
// PostgreSQL caps NOTIFY payloads at 8000 bytes; anything larger
// either truncates or errors depending on driver. For larger
// payloads emit a row id and have the consumer fetch the row.
func Notify(db *DB, ctx context.Context, channel string, payload any) error {
	if channel == "" {
		return errors.New("drops/pg: Notify channel cannot be empty")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, "SELECT pg_notify($1, $2)", channel, string(raw))
	return err
}

// SupportsListen reports whether db's driver implements Listener.
// Useful for code paths that want to choose between Listen-based
// push and polling.
func SupportsListen(db *DB) bool {
	_, ok := db.Driver().(Listener)
	return ok
}
