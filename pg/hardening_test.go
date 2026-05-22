package pg_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// --- Context-safe rollback ------------------------------------------

// rollbackRecorder is a drops.Tx that records what ctx Rollback was
// invoked with, so we can assert it was detached from the (cancelled)
// caller context. The ctx state is captured at call time so the
// assertion isn't fooled by a later defer cancel() running.
type rollbackRecorder struct {
	rolledBack          bool
	rollbackHadDeadline bool
	rollbackErrAtCall   error
	rollbackErr         error
	committed           bool
}

func (r *rollbackRecorder) Exec(_ context.Context, _ string, _ ...any) (drops.Result, error) {
	return fakeResult{}, nil
}
func (r *rollbackRecorder) Query(_ context.Context, _ string, _ ...any) (drops.Rows, error) {
	return &fakeRows{}, nil
}
func (r *rollbackRecorder) Begin(_ context.Context) (drops.Tx, error) {
	return nil, errors.New("nested tx unsupported")
}
func (r *rollbackRecorder) Commit(_ context.Context) error { r.committed = true; return nil }
func (r *rollbackRecorder) Rollback(ctx context.Context) error {
	r.rolledBack = true
	r.rollbackErrAtCall = ctx.Err()
	_, r.rollbackHadDeadline = ctx.Deadline()
	return r.rollbackErr
}

type beginningDriver struct{ tx *rollbackRecorder }

func (d *beginningDriver) Exec(context.Context, string, ...any) (drops.Result, error) {
	return fakeResult{}, nil
}
func (d *beginningDriver) Query(context.Context, string, ...any) (drops.Rows, error) {
	return &fakeRows{}, nil
}
func (d *beginningDriver) Begin(context.Context) (drops.Tx, error) {
	d.tx = &rollbackRecorder{}
	return d.tx, nil
}

func TestInTxRollbackUsesDetachedContextWhenCallerCancelled(t *testing.T) {
	drv := &beginningDriver{}
	db := pg.New(drv)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before InTx runs

	want := errors.New("fn failed")
	if err := db.InTx(ctx, func(*pg.DB) error { return want }); !errors.Is(err, want) {
		t.Fatalf("InTx err = %v, want %v", err, want)
	}
	if !drv.tx.rolledBack {
		t.Fatal("rollback was not invoked")
	}
	if drv.tx.rollbackErrAtCall != nil {
		t.Errorf("rollback ctx was already cancelled when Rollback ran: %v",
			drv.tx.rollbackErrAtCall)
	}
	if !drv.tx.rollbackHadDeadline {
		t.Error("rollback ctx had no deadline (cleanup could hang forever)")
	}
}

func TestDropsInTxRollbackUsesDetachedContextWhenCallerCancelled(t *testing.T) {
	drv := &beginningDriver{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	want := errors.New("fn failed")
	if err := drops.InTx(ctx, drv, func(drops.Tx) error { return want }); !errors.Is(err, want) {
		t.Fatalf("drops.InTx err = %v, want %v", err, want)
	}
	if !drv.tx.rolledBack {
		t.Fatal("rollback was not invoked")
	}
	if drv.tx.rollbackErrAtCall != nil {
		t.Errorf("rollback ctx was already cancelled when Rollback ran: %v",
			drv.tx.rollbackErrAtCall)
	}
	if !drv.tx.rollbackHadDeadline {
		t.Error("rollback ctx had no deadline (cleanup could hang forever)")
	}
}

// --- DB.Close --------------------------------------------------------

type closableDriver struct {
	fakeDriver
	closed bool
}

func (c *closableDriver) Close() error { c.closed = true; return nil }

func TestDBCloseDelegatesToDriver(t *testing.T) {
	c := &closableDriver{}
	db := pg.New(c)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if !c.closed {
		t.Error("driver Close was not invoked")
	}
}

func TestDBCloseIsNoopWhenDriverHasNoCloser(t *testing.T) {
	db := pg.New(&fakeDriver{})
	if err := db.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// --- SelectBuilder.Count --------------------------------------------

func TestSelectCountWrapsOriginalAndReturnsScalar(t *testing.T) {
	var capturedSQL string
	fd := &fakeDriver{handler: func(q string, _ []any) (drops.Rows, error) {
		capturedSQL = q
		return &fakeRows{cols: []string{"count"}, data: [][]any{{int64(42)}}}, nil
	}}
	db := pg.New(fd)
	n, err := db.Select(userID).From(users).Where(userAge.Gte(18)).Count(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 42 {
		t.Errorf("count = %d, want 42", n)
	}
	if !strings.HasPrefix(capturedSQL, "SELECT count(*) FROM (") ||
		!strings.HasSuffix(capturedSQL, ") AS _drops_count") {
		t.Errorf("unexpected SQL shape: %s", capturedSQL)
	}
}

func TestSelectCountOnEmptyResultReturnsZero(t *testing.T) {
	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{cols: []string{"count"}}, nil
	}}
	n, err := pg.New(fd).Select().From(users).Count(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("got %d, want 0", n)
	}
}

// --- LoggerHook redaction -------------------------------------------

func TestLoggerHookRedactsArgs(t *testing.T) {
	var line string
	hook := pg.LoggerHook(
		func(f string, a ...any) { line = fmt.Sprintf(f, a...) },
		pg.LoggerOptions{
			LogArgs: true,
			Redact: func(args []any) []any {
				for i := range args {
					args[i] = "***"
				}
				return args
			},
		},
	)
	hook(context.Background(), drops.QueryEvent{
		Kind:     "exec",
		SQL:      "INSERT INTO secrets VALUES ($1, $2)",
		Args:     []any{"token-123", "user@example.com"},
		Duration: time.Millisecond,
	})
	if strings.Contains(line, "token-123") || strings.Contains(line, "example.com") {
		t.Errorf("secrets leaked: %q", line)
	}
	if !strings.Contains(line, "***") {
		t.Errorf("redaction marker missing: %q", line)
	}
}

func TestLoggerHookRedactDoesNotMutateOriginalArgs(t *testing.T) {
	hook := pg.LoggerHook(
		func(string, ...any) {},
		pg.LoggerOptions{
			LogArgs: true,
			Redact: func(args []any) []any {
				for i := range args {
					args[i] = "x"
				}
				return args
			},
		},
	)
	orig := []any{"sensitive", 42}
	hook(context.Background(), drops.QueryEvent{
		Kind: "exec", SQL: "SELECT 1",
		Args: orig, Duration: time.Millisecond,
	})
	if orig[0] != "sensitive" || orig[1] != 42 {
		t.Errorf("Redact mutated original args: %v", orig)
	}
}
