package clickhouse_test

import (
	"context"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/clickhouse"
)

// The materialised-view scheduler is the only long-running loop in the
// ClickHouse package. It blocks, so it is always running on somebody
// else's goroutine, and it is handed no stop function — the context is
// the entire contract, and it has to return the reason it stopped so a
// supervisor can tell a shutdown from a fault.

type auditDriver struct{}

func (auditDriver) Exec(context.Context, string, ...any) (drops.Result, error) {
	return auditResult{}, nil
}
func (auditDriver) Query(context.Context, string, ...any) (drops.Rows, error) {
	return auditRows{}, nil
}
func (auditDriver) Begin(context.Context) (drops.Tx, error) { return nil, nil }

type auditResult struct{}

func (auditResult) RowsAffected() (int64, error) { return 0, nil }

type auditRows struct{}

func (auditRows) Next() bool                 { return false }
func (auditRows) Scan(...any) error          { return nil }
func (auditRows) Columns() ([]string, error) { return nil, nil }
func (auditRows) Close() error               { return nil }
func (auditRows) Err() error                 { return nil }

func TestMatViewManagerStartReturnsOnCancellation(t *testing.T) {
	m := clickhouse.NewMatViewManager(clickhouse.New(auditDriver{})).WithPollInterval(time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- m.Start(ctx) }()
	cancel()

	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Errorf("Start returned %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not return after its context was cancelled")
	}
}
