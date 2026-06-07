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
// All tracking lives in a dedicated "drops" schema so framework tables
// never pollute the application schema. The migration folder is the source
// of truth; the DB records only what has been applied and when.
//
//	// Load from a branch-per-folder layout:
//	//   migrations/main/main-001_create_users.up.sql
//	//   migrations/feat/payments/pay-001_create_payments.up.sql
//	m := pg.NewTreeMigrator(db)
//	m.AddFS(os.DirFS("."), "migrations")
//
//	m.Up(ctx)                       // apply all pending
//	m.DownTo(ctx, "main-003")       // roll back feat/payments
//	m.Checkout(ctx, "main-003")     // git-style: land exactly there

const defaultTreeSchema = "drops"

// TreeMigration is one node in the migration DAG.
type TreeMigration struct {
	// ID is the stable unique identifier — no commas allowed.
	// Recommended format: "<branch>-<seq>" e.g. "main-001", "pay-002".
	ID string

	// Name is a human-readable label used in Status output.
	Name string

	// Branch is the label for the branch this migration lives on.
	// Defaults to "main" when empty.
	Branch string

	// Parents lists IDs of migrations that must be applied before this
	// one. Empty slice = root node. Two parents = merge commit.
	Parents []string

	// Up applies the migration. nil means the node is a no-op (rare).
	Up func(ctx context.Context, db *DB) error

	// Down reverses the migration. nil means the migration is
	// irreversible — Down() will refuse to roll it back.
	Down func(ctx context.Context, db *DB) error
}

// TreeStatus is one row produced by Status.
type TreeStatus struct {
	ID        string
	Name      string
	Branch    string
	Parents   []string
	Applied   bool
	AppliedAt time.Time // zero if not applied
	IsHead    bool      // applied with no applied successors (current branch tip)
	IsReady   bool      // unapplied but all parents applied (next to run)
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
	mig := TreeMigration{ID: id, Name: name, Branch: branch, Parents: parents}
	if upSQL != "" {
		upSQL := upSQL
		mig.Up = func(ctx context.Context, db *DB) error {
			_, err := db.Exec(ctx, upSQL)
			return err
		}
	}
	if downSQL != "" {
		downSQL := downSQL
		mig.Down = func(ctx context.Context, db *DB) error {
			_, err := db.Exec(ctx, downSQL)
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
//	    main-002_add_email.up.sql        ← contains: -- drops:parents: main-001
//	  feat/payments/
//	    pay-001_create_payments.up.sql   ← contains: -- drops:parents: main-002
//	  merges/
//	    merge-001_unified.up.sql         ← contains: -- drops:parents: main-003,pay-002
//
// Each immediate subdirectory under dir is the branch name.
// File naming follows the linear migrator: <id>_<name>.{up,down}.sql
// Root nodes omit the -- drops:parents: header entirely.
func (m *TreeMigrator) AddFS(fsys fs.FS, dir string) error {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return fmt.Errorf("drops/pg: read migrations dir %q: %w", dir, err)
	}

	type fileEntry struct {
		id, name, branch, upSQL, downSQL string
		parents                          []string
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
				e.parents = parseTreeParentsHeader(string(body))
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
		m.AddSQL(e.id, e.name, e.branch, e.parents, e.upSQL, e.downSQL)
	}
	return nil
}

// parseTreeParentsHeader reads "-- drops:parents: a,b" from the header of
// an up.sql file. Returns nil for root nodes (no header present).
func parseTreeParentsHeader(sql string) []string {
	for _, line := range strings.SplitN(sql, "\n", 20) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "--") {
			break
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "--"))
		if !strings.HasPrefix(rest, "drops:parents:") {
			continue
		}
		val := strings.TrimSpace(strings.TrimPrefix(rest, "drops:parents:"))
		if val == "" {
			return nil
		}
		parts := strings.Split(val, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	return nil
}

// stripTreeHeaders removes -- drops:* comment lines from the top of sql.
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

// Up applies every pending migration in topological order, each in its
// own transaction.
func (m *TreeMigrator) Up(ctx context.Context) error {
	if err := m.ensureSchema(ctx); err != nil {
		return err
	}
	applied, err := m.appliedSet(ctx)
	if err != nil {
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
		applied[id] = time.Now()
	}
	return nil
}

// UpTo applies the target node and every ancestor required to reach it
// that is not yet applied, in topological order.
func (m *TreeMigrator) UpTo(ctx context.Context, id string) error {
	if err := m.ensureSchema(ctx); err != nil {
		return err
	}
	applied, err := m.appliedSet(ctx)
	if err != nil {
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
		applied[nid] = time.Now()
	}
	return nil
}

// Checkout applies or rolls back to land exactly on id's ancestor set —
// the git equivalent of "git checkout <commit>".
func (m *TreeMigrator) Checkout(ctx context.Context, id string) error {
	if err := m.ensureSchema(ctx); err != nil {
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

// Down rolls back all current head migrations (applied leaves) in reverse
// topological order.
func (m *TreeMigrator) Down(ctx context.Context) error {
	if err := m.ensureSchema(ctx); err != nil {
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

// DownTo rolls back all applied descendants of id, stopping before id
// itself. Use this to cleanly retract a feature branch.
//
//	// Roll back feat/payments, leave main untouched:
//	m.DownTo(ctx, "main-003")
func (m *TreeMigrator) DownTo(ctx context.Context, id string) error {
	if err := m.ensureSchema(ctx); err != nil {
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
		out = append(out, TreeStatus{
			ID: id, Name: mig.Name, Branch: mig.Branch, Parents: mig.Parents,
			Applied: true, AppliedAt: applied[id], IsHead: true,
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
		s := TreeStatus{ID: id, Name: mig.Name, Branch: mig.Branch, Parents: mig.Parents}
		if t, ok := applied[id]; ok {
			s.Applied = true
			s.AppliedAt = t
			s.IsHead = headSet[id]
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

func (m *TreeMigrator) ensureSchema(ctx context.Context) error {
	schema := quoteIdent(m.schema)
	for _, stmt := range []string{
		fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s`, schema),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.nodes (
			id         TEXT        PRIMARY KEY,
			name       TEXT        NOT NULL,
			branch     TEXT        NOT NULL DEFAULT 'main',
			parents    TEXT        NOT NULL DEFAULT '',
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`, schema),
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

func (m *TreeMigrator) appliedSet(ctx context.Context) (map[string]time.Time, error) {
	rows, err := m.db.Query(ctx,
		fmt.Sprintf(`SELECT id, applied_at FROM %s.nodes`, quoteIdent(m.schema)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var id string
		var t time.Time
		if err := rows.Scan(&id, &t); err != nil {
			return nil, err
		}
		out[id] = t
	}
	return out, rows.Err()
}

func (m *TreeMigrator) markApplied(ctx context.Context, tx *DB, mig TreeMigration) error {
	_, err := tx.Exec(ctx,
		fmt.Sprintf(`INSERT INTO %s.nodes (id, name, branch, parents)
			VALUES ($1, $2, $3, $4)`, quoteIdent(m.schema)),
		mig.ID, mig.Name, mig.Branch, strings.Join(mig.Parents, ","),
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
// target itself. Returns ErrTreeMissingParent if any referenced ID is
// not registered.
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
func (m *TreeMigrator) computeHeads(applied map[string]time.Time) []string {
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
