package pg_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// jobDriver records statements and answers them from a script, which
// is enough to pin the SQL a queue issues and the way it reads the
// answers. The behaviour that needs a real server — SKIP LOCKED, the
// partial unique index — is asserted on the statement text and the
// DDL instead.
type jobDriver struct {
	stmts    []string
	rows     drops.Rows
	execErr  error
	affected int64
}

func (d *jobDriver) Exec(_ context.Context, sql string, _ ...any) (drops.Result, error) {
	d.stmts = append(d.stmts, sql)
	if d.execErr != nil {
		return nil, d.execErr
	}
	return affectedResult(d.affected), nil
}

func (d *jobDriver) Query(_ context.Context, sql string, _ ...any) (drops.Rows, error) {
	d.stmts = append(d.stmts, sql)
	if d.rows != nil {
		r := d.rows
		d.rows = nil
		return r, nil
	}
	return &stubRows{}, nil
}

func (d *jobDriver) Begin(context.Context) (drops.Tx, error) { return nil, nil }

func (d *jobDriver) last() string {
	if len(d.stmts) == 0 {
		return ""
	}
	return d.stmts[len(d.stmts)-1]
}

type affectedResult int64

func (r affectedResult) RowsAffected() (int64, error) { return int64(r), nil }

// The partial unique index is what makes "start the reindex" safe to
// press twice, so it is worth pinning that it is declared.
func TestJobTableEnforcesOneLiveJobPerKey(t *testing.T) {
	var stmt string
	for _, e := range pg.CreateTableWithIndexes(pg.NewJobTable("jobs")) {
		sql, _ := drops.String(e)
		if strings.Contains(sql, "LiveIdx") {
			stmt = sql
		}
	}
	if stmt == "" {
		t.Fatal("no LiveIdx on the job table")
	}
	if !strings.Contains(stmt, "CREATE UNIQUE INDEX") {
		t.Errorf("the live-job index is not unique; two callers could queue the same job: %s", stmt)
	}
	if !strings.Contains(stmt, "WHERE") || !strings.Contains(stmt, "running") {
		t.Errorf("the live-job index is not partial, so finished rows would block new runs: %s", stmt)
	}
	for _, col := range []string{"kind", "key"} {
		if !strings.Contains(stmt, col) {
			t.Errorf("the live-job index does not cover %q: %s", col, stmt)
		}
	}
}

// A duplicate must come back as ErrJobAlreadyQueued, which callers
// treat as success: the work they asked for is going to happen.
func TestEnqueueTranslatesTheUniqueViolation(t *testing.T) {
	drv := &errDriver{err: &sqlStateOnlyError{code: "23505", msg: "duplicate key value"}}
	q := pg.NewJobQueue(pg.New(drv), "jobs")
	_, err := q.Enqueue(context.Background(), pg.NewJob{Kind: "reindex", Key: "orders"})
	if !errors.Is(err, pg.ErrJobAlreadyQueued) {
		t.Errorf("got %v, want ErrJobAlreadyQueued", err)
	}
}

func TestEnqueueNeedsAKind(t *testing.T) {
	q := pg.NewJobQueue(pg.New(&jobDriver{}), "jobs")
	if _, err := q.Enqueue(context.Background(), pg.NewJob{Key: "orders"}); err == nil {
		t.Error("expected an error for a job with no kind")
	}
}

// Two workers racing must not both take one job, and the loser must
// not wait for the winner.
func TestClaimIsOneStatementWithSkipLocked(t *testing.T) {
	drv := &jobDriver{}
	q := pg.NewJobQueue(pg.New(drv), "jobs")
	if _, _, err := q.Claim(context.Background(), "worker-1", "reindex"); err != nil {
		t.Fatal(err)
	}
	sql := drv.last()
	if !strings.Contains(sql, "FOR UPDATE SKIP LOCKED") {
		t.Errorf("claim does not use SKIP LOCKED:\n%s", sql)
	}
	if !strings.Contains(sql, "UPDATE") || !strings.Contains(sql, "RETURNING") {
		t.Errorf("claim is not a single updating statement:\n%s", sql)
	}
	if strings.Count(sql, "UPDATE") != 2 { // the UPDATE and FOR UPDATE
		t.Errorf("claim issues more than one update:\n%s", sql)
	}
	if !strings.Contains(sql, "kind = ANY") {
		t.Errorf("claim does not filter by kind:\n%s", sql)
	}
}

func TestClaimWithoutKindsTakesAny(t *testing.T) {
	drv := &jobDriver{}
	q := pg.NewJobQueue(pg.New(drv), "jobs")
	if _, _, err := q.Claim(context.Background(), "worker-1"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(drv.last(), "kind = ANY") {
		t.Errorf("claim with no kinds filtered by kind anyway:\n%s", drv.last())
	}
}

func TestClaimNeedsAWorkerName(t *testing.T) {
	q := pg.NewJobQueue(pg.New(&jobDriver{}), "jobs")
	if _, _, err := q.Claim(context.Background(), ""); err == nil {
		t.Error("expected an error for an empty worker name")
	}
}

func TestClaimReportsNoWork(t *testing.T) {
	q := pg.NewJobQueue(pg.New(&jobDriver{}), "jobs")
	_, ok, err := q.Claim(context.Background(), "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("claimed a job from an empty queue")
	}
}

// A heartbeat is how a worker finds out it was canceled or reaped,
// so "no row updated" has to be a distinguishable error.
func TestHeartbeatReportsNotRunning(t *testing.T) {
	drv := &jobDriver{affected: 0}
	q := pg.NewJobQueue(pg.New(drv), "jobs")
	err := q.Heartbeat(context.Background(), 42)
	if !errors.Is(err, pg.ErrJobNotRunning) {
		t.Errorf("got %v, want ErrJobNotRunning", err)
	}
	if !strings.Contains(drv.last(), "state = 'running'") {
		t.Errorf("heartbeat does not require the job to still be running:\n%s", drv.last())
	}
}

func TestHeartbeatRecordsProgress(t *testing.T) {
	drv := &jobDriver{affected: 1}
	q := pg.NewJobQueue(pg.New(drv), "jobs")
	if err := q.Heartbeat(context.Background(), 42, 1000); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(drv.last(), "progress = $2") {
		t.Errorf("progress was not recorded:\n%s", drv.last())
	}
	// A heartbeat with no progress must not reset it to zero.
	if err := q.Heartbeat(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(drv.last(), "progress =") {
		t.Errorf("a bare heartbeat touched progress:\n%s", drv.last())
	}
}

func TestCompleteFailCancelGuardTheState(t *testing.T) {
	for name, run := range map[string]func(*pg.JobQueue) error{
		"complete": func(q *pg.JobQueue) error { return q.Complete(context.Background(), 1) },
		"fail": func(q *pg.JobQueue) error {
			return q.Fail(context.Background(), 1, errors.New("bad row"))
		},
		"cancel": func(q *pg.JobQueue) error { return q.Cancel(context.Background(), 1) },
	} {
		drv := &jobDriver{affected: 0}
		q := pg.NewJobQueue(pg.New(drv), "jobs")
		if err := run(q); !errors.Is(err, pg.ErrJobNotRunning) {
			t.Errorf("%s: got %v, want ErrJobNotRunning", name, err)
		}
		if !strings.Contains(drv.last(), "state") {
			t.Errorf("%s does not guard on state:\n%s", name, drv.last())
		}
	}
}

// Cancel has to reach a job that has not been claimed yet, or the
// button does nothing for the first few seconds after enqueueing.
func TestCancelReachesPendingJobs(t *testing.T) {
	drv := &jobDriver{affected: 1}
	q := pg.NewJobQueue(pg.New(drv), "jobs")
	if err := q.Cancel(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(drv.last(), "'pending'") {
		t.Errorf("cancel does not reach pending jobs:\n%s", drv.last())
	}
}

func TestReapRequiresAThreshold(t *testing.T) {
	q := pg.NewJobQueue(pg.New(&jobDriver{}), "jobs")
	if _, err := q.Reap(context.Background(), 0); err == nil {
		t.Error("expected an error for a zero staleness threshold")
	}
	if _, err := q.Reap(context.Background(), -time.Second); err == nil {
		t.Error("expected an error for a negative staleness threshold")
	}
}

func TestReapReturnsJobsToPending(t *testing.T) {
	drv := &jobDriver{affected: 3}
	q := pg.NewJobQueue(pg.New(drv), "jobs")
	n, err := q.Reap(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("Reap returned %d, want 3", n)
	}
	sql := drv.last()
	if !strings.Contains(sql, "'pending'") || !strings.Contains(sql, "heartbeatAt <") {
		t.Errorf("reap does not return stale running jobs to pending:\n%s", sql)
	}
}

// A long-running job must never be deleted as history.
func TestCleanupSpearsOnlyFinishedJobs(t *testing.T) {
	drv := &jobDriver{affected: 5}
	q := pg.NewJobQueue(pg.New(drv), "jobs")
	if _, err := q.Cleanup(context.Background(), 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(drv.last(), "NOT IN ('pending','running')") {
		t.Errorf("cleanup could delete a live job:\n%s", drv.last())
	}
	if _, err := q.Cleanup(context.Background(), -time.Hour); err == nil {
		t.Error("expected an error for a negative retention")
	}
}

func TestJobFraction(t *testing.T) {
	cases := []struct {
		job  pg.Job
		want float64
	}{
		{pg.Job{Progress: 50, Total: 100}, 0.5},
		{pg.Job{Progress: 0, Total: 100}, 0},
		{pg.Job{Progress: 100, Total: 100}, 1},
		// Progress past the estimate is still finished, not 140%.
		{pg.Job{Progress: 140, Total: 100}, 1},
		// An unknown total reports nothing rather than dividing by zero.
		{pg.Job{Progress: 50}, 0},
	}
	for _, tc := range cases {
		if got := tc.job.Fraction(); got != tc.want {
			t.Errorf("%+v: Fraction() = %v, want %v", tc.job, got, tc.want)
		}
	}
}

func TestJobStateLive(t *testing.T) {
	for _, s := range []pg.JobState{pg.JobPending, pg.JobRunning} {
		if !s.Live() {
			t.Errorf("%s should occupy the per-key slot", s)
		}
	}
	for _, s := range []pg.JobState{pg.JobDone, pg.JobFailed, pg.JobCanceled} {
		if s.Live() {
			t.Errorf("%s should free the per-key slot", s)
		}
	}
}
