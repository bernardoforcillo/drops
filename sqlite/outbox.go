package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Outbox is the transactional outbox pattern for SQLite. The event is
// written into a co-resident outbox table inside the same transaction as
// the business write; a background worker then drains the table and hands
// rows to a publisher.
//
//	ob := sqlite.NewOutbox(db, "outbox")
//	err := db.InTx(ctx, func(tx *sqlite.DB) error {
//	    if err := UserEntity.Create(tx, ctx, &u); err != nil { return err }
//	    return ob.Emit(tx, ctx, "user.created", u)
//	})
//
//	worker := sqlite.NewOutboxWorker(ob).
//	    WithInterval(time.Second).
//	    OnEvent(func(ctx context.Context, e sqlite.OutboxEvent) error {
//	        return publisher.Publish(ctx, e.Kind, e.Payload)
//	    })
//	go worker.Run(ctx)
//
// Differences from drops/pg: SQLite has no LISTEN/NOTIFY, SKIP LOCKED or
// advisory locks, so this is a poll-based, single-worker outbox (run one
// worker per outbox table). Timestamps are stored as INTEGER Unix seconds
// to keep comparisons portable. Delivery is at-least-once — make the
// publisher idempotent on the event id.
type Outbox struct {
	db    *DB
	table string
	now   func() time.Time
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

// EmitOptions extends Emit with aggregate metadata and tracing headers.
type EmitOptions struct {
	AggregateType string
	AggregateID   string
	Headers       map[string]string
}

// NewOutbox returns an Outbox bound to db.
func NewOutbox(db *DB, table string) *Outbox {
	if table == "" {
		table = "outbox"
	}
	return &Outbox{db: db, table: table, now: time.Now}
}

// NewOutboxTable declares the canonical outbox table. Timestamps are
// INTEGER Unix seconds (0 / NULL where unset).
func NewOutboxTable(name string) *Table {
	t := NewTable(name)
	Add(t, BigSerial("id").PrimaryKey())
	Add(t, Text("kind").NotNull())
	Add(t, Text("aggregateType"))
	Add(t, Text("aggregateID"))
	Add(t, JSON("payload").NotNull())
	Add(t, JSON("headers"))
	Add(t, BigInt("createdAt").NotNull().Default("0"))
	Add(t, BigInt("availableAt").NotNull().Default("0"))
	Add(t, BigInt("publishedAt"))
	Add(t, BigInt("failedAt"))
	Add(t, Integer("attempts").NotNull().Default("0"))
	Add(t, Text("lastError"))
	return t
}

// Emit inserts an event using tx (the *DB from an InTx callback).
func (o *Outbox) Emit(tx *DB, ctx context.Context, kind string, payload any) error {
	return o.EmitWith(tx, ctx, kind, payload, EmitOptions{})
}

// EmitWith is the extended variant carrying aggregate metadata and
// tracing headers.
func (o *Outbox) EmitWith(tx *DB, ctx context.Context, kind string, payload any, opts EmitOptions) error {
	if kind == "" {
		return errors.New("drops/sqlite: Outbox.Emit kind cannot be empty")
	}
	raw, err := encodeJSONValue(payload)
	if err != nil {
		return err
	}
	var headers any
	if len(opts.Headers) > 0 {
		h, err := json.Marshal(opts.Headers)
		if err != nil {
			return fmt.Errorf("drops/sqlite: Outbox.Emit headers: %w", err)
		}
		headers = json.RawMessage(h)
	}
	now := o.now().Unix()
	sql := fmt.Sprintf(`INSERT INTO %s ("kind", "aggregateType", "aggregateID", "payload", "headers", "createdAt", "availableAt") VALUES (?, ?, ?, ?, ?, ?, ?)`, quoteIdent(o.table))
	if _, err := tx.Exec(ctx, sql, kind,
		outboxNullableString(opts.AggregateType),
		outboxNullableString(opts.AggregateID),
		raw, headers, now, now); err != nil {
		return fmt.Errorf("drops/sqlite: Outbox.Emit: %w", err)
	}
	return nil
}

// outboxNullableString returns nil for an empty string so the column
// stores SQL NULL rather than "".
func outboxNullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Drain fetches up to limit unpublished, non-failed, currently-available
// events in id order. Mark rows via MarkPublished / MarkFailed after
// handling. Intended for a single worker per table.
func (o *Outbox) Drain(ctx context.Context, limit int) ([]OutboxEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	sql := fmt.Sprintf(`
		SELECT "id", "kind", "aggregateType", "aggregateID", "payload", "headers", "attempts", "lastError", "createdAt"
		FROM %s
		WHERE "publishedAt" IS NULL AND "failedAt" IS NULL AND "availableAt" <= ?
		ORDER BY "id"
		LIMIT ?`, quoteIdent(o.table))
	rows, err := o.db.Query(ctx, sql, o.now().Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOutboxRows(rows)
}

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
			createdAt int64
		)
		if err := rows.Scan(&e.ID, &e.Kind, &aggType, &aggID, &e.Payload, &headers, &e.Attempts, &lastError, &createdAt); err != nil {
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
		e.CreatedAt = time.Unix(createdAt, 0).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// MarkPublished stamps publishedAt on the supplied event ids.
func (o *Outbox) MarkPublished(ctx context.Context, ids ...int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, o.now().Unix())
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	sql := fmt.Sprintf(`UPDATE %s SET "publishedAt" = ? WHERE "id" IN (%s)`,
		quoteIdent(o.table), strings.Join(placeholders, ", "))
	_, err := o.db.Exec(ctx, sql, args...)
	return err
}

// MarkFailed records a handler failure. A zero nextRetryAt parks the row
// as terminally failed (failedAt set); otherwise it becomes available
// again at nextRetryAt with attempts bumped.
func (o *Outbox) MarkFailed(ctx context.Context, id int64, attempts int, nextRetryAt time.Time, lastErr string) error {
	if nextRetryAt.IsZero() {
		sql := fmt.Sprintf(`UPDATE %s SET "attempts" = ?, "lastError" = ?, "failedAt" = ? WHERE "id" = ?`, quoteIdent(o.table))
		_, err := o.db.Exec(ctx, sql, attempts, lastErr, o.now().Unix(), id)
		return err
	}
	sql := fmt.Sprintf(`UPDATE %s SET "attempts" = ?, "lastError" = ?, "availableAt" = ? WHERE "id" = ?`, quoteIdent(o.table))
	_, err := o.db.Exec(ctx, sql, attempts, lastErr, nextRetryAt.Unix(), id)
	return err
}

// Cleanup deletes rows published more than retainAfter ago. Terminally
// failed rows are kept as the poison-message audit log.
func (o *Outbox) Cleanup(ctx context.Context, retainAfter time.Duration) (int64, error) {
	if retainAfter < 0 {
		retainAfter = 0
	}
	cutoff := o.now().Add(-retainAfter).Unix()
	sql := fmt.Sprintf(`DELETE FROM %s WHERE "publishedAt" IS NOT NULL AND "publishedAt" < ?`, quoteIdent(o.table))
	res, err := o.db.Exec(ctx, sql, cutoff)
	if err != nil {
		return 0, err
	}
	if res == nil {
		return 0, nil
	}
	return res.RowsAffected()
}

// ----------------------------------------------------------------------
// Worker
// ----------------------------------------------------------------------

// OutboxHandler is the per-event callback invoked when configured via
// OnEvent. nil marks the row published; an error schedules a retry (or
// terminal failure once MaxAttempts is reached).
type OutboxHandler func(ctx context.Context, e OutboxEvent) error

// OutboxBatchHandler receives the whole drain batch in one call. nil
// marks every event published; an error fails every event in the batch.
type OutboxBatchHandler func(ctx context.Context, events []OutboxEvent) error

// OutboxWorker polls the outbox table and forwards rows to a handler.
// Run a single worker per outbox table (SQLite has no SKIP LOCKED).
type OutboxWorker struct {
	ob           *Outbox
	interval     time.Duration
	batch        int
	maxAttempts  int
	backoff      func(attempt int) time.Duration
	handler      OutboxHandler
	batchHandler OutboxBatchHandler
	onError      func(error)
	now          func() time.Time
}

// NewOutboxWorker returns a worker with sensible defaults: 1s interval,
// 50-row batch, exponential-jitter backoff (1s base, 5min cap),
// unlimited retries.
func NewOutboxWorker(ob *Outbox) *OutboxWorker {
	return &OutboxWorker{
		ob:       ob,
		interval: time.Second,
		batch:    50,
		backoff:  ExponentialJitter(time.Second, 5*time.Minute),
		now:      time.Now,
	}
}

// WithInterval overrides the polling cadence.
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
// parked as terminally failed. Zero means unlimited.
func (w *OutboxWorker) WithMaxAttempts(n int) *OutboxWorker {
	if n > 0 {
		w.maxAttempts = n
	}
	return w
}

// WithBackoff overrides the per-attempt retry delay.
func (w *OutboxWorker) WithBackoff(fn func(attempt int) time.Duration) *OutboxWorker {
	if fn != nil {
		w.backoff = fn
	}
	return w
}

// OnEvent attaches the per-event handler (mutually exclusive with OnBatch).
func (w *OutboxWorker) OnEvent(fn OutboxHandler) *OutboxWorker {
	w.handler = fn
	w.batchHandler = nil
	return w
}

// OnBatch attaches the batch handler (mutually exclusive with OnEvent).
func (w *OutboxWorker) OnBatch(fn OutboxBatchHandler) *OutboxWorker {
	w.batchHandler = fn
	w.handler = nil
	return w
}

// OnError attaches an error sink for drain / mark failures. Handler
// errors are reported through MarkFailed and do not surface here.
func (w *OutboxWorker) OnError(fn func(error)) *OutboxWorker {
	w.onError = fn
	return w
}

// ErrNoHandler is returned by Run when neither OnEvent nor OnBatch was set.
var ErrNoHandler = errors.New("drops/sqlite: OutboxWorker has no handler")

// Run drains the outbox until ctx is cancelled. Blocks the caller;
// typically invoked as go worker.Run(ctx).
func (w *OutboxWorker) Run(ctx context.Context) error {
	if w.handler == nil && w.batchHandler == nil {
		return ErrNoHandler
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if err := w.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
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

// Tick performs one drain-and-dispatch pass. Exposed so callers can
// drive the outbox from their own scheduler instead of Run's ticker.
func (w *OutboxWorker) Tick(ctx context.Context) error {
	if w.handler == nil && w.batchHandler == nil {
		return ErrNoHandler
	}
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

func (w *OutboxWorker) runBatch(ctx context.Context, events []OutboxEvent) error {
	if herr := w.batchHandler(ctx, events); herr != nil {
		for _, e := range events {
			w.failOne(ctx, e, herr)
		}
		return nil
	}
	ids := make([]int64, len(events))
	for i, e := range events {
		ids[i] = e.ID
	}
	if err := w.ob.MarkPublished(ctx, ids...); err != nil && w.onError != nil {
		w.onError(err)
	}
	return nil
}

// failOne bumps attempts, computes the next retry time, and marks the
// row terminally failed once MaxAttempts is reached.
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
