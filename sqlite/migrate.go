package sqlite

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
const DefaultMigrationsTable = "_drops_sqlite_migrations"

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
	AppliedAt time.Time // zero if not applied (or unparseable)
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

// MigrationHook runs around a migration's Up/Down body, inside the same
// transaction, so any data it reads or writes commits atomically with
// the schema change (or rolls back with it on error). This is the seam
// for data migrations that must run between schema migrations —
// backfilling a new column, copying rows into a split-out table,
// rewriting a value before an old column is dropped.
//
// Register hooks with Migrator.BeforeEach / Migrator.AfterEach. A
// "before" hook runs just before the migration body; an "after" hook
// runs just after it — in both directions. Use mig.Version / mig.Name
// to scope a data migration to a specific step, and dir to run it only
// on the way up (or only on the way down):
//
//	m.AfterEach(func(ctx context.Context, tx *sqlite.DB, mig sqlite.Migration, dir sqlite.MigrationDirection) error {
//		if dir == sqlite.DirectionUp && mig.Version == "0003" {
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
	db         *DB
	table      string
	migrations []Migration
	before     []MigrationHook
	after      []MigrationHook
}

// NewMigrator returns a migrator bound to db. Add migrations with Add /
// AddSQL / AddFS, then call Up.
func NewMigrator(db *DB) *Migrator {
	return &Migrator{db: db, table: DefaultMigrationsTable}
}

// WithTable overrides the migrations history table (default
// DefaultMigrationsTable).
func (m *Migrator) WithTable(name string) *Migrator { m.table = name; return m }

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
		return fmt.Errorf("drops/sqlite: read migrations dir %q: %w", dir, err)
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
			return fmt.Errorf("drops/sqlite: migration %s has inconsistent names (%q vs %q)", v, p.name, n)
		}
		body, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return fmt.Errorf("drops/sqlite: read migration %q: %w", e.Name(), err)
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
// SQLite has no dedicated timestamp type, so appliedAt is TEXT with a
// CURRENT_TIMESTAMP default (which SQLite writes as "YYYY-MM-DD HH:MM:SS").
func (m *Migrator) ensureTable(ctx context.Context) error {
	stmt := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
	version TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	appliedAt TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
)`, quoteIdent(m.table))
	_, err := m.db.Exec(ctx, stmt)
	return err
}

// sqliteTimeLayout is the format SQLite's CURRENT_TIMESTAMP produces.
const sqliteTimeLayout = "2006-01-02 15:04:05"

// applied returns the set of applied versions and their timestamps. A
// timestamp that fails to parse leaves a zero time.Time (Applied is
// still reported true).
func (m *Migrator) applied(ctx context.Context) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	rows, err := m.db.Query(ctx,
		fmt.Sprintf("SELECT version, appliedAt FROM %s", quoteIdent(m.table)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var v, at string
		if err := rows.Scan(&v, &at); err != nil {
			return nil, err
		}
		t, perr := time.Parse(sqliteTimeLayout, at)
		if perr != nil {
			t = time.Time{}
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
			return nil, fmt.Errorf("drops/sqlite: duplicate migration version %q", cp[i].Version)
		}
	}
	return cp, nil
}

// Up applies every registered migration that hasn't been applied yet, in
// version order. Each migration runs in its own transaction.
func (m *Migrator) Up(ctx context.Context) error {
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
			return fmt.Errorf("drops/sqlite: migration %s has no Up", mig.Version)
		}
		mig := mig
		if err := m.db.InTx(ctx, func(tx *DB) error {
			if err := m.runHooks(ctx, tx, m.before, mig, DirectionUp); err != nil {
				return fmt.Errorf("drops/sqlite: before-hook for %s_%s: %w", mig.Version, mig.Name, err)
			}
			if err := mig.Up(ctx, tx); err != nil {
				return fmt.Errorf("drops/sqlite: applying %s_%s: %w", mig.Version, mig.Name, err)
			}
			if err := m.runHooks(ctx, tx, m.after, mig, DirectionUp); err != nil {
				return fmt.Errorf("drops/sqlite: after-hook for %s_%s: %w", mig.Version, mig.Name, err)
			}
			_, err := tx.Exec(ctx,
				fmt.Sprintf("INSERT INTO %s (version, name) VALUES (?, ?)", quoteIdent(m.table)),
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
// ErrNoMigrationsApplied if there are none.
func (m *Migrator) Down(ctx context.Context) error {
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
		return fmt.Errorf("drops/sqlite: migration %s_%s is irreversible (no Down)", target.Version, target.Name)
	}
	return m.db.InTx(ctx, func(tx *DB) error {
		if err := m.runHooks(ctx, tx, m.before, *target, DirectionDown); err != nil {
			return fmt.Errorf("drops/sqlite: before-hook for %s_%s: %w", target.Version, target.Name, err)
		}
		if err := target.Down(ctx, tx); err != nil {
			return fmt.Errorf("drops/sqlite: rolling back %s_%s: %w", target.Version, target.Name, err)
		}
		if err := m.runHooks(ctx, tx, m.after, *target, DirectionDown); err != nil {
			return fmt.Errorf("drops/sqlite: after-hook for %s_%s: %w", target.Version, target.Name, err)
		}
		_, err := tx.Exec(ctx,
			fmt.Sprintf("DELETE FROM %s WHERE version = ?", quoteIdent(m.table)),
			target.Version,
		)
		return err
	})
}

// ErrNoMigrationsApplied is returned by Down when the history table is
// empty.
var ErrNoMigrationsApplied = errors.New("drops/sqlite: no migrations applied")

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

// quoteIdent quotes a single identifier per the SQL standard (SQLite
// accepts double-quoted identifiers). Shared by migrate/snapshot/diff.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
