package pg_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// tsDriver records every Exec and returns canned discovery rows
// for the pg_inherits query DropExpired issues.
type tsDriver struct {
	mu          sync.Mutex
	queries     []string
	args        [][]any
	listPartFn  func() []string // returns child relnames for discovery
	dropped     []string
}

func (d *tsDriver) Exec(_ context.Context, sql string, args ...any) (drops.Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.queries = append(d.queries, sql)
	d.args = append(d.args, args)
	if strings.HasPrefix(sql, "DROP TABLE IF EXISTS") {
		// Extract identifier for the dropped list.
		i := strings.IndexByte(sql, '"')
		j := strings.LastIndexByte(sql, '"')
		if i >= 0 && j > i {
			d.dropped = append(d.dropped, sql[i+1:j])
		}
	}
	return nil, nil
}
func (d *tsDriver) Query(_ context.Context, sql string, args ...any) (drops.Rows, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.queries = append(d.queries, sql)
	d.args = append(d.args, args)
	if d.listPartFn != nil {
		names := d.listPartFn()
		data := make([][]any, len(names))
		for i, n := range names {
			data[i] = []any{n}
		}
		return &fakeRows{cols: []string{"relname"}, data: data}, nil
	}
	return &fakeRows{cols: []string{"relname"}}, nil
}
func (d *tsDriver) Begin(_ context.Context) (drops.Tx, error) { return nil, nil }

func TestBootstrapEmitsCreatePartition(t *testing.T) {
	drv := &tsDriver{}
	db := pg.New(drv)
	ts := pg.NewTimeSeriesTable("vehicle_events", "ts").
		PartitionEvery(24 * time.Hour)
	if err := ts.Bootstrap(db, context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	saw := false
	for _, q := range drv.queries {
		if strings.HasPrefix(q, "CREATE TABLE IF NOT EXISTS") &&
			strings.Contains(q, `PARTITION OF "vehicle_events"`) {
			saw = true
		}
	}
	if !saw {
		t.Errorf("Bootstrap should emit CREATE TABLE ... PARTITION OF: %v", drv.queries)
	}
}

func TestEnsureNextCreatesMultiplePartitions(t *testing.T) {
	drv := &tsDriver{}
	db := pg.New(drv)
	ts := pg.NewTimeSeriesTable("events", "ts").
		PartitionEvery(time.Hour)
	if err := ts.EnsureNext(db, context.Background(), 3); err != nil {
		t.Fatalf("EnsureNext: %v", err)
	}
	creates := 0
	for _, q := range drv.queries {
		if strings.HasPrefix(q, "CREATE TABLE IF NOT EXISTS") {
			creates++
		}
	}
	if creates != 3 {
		t.Errorf("expected 3 partition creates, got %d", creates)
	}
}

func TestWithBrinIndexAddsBrinPerPartition(t *testing.T) {
	drv := &tsDriver{}
	db := pg.New(drv)
	ts := pg.NewTimeSeriesTable("events", "ts").
		WithBrinIndex("ts")
	if err := ts.Bootstrap(db, context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	saw := false
	for _, q := range drv.queries {
		if strings.Contains(q, "USING BRIN") {
			saw = true
		}
	}
	if !saw {
		t.Errorf("WithBrinIndex should emit USING BRIN: %v", drv.queries)
	}
}

func TestDropExpiredRemovesOldPartitions(t *testing.T) {
	parent := "events"
	// Build a list of children: one old, one current.
	old := time.Now().UTC().Add(-200 * 24 * time.Hour).Format("20060102")
	cur := time.Now().UTC().Format("20060102")
	drv := &tsDriver{
		listPartFn: func() []string {
			return []string{parent + "_" + old, parent + "_" + cur}
		},
	}
	db := pg.New(drv)
	ts := pg.NewTimeSeriesTable(parent, "ts").
		PartitionEvery(24 * time.Hour).
		Retain(30 * 24 * time.Hour)
	dropped, err := ts.DropExpired(db, context.Background())
	if err != nil {
		t.Fatalf("DropExpired: %v", err)
	}
	if dropped != 1 {
		t.Errorf("expected 1 dropped partition, got %d", dropped)
	}
	if len(drv.dropped) != 1 || !strings.HasSuffix(drv.dropped[0], old) {
		t.Errorf("wrong partition dropped: %v", drv.dropped)
	}
}

func TestDropExpiredSkipsUnknownNames(t *testing.T) {
	drv := &tsDriver{
		listPartFn: func() []string {
			// Names that don't match the format — should be skipped.
			return []string{"events_special_extra", "manually_made"}
		},
	}
	db := pg.New(drv)
	ts := pg.NewTimeSeriesTable("events", "ts").Retain(30 * 24 * time.Hour)
	dropped, err := ts.DropExpired(db, context.Background())
	if err != nil {
		t.Fatalf("DropExpired: %v", err)
	}
	if dropped != 0 {
		t.Errorf("non-matching names should be skipped, got dropped=%d", dropped)
	}
}

func TestMaintainCreatesNextAndDropsExpired(t *testing.T) {
	old := time.Now().UTC().Add(-200 * 24 * time.Hour).Format("20060102")
	drv := &tsDriver{
		listPartFn: func() []string {
			return []string{"events_" + old}
		},
	}
	db := pg.New(drv)
	ts := pg.NewTimeSeriesTable("events", "ts").
		PartitionEvery(24 * time.Hour).
		Retain(30 * 24 * time.Hour)
	if err := ts.Maintain(db, context.Background()); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	// Should have at least 2 creates (EnsureNext default 2) and 1 drop.
	creates := 0
	for _, q := range drv.queries {
		if strings.HasPrefix(q, "CREATE TABLE IF NOT EXISTS") {
			creates++
		}
	}
	if creates < 2 {
		t.Errorf("expected at least 2 partition creates, got %d", creates)
	}
	if len(drv.dropped) != 1 {
		t.Errorf("expected 1 drop, got %d", len(drv.dropped))
	}
}

func TestPartitionNamingByBucket(t *testing.T) {
	// Daily bucket — name is YYYYMMDD.
	drv := &tsDriver{}
	db := pg.New(drv)
	ts := pg.NewTimeSeriesTable("e", "ts").PartitionEvery(24 * time.Hour)
	_ = ts.Bootstrap(db, context.Background())
	if !strings.Contains(drv.queries[0], "_2") { // some YYYY...
		t.Errorf("daily partition naming: %s", drv.queries[0])
	}

	// Hourly bucket — name includes _HH.
	drv2 := &tsDriver{}
	db2 := pg.New(drv2)
	ts2 := pg.NewTimeSeriesTable("e", "ts").PartitionEvery(time.Hour)
	_ = ts2.Bootstrap(db2, context.Background())
	// Match: e_YYYYMMDD_HH
	saw := false
	for _, q := range drv2.queries {
		if strings.Contains(q, `"e_`) && strings.Count(q, "_") >= 2 {
			saw = true
		}
	}
	if !saw {
		t.Errorf("hourly partition naming should include _HH suffix")
	}
}
