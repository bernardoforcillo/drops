package pg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

// Tree/branch migration system — a DAG-based migrator that sits alongside
// the linear Migrator. Each migration is a node with zero or more parent
// IDs, enabling git-like branching and merging:
//
//	main:         A → B → C
//	                   ↘
//	feat/pay:            P1 → P2   ← lives in prod for weeks
//	                          ↘
//	main (merge):              M   ← merge commit, two parents
//
// Security model
//
// All tracking lives in a dedicated "drops" schema so framework tables
// never pollute the application schema.
//
// Before any write, the migrator acquires a session-level advisory lock
// (pg_advisory_lock) keyed on the schema name. This prevents two processes
// from migrating concurrently and avoids the race condition of check-then-
// insert on the nodes table.
//
// Every up.sql's text is SHA-256 hashed and stored in drops.nodes.checksum
// when the migration is recorded. On every subsequent run, Up() and Plan()
// compare the stored hash against the current file content. A mismatch
// returns ErrTreeMigrationTampered before any schema change is attempted —
// a developer (or attacker) modified an already-applied migration file.
//
// Usage
//
//	// Load from a branch-per-folder layout:
//	//   migrations/main/main-001_create_users.up.sql
//	//   migrations/feat/payments/pay-001_create_payments.up.sql  (-- drops:parents: main-002)
//	m := pg.NewTreeMigrator(db)
//	_ = m.AddFS(os.DirFS("."), "migrations")
//
//	steps, _ := m.Plan(ctx)       // dry-run: see what would run
//	m.Up(ctx)                     // apply all pending
//	m.DownTo(ctx, "main-003")     // roll back feat/payments
//	m.Checkout(ctx, "main-003")   // git-style: land exactly there

const defaultTreeSchema = "drops"

// advisoryLockKey computes a deterministic int64 from the schema name so
// the lock is scoped to a specific drops schema within the same PG session.
func advisoryLockKey(schema string) int64 {
	h := fnv.New64a()
	h.Write([]byte("drops:tree:" + schema))
	return int64(h.Sum64())
}

// TreeMigration is one node in the migration DAG.
type TreeMigration struct {
	// ID is the stable unique identifier — no commas allowed.
	ID string

	// Name is a human-readable label used in Status output.
	Name string

	// Branch is the label for the branch this migration lives on.
	// Defaults to "main" when empty.
	Branch string

	// Parents lists IDs of migrations that must be applied before this
	// one. Empty slice = root node. Two parents = merge commit.
	Parents []string

	// Description is a one-line human summary shown in Status output.
	// Populated from the -- drops:description: header in .up.sql files.
	Description string

	// Up applies the migration. nil means the node is a no-op (rare).
	Up func(ctx context.Context, db *DB) error

	// Down reverses the migration. nil = irreversible.
	Down func(ctx context.Context, db *DB) error

	// upSQL is the raw SQL text used for checksum computation. Only
	// populated when the migration was registered via AddSQL or AddFS;
	// programmatic migrations (func-only) have an empty string and are
	// exempt from tamper checking.
	upSQL string
}

// TreeStatus is one row produced by Status.
type TreeStatus struct {
	ID          string
	Name        string
	Branch      string
	Parents     []string
	Description string
	Applied     bool
	AppliedAt   time.Time // zero if not applied
	IsHead      bool      // applied with no applied successors (current branch tip)
	IsReady     bool      // unapplied but all parents applied (next to run)
	// Checksum is the SHA-256 (truncated to 16 hex chars) of Up SQL stored
	// at apply time. Empty for programmatic migrations or legacy rows.
	Checksum string
	// Tampered is true when the stored checksum differs from the current
	// file's checksum — the migration SQL was modified after it was applied.
	Tampered bool
}

// PlanStep is one action in the dry-run output of Plan().
type PlanStep struct {
	ID          string
	Name        string
	Branch      string
	Description string
	Action      string // "apply" or "rollback"
}

// TreeMigrator manages a DAG of migrations tracked in the drops schema.
type TreeMigrator struct {
	db     *DB
	schema string
	nodes  map[string]TreeMigration
	order  []string // insertion order — drives stable topo tie-breaking
}

// Sentinel errors.
var (
	ErrTreeCycleDetected       = errors.New("drops/pg: migration DAG has a cycle")
	ErrTreeMissingParent       = errors.New("drops/pg: migration references unknown parent")
	ErrTreeIrreversible        = errors.New("drops/pg: migration has no Down (irreversible)")
	ErrTreeNoMigrationsApplied = errors.New("drops/pg: no tree migrations applied")
	ErrTreeUnknownMigration    = errors.New("drops/pg: unknown migration ID")
	// ErrTreeMigrationTampered is returned when an already-applied
	// migration's SQL checksum no longer matches what was recorded at
	// apply time. Investigate the change before running again.
	ErrTreeMigrationTampered = errors.New("drops/pg: applied migration SQL has changed — investigate before proceeding")
)

// NewTreeMigrator returns a migrator bound to db.
func NewTreeMigrator(db *DB) *TreeMigrator {
	return &TreeMigrator{
		db:     db,
		schema: defaultTreeSchema,
		nodes:  map[string]TreeMigration{},
	}
}

// WithSchema overrides the tracking schema (default "drops").
func (m *TreeMigrator) WithSchema(schema string) *TreeMigrator {
	m.schema = schema
	return m
}

// Add registers a migration node. Panics on duplicate ID so schema
// declaration bugs surface immediately at process startup.
func (m *TreeMigrator) Add(mig TreeMigration) *TreeMigrator {
	if mig.ID == "" {
		panic("drops/pg: TreeMigration.ID must not be empty")
	}
	if strings.ContainsRune(mig.ID, ',') {
		panic(fmt.Sprintf("drops/pg: migration ID %q must not contain commas", mig.ID))
	}
	if _, exists := m.nodes[mig.ID]; exists {
		panic(fmt.Sprintf("drops/pg: duplicate tree migration ID %q", mig.ID))
	}
	if mig.Branch == "" {
		mig.Branch = "main"
	}
	m.nodes[mig.ID] = mig
	m.order = append(m.order, mig.ID)
	return m
}

// AddSQL is a convenience wrapper for SQL-only migrations.
// downSQL may be empty (irreversible).
func (m *TreeMigrator) AddSQL(id, name, branch string, parents []string, upSQL, downSQL string) *TreeMigrator {
	mig := TreeMigration{ID: id, Name: name, Branch: branch, Parents: parents, upSQL: upSQL}
	if upSQL != "" {
		u := upSQL
		mig.Up = func(ctx context.Context, db *DB) error {
			_, err := db.Exec(ctx, u)
			return err
		}
	}
	if downSQL != "" {
		d := downSQL
		mig.Down = func(ctx context.Context, db *DB) error {
			_, err := db.Exec(ctx, d)
			return err
		}
	}
	return m.Add(mig)
}

// AddFS loads migrations from a branch-per-subdirectory layout.
//
// Folder structure:
//
//	<dir>/
//	  main/
//	    main-001_create_users.up.sql
//	    main-001_create_users.down.sql
//	    main-002_add_email.up.sql        ← -- drops:parents: main-001
//	  feat/payments/
//	    pay-001_create_payments.up.sql   ← -- drops:parents: main-002
//	  merges/
//	    merge-001_unified.up.sql         ← -- drops:parents: main-003,pay-002
//
// Each immediate subdirectory under dir is the branch name.
// File naming: <id>_<name>.{up,down}.sql
// Root nodes omit the -- drops:parents: header entirely.
// Optional: -- drops:description: one-line summary
func (m *TreeMigrator) AddFS(fsys fs.FS, dir string) error {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return fmt.Errorf("drops/pg: read migrations dir %q: %w", dir, err)
	}

	type fileEntry struct {
		id, name, branch, upSQL, downSQL, description string
		parents                                        []string
	}
	all := map[string]*fileEntry{}

	for _, d := range entries {
		if !d.IsDir() {
			continue
		}
		branch := d.Name()
		subdir := path.Join(dir, branch)
		files, err := fs.ReadDir(fsys, subdir)
		if err != nil {
			return fmt.Errorf("drops/pg: read branch dir %q: %w", subdir, err)
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			id, name, kind, ok := parseMigrationName(f.Name())
			if !ok {
				continue
			}
			e, exists := all[id]
			if !exists {
				e = &fileEntry{id: id, name: name, branch: branch}
				all[id] = e
			}
			body, err := fs.ReadFile(fsys, path.Join(subdir, f.Name()))
			if err != nil {
				return fmt.Errorf("drops/pg: read %q: %w", f.Name(), err)
			}
			switch kind {
			case "up":
				e.parents, e.description = parseTreeHeaders(string(body))
				e.upSQL = stripTreeHeaders(string(body))
			case "down":
				e.downSQL = string(body)
			}
		}
	}

	ids := make([]string, 0, len(all))
	for id := range all {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		e := all[id]
		mig := TreeMigration{
			ID:          e.id,
			Name:        e.name,
			Branch:      e.branch,
			Parents:     e.parents,
			Description: e.description,
			upSQL:       e.upSQL,
		}
		if e.upSQL != "" {
			u := e.upSQL
			mig.Up = func(ctx context.Context, db *DB) error {
				_, err := db.Exec(ctx, u)
				return err
			}
		}
		if e.downSQL != "" {
			d := e.downSQL
			mig.Down = func(ctx context.Context, db *DB) error {
				_, err := db.Exec(ctx, d)
				return err
			}
		}
		m.Add(mig)
	}
	return nil
}

// parseTreeHeaders reads -- drops:parents: and -- drops:description:
// from the leading comment block of an up.sql file.
func parseTreeHeaders(sql string) (parents []string, description string) {
	for _, line := range strings.SplitN(sql, "\n", 20) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "--") {
			break
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "--"))
		switch {
		case strings.HasPrefix(rest, "drops:parents:"):
			val := strings.TrimSpace(strings.TrimPrefix(rest, "drops:parents:"))
			for _, p := range strings.Split(val, ",") {
				if p = strings.TrimSpace(p); p != "" {
					parents = append(parents, p)
				}
			}
		case strings.HasPrefix(rest, "drops:description:"):
			description = strings.TrimSpace(strings.TrimPrefix(rest, "drops:description:"))
		}
	}
	return
}

// stripTreeHeaders removes -- drops:* comment lines from the top of sql
// so the actual DDL is clean when executed.
func stripTreeHeaders(sql string) string {
	lines := strings.Split(sql, "\n")
	out := make([]string, 0, len(lines))
	past := false
	for _, line := range lines {
		if !past {
			t := strings.TrimSpace(line)
			if t == "" || strings.HasPrefix(t, "-- drops:") {
				continue
			}
			past = true
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// Validate checks for cycles and missing parent references without
// touching the database. Call it once after all Add calls to fail
// fast before any schema changes.
func (m *TreeMigrator) Validate() error {
	for id, mig := range m.nodes {
		for _, p := range mig.Parents {
			if _, ok := m.nodes[p]; !ok {
				return fmt.Errorf("%w: %q (referenced by %q)", ErrTreeMissingParent, p, id)
			}
		}
	}
	names := make([]string, 0, len(m.nodes))
	for id := range m.nodes {
		names = append(names, id)
	}
	_, err := m.topoSortSubset(names)
	return err
}

// Plan returns the ordered list of migrations that Up() would apply,
// without touching the schema. Use it for CI previews or human review
// before running in production. Returns ErrTreeMigrationTampered if any
// already-applied migration's SQL has changed.
func (m *TreeMigrator) Plan(ctx context.Context) ([]PlanStep, error) {
	if err := m.ensureSchema(ctx); err != nil {
		return nil, err
	}
	applied, err := m.appliedSet(ctx)
	if err != nil {
		return nil, err
	}
	if err := m.checkTampering(applied); err != nil {
		return nil, err
	}
	pending := make([]string, 0, len(m.nodes))
	for _, id := range m.order {
		if _, ok := applied[id]; !ok {
			pending = append(pending, id)
		}
	}
	sorted, err := m.topoSortSubset(pending)
	if err != nil {
		return nil, err
	}
	steps := make([]PlanStep, 0, len(sorted))
	for _, id := range sorted {
		mig := m.nodes[id]
		steps = append(steps, PlanStep{
			ID: id, Name: mig.Name, Branch: mig.Branch,
			Description: mig.Description, Action: "apply",
		})
	}
	return steps, nil
}

// Up applies every pending migration in topological order, each in its
// own transaction. Acquires a session-level advisory lock before writing
// so concurrent processes block rather than racing.
func (m *TreeMigrator) Up(ctx context.Context) error {
	if err := m.ensureSchema(ctx); err != nil {
		return err
	}
	if err := m.acquireLock(ctx); err != nil {
		return err
	}
	defer m.releaseLock(ctx)

	applied, err := m.appliedSet(ctx)
	if err != nil {
		return err
	}
	if err := m.checkTampering(applied); err != nil {
		return err
	}
	pending := make([]string, 0, len(m.nodes))
	for _, id := range m.order {
		if _, ok := applied[id]; !ok {
			pending = append(pending, id)
		}
	}
	sorted, err := m.topoSortSubset(pending)
	if err != nil {
		return err
	}
	for _, id := range sorted {
		if err := m.applyOne(ctx, m.nodes[id]); err != nil {
			return err
		}
		applied[id] = appliedEntry{at: time.Now()}
	}
	return nil
}

// UpTo applies the target node and every ancestor required to reach it
// that is not yet applied, in topological order.
func (m *TreeMigrator) UpTo(ctx context.Context, id string) error {
	if err := m.ensureSchema(ctx); err != nil {
		return err
	}
	if err := m.acquireLock(ctx); err != nil {
		return err
	}
	defer m.releaseLock(ctx)

	applied, err := m.appliedSet(ctx)
	if err != nil {
		return err
	}
	if err := m.checkTampering(applied); err != nil {
		return err
	}
	if _, ok := applied[id]; ok {
		return nil
	}
	want, err := m.ancestorsOf(id)
	if err != nil {
		return err
	}
	pending := make([]string, 0, len(want))
	for wid := range want {
		if _, ok := applied[wid]; !ok {
			pending = append(pending, wid)
		}
	}
	sorted, err := m.topoSortSubset(pending)
	if err != nil {
		return err
	}
	for _, nid := range sorted {
		if err := m.applyOne(ctx, m.nodes[nid]); err != nil {
			return err
		}
		applied[nid] = appliedEntry{at: time.Now()}
	}
	return nil
}

// Checkout applies or rolls back to land exactly on id's ancestor set —
// the git equivalent of "git checkout <commit>".
func (m *TreeMigrator) Checkout(ctx context.Context, id string) error {
	if err := m.ensureSchema(ctx); err != nil {
		return err
	}
	if err := m.acquireLock(ctx); err != nil {
		return err
	}
	defer m.releaseLock(ctx)

	applied, err := m.appliedSet(ctx)
	if err != nil {
		return err
	}
	want, err := m.ancestorsOf(id)
	if err != nil {
		return err
	}

	toRollback := make([]string, 0)
	for aid := range applied {
		if !want[aid] {
			toRollback = append(toRollback, aid)
		}
	}
	if len(toRollback) > 0 {
		rbSorted, err := m.topoSortSubset(toRollback)
		if err != nil {
			return err
		}
		for i := len(rbSorted) - 1; i >= 0; i-- {
			if err := m.rollbackOne(ctx, m.nodes[rbSorted[i]]); err != nil {
				return err
			}
			delete(applied, rbSorted[i])
		}
	}

	toApply := make([]string, 0)
	for wid := range want {
		if _, ok := applied[wid]; !ok {
			toApply = append(toApply, wid)
		}
	}
	if len(toApply) > 0 {
		appSorted, err := m.topoSortSubset(toApply)
		if err != nil {
			return err
		}
		for _, nid := range appSorted {
			if err := m.applyOne(ctx, m.nodes[nid]); err != nil {
				return err
			}
		}
	}
	return nil
}

// Down rolls back all current head migrations (applied leaves) in reverse
// topological order.
func (m *TreeMigrator) Down(ctx context.Context) error {
	if err := m.ensureSchema(ctx); err != nil {
		return err
	}
	if err := m.acquireLock(ctx); err != nil {
		return err
	}
	defer m.releaseLock(ctx)

	applied, err := m.appliedSet(ctx)
	if err != nil {
		return err
	}
	heads := m.computeHeads(applied)
	if len(heads) == 0 {
		return ErrTreeNoMigrationsApplied
	}
	sorted, err := m.topoSortSubset(heads)
	if err != nil {
		return err
	}
	for i := len(sorted) - 1; i >= 0; i-- {
		if err := m.rollbackOne(ctx, m.nodes[sorted[i]]); err != nil {
			return err
		}
	}
	return nil
}

// DownTo rolls back all applied descendants of id, stopping before id
// itself. Use this to cleanly retract a feature branch.
//
//	// Roll back feat/payments, leave main untouched:
//	m.DownTo(ctx, "main-003")
func (m *TreeMigrator) DownTo(ctx context.Context, id string) error {
	if err := m.ensureSchema(ctx); err != nil {
		return err
	}
	if err := m.acquireLock(ctx); err != nil {
		return err
	}
	defer m.releaseLock(ctx)

	applied, err := m.appliedSet(ctx)
	if err != nil {
		return err
	}
	if _, ok := applied[id]; !ok {
		return fmt.Errorf("%w: %q is not applied", ErrTreeUnknownMigration, id)
	}
	desc := m.descendantsOf(id)
	toRollback := make([]string, 0, len(desc))
	for did := range desc {
		if _, ok := applied[did]; ok {
			toRollback = append(toRollback, did)
		}
	}
	if len(toRollback) == 0 {
		return ErrTreeNoMigrationsApplied
	}
	sorted, err := m.topoSortSubset(toRollback)
	if err != nil {
		return err
	}
	for i := len(sorted) - 1; i >= 0; i-- {
		if err := m.rollbackOne(ctx, m.nodes[sorted[i]]); err != nil {
			return err
		}
	}
	return nil
}

// Heads returns the applied leaf nodes — the current tips of all live branches.
func (m *TreeMigrator) Heads(ctx context.Context) ([]TreeStatus, error) {
	if err := m.ensureSchema(ctx); err != nil {
		return nil, err
	}
	applied, err := m.appliedSet(ctx)
	if err != nil {
		return nil, err
	}
	heads := m.computeHeads(applied)
	out := make([]TreeStatus, 0, len(heads))
	for _, id := range heads {
		mig := m.nodes[id]
		e := applied[id]
		out = append(out, TreeStatus{
			ID: id, Name: mig.Name, Branch: mig.Branch, Parents: mig.Parents,
			Description: mig.Description, Applied: true, AppliedAt: e.at,
			Checksum: e.checksum, IsHead: true,
		})
	}
	return out, nil
}

// Branches returns the distinct branch labels of all registered migrations,
// sorted lexicographically.
func (m *TreeMigrator) Branches() []string {
	seen := map[string]bool{}
	for _, mig := range m.nodes {
		seen[mig.Branch] = true
	}
	out := make([]string, 0, len(seen))
	for b := range seen {
		out = append(out, b)
	}
	sort.Strings(out)
	return out
}

// Status returns the full DAG status in topological order.
// Tampered is set true for any applied migration whose SQL has changed
// since it was applied.
func (m *TreeMigrator) Status(ctx context.Context) ([]TreeStatus, error) {
	if err := m.ensureSchema(ctx); err != nil {
		return nil, err
	}
	applied, err := m.appliedSet(ctx)
	if err != nil {
		return nil, err
	}
	headSet := map[string]bool{}
	for _, id := range m.computeHeads(applied) {
		headSet[id] = true
	}
	all := make([]string, 0, len(m.nodes))
	for id := range m.nodes {
		all = append(all, id)
	}
	sorted, err := m.topoSortSubset(all)
	if err != nil {
		sorted = all
		sort.Strings(sorted)
	}
	out := make([]TreeStatus, 0, len(sorted))
	for _, id := range sorted {
		mig := m.nodes[id]
		s := TreeStatus{
			ID: id, Name: mig.Name, Branch: mig.Branch,
			Parents: mig.Parents, Description: mig.Description,
		}
		if e, ok := applied[id]; ok {
			s.Applied = true
			s.AppliedAt = e.at
			s.Checksum = e.checksum
			s.IsHead = headSet[id]
			if e.checksum != "" && mig.upSQL != "" {
				s.Tampered = migrationChecksum(mig.upSQL) != e.checksum
			}
		} else {
			s.IsReady = true
			for _, p := range mig.Parents {
				if _, ok := applied[p]; !ok {
					s.IsReady = false
					break
				}
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// ----------------------------------------------------------------------
// Internal helpers
// ----------------------------------------------------------------------

// appliedEntry holds the state of one applied migration.
type appliedEntry struct {
	at       time.Time
	checksum string // SHA-256 hex stored at apply time; empty for legacy rows
}

func (m *TreeMigrator) ensureSchema(ctx context.Context) error {
	schema := quoteIdent(m.schema)
	for _, stmt := range []string{
		fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s`, schema),
		// Restrict PUBLIC from creating objects in the drops schema.
		// This is best-effort; it fails gracefully if the role lacks
		// GRANT OPTION — in that case the DBA must set permissions manually.
		fmt.Sprintf(`DO $$ BEGIN
			REVOKE CREATE ON SCHEMA %s FROM PUBLIC;
		EXCEPTION WHEN insufficient_privilege THEN NULL; END $$`, schema),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.nodes (
			id          TEXT        PRIMARY KEY,
			name        TEXT        NOT NULL,
			branch      TEXT        NOT NULL DEFAULT 'main',
			parents     TEXT        NOT NULL DEFAULT '',
			checksum    TEXT        NOT NULL DEFAULT '',
			description TEXT        NOT NULL DEFAULT '',
			applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)`, schema),
		// Add columns that may not exist in databases created before this version.
		fmt.Sprintf(`ALTER TABLE %s.nodes ADD COLUMN IF NOT EXISTS checksum    TEXT NOT NULL DEFAULT ''`, schema),
		fmt.Sprintf(`ALTER TABLE %s.nodes ADD COLUMN IF NOT EXISTS description TEXT NOT NULL DEFAULT ''`, schema),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.branches (
			name       TEXT        PRIMARY KEY,
			head_id    TEXT        NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`, schema),
	} {
		if _, err := m.db.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func (m *TreeMigrator) acquireLock(ctx context.Context) error {
	_, err := m.db.Exec(ctx, "SELECT pg_advisory_lock($1)", advisoryLockKey(m.schema))
	return err
}

func (m *TreeMigrator) releaseLock(ctx context.Context) {
	// Use a detached context so a cancelled caller-ctx doesn't skip unlock.
	rctx, cancel := rollbackCtx(ctx)
	defer cancel()
	_, _ = m.db.Exec(rctx, "SELECT pg_advisory_unlock($1)", advisoryLockKey(m.schema))
}

func (m *TreeMigrator) appliedSet(ctx context.Context) (map[string]appliedEntry, error) {
	rows, err := m.db.Query(ctx,
		fmt.Sprintf(`SELECT id, checksum, applied_at FROM %s.nodes`, quoteIdent(m.schema)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]appliedEntry{}
	for rows.Next() {
		var id, checksum string
		var t time.Time
		if err := rows.Scan(&id, &checksum, &t); err != nil {
			return nil, err
		}
		out[id] = appliedEntry{at: t, checksum: checksum}
	}
	return out, rows.Err()
}

// checkTampering returns ErrTreeMigrationTampered if any applied
// migration's stored checksum no longer matches its current SQL.
func (m *TreeMigrator) checkTampering(applied map[string]appliedEntry) error {
	for id, e := range applied {
		if e.checksum == "" {
			continue // legacy row, no checksum stored
		}
		node, ok := m.nodes[id]
		if !ok || node.upSQL == "" {
			continue // not registered or programmatic
		}
		if migrationChecksum(node.upSQL) != e.checksum {
			return fmt.Errorf("%w: migration %q (applied at %s)",
				ErrTreeMigrationTampered, id, e.at.Format(time.RFC3339))
		}
	}
	return nil
}

func (m *TreeMigrator) markApplied(ctx context.Context, tx *DB, mig TreeMigration) error {
	checksum := migrationChecksum(mig.upSQL)
	_, err := tx.Exec(ctx,
		fmt.Sprintf(`INSERT INTO %s.nodes (id, name, branch, parents, checksum, description)
			VALUES ($1, $2, $3, $4, $5, $6)`, quoteIdent(m.schema)),
		mig.ID, mig.Name, mig.Branch, strings.Join(mig.Parents, ","), checksum, mig.Description,
	)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		fmt.Sprintf(`INSERT INTO %s.branches (name, head_id)
			VALUES ($1, $2)
			ON CONFLICT (name) DO UPDATE
			SET head_id = EXCLUDED.head_id, updated_at = now()`, quoteIdent(m.schema)),
		mig.Branch, mig.ID,
	)
	return err
}

func (m *TreeMigrator) markUnapplied(ctx context.Context, tx *DB, id string) error {
	_, err := tx.Exec(ctx,
		fmt.Sprintf(`DELETE FROM %s.nodes WHERE id = $1`, quoteIdent(m.schema)), id)
	return err
}

func (m *TreeMigrator) applyOne(ctx context.Context, mig TreeMigration) error {
	if mig.Up == nil {
		return fmt.Errorf("drops/pg: migration %q has no Up function", mig.ID)
	}
	return m.db.InTx(ctx, func(tx *DB) error {
		if err := mig.Up(ctx, tx); err != nil {
			return fmt.Errorf("drops/pg: applying %q: %w", mig.ID, err)
		}
		return m.markApplied(ctx, tx, mig)
	})
}

func (m *TreeMigrator) rollbackOne(ctx context.Context, mig TreeMigration) error {
	if mig.Down == nil {
		return fmt.Errorf("%w: %q", ErrTreeIrreversible, mig.ID)
	}
	return m.db.InTx(ctx, func(tx *DB) error {
		if err := mig.Down(ctx, tx); err != nil {
			return fmt.Errorf("drops/pg: rolling back %q: %w", mig.ID, err)
		}
		return m.markUnapplied(ctx, tx, mig.ID)
	})
}

// topoSortSubset — Kahn's algorithm. Edges from parents outside the subset
// are treated as already satisfied. Returns ErrTreeCycleDetected if the
// induced subgraph has a cycle.
func (m *TreeMigrator) topoSortSubset(names []string) ([]string, error) {
	subset := make(map[string]bool, len(names))
	for _, n := range names {
		subset[n] = true
	}
	indeg := make(map[string]int, len(names))
	for _, n := range names {
		for _, p := range m.nodes[n].Parents {
			if subset[p] {
				indeg[n]++
			}
		}
	}
	var ready []string
	for _, n := range names {
		if indeg[n] == 0 {
			ready = append(ready, n)
		}
	}
	sort.Strings(ready)
	out := make([]string, 0, len(names))
	for len(ready) > 0 {
		next := ready[0]
		ready = ready[1:]
		out = append(out, next)
		for _, candidate := range names {
			for _, p := range m.nodes[candidate].Parents {
				if p != next {
					continue
				}
				indeg[candidate]--
				if indeg[candidate] == 0 {
					ready = append(ready, candidate)
					sort.Strings(ready)
				}
			}
		}
	}
	if len(out) < len(names) {
		return nil, fmt.Errorf("%w: %d node(s) involved", ErrTreeCycleDetected, len(names)-len(out))
	}
	return out, nil
}

// ancestorsOf returns the transitive parent closure of target, including
// target itself.
func (m *TreeMigrator) ancestorsOf(target string) (map[string]bool, error) {
	if _, ok := m.nodes[target]; !ok {
		return nil, fmt.Errorf("%w: %q", ErrTreeUnknownMigration, target)
	}
	visited := map[string]bool{}
	queue := []string{target}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur] {
			continue
		}
		visited[cur] = true
		node, ok := m.nodes[cur]
		if !ok {
			return nil, fmt.Errorf("%w: %q (ancestor of %q)", ErrTreeMissingParent, cur, target)
		}
		queue = append(queue, node.Parents...)
	}
	return visited, nil
}

// descendantsOf returns all registered nodes reachable downstream from
// target. Does NOT include target itself.
func (m *TreeMigrator) descendantsOf(target string) map[string]bool {
	children := make(map[string][]string, len(m.nodes))
	for id, node := range m.nodes {
		for _, p := range node.Parents {
			children[p] = append(children[p], id)
		}
	}
	visited := map[string]bool{}
	queue := append([]string(nil), children[target]...)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur] {
			continue
		}
		visited[cur] = true
		queue = append(queue, children[cur]...)
	}
	return visited
}

// computeHeads returns applied nodes with no applied successors.
func (m *TreeMigrator) computeHeads(applied map[string]appliedEntry) []string {
	var heads []string
	for id := range applied {
		isHead := true
	outer:
		for childID, child := range m.nodes {
			if _, ok := applied[childID]; !ok {
				continue
			}
			for _, p := range child.Parents {
				if p == id {
					isHead = false
					break outer
				}
			}
		}
		if isHead {
			heads = append(heads, id)
		}
	}
	sort.Strings(heads)
	return heads
}

// migrationChecksum returns a hex-encoded SHA-256 of sql.
func migrationChecksum(sql string) string {
	if sql == "" {
		return ""
	}
	h := sha256.Sum256([]byte(sql))
	return hex.EncodeToString(h[:])
}
