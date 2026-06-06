package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bernardoforcillo/drops"
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
//	ob := pg.NewOutbox(db, "outbox").WithNotifyChannel("outbox_event")
//	err := db.InTx(ctx, func(tx *pg.DB) error {
//	    if err := UserEntity.Create(tx, ctx, &u); err != nil { return err }
//	    return ob.EmitWith(tx, ctx, "user.created", u, pg.EmitOptions{
//	        AggregateType: "user",
//	        AggregateID:   fmt.Sprintf("%d", u.ID),
//	        Headers:       map[string]string{"traceparent": tp},
//	    })
//	})
//
//	// Drain in a worker goroutine
//	worker := pg.NewOutboxWorker(ob).
//	    WithInterval(1 * time.Second).
//	    WithMaxAttempts(10).
//	    WithBackoff(pg.ExponentialJitter(time.Second, 5*time.Minute)).
//	    WithOrdering(pg.OrderingPerAggregate).
//	    OnEvent(func(ctx context.Context, e pg.OutboxEvent) error {
//	        return publisher.Publish(ctx, e.Kind, e.Payload)
//	    })
//	go worker.Run(ctx)
//
// The pattern is at-least-once: a crash between publish and the
// follow-up UPDATE marking the row published will replay it. Make
// the publisher idempotent on Kind+Payload — or use the per-event
// id as the dedup key downstream.
type Outbox struct {
	db            *DB
	table         string
	notifyChannel string
}

// OutboxEvent is one drained row.
type OutboxEvent struct {
	ID            int64
	Kind          string
	AggregateType string
	AggregateID   string
	Payload       json.RawMessage
	Headers       map[string]string
	Attempts      int
	LastError     string
	CreatedAt     time.Time
}

// EmitOptions extends Emit with aggregate metadata and tracing
// headers. Aggregate fields enable per-aggregate ordering in the
// worker (see WithOrdering(OrderingPerAggregate)).
type EmitOptions struct {
	// AggregateType is the entity kind, e.g. "player", "match",
	// "wallet". Optional but recommended — pairs with AggregateID
	// to identify the ordering scope.
	AggregateType string

	// AggregateID is the per-aggregate ordering key, e.g. "42",
	// "match-abc". Events sharing the same AggregateID are
	// delivered in id order when the worker is configured with
	// WithOrdering(OrderingPerAggregate).
	AggregateID string

	// Headers carry tracing metadata (traceparent, correlationID,
	// userID, ...). Stored as jsonb on the row and surfaced to
	// the handler so context propagates from emitter to consumer.
	Headers map[string]string
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

// WithNotifyChannel makes Emit also issue pg_notify(channel, id) so
// a worker that LISTENs on the same channel wakes immediately
// instead of waiting for the polling tick. Falls back gracefully
// when the driver does not implement Listener — Emit succeeds
// either way; only the wakeup latency degrades.
func (o *Outbox) WithNotifyChannel(channel string) *Outbox {
	o.notifyChannel = channel
	return o
}

// NotifyChannel returns the configured pg_notify channel, or "" if
// none was set.
func (o *Outbox) NotifyChannel() string { return o.notifyChannel }

// NewOutboxTable declares the canonical outbox table layout. Add it
// to your schema and let Push / Migrator manage the DDL. The table
// ships with a partial index that keeps the drain query O(log n)
// no matter how many published rows accumulate before cleanup.
func NewOutboxTable(name string) *Table {
	t := NewTable(name)
	Add(t, BigSerial("id").PrimaryKey())
	Add(t, Text("kind").NotNull())
	aggT := Add(t, Text("aggregateType"))
	aggID := Add(t, Text("aggregateID"))
	Add(t, JSONB("payload").NotNull())
	Add(t, JSONB("headers"))
	Add(t, Timestamp("createdAt", true).NotNull().Default("now()"))
	availableAt := Add(t, Timestamp("availableAt", true).NotNull().Default("now()"))
	publishedAt := Add(t, Timestamp("publishedAt", true))
	failedAt := Add(t, Timestamp("failedAt", true))
	Add(t, Integer("attempts").NotNull().Default("0"))
	Add(t, Text("lastError"))

	// Partial index for the hot drain path — only indexes the
	// rows that workers actually consider, so the index stays
	// small even when millions of published rows are awaiting
	// cleanup.
	t.AddIndex(NewIndex(name+"DrainIdx", t, availableAt.Column, t.Col("id")).
		Where(And(IsNull(publishedAt.Column), IsNull(failedAt.Column))))

	// Secondary index for per-aggregate ordering queries — used
	// by OrderingPerAggregate to pick the next aggregate with
	// work pending.
	t.AddIndex(NewIndex(name+"AggIdx", t, aggT.Column, aggID.Column, t.Col("id")).
		Where(And(IsNull(publishedAt.Column), IsNull(failedAt.Column))))

	return t
}

// Emit inserts an event using tx (typically the *DB passed into the
// InTx callback). Encodes payload as JSON; pass an already-encoded
// json.RawMessage to keep control of the serialisation.
//
// Equivalent to EmitWith with zero EmitOptions.
func (o *Outbox) Emit(tx *DB, ctx context.Context, kind string, payload any) error {
	return o.EmitWith(tx, ctx, kind, payload, EmitOptions{})
}

// EmitWith is the extended variant carrying aggregate metadata and
// tracing headers — see EmitOptions.
func (o *Outbox) EmitWith(tx *DB, ctx context.Context, kind string, payload any, opts EmitOptions) error {
	if kind == "" {
		return errors.New("drops/pg: Outbox.Emit kind cannot be empty")
	}
	raw, err := outboxEncodePayload(payload)
	if err != nil {
		return err
	}
	var headers any
	if len(opts.Headers) > 0 {
		h, err := json.Marshal(opts.Headers)
		if err != nil {
			return fmt.Errorf("drops/pg: Outbox.Emit headers: %w", err)
		}
		headers = json.RawMessage(h)
	}
	sql := fmt.Sprintf(`INSERT INTO "%s" ("kind", "aggregateType", "aggregateID", "payload", "headers") VALUES ($1, $2, $3, $4, $5)`, o.table)
	if _, err := tx.Exec(ctx, sql, kind,
		outboxNullableString(opts.AggregateType),
		outboxNullableString(opts.AggregateID),
		raw, headers); err != nil {
		return fmt.Errorf("drops/pg: Outbox.Emit: %w", err)
	}
	// Optional NOTIFY for sub-second wakeup. Same transaction so
	// it commits with the row — the worker never sees a wakeup
	// for an event that hasn't landed.
	if o.notifyChannel != "" {
		if _, err := tx.Exec(ctx, "SELECT pg_notify($1, $2)", o.notifyChannel, kind); err != nil {
			return fmt.Errorf("drops/pg: Outbox.Emit notify: %w", err)
		}
	}
	return nil
}

// outboxNullableString returns nil when s is empty so the column
// stores SQL NULL rather than an empty string — matters for the
// partial index predicates and for downstream "is this set?" checks.
func outboxNullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Drain fetches up to limit unpublished, non-failed, available events
// for processing. Uses SKIP LOCKED so multiple workers can drain in
// parallel without stepping on each other. Caller must mark rows
// published via MarkPublished when the handler succeeds, or
// MarkFailed when it errors.
func (o *Outbox) Drain(ctx context.Context, limit int) ([]OutboxEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	sql := fmt.Sprintf(`
		SELECT "id", "kind", "aggregateType", "aggregateID", "payload", "headers", "attempts", "lastError", "createdAt"
		FROM "%s"
		WHERE "publishedAt" IS NULL
		  AND "failedAt" IS NULL
		  AND "availableAt" <= now()
		ORDER BY "id"
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, o.table)
	rows, err := o.db.Query(ctx, sql, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOutboxRows(rows)
}

// DrainAggregate fetches events for a single aggregate in id order,
// under a transaction-scoped advisory lock keyed on the aggregate ID.
// Parallel workers calling DrainAggregate for the same aggregate skip
// silently (the lock is non-blocking), so per-aggregate order is
// preserved without serialising the whole worker pool.
//
// The callback executes within the lock-holding transaction; the
// lock auto-releases when the transaction ends. Returning an error
// rolls the transaction back — typically used so partial progress
// (MarkPublished calls) inside the callback is undone on failure.
func (o *Outbox) DrainAggregate(ctx context.Context, aggregateType, aggregateID string, limit int, fn func(tx *DB, events []OutboxEvent) error) error {
	if aggregateID == "" {
		return errors.New("drops/pg: Outbox.DrainAggregate requires non-empty aggregateID")
	}
	if limit <= 0 {
		limit = 50
	}
	return o.db.InTx(ctx, func(tx *DB) error {
		// Advisory lock keyed on (aggregateType, aggregateID) so
		// only one worker processes events for that aggregate at
		// a time — preserves per-aggregate order across the pool.
		key := lockKey("outbox:" + aggregateType + ":" + aggregateID)
		rows, err := tx.Query(ctx, "SELECT pg_try_advisory_xact_lock($1)", key)
		if err != nil {
			return err
		}
		var got bool
		if rows.Next() {
			if err := rows.Scan(&got); err != nil {
				rows.Close()
				return err
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if !got {
			return nil
		}
		sql := fmt.Sprintf(`
			SELECT "id", "kind", "aggregateType", "aggregateID", "payload", "headers", "attempts", "lastError", "createdAt"
			FROM "%s"
			WHERE "publishedAt" IS NULL
			  AND "failedAt" IS NULL
			  AND "availableAt" <= now()
			  AND "aggregateType" IS NOT DISTINCT FROM $1
			  AND "aggregateID" = $2
			ORDER BY "id"
			LIMIT $3`, o.table)
		eventRows, err := tx.Query(ctx, sql, outboxNullableString(aggregateType), aggregateID, limit)
		if err != nil {
			return err
		}
		events, err := scanOutboxRows(eventRows)
		eventRows.Close()
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return nil
		}
		return fn(tx, events)
	})
}

// PendingAggregates returns up to limit distinct aggregate keys that
// have unpublished work available right now. Used by the per-
// aggregate worker mode to discover which advisory locks to try.
func (o *Outbox) PendingAggregates(ctx context.Context, limit int) ([]AggregateRef, error) {
	if limit <= 0 {
		limit = 50
	}
	sql := fmt.Sprintf(`
		SELECT DISTINCT "aggregateType", "aggregateID"
		FROM "%s"
		WHERE "publishedAt" IS NULL
		  AND "failedAt" IS NULL
		  AND "availableAt" <= now()
		  AND "aggregateID" IS NOT NULL
		LIMIT $1`, o.table)
	rows, err := o.db.Query(ctx, sql, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AggregateRef
	for rows.Next() {
		var ref AggregateRef
		var typ, id any
		if err := rows.Scan(&typ, &id); err != nil {
			return nil, err
		}
		if s, ok := typ.(string); ok {
			ref.Type = s
		}
		if s, ok := id.(string); ok {
			ref.ID = s
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// AggregateRef identifies an aggregate that has pending outbox work.
type AggregateRef struct {
	Type string
	ID   string
}

// scanOutboxRows reads rows produced by the SELECT shape used in
// Drain / DrainAggregate. Pulled out so both paths share scanning.
func scanOutboxRows(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]OutboxEvent, error) {
	var out []OutboxEvent
	for rows.Next() {
		var (
			e         OutboxEvent
			aggType   any
			aggID     any
			headers   []byte
			lastError any
		)
		if err := rows.Scan(&e.ID, &e.Kind, &aggType, &aggID, &e.Payload, &headers, &e.Attempts, &lastError, &e.CreatedAt); err != nil {
			return nil, err
		}
		if s, ok := aggType.(string); ok {
			e.AggregateType = s
		}
		if s, ok := aggID.(string); ok {
			e.AggregateID = s
		}
		if s, ok := lastError.(string); ok {
			e.LastError = s
		}
		if len(headers) > 0 && string(headers) != "null" {
			m := map[string]string{}
			if err := json.Unmarshal(headers, &m); err == nil {
				e.Headers = m
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MarkPublished updates the publishedAt timestamp for the supplied
// event ids. Safe to call repeatedly; subsequent drains skip
// published rows.
func (o *Outbox) MarkPublished(ctx context.Context, ids ...int64) error {
	return markPublishedOn(ctx, o.db, o.table, ids...)
}

// markPublishedOn is the shared body used by Outbox.MarkPublished and
// the in-transaction path in the per-aggregate worker.
func markPublishedOn(ctx context.Context, exec interface {
	Exec(context.Context, string, ...any) (drops.Result, error)
}, table string, ids ...int64) error {
	if len(ids) == 0 {
		return nil
	}
	sql := fmt.Sprintf(`UPDATE "%s" SET "publishedAt" = now() WHERE "id" = ANY($1)`, table)
	_, err := exec.Exec(ctx, sql, ids)
	return err
}

// MarkFailed records a handler failure on the supplied event. When
// nextRetryAt is the zero value the row is parked as terminally
// failed (failedAt set, won't be drained again). Otherwise the row
// becomes available for retry at nextRetryAt with attempts bumped
// and the error message stored in lastError.
func (o *Outbox) MarkFailed(ctx context.Context, id int64, attempts int, nextRetryAt time.Time, lastErr string) error {
	if nextRetryAt.IsZero() {
		sql := fmt.Sprintf(`UPDATE "%s" SET "attempts" = $2, "lastError" = $3, "failedAt" = now() WHERE "id" = $1`, o.table)
		_, err := o.db.Exec(ctx, sql, id, attempts, lastErr)
		return err
	}
	sql := fmt.Sprintf(`UPDATE "%s" SET "attempts" = $2, "lastError" = $3, "availableAt" = $4 WHERE "id" = $1`, o.table)
	_, err := o.db.Exec(ctx, sql, id, attempts, lastErr, nextRetryAt)
	return err
}

// Cleanup deletes rows that were published more than retainAfter
// ago. Run periodically (typically once per minute) to keep the
// outbox table small; without it the table grows unbounded and the
// drain index, although partial, still has to skip stale rows
// during VACUUM.
//
// Failed (terminal) rows are intentionally kept — they're the audit
// log of poison messages. Drop them manually after triage.
func (o *Outbox) Cleanup(ctx context.Context, retainAfter time.Duration) (int64, error) {
	if retainAfter < 0 {
		retainAfter = 0
	}
	sql := fmt.Sprintf(`DELETE FROM "%s" WHERE "publishedAt" IS NOT NULL AND "publishedAt" < now() - make_interval(secs => $1)`, o.table)
	res, err := o.db.Exec(ctx, sql, retainAfter.Seconds())
	if err != nil {
		return 0, err
	}
	if res == nil {
		return 0, nil
	}
	return res.RowsAffected()
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

// OutboxOrdering controls how the worker schedules events.
type OutboxOrdering int

const (
	// OrderingNone delivers events as soon as drain returns them.
	// Highest throughput; events for different aggregates can
	// interleave, and parallel workers may deliver out of order
	// within the same aggregate when timestamps tie.
	OrderingNone OutboxOrdering = iota

	// OrderingPerAggregate preserves emission order within each
	// (AggregateType, AggregateID) by routing each aggregate
	// through a single worker at a time via advisory locks.
	// Events that lack an AggregateID fall back to OrderingNone.
	OrderingPerAggregate
)

// OutboxHandler is the per-event callback the worker invokes when
// configured via OnEvent. Returning nil marks the row published;
// returning an error schedules a retry (or terminal failure when
// MaxAttempts has been reached).
type OutboxHandler func(ctx context.Context, e OutboxEvent) error

// OutboxBatchHandler is the batched alternative — receives the
// entire drain batch in one call. Useful for message brokers with
// expensive per-call overhead (Kafka producer, etc.).
//
// Returning nil marks every event in the batch published; returning
// an error fails every event in the batch (each row's attempts is
// bumped). Use OnEvent if you need per-event success granularity.
type OutboxBatchHandler func(ctx context.Context, events []OutboxEvent) error

// OutboxWorker polls the outbox table and forwards rows to a
// handler. Single goroutine per worker; spin up several with
// distinct names to parallelise drain — SKIP LOCKED keeps them
// from collisions.
type OutboxWorker struct {
	ob           *Outbox
	interval     time.Duration
	batch        int
	maxAttempts  int
	backoff      func(attempt int) time.Duration
	ordering     OutboxOrdering
	handler      OutboxHandler
	batchHandler OutboxBatchHandler
	onError      func(error)
	now          func() time.Time
}

// NewOutboxWorker returns a worker with sensible defaults: 1s
// polling interval, 50-row batch, exponential-jitter backoff
// (1s base, capped at 5min), unlimited retries.
func NewOutboxWorker(ob *Outbox) *OutboxWorker {
	return &OutboxWorker{
		ob:       ob,
		interval: time.Second,
		batch:    50,
		backoff:  ExponentialJitter(time.Second, 5*time.Minute),
		now:      time.Now,
	}
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

// WithMaxAttempts caps the retry count. After n attempts the row is
// marked terminally failed (failedAt set) and never drained again
// — surface it via metrics / alerting and drop manually after
// triage. Zero means unlimited retries.
func (w *OutboxWorker) WithMaxAttempts(n int) *OutboxWorker {
	if n > 0 {
		w.maxAttempts = n
	}
	return w
}

// WithBackoff overrides the per-attempt retry delay. The function
// receives the new attempt count (1-based) and returns how long to
// wait before the next try. Defaults to ExponentialJitter(1s, 5min).
func (w *OutboxWorker) WithBackoff(fn func(attempt int) time.Duration) *OutboxWorker {
	if fn != nil {
		w.backoff = fn
	}
	return w
}

// WithOrdering selects the scheduling strategy. See OutboxOrdering.
func (w *OutboxWorker) WithOrdering(m OutboxOrdering) *OutboxWorker {
	w.ordering = m
	return w
}

// OnEvent attaches the per-event handler. Mutually exclusive with
// OnBatch — whichever was set last wins.
func (w *OutboxWorker) OnEvent(fn OutboxHandler) *OutboxWorker {
	w.handler = fn
	w.batchHandler = nil
	return w
}

// OnBatch attaches the batch handler. Mutually exclusive with
// OnEvent — whichever was set last wins.
func (w *OutboxWorker) OnBatch(fn OutboxBatchHandler) *OutboxWorker {
	w.batchHandler = fn
	w.handler = nil
	return w
}

// OnError attaches an error sink for drain / mark-published
// failures. Handler errors are reported through MarkFailed and do
// not surface here.
func (w *OutboxWorker) OnError(fn func(error)) *OutboxWorker {
	w.onError = fn
	return w
}

// ErrNoHandler is returned by Run when neither OnEvent nor OnBatch
// was attached.
var ErrNoHandler = errors.New("drops/pg: OutboxWorker has no handler")

// Run drains the outbox until ctx is cancelled. Blocks the calling
// goroutine; typically invoked via go worker.Run(ctx). Returns the
// reason for termination — typically ctx.Err().
//
// When the underlying Outbox was configured with WithNotifyChannel
// and the driver implements Listener, Run wakes immediately on
// each NOTIFY in addition to the periodic tick — sub-second
// delivery without hammering the database.
func (w *OutboxWorker) Run(ctx context.Context) error {
	if w.handler == nil && w.batchHandler == nil {
		return ErrNoHandler
	}

	var notify <-chan Notification
	if w.ob.notifyChannel != "" {
		if l, ok := w.ob.db.Driver().(Listener); ok {
			if ch, err := l.Listen(ctx, w.ob.notifyChannel); err == nil {
				notify = ch
			}
		}
	}

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if err := w.tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
			if w.onError != nil {
				w.onError(err)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-notify:
			// LISTEN wakeup — proceed immediately to the next tick.
		}
	}
}

// tick performs one drain pass.
func (w *OutboxWorker) tick(ctx context.Context) error {
	if w.ordering == OrderingPerAggregate {
		return w.tickPerAggregate(ctx)
	}
	return w.tickNone(ctx)
}

// tickNone is the unordered drain — events are processed in id
// order across the batch but no per-aggregate guarantee is held.
func (w *OutboxWorker) tickNone(ctx context.Context) error {
	events, err := w.ob.Drain(ctx, w.batch)
	if err != nil || len(events) == 0 {
		return err
	}
	if w.batchHandler != nil {
		return w.runBatch(ctx, events)
	}
	for _, e := range events {
		if herr := w.handler(ctx, e); herr == nil {
			if err := w.ob.MarkPublished(ctx, e.ID); err != nil && w.onError != nil {
				w.onError(err)
			}
		} else {
			w.failOne(ctx, e, herr)
		}
	}
	return nil
}

// tickPerAggregate processes one aggregate at a time inside its
// advisory lock so per-aggregate order is preserved even when many
// workers are running in parallel. Events without an AggregateID
// fall through to the unordered path so the worker still drains
// everything.
func (w *OutboxWorker) tickPerAggregate(ctx context.Context) error {
	aggs, err := w.ob.PendingAggregates(ctx, w.batch)
	if err != nil {
		return err
	}
	for _, agg := range aggs {
		err := w.ob.DrainAggregate(ctx, agg.Type, agg.ID, w.batch, func(tx *DB, events []OutboxEvent) error {
			if w.batchHandler != nil {
				return w.runBatchInTx(ctx, tx, events)
			}
			return w.runSequentialInTx(ctx, tx, events)
		})
		if err != nil && w.onError != nil {
			w.onError(err)
		}
	}
	// Fall back to the unordered drain so events without an
	// aggregate ID still flow.
	return w.tickNone(ctx)
}

// runBatch publishes a whole batch via the OnBatch handler. On
// success every event is marked published; on failure every event
// is marked failed (single round-trip each).
func (w *OutboxWorker) runBatch(ctx context.Context, events []OutboxEvent) error {
	herr := w.batchHandler(ctx, events)
	if herr == nil {
		ids := make([]int64, len(events))
		for i, e := range events {
			ids[i] = e.ID
		}
		if err := w.ob.MarkPublished(ctx, ids...); err != nil && w.onError != nil {
			w.onError(err)
		}
		return nil
	}
	for _, e := range events {
		w.failOne(ctx, e, herr)
	}
	return nil
}

// runBatchInTx is the in-transaction variant used by the per-
// aggregate path. Uses the tx for MarkPublished so the publish-
// state change rolls back if anything later fails.
func (w *OutboxWorker) runBatchInTx(ctx context.Context, tx *DB, events []OutboxEvent) error {
	herr := w.batchHandler(ctx, events)
	if herr == nil {
		ids := make([]int64, len(events))
		for i, e := range events {
			ids[i] = e.ID
		}
		return markPublishedOn(ctx, tx, w.ob.table, ids...)
	}
	for _, e := range events {
		w.failOne(ctx, e, herr)
	}
	return nil
}

// runSequentialInTx delivers events one by one in id order. Stops
// at the first failure so per-aggregate order is preserved — the
// stuck event blocks the queue for its aggregate until it's
// resolved (or hits MaxAttempts and is parked).
func (w *OutboxWorker) runSequentialInTx(ctx context.Context, tx *DB, events []OutboxEvent) error {
	for _, e := range events {
		if herr := w.handler(ctx, e); herr != nil {
			w.failOne(ctx, e, herr)
			return nil
		}
		if err := markPublishedOn(ctx, tx, w.ob.table, e.ID); err != nil {
			return err
		}
	}
	return nil
}

// failOne handles a per-event failure: bump attempts, compute next
// retry time, mark terminal if MaxAttempts is reached.
func (w *OutboxWorker) failOne(ctx context.Context, e OutboxEvent, herr error) {
	attempts := e.Attempts + 1
	var nextRetry time.Time
	if w.maxAttempts == 0 || attempts < w.maxAttempts {
		nextRetry = w.now().Add(w.computeBackoff(attempts))
	}
	if err := w.ob.MarkFailed(ctx, e.ID, attempts, nextRetry, herr.Error()); err != nil && w.onError != nil {
		w.onError(err)
	}
}

func (w *OutboxWorker) computeBackoff(attempt int) time.Duration {
	if w.backoff == nil {
		return time.Second
	}
	return w.backoff(attempt)
}
