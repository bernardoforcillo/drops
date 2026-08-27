package integration_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/pg"
)

// The job queue's guarantees are all facts about the server: a partial
// unique index that two concurrent inserts race against, SKIP LOCKED
// deciding which of two workers gets the row, an interval comparison
// against now(). A rendered SQL string says nothing about any of them.

func jobFixture(t *testing.T) *pg.JobQueue {
	t.Helper()
	db := openPG(t)
	name := integration.UniqueName(t, "jobs")
	tbl := pg.NewJobTable(name)
	dropPG(t, db, tbl)
	for _, e := range pg.CreateTableWithIndexes(tbl) {
		execPG(t, db, e)
	}
	return pg.NewJobQueue(db, name)
}

// The whole lifecycle, against a server that has to accept every
// statement in it.
func TestPGJobLifecycle(t *testing.T) {
	q := jobFixture(t)
	ctx := context.Background()

	id, err := q.Enqueue(ctx, pg.NewJob{
		Kind: "reindex", Key: "orders", Total: 1000,
		Payload: map[string]any{"from": 1, "to": 1000},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	got, err := q.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != pg.JobPending || got.Kind != "reindex" || got.Key != "orders" {
		t.Fatalf("enqueued job = %+v", got)
	}
	if got.Total != 1000 || got.CreatedAt.IsZero() {
		t.Errorf("total or createdAt did not round-trip: %+v", got)
	}
	if !strings.Contains(string(got.Payload), `"from"`) {
		t.Errorf("payload did not round-trip: %s", got.Payload)
	}

	job, ok, err := q.Claim(ctx, "worker-1", "reindex")
	if err != nil || !ok {
		t.Fatalf("Claim: %v, ok=%v", err, ok)
	}
	if job.ID != id || job.State != pg.JobRunning || job.Worker != "worker-1" {
		t.Fatalf("claimed job = %+v", job)
	}
	if job.Attempts != 1 || job.StartedAt.IsZero() || job.HeartbeatAt.IsZero() {
		t.Errorf("claim did not stamp the job: %+v", job)
	}

	if err := q.Heartbeat(ctx, id, 500); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	got, _ = q.Get(ctx, id)
	if got.Progress != 500 {
		t.Errorf("progress = %d, want 500", got.Progress)
	}
	if f := got.Fraction(); f != 0.5 {
		t.Errorf("Fraction() = %v, want 0.5", f)
	}

	live, err := q.Live(ctx)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if len(live) != 1 || live[0].ID != id {
		t.Errorf("Live() = %+v, want the one running job", live)
	}

	if err := q.Complete(ctx, id); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got, _ = q.Get(ctx, id)
	if got.State != pg.JobDone || got.FinishedAt.IsZero() {
		t.Errorf("completed job = %+v", got)
	}
	if live, _ = q.Live(ctx); len(live) != 0 {
		t.Errorf("a done job is still live: %+v", live)
	}
}

// The partial unique index is the load-bearing part, and the only test
// that means anything is one where the database enforces it.
func TestPGJobOneLivePerKey(t *testing.T) {
	q := jobFixture(t)
	ctx := context.Background()

	if _, err := q.Enqueue(ctx, pg.NewJob{Kind: "reindex", Key: "orders"}); err != nil {
		t.Fatal(err)
	}
	_, err := q.Enqueue(ctx, pg.NewJob{Kind: "reindex", Key: "orders"})
	if !errors.Is(err, pg.ErrJobAlreadyQueued) {
		t.Fatalf("second enqueue = %v, want ErrJobAlreadyQueued", err)
	}

	// A different key, and a different kind, are unaffected.
	if _, err := q.Enqueue(ctx, pg.NewJob{Kind: "reindex", Key: "customers"}); err != nil {
		t.Errorf("a different key was refused: %v", err)
	}
	if _, err := q.Enqueue(ctx, pg.NewJob{Kind: "backup", Key: "orders"}); err != nil {
		t.Errorf("a different kind was refused: %v", err)
	}

	// Claiming does not free the slot — a running job is still live.
	if _, _, err := q.Claim(ctx, "w", "reindex"); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Enqueue(ctx, pg.NewJob{Kind: "reindex", Key: "orders"}); !errors.Is(err, pg.ErrJobAlreadyQueued) {
		t.Errorf("enqueue against a running job = %v, want ErrJobAlreadyQueued", err)
	}
}

// Finishing frees the key, or a job could never be run twice.
func TestPGJobFinishingFreesTheKey(t *testing.T) {
	q := jobFixture(t)
	ctx := context.Background()

	for _, finish := range []func(int64) error{
		func(id int64) error { return q.Complete(ctx, id) },
		func(id int64) error { return q.Fail(ctx, id, errors.New("bad row")) },
		func(id int64) error { return q.Cancel(ctx, id) },
	} {
		id, err := q.Enqueue(ctx, pg.NewJob{Kind: "reindex", Key: "orders"})
		if err != nil {
			t.Fatalf("enqueue after the previous job finished: %v", err)
		}
		if _, _, err := q.Claim(ctx, "w", "reindex"); err != nil {
			t.Fatal(err)
		}
		if err := finish(id); err != nil {
			t.Fatalf("finishing: %v", err)
		}
	}
}

// Many workers, many jobs, one server. SKIP LOCKED is the whole
// mechanism and it cannot be checked any other way.
func TestPGJobClaimIsExclusive(t *testing.T) {
	q := jobFixture(t)
	ctx := context.Background()

	const n = 8
	for i := 0; i < n; i++ {
		if _, err := q.Enqueue(ctx, pg.NewJob{
			Kind: "reindex", Key: fmt.Sprintf("shard-%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	claimed := map[int64]string{}
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			worker := fmt.Sprintf("worker-%d", w)
			for {
				job, ok, err := q.Claim(ctx, worker, "reindex")
				if err != nil || !ok {
					return
				}
				mu.Lock()
				if prev, dup := claimed[job.ID]; dup {
					t.Errorf("job %d claimed twice: by %s and %s", job.ID, prev, worker)
				}
				claimed[job.ID] = worker
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()

	if len(claimed) != n {
		t.Errorf("%d of %d jobs were claimed", len(claimed), n)
	}
}

// A worker that dies holds nothing; only Reap recovers its job.
func TestPGJobReapReturnsAbandonedWork(t *testing.T) {
	q := jobFixture(t)
	ctx := context.Background()

	id, err := q.Enqueue(ctx, pg.NewJob{Kind: "reindex", Key: "orders"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.Claim(ctx, "doomed-worker", "reindex"); err != nil {
		t.Fatal(err)
	}

	// Nothing is stale yet, and reaping a healthy job would be worse
	// than not reaping at all.
	n, err := q.Reap(ctx, time.Hour)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if n != 0 {
		t.Fatalf("reaped %d healthy jobs", n)
	}

	// A zero threshold is refused, so "reap everything" cannot be
	// written by accident.
	if _, err := q.Reap(ctx, 0); err == nil {
		t.Error("Reap(0) was accepted")
	}

	time.Sleep(1100 * time.Millisecond)
	n, err = q.Reap(ctx, time.Second)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if n != 1 {
		t.Fatalf("reaped %d, want 1", n)
	}

	got, _ := q.Get(ctx, id)
	if got.State != pg.JobPending || got.Worker != "" {
		t.Errorf("reaped job = %+v, want pending and unowned", got)
	}
	if !strings.Contains(got.LastError, "reaped") {
		t.Errorf("the reaped job does not say why: %q", got.LastError)
	}

	// And somebody else can now take it.
	job, ok, err := q.Claim(ctx, "worker-2", "reindex")
	if err != nil || !ok || job.ID != id {
		t.Fatalf("re-claim after reap: %v ok=%v", err, ok)
	}
	if job.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", job.Attempts)
	}
}

// Cancel marks a column; the worker finds out at its next heartbeat.
// Nothing is killed from outside.
func TestPGJobCancelReachesTheWorkerThroughHeartbeat(t *testing.T) {
	q := jobFixture(t)
	ctx := context.Background()

	id, err := q.Enqueue(ctx, pg.NewJob{Kind: "reindex", Key: "orders"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.Claim(ctx, "w", "reindex"); err != nil {
		t.Fatal(err)
	}
	if err := q.Heartbeat(ctx, id, 10); err != nil {
		t.Fatalf("heartbeat before cancel: %v", err)
	}

	if err := q.Cancel(ctx, id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if err := q.Heartbeat(ctx, id, 20); !errors.Is(err, pg.ErrJobNotRunning) {
		t.Errorf("heartbeat after cancel = %v, want ErrJobNotRunning", err)
	}
	// So do the other transitions, which is how a worker racing the
	// cancel cannot mark a cancelled job done.
	if err := q.Complete(ctx, id); !errors.Is(err, pg.ErrJobNotRunning) {
		t.Errorf("Complete after cancel = %v, want ErrJobNotRunning", err)
	}
}

// A pending job can be cancelled too, or the button does nothing for
// the first few seconds after enqueueing.
func TestPGJobCancelReachesPendingJobs(t *testing.T) {
	q := jobFixture(t)
	ctx := context.Background()

	id, err := q.Enqueue(ctx, pg.NewJob{Kind: "reindex", Key: "orders"})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Cancel(ctx, id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if _, ok, err := q.Claim(ctx, "w", "reindex"); err != nil || ok {
		t.Errorf("a cancelled job was claimable: ok=%v err=%v", ok, err)
	}
}

// Cleanup must never take a job that is still live, however old.
func TestPGJobCleanupSparesLiveJobs(t *testing.T) {
	q := jobFixture(t)
	ctx := context.Background()

	doneID, err := q.Enqueue(ctx, pg.NewJob{Kind: "reindex", Key: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.Claim(ctx, "w", "reindex"); err != nil {
		t.Fatal(err)
	}
	if err := q.Complete(ctx, doneID); err != nil {
		t.Fatal(err)
	}
	liveID, err := q.Enqueue(ctx, pg.NewJob{Kind: "reindex", Key: "b"})
	if err != nil {
		t.Fatal(err)
	}

	// Retain nothing: everything finished is old enough.
	n, err := q.Cleanup(ctx, 0)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if n != 1 {
		t.Errorf("cleaned up %d rows, want 1", n)
	}
	if _, err := q.Get(ctx, doneID); err == nil {
		t.Error("the finished job survived cleanup")
	}
	if _, err := q.Get(ctx, liveID); err != nil {
		t.Errorf("cleanup took a pending job: %v", err)
	}
}

// Claim with no kinds takes anything, which is what a general worker
// pool wants.
func TestPGJobClaimAnyKind(t *testing.T) {
	q := jobFixture(t)
	ctx := context.Background()

	if _, err := q.Enqueue(ctx, pg.NewJob{Kind: "backup", Key: "a"}); err != nil {
		t.Fatal(err)
	}
	job, ok, err := q.Claim(ctx, "w")
	if err != nil || !ok {
		t.Fatalf("Claim(any): %v ok=%v", err, ok)
	}
	if job.Kind != "backup" {
		t.Errorf("claimed %q", job.Kind)
	}

	// And a kind filter that matches nothing claims nothing rather
	// than taking the wrong job.
	if _, ok, err := q.Claim(ctx, "w", "reindex"); err != nil || ok {
		t.Errorf("Claim(reindex) took a backup job: ok=%v err=%v", ok, err)
	}
}
