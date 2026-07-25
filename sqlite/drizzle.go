package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

// Drizzle-compatible migrations.
//
// drizzle-kit emits migrations as a directory containing one .sql file
// per migration plus meta/_journal.json describing the order. At apply
// time, drizzle-orm reads the journal, hashes each file with SHA-256,
// looks up the hash in drizzle.__drizzle_migrations, and runs the
// pending ones — splitting each file on the literal "--> statement-
// breakpoint" delimiter when the journal entry's breakpoints flag is
// true.
//
// DrizzleMigrator implements that same protocol so a migration set
// authored with drizzle-kit can be applied with drops, or vice versa,
// from the same database without conflict.

// DrizzleTable is the migration history table. SQLite has no schemas, so
// (unlike drops/pg) the table is unqualified — matching how drizzle-orm's
// sqlite driver stores its history.
const DrizzleTable = "__drizzle_migrations"

// StatementBreakpoint is the delimiter drizzle-kit emits between
// statements when breakpoints are enabled.
const StatementBreakpoint = "--> statement-breakpoint"

// DrizzleHook runs around a drizzle migration file, inside the same
// transaction as the file's SQL, so a data migration it performs
// commits atomically with the schema change (or rolls back with it on
// error). Because drizzle files are pure SQL, this is the only seam
// for Go-based data migrations — backfills, cross-table copies — that
// must run between the generated schema statements.
//
// Register hooks with DrizzleMigrator.BeforeEach / AfterEach: "before"
// runs before the file's statements, "after" runs after them. Use
// entry.Tag to scope a data migration to a specific file. A hook that
// returns an error aborts that migration; the whole transaction rolls
// back and Up returns the error.
type DrizzleHook func(ctx context.Context, tx *DB, entry DrizzleEntry) error

// DrizzleMigrator runs migrations from a drizzle-kit-formatted directory.
type DrizzleMigrator struct {
	db     *DB
	fsys   fs.FS
	dir    string
	table  string
	before []DrizzleHook
	after  []DrizzleHook
}

// NewDrizzleMigrator wraps db with a migrator that reads from dir within
// fsys. dir is typically "drizzle" when using
// `//go:embed drizzle/*` from a project root that has a `drizzle/`
// directory; pass "." when fsys is already rooted at the migrations
// directory.
func NewDrizzleMigrator(db *DB, fsys fs.FS, dir string) *DrizzleMigrator {
	return &DrizzleMigrator{
		db:    db,
		fsys:  fsys,
		dir:   dir,
		table: DrizzleTable,
	}
}

// WithTable overrides the migration history table name. Match
// drizzle.config.ts's `migrationsTable` to stay interoperable.
func (d *DrizzleMigrator) WithTable(table string) *DrizzleMigrator {
	d.table = table
	return d
}

// BeforeEach registers a hook that runs immediately before each
// migration file's statements, within that migration's transaction.
// Hooks fire in registration order; the first to error aborts the
// migration. See DrizzleHook.
func (d *DrizzleMigrator) BeforeEach(h DrizzleHook) *DrizzleMigrator {
	d.before = append(d.before, h)
	return d
}

// AfterEach registers a hook that runs immediately after each
// migration file's statements, within that migration's transaction.
// Hooks fire in registration order; the first to error aborts the
// migration. This is the usual home for a data migration that depends
// on the file's schema change having just landed. See DrizzleHook.
func (d *DrizzleMigrator) AfterEach(h DrizzleHook) *DrizzleMigrator {
	d.after = append(d.after, h)
	return d
}

// runHooks invokes each hook in order, stopping at the first error.
func (d *DrizzleMigrator) runHooks(ctx context.Context, tx *DB, hooks []DrizzleHook, e DrizzleEntry) error {
	for _, h := range hooks {
		if h == nil {
			continue
		}
		if err := h(ctx, tx, e); err != nil {
			return err
		}
	}
	return nil
}

// drizzleJournal mirrors meta/_journal.json.
type drizzleJournal struct {
	Version string                `json:"version"`
	Dialect string                `json:"dialect"`
	Entries []drizzleJournalEntry `json:"entries"`
}

type drizzleJournalEntry struct {
	Idx         int    `json:"idx"`
	Version     string `json:"version"`
	When        int64  `json:"when"`
	Tag         string `json:"tag"`
	Breakpoints bool   `json:"breakpoints"`
}

// DrizzleEntry is a parsed, hash-computed migration ready to apply.
type DrizzleEntry struct {
	Tag         string
	SQL         string
	Hash        string
	Breakpoints bool
	When        int64
}

// loadJournal reads meta/_journal.json from the configured directory.
func (d *DrizzleMigrator) loadJournal() (*drizzleJournal, error) {
	p := path.Join(d.dir, "meta", "_journal.json")
	body, err := fs.ReadFile(d.fsys, p)
	if err != nil {
		return nil, fmt.Errorf("drops/sqlite: read drizzle journal %q: %w", p, err)
	}
	var j drizzleJournal
	if err := json.Unmarshal(body, &j); err != nil {
		return nil, fmt.Errorf("drops/sqlite: parse drizzle journal: %w", err)
	}
	if j.Dialect != "" && j.Dialect != "sqlite" {
		return nil, fmt.Errorf("drops/sqlite: drizzle journal dialect is %q; only sqlite is supported", j.Dialect)
	}
	return &j, nil
}

// LoadEntries reads and hashes every migration referenced by the journal.
// Useful for tooling — Up calls it internally.
func (d *DrizzleMigrator) LoadEntries() ([]DrizzleEntry, error) {
	j, err := d.loadJournal()
	if err != nil {
		return nil, err
	}
	sort.Slice(j.Entries, func(a, b int) bool { return j.Entries[a].Idx < j.Entries[b].Idx })
	out := make([]DrizzleEntry, 0, len(j.Entries))
	for _, e := range j.Entries {
		body, err := fs.ReadFile(d.fsys, path.Join(d.dir, e.Tag+".sql"))
		if err != nil {
			return nil, fmt.Errorf("drops/sqlite: read migration %s.sql: %w", e.Tag, err)
		}
		sum := sha256.Sum256(body)
		out = append(out, DrizzleEntry{
			Tag:         e.Tag,
			SQL:         string(body),
			Hash:        hex.EncodeToString(sum[:]),
			Breakpoints: e.Breakpoints,
			When:        e.When,
		})
	}
	return out, nil
}

// ensureSchema creates the migration history table (SQLite has no
// schemas, so there is no CREATE SCHEMA step).
func (d *DrizzleMigrator) ensureSchema(ctx context.Context) error {
	stmt := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		id INTEGER PRIMARY KEY,
		hash text NOT NULL,
		created_at numeric
	)`, quoteIdent(d.table))
	_, err := d.db.Exec(ctx, stmt)
	return err
}

// appliedHashes returns the set of hashes already applied.
func (d *DrizzleMigrator) appliedHashes(ctx context.Context) (map[string]bool, error) {
	rows, err := d.db.Query(ctx,
		fmt.Sprintf(`SELECT hash FROM %s ORDER BY id`, quoteIdent(d.table)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out[h] = true
	}
	return out, rows.Err()
}

// Up applies every pending migration in journal order. Each migration
// runs in its own transaction; failure of any statement rolls back that
// migration only.
func (d *DrizzleMigrator) Up(ctx context.Context) error {
	if err := d.ensureSchema(ctx); err != nil {
		return err
	}
	entries, err := d.LoadEntries()
	if err != nil {
		return err
	}
	applied, err := d.appliedHashes(ctx)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if applied[e.Hash] {
			continue
		}
		if err := d.applyOne(ctx, e); err != nil {
			return fmt.Errorf("drops/sqlite: applying drizzle migration %s: %w", e.Tag, err)
		}
	}
	return nil
}

// applyOne runs one migration plus the bookkeeping insert in a single tx.
func (d *DrizzleMigrator) applyOne(ctx context.Context, e DrizzleEntry) error {
	return d.db.InTx(ctx, func(tx *DB) error {
		if err := d.runHooks(ctx, tx, d.before, e); err != nil {
			return fmt.Errorf("before-hook: %w", err)
		}
		for _, stmt := range splitDrizzleStatements(e.SQL, e.Breakpoints) {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("statement %q: %w", excerptSQL(stmt), err)
			}
		}
		if err := d.runHooks(ctx, tx, d.after, e); err != nil {
			return fmt.Errorf("after-hook: %w", err)
		}
		_, err := tx.Exec(ctx,
			fmt.Sprintf(`INSERT INTO %s (hash, created_at) VALUES (?, ?)`, quoteIdent(d.table)),
			e.Hash, time.Now().UnixMilli(),
		)
		return err
	})
}

// splitDrizzleStatements splits SQL on the breakpoint delimiter when
// breakpoints is true; otherwise returns the SQL whole.
func splitDrizzleStatements(sql string, breakpoints bool) []string {
	if !breakpoints {
		return []string{sql}
	}
	return strings.Split(sql, StatementBreakpoint)
}

// DrizzleStatus is one row of the Status report.
type DrizzleStatus struct {
	Tag     string
	Hash    string
	Applied bool
	When    int64 // journal timestamp (unix milliseconds)
}

// Status reports every entry in the journal and whether it has been
// applied (matched by hash, the same way drizzle-orm matches).
func (d *DrizzleMigrator) Status(ctx context.Context) ([]DrizzleStatus, error) {
	if err := d.ensureSchema(ctx); err != nil {
		return nil, err
	}
	entries, err := d.LoadEntries()
	if err != nil {
		return nil, err
	}
	applied, err := d.appliedHashes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]DrizzleStatus, len(entries))
	for i, e := range entries {
		out[i] = DrizzleStatus{
			Tag:     e.Tag,
			Hash:    e.Hash,
			Applied: applied[e.Hash],
			When:    e.When,
		}
	}
	return out, nil
}
