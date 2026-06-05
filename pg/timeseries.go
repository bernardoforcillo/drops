package pg

import (
	"context"
	"fmt"
	"time"
)

// Time-series ingestion at scale — IoT telemetry, message
// logs, event streams — demands partitioned tables. Without
// them the heap turns to soup within days. PostgreSQL supports
// native PARTITION BY RANGE, but managing the partitions
// (create the next one before traffic arrives, drop the old
// ones once retention is over) is the operational burden every
// team ends up writing a cron for.
//
// TimeSeriesTable encodes that pattern:
//
//	ts := pg.NewTimeSeriesTable("vehicle_events", "ts").
//	    PartitionEvery(24 * time.Hour).
//	    Retain(90 * 24 * time.Hour).
//	    Bootstrap(db, ctx)
//
//	// Scheduled (every hour):
//	ts.Maintain(db, ctx) // creates upcoming + drops expired
//
// The parent table must be declared elsewhere with PARTITION BY
// RANGE(<timeCol>) — drops doesn't generate it because column
// definitions belong to the user's schema. Bootstrap creates the
// initial partition spanning the configured window; subsequent
// EnsureNext / DropExpired calls handle ongoing maintenance.
//
// Partition naming is deterministic:
// "<table>_<YYYYMMDD>[_HH[MM]]" so a glance at \dt shows the
// time range each child covers.
//
// BRIN indexes are the cheap, scale-friendly index choice for
// time-series — drops emits one per partition via WithBrinIndex.

// TimeSeriesTable declares a partition-by-range pattern with
// time-bucketed children and an automatic retention window.
type TimeSeriesTable struct {
	parent       string
	timeCol      string
	bucketSize   time.Duration
	retention    time.Duration
	brinIndexCol string // column to BRIN-index per partition; "" = none
}

// NewTimeSeriesTable returns a fresh descriptor. parent is the
// SQL identifier of the partitioned parent table; timeCol is the
// column the parent is partitioned by.
func NewTimeSeriesTable(parent, timeCol string) *TimeSeriesTable {
	return &TimeSeriesTable{
		parent:     parent,
		timeCol:    timeCol,
		bucketSize: 24 * time.Hour,
		retention:  90 * 24 * time.Hour,
	}
}

// PartitionEvery sets the bucket size — the duration each child
// partition covers. Defaults to 24h.
func (t *TimeSeriesTable) PartitionEvery(d time.Duration) *TimeSeriesTable {
	if d > 0 {
		t.bucketSize = d
	}
	return t
}

// Retain sets the retention window — partitions whose upper
// bound is older than now-retention are dropped by DropExpired.
// Pass 0 to disable retention.
func (t *TimeSeriesTable) Retain(d time.Duration) *TimeSeriesTable {
	t.retention = d
	return t
}

// WithBrinIndex names the column that EnsureNext / Bootstrap
// will BRIN-index on each newly created partition. BRIN is the
// canonical time-series index — tiny, fast to build, great for
// range scans on naturally-ordered columns.
func (t *TimeSeriesTable) WithBrinIndex(col string) *TimeSeriesTable {
	t.brinIndexCol = col
	return t
}

// Bootstrap creates the partition that covers now (rounded down
// to the bucket boundary) and BRIN-indexes it when configured.
// Idempotent — re-running it is safe.
func (t *TimeSeriesTable) Bootstrap(db *DB, ctx context.Context) error {
	return t.ensurePartitionForTime(db, ctx, time.Now().UTC())
}

// EnsureNext creates the next `count` partitions ahead of now,
// rounded to bucket boundaries. Scheduled invocation keeps the
// window of "ready" partitions ahead of traffic.
func (t *TimeSeriesTable) EnsureNext(db *DB, ctx context.Context, count int) error {
	if count < 1 {
		count = 1
	}
	now := time.Now().UTC().Truncate(t.bucketSize)
	for i := 0; i < count; i++ {
		when := now.Add(time.Duration(i) * t.bucketSize)
		if err := t.ensurePartitionForTime(db, ctx, when); err != nil {
			return err
		}
	}
	return nil
}

// DropExpired removes every child partition whose upper bound is
// older than now-retention. Disabled when retention is 0.
// Partition discovery uses pg_inherits + pg_class, so it skips
// the parent and any partitions belonging to a different schema.
func (t *TimeSeriesTable) DropExpired(db *DB, ctx context.Context) (int, error) {
	if t.retention <= 0 {
		return 0, nil
	}
	cutoff := time.Now().UTC().Add(-t.retention).Truncate(t.bucketSize)
	// Discover children via pg_inherits.
	rows, err := db.Query(ctx, `
		SELECT c.relname
		FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		JOIN pg_class p ON p.oid = i.inhparent
		WHERE p.relname = $1`, t.parent)
	if err != nil {
		return 0, err
	}
	var children []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return 0, err
		}
		children = append(children, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	dropped := 0
	for _, name := range children {
		upper, ok := upperBoundFromName(name, t.parent, t.bucketSize)
		if !ok {
			continue
		}
		if upper.After(cutoff) {
			continue
		}
		if _, err := db.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %q`, name)); err != nil {
			return dropped, err
		}
		dropped++
	}
	return dropped, nil
}

// Maintain runs EnsureNext(2) followed by DropExpired — the
// typical "every hour" scheduled invocation.
func (t *TimeSeriesTable) Maintain(db *DB, ctx context.Context) error {
	if err := t.EnsureNext(db, ctx, 2); err != nil {
		return err
	}
	if _, err := t.DropExpired(db, ctx); err != nil {
		return err
	}
	return nil
}

// ensurePartitionForTime creates the child covering the given
// bucket, if it doesn't already exist.
func (t *TimeSeriesTable) ensurePartitionForTime(db *DB, ctx context.Context, when time.Time) error {
	lo := when.UTC().Truncate(t.bucketSize)
	hi := lo.Add(t.bucketSize)
	child := t.childName(lo)
	create := fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %q PARTITION OF %q FOR VALUES FROM ('%s') TO ('%s')`,
		child, t.parent,
		lo.Format("2006-01-02 15:04:05"),
		hi.Format("2006-01-02 15:04:05"),
	)
	if _, err := db.Exec(ctx, create); err != nil {
		return fmt.Errorf("drops/pg: create partition %s: %w", child, err)
	}
	if t.brinIndexCol != "" {
		idxName := child + "_" + t.brinIndexCol + "_brin"
		brin := fmt.Sprintf(
			`CREATE INDEX IF NOT EXISTS %q ON %q USING BRIN (%q)`,
			idxName, child, t.brinIndexCol,
		)
		if _, err := db.Exec(ctx, brin); err != nil {
			return fmt.Errorf("drops/pg: create BRIN index on %s: %w", child, err)
		}
	}
	return nil
}

// childName builds the deterministic partition identifier.
// Sub-day buckets append _HH or _HHMM as needed so partitions
// stay unique without colliding in pg_class.
func (t *TimeSeriesTable) childName(when time.Time) string {
	if t.bucketSize >= 24*time.Hour {
		return fmt.Sprintf("%s_%s", t.parent, when.Format("20060102"))
	}
	if t.bucketSize >= time.Hour {
		return fmt.Sprintf("%s_%s_%02d", t.parent, when.Format("20060102"), when.Hour())
	}
	return fmt.Sprintf("%s_%s_%02d%02d", t.parent, when.Format("20060102"), when.Hour(), when.Minute())
}

// upperBoundFromName parses the child name to recover the upper
// bound (lo + bucketSize). Returns ok=false on unrecognised
// formats so the caller skips them — partitions a human created
// manually shouldn't be dropped.
func upperBoundFromName(name, parent string, bucket time.Duration) (time.Time, bool) {
	prefix := parent + "_"
	if len(name) < len(prefix) || name[:len(prefix)] != prefix {
		return time.Time{}, false
	}
	rest := name[len(prefix):]
	formats := []string{
		"20060102_1504",
		"20060102_15",
		"20060102",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, rest); err == nil {
			return t.Add(bucket).UTC(), true
		}
	}
	return time.Time{}, false
}
