package pg

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Materialized view refresh manager — keeps derived views fresh
// without the manual "what depends on what" bookkeeping that
// usually leads to a stale dashboard at 3am.
//
// Register each materialised view with its upstream dependencies;
// when an upstream changes (or the manager's periodic scheduler
// fires), drops refreshes the view and every view downstream of it
// in topological order. Use Concurrently mode for live workloads
// to avoid the exclusive lock REFRESH MATERIALIZED VIEW takes on
// the view during rebuild.
//
//	mvm := pg.NewMatViewManager(db).
//	    Add(pg.MatView{Name: "playerStatsByRegion",
//	        DependsOn: []string{"players"},
//	        Mode: pg.RefreshConcurrently,
//	        Every: 30 * time.Second}).
//	    Add(pg.MatView{Name: "leaderboardTop100",
//	        DependsOn: []string{"playerStatsByRegion", "matches"},
//	        Mode: pg.RefreshConcurrently,
//	        Every: time.Minute})
//
//	// Manual: refresh playerStatsByRegion + leaderboardTop100 (it
//	// depends on player stats) in topological order.
//	mvm.RefreshDownstream(ctx, "players")
//
//	// Background scheduler honouring the per-view Every interval.
//	go mvm.Start(ctx)

// RefreshMode controls whether REFRESH locks the view or runs
// CONCURRENTLY (no lock, but the view must have a UNIQUE index).
type RefreshMode int

const (
	// RefreshLocking issues plain REFRESH MATERIALIZED VIEW —
	// fastest but takes ACCESS EXCLUSIVE on the view for the
	// duration. Acceptable on small views during quiet hours.
	RefreshLocking RefreshMode = iota
	// RefreshConcurrently issues REFRESH MATERIALIZED VIEW
	// CONCURRENTLY — readers stay unblocked but the view must
	// have a UNIQUE index defined.
	RefreshConcurrently
)

// MatView registers one materialised view with the manager.
type MatView struct {
	// Name is the (unquoted) view name.
	Name string

	// DependsOn lists the upstream relations (base tables and
	// other materialised views) feeding into this view. Used to
	// pick refresh order in RefreshDownstream.
	DependsOn []string

	// Mode controls REFRESH's lock semantics.
	Mode RefreshMode

	// Every, when non-zero, schedules a periodic refresh under
	// Start.
	Every time.Duration
}

// MatViewManager tracks the registered views and coordinates
// refresh across dependents.
type MatViewManager struct {
	db       *DB
	mu       sync.RWMutex
	views    map[string]MatView
	last     map[string]time.Time // last successful refresh per view
	now      func() time.Time
	pollEvery time.Duration
}

// NewMatViewManager returns an empty manager bound to db.
func NewMatViewManager(db *DB) *MatViewManager {
	return &MatViewManager{
		db:        db,
		views:     map[string]MatView{},
		last:      map[string]time.Time{},
		now:       time.Now,
		pollEvery: 250 * time.Millisecond,
	}
}

// WithPollInterval overrides how often Start wakes to check for due
// refreshes. The default (250ms) is fine for typical schedules
// measured in seconds; turn it down for sub-second cadences or up
// when refreshes are minute-scale and you want to spare the timer.
func (m *MatViewManager) WithPollInterval(d time.Duration) *MatViewManager {
	if d > 0 {
		m.pollEvery = d
	}
	return m
}

// Add registers v. Returns the manager for chaining. Duplicate
// names overwrite the prior entry — typically used when the
// schedule changes between deployments.
func (m *MatViewManager) Add(v MatView) *MatViewManager {
	if v.Name == "" {
		return m
	}
	m.mu.Lock()
	m.views[v.Name] = v
	m.mu.Unlock()
	return m
}

// Views returns the registered views in name-sorted order.
func (m *MatViewManager) Views() []MatView {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]MatView, 0, len(m.views))
	for _, v := range m.views {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// LastRefresh reports the last successful refresh time for a view,
// or zero if it has never been refreshed by this manager.
func (m *MatViewManager) LastRefresh(name string) time.Time {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.last[name]
}

// Refresh issues REFRESH MATERIALIZED VIEW (with optional
// CONCURRENTLY) on the single view name. Returns an error when the
// view is not registered or the SQL fails.
func (m *MatViewManager) Refresh(ctx context.Context, name string) error {
	m.mu.RLock()
	v, ok := m.views[name]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("drops/pg: MatViewManager.Refresh: unknown view %q", name)
	}
	return m.refreshOne(ctx, v)
}

// RefreshDownstream refreshes every view registered with the
// manager that transitively depends on upstream, in topological
// order. Used when an upstream base table has been written to and
// callers want to fan the refresh out to derived views.
func (m *MatViewManager) RefreshDownstream(ctx context.Context, upstream string) error {
	order := m.downstreamOrder(upstream)
	for _, name := range order {
		v := m.views[name]
		if err := m.refreshOne(ctx, v); err != nil {
			return err
		}
	}
	return nil
}

// RefreshAll refreshes every registered view in topological order.
// Failures abort the run.
func (m *MatViewManager) RefreshAll(ctx context.Context) error {
	order := m.topoOrder()
	for _, name := range order {
		v := m.views[name]
		if err := m.refreshOne(ctx, v); err != nil {
			return err
		}
	}
	return nil
}

// refreshOne issues the SQL for v and records the success time.
func (m *MatViewManager) refreshOne(ctx context.Context, v MatView) error {
	sql := fmt.Sprintf(`REFRESH MATERIALIZED VIEW "%s"`, v.Name)
	if v.Mode == RefreshConcurrently {
		sql = fmt.Sprintf(`REFRESH MATERIALIZED VIEW CONCURRENTLY "%s"`, v.Name)
	}
	if _, err := m.db.Exec(ctx, sql); err != nil {
		return err
	}
	m.mu.Lock()
	m.last[v.Name] = m.now()
	m.mu.Unlock()
	return nil
}

// Start runs the periodic scheduler honouring each view's Every
// interval. Blocks until ctx is cancelled.
func (m *MatViewManager) Start(ctx context.Context) error {
	ticker := time.NewTicker(m.pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			m.runDueRefreshes(ctx)
		}
	}
}

// runDueRefreshes picks every view whose Every interval has elapsed
// and refreshes it. Failures are swallowed — callers wanting strict
// behaviour should wrap RefreshAll in their own loop.
func (m *MatViewManager) runDueRefreshes(ctx context.Context) {
	now := m.now()
	m.mu.RLock()
	due := make([]MatView, 0, len(m.views))
	for _, v := range m.views {
		if v.Every <= 0 {
			continue
		}
		last := m.last[v.Name]
		if last.IsZero() || now.Sub(last) >= v.Every {
			due = append(due, v)
		}
	}
	m.mu.RUnlock()
	for _, v := range due {
		_ = m.refreshOne(ctx, v)
	}
}

// downstreamOrder returns the views that transitively depend on
// upstream, in topological order.
func (m *MatViewManager) downstreamOrder(upstream string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	// Build the dependents graph: for each view, which views
	// depend on it.
	dependents := map[string][]string{}
	for _, v := range m.views {
		for _, dep := range v.DependsOn {
			dependents[dep] = append(dependents[dep], v.Name)
		}
	}
	// Collect reachable from upstream.
	visited := map[string]bool{}
	var stack []string
	for _, n := range dependents[upstream] {
		stack = append(stack, n)
	}
	var collected []string
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visited[n] {
			continue
		}
		visited[n] = true
		collected = append(collected, n)
		stack = append(stack, dependents[n]...)
	}
	// Sort collected in topological order so each view is
	// refreshed after its dependencies.
	return m.topoSortSubset(collected)
}

// topoOrder returns every registered view in topological order.
func (m *MatViewManager) topoOrder() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.views))
	for n := range m.views {
		names = append(names, n)
	}
	return m.topoSortSubset(names)
}

// topoSortSubset orders names so each precedes its dependents. The
// algorithm is Kahn's: pick nodes with no remaining incoming edges
// from the subset, emit, repeat. Ties broken by name for stable
// output.
func (m *MatViewManager) topoSortSubset(names []string) []string {
	subset := map[string]bool{}
	for _, n := range names {
		subset[n] = true
	}
	indeg := map[string]int{}
	for _, n := range names {
		indeg[n] = 0
	}
	for _, n := range names {
		for _, dep := range m.views[n].DependsOn {
			if subset[dep] {
				indeg[n]++
			}
		}
	}
	var ready []string
	for n, d := range indeg {
		if d == 0 {
			ready = append(ready, n)
		}
	}
	sort.Strings(ready)
	var out []string
	for len(ready) > 0 {
		next := ready[0]
		ready = ready[1:]
		out = append(out, next)
		// Reduce indegree of subset views that depend on next.
		for _, candidate := range names {
			for _, dep := range m.views[candidate].DependsOn {
				if dep != next {
					continue
				}
				indeg[candidate]--
				if indeg[candidate] == 0 {
					ready = append(ready, candidate)
				}
			}
		}
		sort.Strings(ready)
	}
	return out
}

// ErrCircularDependency is returned by topological helpers when a
// cycle is detected. Kept as a package value so handlers can
// detect it without string matching.
var ErrCircularDependency = errors.New("drops/pg: MatViewManager: circular dependency")
