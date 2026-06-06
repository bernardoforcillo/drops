package pg

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Online chunked backfill — the only safe way to modify 100M+ row
// tables without taking the application offline. drops drives the
// chunk loop, persists per-chunk state so a crash can resume from
// the last committed checkpoint, throttles between chunks to bound
// the load, and (optionally) backs off when replicas are falling
// behind.
//
//	bf := pg.NewBackfill(db, "playerRegionFill-2026Q2").
//	    ChunkSize(10_000).
//	    Throttle(50 * time.Millisecond).
//	    Fetch(func(ctx context.Context, lastID int64, limit int) ([]int64, int64, error) {
//	        // SELECT id FROM players WHERE id > $1 AND region IS NULL ORDER BY id LIMIT $2
//	        rows, err := db.Query(ctx,
//	            `SELECT "id" FROM "players" WHERE "id" > $1 AND "region" IS NULL ORDER BY "id" LIMIT $2`,
//	            lastID, limit)
//	        if err != nil { return nil, lastID, err }
//	        defer rows.Close()
//	        var ids []int64
//	        var maxID int64 = lastID
//	        for rows.Next() {
//	            var id int64
//	            if err := rows.Scan(&id); err != nil { return nil, lastID, err }
//	            ids = append(ids, id)
//	            if id > maxID { maxID = id }
//	        }
//	        return ids, maxID, rows.Err()
//	    }).
//	    Process(func(ctx context.Context, tx *pg.DB, ids []int64) error {
//	        _, err := tx.Exec(ctx,
//	            `UPDATE "players" SET "region" = regionLookup("id") WHERE "id" = ANY($1)`, ids)
//	        return err
//	    }).
//	    OnProgress(func(processed, _ int64) { log.Printf("backfilled %d", processed) })
//
//	if err := bf.Run(ctx); err != nil { /* ... */ }
//
// The chunk is processed inside a transaction; failures roll back
// per chunk so state always advances atomically. State is persisted
// between chunks so crashes resume from the last committed position.

// Backfill orchestrates the chunk loop.
type Backfill struct {
	db         *DB
	name       string
	chunkSize  int
	throttle   time.Duration
	stateTable string

	fetch      func(ctx context.Context, lastID int64, limit int) ([]int64, int64, error)
	process    func(ctx context.Context, tx *DB, ids []int64) error
	onProgress func(processed, lastID int64)
	pauseLag   *replicaLagGate

	now func() time.Time
}

// NewBackfill returns a Backfill bound to db. Name is the unique
// state key used for resuming across process restarts; choose
// something descriptive ("playerRegionFill-2026Q2") so the entry in
// the state table is self-documenting.
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

// ChunkSize sets the per-chunk row cap. Default 1000. Larger chunks
// finish faster but hold locks longer and inflate WAL replication
// volume; smaller chunks are gentler but extend wall-clock time.
func (b *Backfill) ChunkSize(n int) *Backfill {
	if n > 0 {
		b.chunkSize = n
	}
	return b
}

// Throttle sleeps between chunks to bound the rate. Default 50ms.
// Set to zero to run as fast as the database can keep up — usually
// only appropriate during scheduled maintenance windows.
func (b *Backfill) Throttle(d time.Duration) *Backfill {
	if d >= 0 {
		b.throttle = d
	}
	return b
}

// StateTable overrides the state table name. Default "backfillJobs".
func (b *Backfill) StateTable(name string) *Backfill {
	if name != "" {
		b.stateTable = name
	}
	return b
}

// Fetch installs the chunk-fetching callback. It receives the last
// processed ID and a limit; returns the next batch of IDs, the new
// last-ID (typically max(ids)), and any error. Return an empty
// slice to signal completion.
func (b *Backfill) Fetch(fn func(ctx context.Context, lastID int64, limit int) ([]int64, int64, error)) *Backfill {
	b.fetch = fn
	return b
}

// Process installs the per-chunk worker. The callback runs inside a
// transaction so partial-chunk failures roll back cleanly.
func (b *Backfill) Process(fn func(ctx context.Context, tx *DB, ids []int64) error) *Backfill {
	b.process = fn
	return b
}

// OnProgress wires a progress callback fired after each successful
// chunk commit. The second argument is the latest processed ID so
// callers can render an ETA against the table's max(id).
func (b *Backfill) OnProgress(fn func(processed, lastID int64)) *Backfill {
	b.onProgress = fn
	return b
}

// PauseIfLag installs a gate that delays the next chunk while the
// configured replica set's lag exceeds threshold bytes. Pairs with
// Replicated.WithLSNTracking — the gate reuses the same primary /
// replica handles.
func (b *Backfill) PauseIfLag(repl *Replicated, thresholdBytes uint64) *Backfill {
	b.pauseLag = &replicaLagGate{repl: repl, threshold: thresholdBytes}
	return b
}

// NewBackfillStateTable declares the canonical state table layout.
// Add to your schema once; every Backfill in the system shares the
// same table keyed by name.
func NewBackfillStateTable(name string) *Table {
	t := NewTable(name)
	Add(t, Text("name").PrimaryKey())
	Add(t, BigInt("lastID").NotNull().Default("0"))
	Add(t, BigInt("processed").NotNull().Default("0"))
	Add(t, Timestamp("completedAt", true))
	Add(t, Text("lastError"))
	Add(t, Timestamp("updatedAt", true).NotNull().Default("now()"))
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

// Status loads the current persisted state. Returns a zero status
// with LastID=0 when the job has never run.
func (b *Backfill) Status(ctx context.Context) (BackfillStatus, error) {
	sql := fmt.Sprintf(`SELECT "name", "lastID", "processed", "completedAt", "lastError", "updatedAt" FROM "%s" WHERE "name" = $1`, b.stateTable)
	rows, err := b.db.Query(ctx, sql, b.name)
	if err != nil {
		return BackfillStatus{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return BackfillStatus{Name: b.name}, nil
	}
	var s BackfillStatus
	var completedAt, lastError any
	if err := rows.Scan(&s.Name, &s.LastID, &s.Processed, &completedAt, &lastError, &s.UpdatedAt); err != nil {
		return BackfillStatus{}, err
	}
	if t, ok := completedAt.(time.Time); ok {
		s.CompletedAt = &t
	}
	if e, ok := lastError.(string); ok {
		s.LastError = e
	}
	return s, nil
}

// Reset clears persisted state for the job. Run once before
// re-executing a completed backfill from scratch.
func (b *Backfill) Reset(ctx context.Context) error {
	_, err := b.db.Exec(ctx,
		fmt.Sprintf(`DELETE FROM "%s" WHERE "name" = $1`, b.stateTable), b.name)
	return err
}

// Run drives the backfill loop until the Fetch callback returns an
// empty batch, ctx is cancelled, or a chunk returns an error. State
// is persisted between chunks so a crash can resume from the last
// successful commit.
func (b *Backfill) Run(ctx context.Context) error {
	if b.fetch == nil {
		return errors.New("drops/pg: Backfill.Fetch not set")
	}
	if b.process == nil {
		return errors.New("drops/pg: Backfill.Process not set")
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
		if err := b.waitForReplicaLag(ctx); err != nil {
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
		// Process the chunk inside a transaction so partial
		// failures roll back cleanly.
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
		INSERT INTO "%s" ("name", "lastID", "processed", "updatedAt")
		VALUES ($1, $2, $3, now())
		ON CONFLICT ("name") DO UPDATE
		SET "lastID" = EXCLUDED."lastID",
		    "processed" = EXCLUDED."processed",
		    "lastError" = NULL,
		    "updatedAt" = EXCLUDED."updatedAt"`, b.stateTable)
	_, err := b.db.Exec(ctx, sql, b.name, lastID, processed)
	return err
}

// persistComplete marks the job done. Idempotent — subsequent
// Run calls return immediately.
func (b *Backfill) persistComplete(ctx context.Context, lastID, processed int64) error {
	sql := fmt.Sprintf(`
		INSERT INTO "%s" ("name", "lastID", "processed", "completedAt", "updatedAt")
		VALUES ($1, $2, $3, now(), now())
		ON CONFLICT ("name") DO UPDATE
		SET "lastID" = EXCLUDED."lastID",
		    "processed" = EXCLUDED."processed",
		    "completedAt" = EXCLUDED."completedAt",
		    "updatedAt" = EXCLUDED."updatedAt"`, b.stateTable)
	_, err := b.db.Exec(ctx, sql, b.name, lastID, processed)
	return err
}

// persistError records a failure without flipping the job to a
// terminal state — the operator decides whether to retry.
func (b *Backfill) persistError(ctx context.Context, lastID, processed int64, fail error) error {
	sql := fmt.Sprintf(`
		INSERT INTO "%s" ("name", "lastID", "processed", "lastError", "updatedAt")
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT ("name") DO UPDATE
		SET "lastID" = EXCLUDED."lastID",
		    "processed" = EXCLUDED."processed",
		    "lastError" = EXCLUDED."lastError",
		    "updatedAt" = EXCLUDED."updatedAt"`, b.stateTable)
	_, err := b.db.Exec(ctx, sql, b.name, lastID, processed, fail.Error())
	return err
}

// waitForReplicaLag is the pause-on-lag gate. No-op when PauseIfLag
// hasn't been called.
func (b *Backfill) waitForReplicaLag(ctx context.Context) error {
	if b.pauseLag == nil {
		return nil
	}
	return b.pauseLag.wait(ctx)
}

// replicaLagGate blocks until the worst replica's WAL lag is below
// threshold. Uses Replicated's primary + replicas as the source of
// truth.
type replicaLagGate struct {
	repl      *Replicated
	threshold uint64
}

func (g *replicaLagGate) wait(ctx context.Context) error {
	for {
		primaryLSN, err := queryLSN(ctx, g.repl.primary, "SELECT pg_current_wal_lsn()::text")
		if err != nil {
			return nil
		}
		worst := primaryLSN
		for _, rep := range g.repl.replicas {
			lsn, err := queryLSN(ctx, rep, "SELECT pg_last_wal_replay_lsn()::text")
			if err != nil {
				continue
			}
			if lsn < worst {
				worst = lsn
			}
		}
		lag := primaryLSN - worst
		if lag <= g.threshold {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
