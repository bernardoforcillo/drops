package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Outbox is the transactional outbox pattern wired to drops/pg. It
// solves the "publish only if the DB write committed" problem by
// writing the event into a co-resident outbox table inside the same
// transaction as the business write; a background worker then drains
// the table and hands rows to a publisher.
//
//	// Schema setup (once)
//	OutboxTable := pg.NewOutboxTable("outbox")
//	schema := pg.NewSchema(Users, OutboxTable)
//	_ = pg.Push(ctx, db, schema)
//
//	// Emit inside the business transaction
//	ob := pg.NewOutbox(db, "outbox")
//	err := db.InTx(ctx, func(tx *pg.DB) error {
//	    if err := UserEntity.Create(tx, ctx, &u); err != nil { return err }
//	    return ob.Emit(tx, ctx, "user.created", u)
//	})
//
//	// Drain in a worker goroutine
//	worker := pg.NewOutboxWorker(ob).WithInterval(1*time.Second)
//	worker.OnEvent(func(ctx context.Context, e pg.OutboxEvent) error {
//	    return publisher.Publish(ctx, e.Kind, e.Payload)
//	})
//	go worker.Run(ctx)
//
// The pattern is at-least-once: a crash between publish and the
// follow-up UPDATE marking the row published will replay it. Make
// the publisher idempotent on Kind+Payload.
type Outbox struct {
	db    *DB
	table string
}

// OutboxEvent is one drained row. Payload is raw jsonb so the
// caller is free to deserialise into whatever Go type it needs.
type OutboxEvent struct {
	ID        int64
	Kind      string
	Payload   json.RawMessage
	CreatedAt time.Time
}

// NewOutbox returns an Outbox bound to db. Table is the SQL
// identifier of the outbox table; use NewOutboxTable to declare
// matching DDL.
func NewOutbox(db *DB, table string) *Outbox {
	if table == "" {
		table = "outbox"
	}
	return &Outbox{db: db, table: table}
}

// NewOutboxTable declares the canonical outbox table layout. Add it
// to your schema and let Push / Migrator manage the DDL.
func NewOutboxTable(name string) *Table {
	t := NewTable(name)
	Add(t, BigSerial("id").PrimaryKey())
	Add(t, Text("kind").NotNull())
	Add(t, JSONB("payload").NotNull())
	Add(t, Timestamp("createdAt", true).NotNull().Default("now()"))
	Add(t, Timestamp("publishedAt", true))
	return t
}

// Emit inserts an event using tx (typically the *DB passed into the
// InTx callback). Encodes payload as JSON; pass an already-encoded
// json.RawMessage to keep control of the serialisation.
func (o *Outbox) Emit(tx *DB, ctx context.Context, kind string, payload any) error {
	if kind == "" {
		return errors.New("drops/pg: Outbox.Emit kind cannot be empty")
	}
	raw, err := outboxEncodePayload(payload)
	if err != nil {
		return err
	}
	sql := fmt.Sprintf(`INSERT INTO "%s" ("kind", "payload") VALUES ($1, $2)`, o.table)
	if _, err := tx.Exec(ctx, sql, kind, raw); err != nil {
		return fmt.Errorf("drops/pg: Outbox.Emit: %w", err)
	}
	return nil
}

// Drain fetches up to limit unpublished events for processing.
// Uses SKIP LOCKED so multiple workers can drain in parallel
// without stepping on each other. Caller must mark rows published
// via MarkPublished when the handler succeeds.
func (o *Outbox) Drain(ctx context.Context, limit int) ([]OutboxEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	sql := fmt.Sprintf(`
		SELECT "id", "kind", "payload", "createdAt"
		FROM "%s"
		WHERE "publishedAt" IS NULL
		ORDER BY "id"
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, o.table)
	rows, err := o.db.Query(ctx, sql, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxEvent
	for rows.Next() {
		var e OutboxEvent
		if err := rows.Scan(&e.ID, &e.Kind, &e.Payload, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MarkPublished updates the publishedAt timestamp for the supplied
// event ids. Safe to call repeatedly; subsequent draws skip
// published rows.
func (o *Outbox) MarkPublished(ctx context.Context, ids ...int64) error {
	if len(ids) == 0 {
		return nil
	}
	sql := fmt.Sprintf(`UPDATE "%s" SET "publishedAt" = now() WHERE "id" = ANY($1)`, o.table)
	_, err := o.db.Exec(ctx, sql, ids)
	return err
}

// outboxEncodePayload converts payload to json.RawMessage. Already
// encoded values pass through.
func outboxEncodePayload(payload any) (json.RawMessage, error) {
	switch v := payload.(type) {
	case nil:
		return json.RawMessage(`null`), nil
	case json.RawMessage:
		return v, nil
	case []byte:
		return json.RawMessage(v), nil
	case string:
		return json.RawMessage([]byte(v)), nil
	default:
		return json.Marshal(payload)
	}
}

// ----------------------------------------------------------------------
// Worker
// ----------------------------------------------------------------------

// OutboxHandler is the callback the worker invokes for each drained
// event. Returning an error keeps the row in the outbox so the next
// tick can retry; returning nil marks the row published.
type OutboxHandler func(ctx context.Context, e OutboxEvent) error

// OutboxWorker polls the outbox table and forwards rows to a
// handler. Single goroutine per worker; spin up several with
// distinct names to parallelise drain — SKIP LOCKED keeps them
// from collisions.
type OutboxWorker struct {
	ob       *Outbox
	interval time.Duration
	batch    int
	handler  OutboxHandler
	onError  func(error)
}

// NewOutboxWorker returns a worker with sensible defaults: 1s
// polling interval, 50-row batch.
func NewOutboxWorker(ob *Outbox) *OutboxWorker {
	return &OutboxWorker{ob: ob, interval: time.Second, batch: 50}
}

// WithInterval overrides the polling cadence. Returns the worker
// for chaining.
func (w *OutboxWorker) WithInterval(d time.Duration) *OutboxWorker {
	if d > 0 {
		w.interval = d
	}
	return w
}

// WithBatch overrides the per-tick batch size.
func (w *OutboxWorker) WithBatch(n int) *OutboxWorker {
	if n > 0 {
		w.batch = n
	}
	return w
}

// OnEvent attaches the handler. Must be called before Run.
func (w *OutboxWorker) OnEvent(fn OutboxHandler) *OutboxWorker {
	w.handler = fn
	return w
}

// OnError attaches an error sink for drain / mark-published
// failures. Without one the worker swallows transient errors and
// retries on the next tick.
func (w *OutboxWorker) OnError(fn func(error)) *OutboxWorker {
	w.onError = fn
	return w
}

// ErrNoHandler is returned by Run when no handler was attached.
var ErrNoHandler = errors.New("drops/pg: OutboxWorker has no handler")

// Run drains the outbox until ctx is cancelled. Blocks the calling
// goroutine; typically invoked via go worker.Run(ctx). Returns the
// reason for termination — typically ctx.Err().
func (w *OutboxWorker) Run(ctx context.Context) error {
	if w.handler == nil {
		return ErrNoHandler
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		// Drain as much as the batch allows before sleeping. This
		// reduces tail latency when the worker has been idle and
		// the table has a backlog.
		if err := w.tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
			if w.onError != nil {
				w.onError(err)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// tick performs one drain pass.
func (w *OutboxWorker) tick(ctx context.Context) error {
	events, err := w.ob.Drain(ctx, w.batch)
	if err != nil || len(events) == 0 {
		return err
	}
	var ok []int64
	for _, e := range events {
		if herr := w.handler(ctx, e); herr == nil {
			ok = append(ok, e.ID)
		}
	}
	return w.ob.MarkPublished(ctx, ok...)
}

