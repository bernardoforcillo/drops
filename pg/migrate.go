package pg

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

// DefaultMigrationsTable is the table used to track applied migrations
// when no override is set on the Migrator.
const DefaultMigrationsTable = "_drops_migrations"

// Migration is one unit of schema change. Up and Down may be nil; a nil
// Down means the migration is irreversible (Down() will refuse to roll
// it back).
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

// MigrationHook runs around a migration's Up/Down body, inside the
// same transaction, so any data it reads or writes commits atomically
// with the schema change (or rolls back with it on error). This is the
// seam for data migrations that must run between schema migrations —
// backfilling a new column, copying rows into a split-out table,
// rewriting a value before an old column is dropped.
//
// Register hooks with Migrator.BeforeEach / Migrator.AfterEach. A
// "before" hook runs just before the migration body; an "after" hook
// runs just after it — in both directions. Use mig.Version / mig.Name
// to scope a data migration to a specific step, and dir to run it only
// on the way up (or only on the way down):
//
//	m.AfterEach(func(ctx context.Context, tx *pg.DB, mig pg.Migration, dir pg.MigrationDirection) error {
//		if dir == pg.DirectionUp && mig.Version == "0003" {
//			_, err := tx.Exec(ctx, `UPDATE users SET full_name = first_name || ' ' || last_name`)
//			return err
//		}
//		return nil
//	})
//
// A hook that returns an error aborts the migration: the whole
// transaction (schema change plus every hook) rolls back and Up/Down
// return the error.
type MigrationHook func(ctx context.Context, tx *DB, mig Migration, dir MigrationDirection) error

// Migrator runs database migrations and tracks their history in a table.
type Migrator struct {
	db          *DB
	table       string
	migrations  []Migration
	before      []MigrationHook
	after       []MigrationHook
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

// ErrMigrationLocked is returned by Up and Down when another run holds
// the migration lock and the wait configured by [Migrator.WithLockTimeout]
// expired. It is a distinct sentinel so a deploy script can tell "someone
// else is already migrating" — which is usually fine, and usually means
// exit 0 — from a migration that actually failed.
var ErrMigrationLocked = errors.New("drops/pg: another run holds the migration lock")

// WithLockTimeout caps how long Up and Down wait for the migration lock
// before giving up with [ErrMigrationLocked]. Zero, the default, waits
// as long as the other run takes; the wait still ends when ctx does.
//
// Set it when a deploy would rather fail fast than block behind a
// migration it cannot see. Leave it alone when every replica running
// the migrator is expected to converge, which is the usual shape: the
// winner applies, the losers wait a moment and find nothing to do.
func (m *Migrator) WithLockTimeout(d time.Duration) *Migrator {
	if d > 0 {
		m.lockTimeout = d
	}
	return m
}

// WithoutLock runs Up and Down without taking the migration lock.
//
// The lock needs a connection of its own for the duration of the run
// (see [Migrator.LockKey] for why), so a pool capped at one connection
// would deadlock against it. That is the case this exists for. It is
// not a way to run two migrators at once: without the lock they race,
// and the loser fails somewhere inside PostgreSQL's catalogue.
func (m *Migrator) WithoutLock() *Migrator { m.noLock = true; return m }

// LockKey returns the advisory-lock key this migrator serialises on,
// so an operator can find the holder in pg_locks:
//
//	SELECT * FROM pg_locks WHERE locktype = 'advisory' AND
//	       (classid::bigint << 32 | objid::bigint) = <key>
//
// The key is derived from the history table name, so two migrators
// with different histories in one database do not block each other.
func (m *Migrator) LockKey() int64 { return lockKey("drops/pg:migrate:" + m.table) }

// withLock runs fn while holding this migrator's lock.
func (m *Migrator) withLock(ctx context.Context, fn func() error) error {
	if m.noLock {
		return fn()
	}
	return withMigrationLock(ctx, m.db, m.LockKey(), m.table, m.lockTimeout, fn)
}

// withMigrationLock runs fn while holding the advisory lock keyed by
// key, waiting at most timeout (zero waits indefinitely). name appears
// in the [ErrMigrationLocked] message so an operator reading a failed
// deploy can tell which history table is contended.
//
// The lock lives in a transaction of its own. It cannot be scoped to a
// migration's transaction, because each migration gets one of those
// and the point is to hold the lock across the whole run; and it
// cannot be a session lock, because a drops.Driver is a pool and hands
// out whichever connection is free per statement, so there is no
// session to pin one to. So the run costs one extra connection, held
// idle, and released by the rollback below — there is nothing to
// commit, the transaction exists only to give the lock a lifetime.
func withMigrationLock(ctx context.Context, db *DB, key int64, name string, timeout time.Duration, fn func() error) error {
	holder, tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		rctx, cancel := rollbackCtx(ctx)
		defer cancel()
		_ = tx.Rollback(rctx)
	}()
	if timeout > 0 {
		// SET LOCAL is scoped to this transaction, and lock_timeout
		// bounds the wait on an advisory lock like on any other.
		ms := timeout.Milliseconds()
		if ms < 1 {
			ms = 1
		}
		if _, err := holder.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = %d", ms)); err != nil {
			return err
		}
	}
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", key); err != nil {
		if isLockTimeout(err) {
			return fmt.Errorf("drops/pg: waited %s for the migration lock on %s (key %d): %w",
				timeout, name, key, ErrMigrationLocked)
		}
		return err
	}
	return fn()
}

// isLockTimeout reports whether err is PostgreSQL's lock_not_available
// (55P03), which is what lock_timeout raises.
func isLockTimeout(err error) bool {
	var pgErr *PgError
	return errors.As(err, &pgErr) && pgErr.Code == "55P03"
}

// Add registers a single migration.
func (m *Migrator) Add(mig Migration) *Migrator {
	m.migrations = append(m.migrations, mig)
	return m
}

// BeforeEach registers a hook that runs immediately before every
// migration body, within the migration's transaction. Hooks fire in
// registration order; the first one to error aborts the migration.
// See MigrationHook.
func (m *Migrator) BeforeEach(h MigrationHook) *Migrator {
	m.before = append(m.before, h)
	return m
}

// AfterEach registers a hook that runs immediately after every
// migration body, within the migration's transaction. Hooks fire in
// registration order; the first one to error aborts the migration.
// This is the usual home for a data migration that depends on the
// schema change having just landed. See MigrationHook.
func (m *Migrator) AfterEach(h MigrationHook) *Migrator {
	m.after = append(m.after, h)
	return m
}

// runHooks invokes each hook in order, stopping at the first error.
func (m *Migrator) runHooks(ctx context.Context, tx *DB, hooks []MigrationHook, mig Migration, dir MigrationDirection) error {
	for _, h := range hooks {
		if h == nil {
			continue
		}
		if err := h(ctx, tx, mig, dir); err != nil {
			return err
		}
	}
	return nil
}

// AddSQL registers a migration whose Up and Down are raw SQL. downSQL may
// be empty.
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
		return fmt.Errorf("drops/pg: read migrations dir %q: %w", dir, err)
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
			return fmt.Errorf("drops/pg: migration %s has inconsistent names (%q vs %q)", v, p.name, n)
		}
		body, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return fmt.Errorf("drops/pg: read migration %q: %w", e.Name(), err)
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

// ensureTable creates the migrations history table if it does not exist.
//
// IF NOT EXISTS is not atomic against a concurrent CREATE: the two
// sessions both find the table absent, both insert into pg_type, and
// the loser reports a duplicate key on pg_type_typname_nsp_index — a
// catalogue index that names nothing an operator has ever heard of.
// Up and Down hold the migration lock, so they never see it; Status
// does not, and neither does a caller reaching this through an
// unlocked path. The failure means the table now exists, which is what
// was asked for, so it is retried once rather than reported.
func (m *Migrator) ensureTable(ctx context.Context) error {
	stmt := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		version VARCHAR(255) PRIMARY KEY,
		name TEXT NOT NULL,
		appliedAt TIMESTAMPTZ NOT NULL DEFAULT now()
	)`, quoteIdent(m.table))
	return execIgnoringConcurrentCreate(ctx, m.db, stmt)
}

// execIgnoringConcurrentCreate runs a CREATE ... IF NOT EXISTS,
// retrying once when it loses the catalogue race described above. The
// retry is safe because IF NOT EXISTS makes the statement a no-op the
// second time round, and it is bounded at one because after the first
// failure the object provably exists.
func execIgnoringConcurrentCreate(ctx context.Context, db *DB, stmt string) error {
	_, err := db.Exec(ctx, stmt)
	if err != nil && errors.Is(err, ErrUniqueViolation) {
		_, err = db.Exec(ctx, stmt)
	}
	return err
}

// applied returns the set of applied versions and their timestamps.
func (m *Migrator) applied(ctx context.Context) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	rows, err := m.db.Query(ctx,
		fmt.Sprintf("SELECT version, appliedAt FROM %s", quoteIdent(m.table)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		var t time.Time
		if err := rows.Scan(&v, &t); err != nil {
			return nil, err
		}
		out[v] = t
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
			return nil, fmt.Errorf("drops/pg: duplicate migration version %q", cp[i].Version)
		}
	}
	return cp, nil
}

// Up applies every registered migration that hasn't been applied yet, in
// version order. Each migration runs in its own transaction.
//
// The whole run is serialised against other runs by a PostgreSQL
// advisory lock, so the ordinary rolling deploy — one migrator per
// replica, all starting at the same second — converges: the first one
// applies, the rest wait, then find the history already up to date and
// do nothing. See [Migrator.WithLockTimeout] to bound the wait,
// [Migrator.WithoutLock] to skip it, and [Migrator.LockKey] to find
// the holder.
func (m *Migrator) Up(ctx context.Context) error {
	return m.withLock(ctx, func() error { return m.up(ctx) })
}

func (m *Migrator) up(ctx context.Context) error {
	if err := m.ensureTable(ctx); err != nil {
		return err
	}
	migs, err := m.sorted()
	if err != nil {
		return err
	}
	applied, err := m.applied(ctx)
	if err != nil {
		return err
	}
	for _, mig := range migs {
		if _, ok := applied[mig.Version]; ok {
			continue
		}
		if mig.Up == nil {
			return fmt.Errorf("drops/pg: migration %s has no Up", mig.Version)
		}
		if err := m.db.InTx(ctx, func(tx *DB) error {
			if err := m.runHooks(ctx, tx, m.before, mig, DirectionUp); err != nil {
				return fmt.Errorf("drops/pg: before-hook for %s_%s: %w", mig.Version, mig.Name, err)
			}
			if err := mig.Up(ctx, tx); err != nil {
				return fmt.Errorf("drops/pg: applying %s_%s: %w", mig.Version, mig.Name, err)
			}
			if err := m.runHooks(ctx, tx, m.after, mig, DirectionUp); err != nil {
				return fmt.Errorf("drops/pg: after-hook for %s_%s: %w", mig.Version, mig.Name, err)
			}
			_, err := tx.Exec(ctx,
				fmt.Sprintf("INSERT INTO %s (version, name) VALUES ($1, $2)", quoteIdent(m.table)),
				mig.Version, mig.Name,
			)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

// Down rolls back the most recently applied migration. Returns
// ErrNoMigrationsApplied if there are none. It takes the same lock Up
// does, for the same reason.
func (m *Migrator) Down(ctx context.Context) error {
	return m.withLock(ctx, func() error { return m.down(ctx) })
}

func (m *Migrator) down(ctx context.Context) error {
	if err := m.ensureTable(ctx); err != nil {
		return err
	}
	migs, err := m.sorted()
	if err != nil {
		return err
	}
	applied, err := m.applied(ctx)
	if err != nil {
		return err
	}
	// Find the highest-version applied migration.
	var target *Migration
	for i := len(migs) - 1; i >= 0; i-- {
		if _, ok := applied[migs[i].Version]; ok {
			target = &migs[i]
			break
		}
	}
	if target == nil {
		return ErrNoMigrationsApplied
	}
	if target.Down == nil {
		return fmt.Errorf("drops/pg: migration %s_%s is irreversible (no Down)", target.Version, target.Name)
	}
	return m.db.InTx(ctx, func(tx *DB) error {
		if err := m.runHooks(ctx, tx, m.before, *target, DirectionDown); err != nil {
			return fmt.Errorf("drops/pg: before-hook for %s_%s: %w", target.Version, target.Name, err)
		}
		if err := target.Down(ctx, tx); err != nil {
			return fmt.Errorf("drops/pg: rolling back %s_%s: %w", target.Version, target.Name, err)
		}
		if err := m.runHooks(ctx, tx, m.after, *target, DirectionDown); err != nil {
			return fmt.Errorf("drops/pg: after-hook for %s_%s: %w", target.Version, target.Name, err)
		}
		_, err := tx.Exec(ctx,
			fmt.Sprintf("DELETE FROM %s WHERE version = $1", quoteIdent(m.table)),
			target.Version,
		)
		return err
	})
}

// ErrNoMigrationsApplied is returned by Down when the history table is
// empty.
var ErrNoMigrationsApplied = errors.New("drops/pg: no migrations applied")

// Status reports every registered migration and whether it has been
// applied.
func (m *Migrator) Status(ctx context.Context) ([]Status, error) {
	if err := m.ensureTable(ctx); err != nil {
		return nil, err
	}
	migs, err := m.sorted()
	if err != nil {
		return nil, err
	}
	applied, err := m.applied(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Status, len(migs))
	for i, mig := range migs {
		s := Status{Version: mig.Version, Name: mig.Name}
		if t, ok := applied[mig.Version]; ok {
			s.Applied = true
			s.AppliedAt = t
		}
		out[i] = s
	}
	return out, nil
}

// quoteIdent quotes a single identifier per the SQL standard.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
