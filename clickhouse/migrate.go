package clickhouse

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

// Tree/branch migration system for ClickHouse — same git-like model as
// drops/pg but adapted for ClickHouse:
//
//   - Tracking tables live in a dedicated "drops" DATABASE (CH uses
//     databases, not schemas).
//   - No per-migration transactions (CH transaction support is limited).
//   - DateTime64(9,'UTC') timestamps, LowCardinality(String) for branch.
//   - MergeTree for the nodes table; ReplacingMergeTree for branches.
//   - Lightweight DELETE (CH 22.8+) for rollback bookkeeping.
//
// Security model
//
// Every up.sql's text is SHA-256 hashed and stored in drops.nodes.checksum
// at apply time. On every subsequent run, Up() and Plan() compare the stored
// hash against the current file. A mismatch returns ErrTreeMigrationTampered
// before any change is attempted.
//
// Note: ClickHouse has no advisory locks equivalent to pg_advisory_lock.
// For environments with concurrent migration runners, use an external
// distributed lock (Redis SETNX, ZooKeeper ephemeral node, etc.) around
// Up / Down calls.
//
// Usage
//
//	m := clickhouse.NewTreeMigrator(db)
//	_ = m.AddFS(os.DirFS("."), "migrations")
//	steps, _ := m.Plan(ctx)        // dry-run
//	m.Up(ctx)
//	m.DownTo(ctx, "main-003")
//	m.Checkout(ctx, "main-003")

const defaultTreeDatabase = "drops"

// TreeMigration is one node in the migration DAG.
type TreeMigration struct {
	ID          string
	Name        string
	Branch      string   // defaults to "main"
	Parents     []string // empty = root; two entries = merge commit
	Description string   // from -- drops:description: header
	Up          func(ctx context.Context, db *DB) error
	Down        func(ctx context.Context, db *DB) error
	upSQL       string // stored for checksum; empty for programmatic migrations
}

// TreeStatus is one row produced by Status.
type TreeStatus struct {
	ID          string
	Name        string
	Branch      string
	Parents     []string
	Description string
	Applied     bool
	AppliedAt   time.Time
	IsHead      bool  // applied with no applied successors
	IsReady     bool  // unapplied, all parents applied
	Checksum    string
	Tampered    bool  // stored checksum differs from current SQL
}

// PlanStep is one action in the dry-run output of Plan().
type PlanStep struct {
	ID          string
	Name        string
	Branch      string
	Description string
	Action      string // "apply" or "rollback"
}

// TreeMigrator manages a DAG of migrations tracked in the drops database.
type TreeMigrator struct {
	db       *DB
	database string
	nodes    map[string]TreeMigration
	order    []string
}

// Sentinel errors.
var (
	ErrTreeCycleDetected       = errors.New("drops/clickhouse: migration DAG has a cycle")
	ErrTreeMissingParent       = errors.New("drops/clickhouse: migration references unknown parent")
	ErrTreeIrreversible        = errors.New("drops/clickhouse: migration has no Down (irreversible)")
	ErrTreeNoMigrationsApplied = errors.New("drops/clickhouse: no tree migrations applied")
	ErrTreeUnknownMigration    = errors.New("drops/clickhouse: unknown migration ID")
	// ErrTreeMigrationTampered is returned when an already-applied
	// migration's SQL checksum no longer matches what was recorded at
	// apply time.
	ErrTreeMigrationTampered = errors.New("drops/clickhouse: applied migration SQL has changed — investigate before proceeding")
)

// NewTreeMigrator returns a migrator bound to db.
func NewTreeMigrator(db *DB) *TreeMigrator {
	return &TreeMigrator{
		db:       db,
		database: defaultTreeDatabase,
		nodes:    map[string]TreeMigration{},
	}
}

// WithDatabase overrides the tracking database (default "drops").
func (m *TreeMigrator) WithDatabase(database string) *TreeMigrator {
	m.database = database
	return m
}

// Add registers a migration node. Panics on duplicate ID.
func (m *TreeMigrator) Add(mig TreeMigration) *TreeMigrator {
	if mig.ID == "" {
		panic("drops/clickhouse: TreeMigration.ID must not be empty")
	}
	if strings.ContainsRune(mig.ID, ',') {
		panic(fmt.Sprintf("drops/clickhouse: migration ID %q must not contain commas", mig.ID))
	}
	if _, exists := m.nodes[mig.ID]; exists {
		panic(fmt.Sprintf("drops/clickhouse: duplicate tree migration ID %q", mig.ID))
	}
	if mig.Branch == "" {
		mig.Branch = "main"
	}
	m.nodes[mig.ID] = mig
	m.order = append(m.order, mig.ID)
	return m
}

// AddSQL registers a SQL-only migration.
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
//	    main-001_create_events.up.sql
//	    main-001_create_events.down.sql
//	  feat/payments/
//	    pay-001_payments_table.up.sql   ← -- drops:parents: main-002
//	  merges/
//	    merge-001_unified.up.sql        ← -- drops:parents: main-003,pay-002
//
// Optional header: -- drops:description: one-line summary
func (m *TreeMigrator) AddFS(fsys fs.FS, dir string) error {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return fmt.Errorf("drops/clickhouse: read migrations dir %q: %w", dir, err)
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
			return fmt.Errorf("drops/clickhouse: read branch dir %q: %w", subdir, err)
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			id, name, kind, ok := parseCHMigrationName(f.Name())
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
				return fmt.Errorf("drops/clickhouse: read %q: %w", f.Name(), err)
			}
			switch kind {
			case "up":
				e.parents, e.description = parseCHTreeHeaders(string(body))
				e.upSQL = stripCHHeaders(string(body))
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
			ID: e.id, Name: e.name, Branch: e.branch,
			Parents: e.parents, Description: e.description, upSQL: e.upSQL,
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

// parseCHMigrationName parses "<id>_<name>.{up,down}.sql".
func parseCHMigrationName(filename string) (id, name, kind string, ok bool) {
	if !strings.HasSuffix(filename, ".sql") {
		return
	}
	stem := strings.TrimSuffix(filename, ".sql")
	switch {
	case strings.HasSuffix(stem, ".up"):
		stem, kind = strings.TrimSuffix(stem, ".up"), "up"
	case strings.HasSuffix(stem, ".down"):
		stem, kind = strings.TrimSuffix(stem, ".down"), "down"
	default:
		return
	}
	idx := strings.IndexByte(stem, '_')
	if idx < 1 || idx == len(stem)-1 {
		return
	}
	return stem[:idx], stem[idx+1:], kind, true
}

// parseCHTreeHeaders reads -- drops:parents: and -- drops:description:
// from the leading comment block of an up.sql file.
func parseCHTreeHeaders(sql string) (parents []string, description string) {
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

func stripCHHeaders(sql string) string {
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

// Validate checks for cycles and missing parents without touching the DB.
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
// without touching the schema. Returns ErrTreeMigrationTampered if any
// applied migration's SQL has changed.
func (m *TreeMigrator) Plan(ctx context.Context) ([]PlanStep, error) {
	if err := m.ensureDatabase(ctx); err != nil {
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

// Up applies every pending migration in topological order.
// No transaction wrapper — CH transaction support is limited.
// If markApplied fails after a successful Up, re-running is safe
// only when Up is idempotent (recommended for CH migrations).
func (m *TreeMigrator) Up(ctx context.Context) error {
	if err := m.ensureDatabase(ctx); err != nil {
		return err
	}
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

// UpTo applies the target and all ancestors not yet applied.
func (m *TreeMigrator) UpTo(ctx context.Context, id string) error {
	if err := m.ensureDatabase(ctx); err != nil {
		return err
	}
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

// Checkout applies or rolls back to land exactly on id's ancestor set.
func (m *TreeMigrator) Checkout(ctx context.Context, id string) error {
	if err := m.ensureDatabase(ctx); err != nil {
		return err
	}
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

// Down rolls back all current head migrations in reverse topological order.
func (m *TreeMigrator) Down(ctx context.Context) error {
	if err := m.ensureDatabase(ctx); err != nil {
		return err
	}
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

// DownTo rolls back all applied descendants of id, stopping before id.
func (m *TreeMigrator) DownTo(ctx context.Context, id string) error {
	if err := m.ensureDatabase(ctx); err != nil {
		return err
	}
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

// Heads returns the applied leaf nodes — current tips of all live branches.
func (m *TreeMigrator) Heads(ctx context.Context) ([]TreeStatus, error) {
	if err := m.ensureDatabase(ctx); err != nil {
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

// Branches returns distinct branch labels sorted lexicographically.
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

// Status returns the full DAG status in topological order. Tampered is
// set true for applied migrations whose SQL has changed since apply time.
func (m *TreeMigrator) Status(ctx context.Context) ([]TreeStatus, error) {
	if err := m.ensureDatabase(ctx); err != nil {
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
				s.Tampered = chMigrationChecksum(mig.upSQL) != e.checksum
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

type appliedEntry struct {
	at       time.Time
	checksum string
}

func (m *TreeMigrator) dbPrefix() string {
	return quoteIdent(m.database) + "."
}

func (m *TreeMigrator) ensureDatabase(ctx context.Context) error {
	db := quoteIdent(m.database)
	if _, err := m.db.Exec(ctx, fmt.Sprintf(`CREATE DATABASE IF NOT EXISTS %s`, db)); err != nil {
		return err
	}
	if _, err := m.db.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %snodes (
			id          String NOT NULL,
			name        String NOT NULL,
			branch      LowCardinality(String) NOT NULL,
			parents     String NOT NULL,
			checksum    String NOT NULL,
			description String NOT NULL,
			applied_at  DateTime64(9, 'UTC') NOT NULL
		) ENGINE = MergeTree() ORDER BY id`, m.dbPrefix())); err != nil {
		return err
	}
	if _, err := m.db.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %sbranches (
			name       LowCardinality(String) NOT NULL,
			head_id    String NOT NULL,
			updated_at DateTime64(9, 'UTC') NOT NULL
		) ENGINE = ReplacingMergeTree(updated_at) ORDER BY name`, m.dbPrefix())); err != nil {
		return err
	}
	return nil
}

func (m *TreeMigrator) appliedSet(ctx context.Context) (map[string]appliedEntry, error) {
	rows, err := m.db.Query(ctx,
		fmt.Sprintf(`SELECT id, checksum, applied_at FROM %snodes`, m.dbPrefix()))
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

func (m *TreeMigrator) checkTampering(applied map[string]appliedEntry) error {
	for id, e := range applied {
		if e.checksum == "" {
			continue
		}
		node, ok := m.nodes[id]
		if !ok || node.upSQL == "" {
			continue
		}
		if chMigrationChecksum(node.upSQL) != e.checksum {
			return fmt.Errorf("%w: migration %q (applied at %s)",
				ErrTreeMigrationTampered, id, e.at.Format(time.RFC3339))
		}
	}
	return nil
}

func (m *TreeMigrator) markApplied(ctx context.Context, db *DB, mig TreeMigration) error {
	// Guard against duplicate rows — CH MergeTree has no PK enforcement.
	rows, err := db.Query(ctx,
		fmt.Sprintf(`SELECT count() FROM %snodes WHERE id = ?`, m.dbPrefix()), mig.ID)
	if err != nil {
		return err
	}
	var n int64
	if rows.Next() {
		_ = rows.Scan(&n)
	}
	rows.Close()
	if n > 0 {
		return nil
	}
	checksum := chMigrationChecksum(mig.upSQL)
	if _, err := db.Exec(ctx,
		fmt.Sprintf(`INSERT INTO %snodes (id, name, branch, parents, checksum, description, applied_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, m.dbPrefix()),
		mig.ID, mig.Name, mig.Branch, strings.Join(mig.Parents, ","),
		checksum, mig.Description, time.Now().UTC(),
	); err != nil {
		return err
	}
	_, err = db.Exec(ctx,
		fmt.Sprintf(`INSERT INTO %sbranches (name, head_id, updated_at)
			VALUES (?, ?, ?)`, m.dbPrefix()),
		mig.Branch, mig.ID, time.Now().UTC(),
	)
	return err
}

func (m *TreeMigrator) markUnapplied(ctx context.Context, db *DB, id string) error {
	// Lightweight DELETE — requires ClickHouse 22.8+.
	_, err := db.Exec(ctx,
		fmt.Sprintf(`DELETE FROM %snodes WHERE id = ?`, m.dbPrefix()), id)
	return err
}

func (m *TreeMigrator) applyOne(ctx context.Context, mig TreeMigration) error {
	if mig.Up == nil {
		return fmt.Errorf("drops/clickhouse: migration %q has no Up function", mig.ID)
	}
	if err := mig.Up(ctx, m.db); err != nil {
		return fmt.Errorf("drops/clickhouse: applying %q: %w", mig.ID, err)
	}
	return m.markApplied(ctx, m.db, mig)
}

func (m *TreeMigrator) rollbackOne(ctx context.Context, mig TreeMigration) error {
	if mig.Down == nil {
		return fmt.Errorf("%w: %q", ErrTreeIrreversible, mig.ID)
	}
	if err := mig.Down(ctx, m.db); err != nil {
		return fmt.Errorf("drops/clickhouse: rolling back %q: %w", mig.ID, err)
	}
	return m.markUnapplied(ctx, m.db, mig.ID)
}

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

func chMigrationChecksum(sql string) string {
	if sql == "" {
		return ""
	}
	h := sha256.Sum256([]byte(sql))
	return hex.EncodeToString(h[:])
}
