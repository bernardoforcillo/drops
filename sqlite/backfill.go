package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Online chunked backfill — the safe way to modify large tables without
// long locks. drops drives the chunk loop, persists per-chunk state so a
// crash resumes from the last committed checkpoint, and throttles
// between chunks to bound load.
//
//	bf := sqlite.NewBackfill(db, "userRegionFill").
//	    ChunkSize(10_000).
//	    Throttle(50 * time.Millisecond).
//	    Fetch(func(ctx context.Context, lastID int64, limit int) ([]int64, int64, error) {
//	        // SELECT id FROM users WHERE id > ? AND region IS NULL ORDER BY id LIMIT ?
//	        // return ids, max(ids), nil
//	    }).
//	    Process(func(ctx context.Context, tx *sqlite.DB, ids []int64) error {
//	        // UPDATE users SET region = ? WHERE id IN (...)
//	        return nil
//	    })
//	err := bf.Run(ctx)
//
// Each chunk is processed inside a transaction; failures roll back per
// chunk so state always advances atomically. Timestamps in the state
// table are stored as Unix seconds (INTEGER) to sidestep SQLite's
// datetime-format ambiguity.
type Backfill struct {
	db         *DB
	name       string
	chunkSize  int
	throttle   time.Duration
	stateTable string

	fetch      func(ctx context.Context, lastID int64, limit int) ([]int64, int64, error)
	process    func(ctx context.Context, tx *DB, ids []int64) error
	onProgress func(processed, lastID int64)

	now func() time.Time
}

// NewBackfill returns a Backfill bound to db. name is the unique state
// key used for resuming across restarts.
func NewBackfill(db *DB, name string) *Backfill {
	return &Backfill{
		db:         db,
		name:       name,
		chunkSize:  1000,
		throttle:   50 * time.Millisecond,
		stateTable: "backfillJobs",
		now:        time.Now,
	}
}

// ChunkSize sets the per-chunk row cap (default 1000).
func (b *Backfill) ChunkSize(n int) *Backfill {
	if n > 0 {
		b.chunkSize = n
	}
	return b
}

// Throttle sleeps between chunks to bound the rate (default 50ms; 0 runs
// as fast as possible).
func (b *Backfill) Throttle(d time.Duration) *Backfill {
	if d >= 0 {
		b.throttle = d
	}
	return b
}

// StateTable overrides the state table name (default "backfillJobs").
func (b *Backfill) StateTable(name string) *Backfill {
	if name != "" {
		b.stateTable = name
	}
	return b
}

// Fetch installs the chunk-fetching callback: given the last processed
// ID and a limit, return the next batch of IDs, the new last-ID
// (typically max(ids)), and any error. An empty slice signals
// completion.
func (b *Backfill) Fetch(fn func(ctx context.Context, lastID int64, limit int) ([]int64, int64, error)) *Backfill {
	b.fetch = fn
	return b
}

// Process installs the per-chunk worker, run inside a transaction.
func (b *Backfill) Process(fn func(ctx context.Context, tx *DB, ids []int64) error) *Backfill {
	b.process = fn
	return b
}

// OnProgress wires a callback fired after each successful chunk commit.
func (b *Backfill) OnProgress(fn func(processed, lastID int64)) *Backfill {
	b.onProgress = fn
	return b
}

// NewBackfillStateTable declares the canonical state table. Timestamps
// are INTEGER Unix seconds.
func NewBackfillStateTable(name string) *Table {
	t := NewTable(name)
	Add(t, Text("name").PrimaryKey())
	Add(t, BigInt("lastID").NotNull().Default("0"))
	Add(t, BigInt("processed").NotNull().Default("0"))
	Add(t, BigInt("completedAt"))
	Add(t, Text("lastError"))
	Add(t, BigInt("updatedAt").NotNull().Default("0"))
	return t
}

// BackfillStatus describes the persisted state of a backfill job.
type BackfillStatus struct {
	Name        string
	LastID      int64
	Processed   int64
	CompletedAt *time.Time
	UpdatedAt   time.Time
	LastError   string
}

// Status loads the current persisted state, or a zero status (LastID=0)
// when the job has never run.
func (b *Backfill) Status(ctx context.Context) (BackfillStatus, error) {
	q := fmt.Sprintf(`SELECT "name", "lastID", "processed", "completedAt", "lastError", "updatedAt" FROM %s WHERE "name" = ?`, quoteIdent(b.stateTable))
	rows, err := b.db.Query(ctx, q, b.name)
	if err != nil {
		return BackfillStatus{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return BackfillStatus{Name: b.name}, nil
	}
	var (
		s           BackfillStatus
		completedAt sql.NullInt64
		lastError   sql.NullString
		updatedAt   int64
	)
	if err := rows.Scan(&s.Name, &s.LastID, &s.Processed, &completedAt, &lastError, &updatedAt); err != nil {
		return BackfillStatus{}, err
	}
	if completedAt.Valid {
		t := time.Unix(completedAt.Int64, 0).UTC()
		s.CompletedAt = &t
	}
	if lastError.Valid {
		s.LastError = lastError.String
	}
	s.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	return s, nil
}

// Reset clears persisted state for the job.
func (b *Backfill) Reset(ctx context.Context) error {
	_, err := b.db.Exec(ctx,
		fmt.Sprintf(`DELETE FROM %s WHERE "name" = ?`, quoteIdent(b.stateTable)), b.name)
	return err
}

// Run drives the backfill loop until Fetch returns an empty batch, ctx
// is cancelled, or a chunk errors. State is persisted between chunks so
// a crash resumes from the last successful commit.
func (b *Backfill) Run(ctx context.Context) error {
	if b.fetch == nil {
		return errors.New("drops/sqlite: Backfill.Fetch not set")
	}
	if b.process == nil {
		return errors.New("drops/sqlite: Backfill.Process not set")
	}

	status, err := b.Status(ctx)
	if err != nil {
		return err
	}
	if status.CompletedAt != nil {
		return nil
	}
	lastID := status.LastID
	processed := status.Processed

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ids, nextLastID, err := b.fetch(ctx, lastID, b.chunkSize)
		if err != nil {
			_ = b.persistError(ctx, lastID, processed, err)
			return err
		}
		if len(ids) == 0 {
			return b.persistComplete(ctx, lastID, processed)
		}
		if err := b.db.InTx(ctx, func(tx *DB) error {
			return b.process(ctx, tx, ids)
		}); err != nil {
			_ = b.persistError(ctx, lastID, processed, err)
			return err
		}
		processed += int64(len(ids))
		lastID = nextLastID
		if err := b.persistProgress(ctx, lastID, processed); err != nil {
			return err
		}
		if b.onProgress != nil {
			b.onProgress(processed, lastID)
		}
		if b.throttle > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(b.throttle):
			}
		}
	}
}

// persistProgress upserts the current state row.
func (b *Backfill) persistProgress(ctx context.Context, lastID, processed int64) error {
	sql := fmt.Sprintf(`
		INSERT INTO %s ("name", "lastID", "processed", "updatedAt")
		VALUES (?, ?, ?, ?)
		ON CONFLICT ("name") DO UPDATE
		SET "lastID" = excluded."lastID",
		    "processed" = excluded."processed",
		    "lastError" = NULL,
		    "updatedAt" = excluded."updatedAt"`, quoteIdent(b.stateTable))
	_, err := b.db.Exec(ctx, sql, b.name, lastID, processed, b.now().Unix())
	return err
}

// persistComplete marks the job done (idempotent).
func (b *Backfill) persistComplete(ctx context.Context, lastID, processed int64) error {
	now := b.now().Unix()
	sql := fmt.Sprintf(`
		INSERT INTO %s ("name", "lastID", "processed", "completedAt", "updatedAt")
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT ("name") DO UPDATE
		SET "lastID" = excluded."lastID",
		    "processed" = excluded."processed",
		    "completedAt" = excluded."completedAt",
		    "updatedAt" = excluded."updatedAt"`, quoteIdent(b.stateTable))
	_, err := b.db.Exec(ctx, sql, b.name, lastID, processed, now, now)
	return err
}

// persistError records a failure without flipping the job terminal.
func (b *Backfill) persistError(ctx context.Context, lastID, processed int64, fail error) error {
	sql := fmt.Sprintf(`
		INSERT INTO %s ("name", "lastID", "processed", "lastError", "updatedAt")
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT ("name") DO UPDATE
		SET "lastID" = excluded."lastID",
		    "processed" = excluded."processed",
		    "lastError" = excluded."lastError",
		    "updatedAt" = excluded."updatedAt"`, quoteIdent(b.stateTable))
	_, err := b.db.Exec(ctx, sql, b.name, lastID, processed, fail.Error(), b.now().Unix())
	return err
}
