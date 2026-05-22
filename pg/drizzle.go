package pg

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

// DrizzleSchema is the schema where drizzle stores migration history.
const DrizzleSchema = "drizzle"

// DrizzleTable is the migration history table.
const DrizzleTable = "__drizzle_migrations"

// StatementBreakpoint is the delimiter drizzle-kit emits between
// statements when breakpoints are enabled.
const StatementBreakpoint = "--> statement-breakpoint"

// DrizzleMigrator runs migrations from a drizzle-kit-formatted directory.
type DrizzleMigrator struct {
	db     *DB
	fsys   fs.FS
	dir    string
	schema string
	table  string
}

// NewDrizzleMigrator wraps db with a migrator that reads from dir within
// fsys. dir is typically "drizzle" when using
// `//go:embed drizzle/*` from a project root that has a `drizzle/`
// directory; pass "." when fsys is already rooted at the migrations
// directory.
func NewDrizzleMigrator(db *DB, fsys fs.FS, dir string) *DrizzleMigrator {
	return &DrizzleMigrator{
		db:     db,
		fsys:   fsys,
		dir:    dir,
		schema: DrizzleSchema,
		table:  DrizzleTable,
	}
}

// WithSchema overrides the migration history schema. Match
// drizzle.config.ts's `migrationsSchema` to stay interoperable.
func (d *DrizzleMigrator) WithSchema(schema string) *DrizzleMigrator {
	d.schema = schema
	return d
}

// WithTable overrides the migration history table name. Match
// drizzle.config.ts's `migrationsTable` to stay interoperable.
func (d *DrizzleMigrator) WithTable(table string) *DrizzleMigrator {
	d.table = table
	return d
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
		return nil, fmt.Errorf("drops/pg: read drizzle journal %q: %w", p, err)
	}
	var j drizzleJournal
	if err := json.Unmarshal(body, &j); err != nil {
		return nil, fmt.Errorf("drops/pg: parse drizzle journal: %w", err)
	}
	if j.Dialect != "" && j.Dialect != "postgresql" {
		return nil, fmt.Errorf("drops/pg: drizzle journal dialect is %q; only postgresql is supported", j.Dialect)
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
			return nil, fmt.Errorf("drops/pg: read migration %s.sql: %w", e.Tag, err)
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

// ensureSchema creates the drizzle schema and migration history table.
func (d *DrizzleMigrator) ensureSchema(ctx context.Context) error {
	if _, err := d.db.Exec(ctx,
		fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s`, quoteIdent(d.schema))); err != nil {
		return err
	}
	stmt := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.%s (
		id SERIAL PRIMARY KEY,
		hash text NOT NULL,
		created_at bigint
	)`, quoteIdent(d.schema), quoteIdent(d.table))
	_, err := d.db.Exec(ctx, stmt)
	return err
}

// appliedHashes returns the set of hashes already applied.
func (d *DrizzleMigrator) appliedHashes(ctx context.Context) (map[string]bool, error) {
	rows, err := d.db.Query(ctx,
		fmt.Sprintf(`SELECT hash FROM %s.%s ORDER BY id`,
			quoteIdent(d.schema), quoteIdent(d.table)))
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
			return fmt.Errorf("drops/pg: applying drizzle migration %s: %w", e.Tag, err)
		}
	}
	return nil
}

// applyOne runs one migration plus the bookkeeping insert in a single tx.
func (d *DrizzleMigrator) applyOne(ctx context.Context, e DrizzleEntry) error {
	return d.db.InTx(ctx, func(tx *DB) error {
		for _, stmt := range splitDrizzleStatements(e.SQL, e.Breakpoints) {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("statement %q: %w", excerptSQL(stmt), err)
			}
		}
		_, err := tx.Exec(ctx,
			fmt.Sprintf(`INSERT INTO %s.%s (hash, created_at) VALUES ($1, $2)`,
				quoteIdent(d.schema), quoteIdent(d.table)),
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

// excerptSQL produces a short, single-line excerpt for error messages.
func excerptSQL(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 80 {
		return s[:77] + "..."
	}
	return s
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
