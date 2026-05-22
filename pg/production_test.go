package pg_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// --- Sentinel errors -------------------------------------------------

func TestSentinelErrors(t *testing.T) {
	db := pg.New(&fakeDriver{})
	ctx := context.Background()

	// INSERT with no rows.
	_, err := db.Insert(users).Exec(ctx)
	if !errors.Is(err, pg.ErrNoRowsToInsert) {
		t.Errorf("INSERT empty: want ErrNoRowsToInsert, got %v", err)
	}

	// UPDATE with no Set.
	_, err = db.Update(users).Where(userID.Eq(1)).Exec(ctx)
	if !errors.Is(err, pg.ErrNoUpdateAssignments) {
		t.Errorf("UPDATE empty: want ErrNoUpdateAssignments, got %v", err)
	}

	// All/One without RETURNING.
	if err := db.Insert(users).Row(userName.Val("a")).All(ctx, &[]struct{}{}); !errors.Is(err, pg.ErrReturningRequired) {
		t.Errorf("INSERT.All: want ErrReturningRequired, got %v", err)
	}
	if err := db.Update(users).Set(userAge.Val(1)).Where(userID.Eq(1)).All(ctx, &[]struct{}{}); !errors.Is(err, pg.ErrReturningRequired) {
		t.Errorf("UPDATE.All: want ErrReturningRequired, got %v", err)
	}
	if err := db.Delete(users).Where(userID.Eq(1)).All(ctx, &[]struct{}{}); !errors.Is(err, pg.ErrReturningRequired) {
		t.Errorf("DELETE.All: want ErrReturningRequired, got %v", err)
	}

	// Push needs a schema.
	if _, err := pg.Push(ctx, db, nil); !errors.Is(err, pg.ErrSchemaRequired) {
		t.Errorf("Push(nil): want ErrSchemaRequired, got %v", err)
	}
}

// --- Hook + QueryEvent ----------------------------------------------

type recordedEvent struct {
	kind string
	sql  string
	err  error
}

func TestHookFiresOnExecQueryBeginCommitRollback(t *testing.T) {
	var (
		mu     sync.Mutex
		events []recordedEvent
	)
	hook := func(_ context.Context, e drops.QueryEvent) {
		mu.Lock()
		events = append(events, recordedEvent{e.Kind, strings.Join(strings.Fields(e.SQL), " "), e.Err})
		mu.Unlock()
	}

	db := pg.New(&fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{cols: []string{"id"}, data: [][]any{{int64(1)}}}, nil
	}}).WithHook(hook)
	ctx := context.Background()

	// Exec via builder routes through DB.Query (UPDATE has RETURNING)/DB.Exec.
	if _, err := db.Update(users).Set(userAge.Val(30)).Where(userID.Eq(1)).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	// Plain raw Query.
	if _, err := db.Query(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}

	// Transaction lifecycle.
	if err := db.InTx(ctx, func(tx *pg.DB) error {
		_, err := tx.Exec(ctx, "INSERT INTO foo VALUES (1)")
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Rolled-back transaction.
	want := errors.New("boom")
	if err := db.InTx(ctx, func(tx *pg.DB) error { return want }); !errors.Is(err, want) {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	kinds := make([]string, 0, len(events))
	for _, e := range events {
		kinds = append(kinds, e.kind)
	}
	wantKinds := []string{"exec", "query", "begin", "exec", "commit", "begin", "rollback"}
	if fmt.Sprint(kinds) != fmt.Sprint(wantKinds) {
		t.Errorf("kinds mismatch\n  got:  %v\n  want: %v", kinds, wantKinds)
	}
}

func TestPingEmitsPingEvent(t *testing.T) {
	var captured drops.QueryEvent
	db := pg.New(&fakeDriver{}).WithHook(func(_ context.Context, e drops.QueryEvent) {
		captured = e
	})
	if err := db.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if captured.Kind != "ping" {
		t.Errorf("kind = %q, want %q", captured.Kind, "ping")
	}
}

func TestWithHookReturnsCopy(t *testing.T) {
	base := pg.New(&fakeDriver{})
	if base.Hook() != nil {
		t.Fatal("base hook should be nil")
	}
	hooked := base.WithHook(func(context.Context, drops.QueryEvent) {})
	if base.Hook() != nil {
		t.Error("WithHook mutated original DB")
	}
	if hooked.Hook() == nil {
		t.Error("WithHook didn't install hook on copy")
	}
}

func TestChainHooksInvokesAllInOrder(t *testing.T) {
	var seen []string
	h := drops.ChainHooks(
		func(_ context.Context, e drops.QueryEvent) { seen = append(seen, "a:"+e.Kind) },
		nil,
		func(_ context.Context, e drops.QueryEvent) { seen = append(seen, "b:"+e.Kind) },
	)
	h(context.Background(), drops.QueryEvent{Kind: "x"})
	if fmt.Sprint(seen) != "[a:x b:x]" {
		t.Errorf("seen=%v", seen)
	}
}

func TestChainHooksDegenerate(t *testing.T) {
	if drops.ChainHooks() != nil {
		t.Error("ChainHooks() should return nil")
	}
	single := func(context.Context, drops.QueryEvent) {}
	got := drops.ChainHooks(single)
	if got == nil {
		t.Error("ChainHooks(one) should return the single hook")
	}
}

// --- Identifier validation ------------------------------------------

func TestNewTablePanicsOnEmpty(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic")
		}
		err, ok := r.(error)
		if !ok || !errors.Is(err, pg.ErrInvalidIdentifier) {
			t.Errorf("expected ErrInvalidIdentifier panic, got %v", r)
		}
	}()
	pg.NewTable("")
}

func TestColumnPanicsOnNULByte(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic")
		}
		err, ok := r.(error)
		if !ok || !errors.Is(err, pg.ErrInvalidIdentifier) {
			t.Errorf("expected ErrInvalidIdentifier panic, got %v", r)
		}
	}()
	pg.Text("bad\x00name")
}

func TestNewSchemaTableValidatesBoth(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic")
		}
	}()
	pg.NewSchemaTable("public", "")
}

// --- LoggerHook -----------------------------------------------------

func TestLoggerHookRespectsSlowQueryAndArgs(t *testing.T) {
	var lines []string
	log := func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	hook := pg.LoggerHook(log, pg.LoggerOptions{
		SlowQuery: 100 * time.Millisecond,
		LogArgs:   true,
	})
	ctx := context.Background()

	// Fast query, no error → skipped.
	hook(ctx, drops.QueryEvent{Kind: "query", SQL: "SELECT 1", Duration: 5 * time.Millisecond})
	if len(lines) != 0 {
		t.Fatalf("fast query was logged: %v", lines)
	}

	// Slow query → logged with args.
	hook(ctx, drops.QueryEvent{
		Kind: "query", SQL: "SELECT * FROM t WHERE id = $1",
		Args: []any{42}, Duration: 200 * time.Millisecond,
	})
	if len(lines) != 1 || !strings.Contains(lines[0], "args=[42]") {
		t.Errorf("expected slow query logged with args, got %v", lines)
	}

	// Error always logs, regardless of duration.
	hook(ctx, drops.QueryEvent{Kind: "exec", Err: errors.New("nope"), Duration: 1 * time.Millisecond})
	if len(lines) != 2 || !strings.Contains(lines[1], "err=nope") {
		t.Errorf("expected error logged, got %v", lines)
	}
}

func TestLoggerHookTruncatesSQL(t *testing.T) {
	var line string
	hook := pg.LoggerHook(
		func(f string, a ...any) { line = fmt.Sprintf(f, a...) },
		pg.LoggerOptions{MaxSQLLength: 10},
	)
	hook(context.Background(), drops.QueryEvent{
		Kind: "exec", SQL: "SELECT ABCDEFGHIJKLMNOP",
		Duration: time.Millisecond,
	})
	if !strings.Contains(line, `sql="SELECT ABC…"`) {
		t.Errorf("expected truncated SQL, got %q", line)
	}
}
