package pg_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// matViewDriver captures REFRESH MATERIALIZED VIEW statements in
// order so tests can assert dependency-aware topology.
type matViewDriver struct {
	mu        sync.Mutex
	refreshed []string
	execs     atomic.Int32
}

func (d *matViewDriver) Exec(_ context.Context, sql string, _ ...any) (drops.Result, error) {
	d.execs.Add(1)
	if strings.HasPrefix(sql, "REFRESH MATERIALIZED VIEW") {
		d.mu.Lock()
		d.refreshed = append(d.refreshed, sql)
		d.mu.Unlock()
	}
	return matViewResult{}, nil
}
func (d *matViewDriver) Query(context.Context, string, ...any) (drops.Rows, error) {
	return nil, nil
}
func (d *matViewDriver) Begin(context.Context) (drops.Tx, error) { return nil, nil }

func (d *matViewDriver) names() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.refreshed))
	for _, sql := range d.refreshed {
		// Extract the view name from REFRESH MATERIALIZED VIEW
		// [CONCURRENTLY] "name"
		left := strings.Index(sql, `"`)
		right := strings.LastIndex(sql, `"`)
		if left >= 0 && right > left {
			out = append(out, sql[left+1:right])
		}
	}
	return out
}

type matViewResult struct{}

func (matViewResult) RowsAffected() (int64, error) { return 0, nil }

func TestMatViewRefreshIssuesPlainSQL(t *testing.T) {
	drv := &matViewDriver{}
	db := pg.New(drv)
	mvm := pg.NewMatViewManager(db).
		Add(pg.MatView{Name: "leaderboard", Mode: pg.RefreshLocking})

	if err := mvm.Refresh(context.Background(), "leaderboard"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(drv.refreshed) != 1 {
		t.Fatalf("expected 1 REFRESH, got %d", len(drv.refreshed))
	}
	if strings.Contains(drv.refreshed[0], "CONCURRENTLY") {
		t.Errorf("locking mode must not emit CONCURRENTLY: %s", drv.refreshed[0])
	}
}

func TestMatViewRefreshConcurrentlyEmitsKeyword(t *testing.T) {
	drv := &matViewDriver{}
	db := pg.New(drv)
	mvm := pg.NewMatViewManager(db).
		Add(pg.MatView{Name: "leaderboard", Mode: pg.RefreshConcurrently})

	_ = mvm.Refresh(context.Background(), "leaderboard")
	if !strings.Contains(drv.refreshed[0], "CONCURRENTLY") {
		t.Errorf("concurrently mode must emit CONCURRENTLY: %s", drv.refreshed[0])
	}
}

func TestMatViewRefreshUnknownErrors(t *testing.T) {
	drv := &matViewDriver{}
	mvm := pg.NewMatViewManager(pg.New(drv))
	if err := mvm.Refresh(context.Background(), "missing"); err == nil {
		t.Error("expected error for unknown view")
	}
}

func TestMatViewRefreshDownstreamHonoursTopology(t *testing.T) {
	drv := &matViewDriver{}
	db := pg.New(drv)
	mvm := pg.NewMatViewManager(db).
		Add(pg.MatView{Name: "playerStats", DependsOn: []string{"players"}}).
		Add(pg.MatView{Name: "leaderboard", DependsOn: []string{"playerStats", "matches"}}).
		Add(pg.MatView{Name: "regionAggregates", DependsOn: []string{"players"}})

	if err := mvm.RefreshDownstream(context.Background(), "players"); err != nil {
		t.Fatalf("RefreshDownstream: %v", err)
	}
	got := drv.names()
	// playerStats must come before leaderboard (leaderboard depends
	// on playerStats). regionAggregates can come anywhere.
	idx := map[string]int{}
	for i, n := range got {
		idx[n] = i
	}
	if idx["playerStats"] >= idx["leaderboard"] {
		t.Errorf("topology violated: playerStats(%d) must precede leaderboard(%d) in %v",
			idx["playerStats"], idx["leaderboard"], got)
	}
	if _, ok := idx["regionAggregates"]; !ok {
		t.Errorf("regionAggregates should have been refreshed: %v", got)
	}
}

func TestMatViewRefreshAllCoversEveryRegisteredView(t *testing.T) {
	drv := &matViewDriver{}
	db := pg.New(drv)
	mvm := pg.NewMatViewManager(db).
		Add(pg.MatView{Name: "a"}).
		Add(pg.MatView{Name: "b", DependsOn: []string{"a"}}).
		Add(pg.MatView{Name: "c", DependsOn: []string{"b"}})

	if err := mvm.RefreshAll(context.Background()); err != nil {
		t.Fatalf("RefreshAll: %v", err)
	}
	got := drv.names()
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("topological order broken: %v", got)
	}
}

func TestMatViewLastRefreshUpdatesOnSuccess(t *testing.T) {
	drv := &matViewDriver{}
	db := pg.New(drv)
	mvm := pg.NewMatViewManager(db).Add(pg.MatView{Name: "v"})

	before := mvm.LastRefresh("v")
	if !before.IsZero() {
		t.Errorf("LastRefresh before refresh should be zero, got %v", before)
	}
	_ = mvm.Refresh(context.Background(), "v")
	if mvm.LastRefresh("v").IsZero() {
		t.Error("LastRefresh after refresh must be set")
	}
}

func TestMatViewViewsReturnsSortedSnapshot(t *testing.T) {
	mvm := pg.NewMatViewManager(pg.New(&matViewDriver{})).
		Add(pg.MatView{Name: "zeta"}).
		Add(pg.MatView{Name: "alpha"}).
		Add(pg.MatView{Name: "mu"})
	vs := mvm.Views()
	if len(vs) != 3 || vs[0].Name != "alpha" || vs[1].Name != "mu" || vs[2].Name != "zeta" {
		t.Errorf("expected name-sorted views, got %v", vs)
	}
}

func TestMatViewSchedulerRefreshesDueViews(t *testing.T) {
	drv := &matViewDriver{}
	db := pg.New(drv)
	mvm := pg.NewMatViewManager(db).
		WithPollInterval(10 * time.Millisecond).
		Add(pg.MatView{Name: "v", Every: 30 * time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	_ = mvm.Start(ctx)

	got := drv.names()
	if len(got) == 0 {
		t.Error("scheduler should have refreshed v at least once")
	}
}
