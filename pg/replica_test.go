package pg_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// taggedDriver records its name on every call so tests can assert
// which underlying driver served a particular request.
type taggedDriver struct {
	name   string
	execs  atomic.Int32
	reads  atomic.Int32
	begins atomic.Int32
}

func (d *taggedDriver) Exec(_ context.Context, _ string, _ ...any) (drops.Result, error) {
	d.execs.Add(1)
	return nil, nil
}
func (d *taggedDriver) Query(_ context.Context, _ string, _ ...any) (drops.Rows, error) {
	d.reads.Add(1)
	return &fakeRows{}, nil
}
func (d *taggedDriver) Begin(_ context.Context) (drops.Tx, error) {
	d.begins.Add(1)
	return nil, nil
}

func TestReplicatedRoutesExecToPrimary(t *testing.T) {
	primary := &taggedDriver{name: "primary"}
	r1 := &taggedDriver{name: "r1"}
	repl := pg.NewReplicated(primary, r1)
	db := pg.New(repl)

	for i := 0; i < 5; i++ {
		_, _ = db.Exec(context.Background(), "INSERT ...")
	}
	if primary.execs.Load() != 5 {
		t.Errorf("Exec must hit primary, got %d", primary.execs.Load())
	}
	if r1.execs.Load() != 0 {
		t.Errorf("Exec must NOT hit replica, got %d", r1.execs.Load())
	}
}

func TestReplicatedRoundRobinsQueriesAcrossReplicas(t *testing.T) {
	primary := &taggedDriver{name: "primary"}
	r1 := &taggedDriver{name: "r1"}
	r2 := &taggedDriver{name: "r2"}
	repl := pg.NewReplicated(primary, r1, r2)
	db := pg.New(repl)

	// 10 reads across 2 replicas → 5 each.
	for i := 0; i < 10; i++ {
		_, _ = db.Query(context.Background(), "SELECT ...")
	}
	if r1.reads.Load() != 5 || r2.reads.Load() != 5 {
		t.Errorf("expected 5+5 reads across replicas, got %d + %d",
			r1.reads.Load(), r2.reads.Load())
	}
	if primary.reads.Load() != 0 {
		t.Errorf("primary should serve no reads without RYW, got %d", primary.reads.Load())
	}
}

func TestReplicatedFallsBackToPrimaryWithoutReplicas(t *testing.T) {
	primary := &taggedDriver{name: "primary"}
	repl := pg.NewReplicated(primary)
	db := pg.New(repl)
	_, _ = db.Query(context.Background(), "SELECT ...")
	if primary.reads.Load() != 1 {
		t.Errorf("primary should serve the read when no replicas, got %d", primary.reads.Load())
	}
}

func TestReadYourWritesStickyAfterExec(t *testing.T) {
	primary := &taggedDriver{name: "primary"}
	r1 := &taggedDriver{name: "r1"}
	repl := pg.NewReplicated(primary, r1)
	db := pg.New(repl)

	ctx := pg.WithReadYourWrites(context.Background(), 200*time.Millisecond)

	// Pre-write read: no active window yet → replica.
	_, _ = db.Query(ctx, "SELECT pre-write")
	if r1.reads.Load() != 1 {
		t.Errorf("pre-write read should hit replica, got %d on primary, %d on replica",
			primary.reads.Load(), r1.reads.Load())
	}

	// Write: arms the window.
	_, _ = db.Exec(ctx, "UPDATE ...")

	// Subsequent reads within the window → primary.
	for i := 0; i < 3; i++ {
		_, _ = db.Query(ctx, "SELECT post-write")
	}
	if primary.reads.Load() != 3 {
		t.Errorf("post-write reads should hit primary, got %d", primary.reads.Load())
	}

	// After the window expires, reads fall back to replica.
	time.Sleep(220 * time.Millisecond)
	_, _ = db.Query(ctx, "SELECT after-window")
	// Expect at least one more replica read.
	if r1.reads.Load() < 2 {
		t.Errorf("post-window read should return to replica, got %d", r1.reads.Load())
	}
}

func TestReadYourWritesDoesNotApplyWithoutCtx(t *testing.T) {
	primary := &taggedDriver{name: "primary"}
	r1 := &taggedDriver{name: "r1"}
	repl := pg.NewReplicated(primary, r1)
	db := pg.New(repl)

	// No WithReadYourWrites: Exec then Query → write on primary,
	// read on replica.
	_, _ = db.Exec(context.Background(), "UPDATE ...")
	_, _ = db.Query(context.Background(), "SELECT ...")
	if r1.reads.Load() != 1 {
		t.Errorf("untagged ctx should hit replica, got %d", r1.reads.Load())
	}
}

func TestReplicatedPanicsWithoutPrimary(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("NewReplicated(nil, ...) should panic")
		}
	}()
	_ = pg.NewReplicated(nil)
}
