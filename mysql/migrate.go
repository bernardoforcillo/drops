package mysql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

// The migrator, and the one thing MySQL will not let it borrow.
//
// drops/pg and drops/sqlite run each migration inside a transaction:
// the hooks, the body and the history row commit together, so a failure
// leaves the database exactly as it was and the migration simply
// unapplied. That is not available here, and the reason is not a
// missing feature that might arrive — MySQL commits the open
// transaction IMPLICITLY when it meets DDL. CREATE TABLE, ALTER TABLE,
// DROP TABLE, TRUNCATE: each ends the transaction around it before it
// runs.
//
// Copying the transactional shape anyway would have been the worst
// option available, because it fails silently and in the direction that
// corrupts. A migration that ALTERs and then errors would have
// committed the ALTER, and the history INSERT — still inside what was
// left of the "transaction" — would roll back. The next run finds the
// migration unapplied, applies it again, and fails on a column that
// already exists. The database is changed, the record says it is not,
// and nothing reported anything.
//
// # What this does instead
//
// The history row is written in two phases, and the gap between them is
// the record that something was in flight:
//
//  1. INSERT the row with appliedAt NULL, before the migration runs.
//  2. Run the hooks and the body.
//  3. UPDATE appliedAt to now(), after they succeed.
//
// A migration that fails, or a process that dies, leaves a row with a
// NULL appliedAt. The next Up finds it and REFUSES — it does not retry
// and it does not skip. Both of those are guesses about a schema
// somebody has to look at, and the failure this design exists to
// prevent is precisely a guess that goes unnoticed.
// [ErrMigrationInterrupted] names the migration and says what the two
// ways out are.
//
// So the guarantee here is weaker than PostgreSQL's, and honestly so:
// pg promises a migration either happened or did not, and this promises
// that one which half-happened is never mistaken for either. Where a
// step must be all-or-nothing on MySQL, keep the DDL out of it and put
// the data change in a hook, which does run in a transaction of its
// own.
//
// # Two runs at once
//
// Up and Down take a named lock (MySQL's GET_LOCK) so a second process
// starting the same migrations waits rather than running them
// concurrently. It mirrors drops/pg's advisory lock; see
// [Migrator.LockKey] for the name, and [Migrator.WithoutLock] for when
// to turn it off.

// DefaultMigrationsTable is the table used to track applied migrations
// when no override is set on the Migrator.
const DefaultMigrationsTable = "_drops_mysql_migrations"

// Migration is one unit of schema change. Up and Down may be nil; a nil
// Down means the migration is irreversible (Down will refuse to roll it
// back).
type Migration struct {
	Version string // sortable string — zero-padded numeric is recommended ("0001")
	Name    string // human-readable label, used only for status output
	Up      func(ctx context.Context, db *DB) error
	Down    func(ctx context.Context, db *DB) error
}

// Status is a single row produced by Migrator.Status.
type Status struct {
	Version   string
	Name      string
	Applied   bool
	AppliedAt time.Time // zero if not applied

	// Interrupted is true for a migration that started and never
	// finished — the row exists with no appliedAt. It is not applied
	// and not unapplied, and Up refuses while it is there.
	Interrupted bool
	// StartedAt is when the interrupted (or applied) run began.
	StartedAt time.Time
}

// MigrationDirection tells a MigrationHook whether the migrator is
// applying a migration (DirectionUp) or rolling one back
// (DirectionDown).
type MigrationDirection int

const (
	// DirectionUp is passed to hooks firing during Up.
	DirectionUp MigrationDirection = iota
	// DirectionDown is passed to hooks firing during Down.
	DirectionDown
)

// String renders the direction as "up" or "down".
func (d MigrationDirection) String() string {
	if d == DirectionDown {
		return "down"
	}
	return "up"
}

// MigrationHook runs around a migration's Up/Down body. This is the
// seam for data migrations that must run between schema migrations —
// backfilling a new column, copying rows into a split-out table,
// rewriting a value before an old column is dropped.
//
// Unlike drops/pg's and drops/sqlite's, the hook does NOT share a
// transaction with the migration body, because on MySQL there is no
// such transaction to share: see the note at the top of this file. It
// gets a transaction of its own, so what the hook itself does is atomic
// — a backfill either lands or does not — but it does not roll back
// with the DDL beside it.
//
// That difference is why a hook is the right home for a data change on
// MySQL and the migration body is not. Register hooks with
// Migrator.BeforeEach / Migrator.AfterEach, and use mig.Version /
// mig.Name to scope one to a step and dir to run it in one direction:
//
//	m.AfterEach(func(ctx context.Context, tx *mysql.DB, mig mysql.Migration, dir mysql.MigrationDirection) error {
//		if dir == mysql.DirectionUp && mig.Version == "0003" {
//			_, err := tx.Exec(ctx, `UPDATE users SET status = 'active' WHERE status IS NULL`)
//			return err
//		}
//		return nil
//	})
type MigrationHook func(ctx context.Context, tx *DB, mig Migration, dir MigrationDirection) error

// Migrator applies migrations in version order and records them.
type Migrator struct {
	db         *DB
	table      string
	migrations []Migration
	before     []MigrationHook
	after      []MigrationHook

	lockTimeout time.Duration
	noLock      bool
}

// NewMigrator returns a migrator bound to db. Add migrations with Add /
// AddSQL / AddFS, then call Up.
func NewMigrator(db *DB) *Migrator {
	return &Migrator{db: db, table: DefaultMigrationsTable}
}

// WithTable overrides the migrations history table (default
// DefaultMigrationsTable).
func (m *Migrator) WithTable(name string) *Migrator { m.table = name; return m }

// WithLockTimeout caps how long Up and Down wait for the migration lock
// before giving up with [ErrMigrationLocked]. Zero, the default, waits
// ten seconds — MySQL's GET_LOCK requires a timeout and has no "wait
// for ever", so unlike drops/pg there is no unlimited setting to
// default to. A negative duration means do not wait at all.
func (m *Migrator) WithLockTimeout(d time.Duration) *Migrator {
	m.lockTimeout = d
	return m
}

// WithoutLock runs Up and Down without taking the migration lock.
//
// MySQL's named locks are held by a CONNECTION, so a migrator running
// through a pool must take and release the lock on the same one. Up
// pins a connection for the whole run to guarantee that; if your
// deployment already serialises migrations some other way — a single
// job, a leader election — this avoids the pinning.
func (m *Migrator) WithoutLock() *Migrator { m.noLock = true; return m }

// LockKey returns the name this migrator serialises on, so an operator
// can see who holds it:
//
//	SELECT * FROM performance_schema.metadata_locks;
//	SELECT IS_USED_LOCK('<key>');
//
// It is derived from the history table name, so two migrators with
// different tables do not block one another.
func (m *Migrator) LockKey() string {
	sum := sha256.Sum256([]byte("drops/mysql migrations:" + m.table))
	// MySQL 5.7+ caps a lock name at 64 characters.
	return "drops_mig_" + hex.EncodeToString(sum[:8])
}

// lockWaitSeconds is what GET_LOCK is given. MySQL takes a number of
// seconds and treats a negative one as "wait for ever" on some
// versions, which is exactly the behaviour this must not have by
// accident, so it is clamped here rather than passed through.
func (m *Migrator) lockWaitSeconds() int {
	switch {
	case m.lockTimeout < 0:
		return 0
	case m.lockTimeout == 0:
		return 10
	default:
		s := int(m.lockTimeout / time.Second)
		if s < 1 {
			s = 1
		}
		return s
	}
}

// ErrMigrationLocked is returned by Up and Down when another run holds
// the migration lock and the wait configured by
// [Migrator.WithLockTimeout] ran out.
var ErrMigrationLocked = errors.New("drops/mysql: another run holds the migration lock")

// ErrMigrationInterrupted is returned by Up when the history table
// holds a migration that started and never finished.
//
// It is not a retry and not a skip, because both are guesses about a
// database somebody has to look at. On MySQL a migration that failed
// partway has usually left some of its DDL committed — that is the
// whole reason the row is written before the body runs — so the schema
// is in a state only its author can recognise.
//
// The two ways out: finish the change by hand and mark the row applied
// (UPDATE ... SET appliedAt = NOW() WHERE version = ...), or undo the
// part that landed and delete the row.
var ErrMigrationInterrupted = errors.New("drops/mysql: a migration started and never finished")

// Add registers a single migration.
func (m *Migrator) Add(mig Migration) *Migrator {
	m.migrations = append(m.migrations, mig)
	return m
}

// BeforeEach registers a hook that runs immediately before every
// migration body. Hooks fire in registration order; the first one to
// error aborts the migration. See MigrationHook.
func (m *Migrator) BeforeEach(h MigrationHook) *Migrator {
	m.before = append(m.before, h)
	return m
}

// AfterEach registers a hook that runs immediately after every
// migration body. Hooks fire in registration order; the first one to
// error aborts the migration. This is the usual home for a data
// migration that depends on the schema change having just landed. See
// MigrationHook.
func (m *Migrator) AfterEach(h MigrationHook) *Migrator {
	m.after = append(m.after, h)
	return m
}

// runHooks invokes each hook in order, stopping at the first error.
// Each hook gets its own transaction: there is no migration-wide one to
// enrol it in, and running it with no transaction at all would make a
// half-finished backfill indistinguishable from a finished one.
func (m *Migrator) runHooks(ctx context.Context, hooks []MigrationHook, mig Migration, dir MigrationDirection) error {
	for _, h := range hooks {
		if h == nil {
			continue
		}
		if err := m.db.InTx(ctx, func(tx *DB) error {
			return h(ctx, tx, mig, dir)
		}); err != nil {
			return err
		}
	}
	return nil
}

// AddSQL registers a migration whose Up and Down are raw SQL. downSQL
// may be empty.
func (m *Migrator) AddSQL(version, name, upSQL, downSQL string) *Migrator {
	mig := Migration{Version: version, Name: name}
	if upSQL != "" {
		mig.Up = func(ctx context.Context, db *DB) error {
			_, err := db.Exec(ctx, upSQL)
			return err
		}
	}
	if downSQL != "" {
		mig.Down = func(ctx context.Context, db *DB) error {
			_, err := db.Exec(ctx, downSQL)
			return err
		}
	}
	m.migrations = append(m.migrations, mig)
	return m
}

// AddFS scans dir within fsys for migration files and registers them.
//
// Filename format: <version>_<name>.up.sql and (optionally)
// <version>_<name>.down.sql — for example, "0001_create_users.up.sql".
// Versions are compared lexicographically; zero-pad numeric versions.
func (m *Migrator) AddFS(fsys fs.FS, dir string) error {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return fmt.Errorf("drops/mysql: read migrations dir %q: %w", dir, err)
	}
	type pair struct {
		version, name, up, down string
	}
	pairs := map[string]*pair{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		v, n, kind, ok := parseMigrationName(e.Name())
		if !ok {
			continue
		}
		p, exists := pairs[v]
		if !exists {
			p = &pair{version: v, name: n}
			pairs[v] = p
		} else if p.name != n {
			return fmt.Errorf("drops/mysql: migration %s has inconsistent names (%q vs %q)", v, p.name, n)
		}
		body, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return fmt.Errorf("drops/mysql: read migration %q: %w", e.Name(), err)
		}
		switch kind {
		case "up":
			p.up = string(body)
		case "down":
			p.down = string(body)
		}
	}
	versions := make([]string, 0, len(pairs))
	for v := range pairs {
		versions = append(versions, v)
	}
	sort.Strings(versions)
	for _, v := range versions {
		p := pairs[v]
		m.AddSQL(p.version, p.name, p.up, p.down)
	}
	return nil
}

// ParseMigrationName recognises "<version>_<name>.{up,down}.sql" and
// returns the version, name and kind ("up" or "down"). It is exposed so
// callers can validate filenames before adding them.
func ParseMigrationName(filename string) (version, name, kind string, ok bool) {
	return parseMigrationName(filename)
}

// parseMigrationName recognises "<version>_<name>.{up,down}.sql".
func parseMigrationName(filename string) (version, name, kind string, ok bool) {
	if !strings.HasSuffix(filename, ".sql") {
		return "", "", "", false
	}
	stem := strings.TrimSuffix(filename, ".sql")
	switch {
	case strings.HasSuffix(stem, ".up"):
		stem = strings.TrimSuffix(stem, ".up")
		kind = "up"
	case strings.HasSuffix(stem, ".down"):
		stem = strings.TrimSuffix(stem, ".down")
		kind = "down"
	default:
		return "", "", "", false
	}
	idx := strings.IndexByte(stem, '_')
	if idx < 1 || idx == len(stem)-1 {
		return "", "", "", false
	}
	return stem[:idx], stem[idx+1:], kind, true
}

// ensureTable creates the migrations history table if it does not
// exist.
//
// appliedAt is NULLABLE and that is the whole design: a row with a NULL
// there is a migration that started and has not finished. A DEFAULT
// would defeat it, and so would DATETIME NOT NULL — this column has to
// be able to say "not yet".
func (m *Migrator) ensureTable(ctx context.Context) error {
	stmt := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n"+
		"  version VARCHAR(255) NOT NULL PRIMARY KEY,\n"+
		"  name VARCHAR(255) NOT NULL,\n"+
		"  startedAt DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,\n"+
		"  appliedAt DATETIME NULL\n"+
		") ENGINE=InnoDB", quoteIdent(m.table))
	_, err := m.db.Exec(ctx, stmt)
	return err
}

// historyRow is one row of the history table.
type historyRow struct {
	startedAt time.Time
	appliedAt time.Time
	applied   bool
}

// history reads the whole history table, applied and in-flight alike.
func (m *Migrator) history(ctx context.Context) (map[string]historyRow, error) {
	out := map[string]historyRow{}
	rows, err := m.db.Query(ctx,
		fmt.Sprintf("SELECT version, startedAt, appliedAt FROM %s", quoteIdent(m.table)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		var started time.Time
		var applied *time.Time
		if err := rows.Scan(&v, &started, &applied); err != nil {
			return nil, err
		}
		row := historyRow{startedAt: started}
		if applied != nil {
			row.applied = true
			row.appliedAt = *applied
		}
		out[v] = row
	}
	return out, rows.Err()
}

// sorted returns m.migrations sorted by Version. It also detects
// duplicate versions.
func (m *Migrator) sorted() ([]Migration, error) {
	cp := append([]Migration(nil), m.migrations...)
	sort.Slice(cp, func(i, j int) bool { return cp[i].Version < cp[j].Version })
	for i := 1; i < len(cp); i++ {
		if cp[i].Version == cp[i-1].Version {
			return nil, fmt.Errorf("drops/mysql: duplicate migration version %q", cp[i].Version)
		}
	}
	return cp, nil
}

// withLock runs fn holding the migration lock.
//
// MySQL's named locks belong to a CONNECTION rather than to a
// transaction, and a drops.Driver is a pool that hands out whichever
// connection is free per statement — so GET_LOCK on one checkout and
// RELEASE_LOCK on the next would be releasing a lock nobody took.
//
// The trick drops/pg uses works here too: open a transaction and take
// the lock on it. The transaction has nothing to commit and exists only
// to pin a connection for the length of the run, so the lock has
// somewhere to live. The one difference from pg is the release. pg uses
// pg_advisory_xact_lock, which the transaction's end drops by itself;
// MySQL's GET_LOCK outlives both COMMIT and ROLLBACK and is only
// dropped by RELEASE_LOCK or by the connection going away. Leaving that
// to the connection closing would hold the lock for as long as the pool
// keeps it, which is how the second deploy of the day hangs — so it is
// released explicitly, on the same connection, before the rollback.
//
// fn runs against the pool rather than against the holder, so the
// migrations themselves are not serialised through one connection.
func (m *Migrator) withLock(ctx context.Context, fn func(db *DB) error) error {
	if m.noLock {
		return fn(m.db)
	}
	holder, tx, err := m.db.Begin(ctx)
	if err != nil {
		return err
	}
	released := false
	defer func() {
		if !released {
			// Best effort on a context that may already be cancelled:
			// a lock left behind outlives the process's interest in it.
			rctx := context.WithoutCancel(ctx)
			_, _ = holder.Exec(rctx, "DO RELEASE_LOCK(?)", m.LockKey())
		}
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()

	var got *int64
	rows, err := holder.Query(ctx, "SELECT GET_LOCK(?, ?)", m.LockKey(), m.lockWaitSeconds())
	if err != nil {
		return fmt.Errorf("drops/mysql: taking the migration lock: %w", err)
	}
	if rows.Next() {
		if err := rows.Scan(&got); err != nil {
			_ = rows.Close()
			return fmt.Errorf("drops/mysql: taking the migration lock: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("drops/mysql: taking the migration lock: %w", err)
	}
	_ = rows.Close()
	// NULL means the wait ended in an error rather than a timeout; 0
	// means somebody else holds it. Neither is a lock.
	if got == nil || *got != 1 {
		return fmt.Errorf("%w: waited %ds for %s", ErrMigrationLocked, m.lockWaitSeconds(), m.LockKey())
	}
	defer func() {
		released = true
		_, _ = holder.Exec(context.WithoutCancel(ctx), "DO RELEASE_LOCK(?)", m.LockKey())
	}()
	return fn(m.db)
}

// Up applies every registered migration that hasn't been applied yet,
// in version order.
//
// Each migration is recorded before it runs and marked applied after,
// so a failure leaves a row nobody can mistake for either outcome. See
// the note at the top of this file, and [ErrMigrationInterrupted].
func (m *Migrator) Up(ctx context.Context) error {
	if err := m.ensureTable(ctx); err != nil {
		return err
	}
	return m.withLock(ctx, func(db *DB) error {
		migs, err := m.sorted()
		if err != nil {
			return err
		}
		hist, err := m.history(ctx)
		if err != nil {
			return err
		}
		// Refuse before doing anything, not on reaching the row: a
		// half-finished migration earlier in the order may be exactly
		// what a later one depends on.
		for _, mig := range migs {
			if row, ok := hist[mig.Version]; ok && !row.applied {
				return fmt.Errorf("%w: %s_%s started at %s and has no appliedAt. "+
					"Some of its DDL has probably landed — MySQL commits DDL as it meets it — so drops "+
					"will not guess whether to repeat it or skip it. Look at the schema, then either finish "+
					"the change and UPDATE %s SET appliedAt = NOW() WHERE version = %q, or undo what landed "+
					"and DELETE the row",
					ErrMigrationInterrupted, mig.Version, mig.Name,
					row.startedAt.Format(time.RFC3339), quoteIdent(m.table), mig.Version)
			}
		}
		for _, mig := range migs {
			if row, ok := hist[mig.Version]; ok && row.applied {
				continue
			}
			if mig.Up == nil {
				return fmt.Errorf("drops/mysql: migration %s has no Up", mig.Version)
			}
			if _, err := db.Exec(ctx,
				fmt.Sprintf("INSERT INTO %s (version, name) VALUES (?, ?)", quoteIdent(m.table)),
				mig.Version, mig.Name); err != nil {
				return fmt.Errorf("drops/mysql: claiming %s_%s: %w", mig.Version, mig.Name, err)
			}
			if err := m.runHooks(ctx, m.before, mig, DirectionUp); err != nil {
				return fmt.Errorf("drops/mysql: before-hook for %s_%s: %w", mig.Version, mig.Name, err)
			}
			if err := mig.Up(ctx, db); err != nil {
				return fmt.Errorf("drops/mysql: applying %s_%s: %w", mig.Version, mig.Name, err)
			}
			if err := m.runHooks(ctx, m.after, mig, DirectionUp); err != nil {
				return fmt.Errorf("drops/mysql: after-hook for %s_%s: %w", mig.Version, mig.Name, err)
			}
			if _, err := db.Exec(ctx,
				fmt.Sprintf("UPDATE %s SET appliedAt = NOW() WHERE version = ?", quoteIdent(m.table)),
				mig.Version); err != nil {
				return fmt.Errorf("drops/mysql: recording %s_%s as applied: %w", mig.Version, mig.Name, err)
			}
		}
		return nil
	})
}

// Down rolls back the most recently applied migration. Returns
// ErrNoMigrationsApplied if there are none.
//
// The history row is deleted only after the Down body succeeds, which
// is the same two-phase reasoning as Up read backwards: a rollback that
// failed partway leaves the row in place and the migration still
// recorded as applied, so nothing later mistakes the schema for one
// that has been rolled back.
func (m *Migrator) Down(ctx context.Context) error {
	if err := m.ensureTable(ctx); err != nil {
		return err
	}
	return m.withLock(ctx, func(db *DB) error {
		migs, err := m.sorted()
		if err != nil {
			return err
		}
		hist, err := m.history(ctx)
		if err != nil {
			return err
		}
		var target *Migration
		for i := len(migs) - 1; i >= 0; i-- {
			if row, ok := hist[migs[i].Version]; ok && row.applied {
				target = &migs[i]
				break
			}
		}
		if target == nil {
			return ErrNoMigrationsApplied
		}
		if target.Down == nil {
			return fmt.Errorf("drops/mysql: migration %s_%s is irreversible (no Down)", target.Version, target.Name)
		}
		if err := m.runHooks(ctx, m.before, *target, DirectionDown); err != nil {
			return fmt.Errorf("drops/mysql: before-hook for %s_%s: %w", target.Version, target.Name, err)
		}
		if err := target.Down(ctx, db); err != nil {
			return fmt.Errorf("drops/mysql: rolling back %s_%s: %w", target.Version, target.Name, err)
		}
		if err := m.runHooks(ctx, m.after, *target, DirectionDown); err != nil {
			return fmt.Errorf("drops/mysql: after-hook for %s_%s: %w", target.Version, target.Name, err)
		}
		_, err = db.Exec(ctx,
			fmt.Sprintf("DELETE FROM %s WHERE version = ?", quoteIdent(m.table)),
			target.Version)
		return err
	})
}

// ErrNoMigrationsApplied is returned by Down when nothing is applied.
var ErrNoMigrationsApplied = errors.New("drops/mysql: no migrations applied")

// Status reports every registered migration and whether it has been
// applied, started, or neither.
func (m *Migrator) Status(ctx context.Context) ([]Status, error) {
	if err := m.ensureTable(ctx); err != nil {
		return nil, err
	}
	migs, err := m.sorted()
	if err != nil {
		return nil, err
	}
	hist, err := m.history(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Status, len(migs))
	for i, mig := range migs {
		s := Status{Version: mig.Version, Name: mig.Name}
		if row, ok := hist[mig.Version]; ok {
			s.StartedAt = row.startedAt
			s.Applied = row.applied
			s.AppliedAt = row.appliedAt
			s.Interrupted = !row.applied
		}
		out[i] = s
	}
	return out, nil
}
