package sqlite_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/sqlite"
)

// planRows returns EXPLAIN QUERY PLAN rows (id, parent, notused, detail).
type planRows struct {
	data [][]any
	pos  int
}

func (r *planRows) Next() bool {
	if r.pos >= len(r.data) {
		return false
	}
	r.pos++
	return true
}
func (r *planRows) Scan(dest ...any) error {
	row := r.data[r.pos-1]
	for i, d := range dest {
		switch p := d.(type) {
		case *int64:
			*p = row[i].(int64)
		case *string:
			*p = row[i].(string)
		}
	}
	return nil
}
func (r *planRows) Columns() ([]string, error) { return nil, nil }
func (r *planRows) Close() error               { return nil }
func (r *planRows) Err() error                 { return nil }

type planDriver struct {
	entDriver
	rows *planRows
}

func (d *planDriver) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	d.entDriver.queries = append(d.entDriver.queries, sql)
	return d.rows, nil
}

func TestExplainSeqScanAndIndex(t *testing.T) {
	drv := &planDriver{rows: &planRows{data: [][]any{
		{int64(2), int64(0), int64(0), "SCAN users"},
		{int64(3), int64(0), int64(0), "SEARCH orders USING INDEX ix_orders_user (userId=?)"},
	}}}
	db := sqlite.New(drv)
	plan, err := sqlite.Explain(db, context.Background(), "SELECT ...", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(drv.entDriver.queries[0], "EXPLAIN QUERY PLAN ") {
		t.Errorf("prefix: %s", drv.entDriver.queries[0])
	}
	seq := plan.SeqScans()
	if len(seq) != 1 || seq[0] != "users" {
		t.Errorf("seq scans: %v", seq)
	}
	idx := plan.UsedIndexes()
	if len(idx) != 1 || idx[0] != "ix_orders_user" {
		t.Errorf("used indexes: %v", idx)
	}
	if plan.Fingerprint() == "" {
		t.Error("empty fingerprint")
	}
}

func TestExplainCoveringIndexNotSeqScan(t *testing.T) {
	drv := &planDriver{rows: &planRows{data: [][]any{
		{int64(2), int64(0), int64(0), "SCAN users USING COVERING INDEX ix_users_email"},
	}}}
	db := sqlite.New(drv)
	plan, _ := sqlite.Explain(db, context.Background(), "SELECT ...")
	if len(plan.SeqScans()) != 0 {
		t.Errorf("covering index scan should not count as seq scan: %v", plan.SeqScans())
	}
	if got := plan.UsedIndexes(); len(got) != 1 || got[0] != "ix_users_email" {
		t.Errorf("used indexes: %v", got)
	}
}
