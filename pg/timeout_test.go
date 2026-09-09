package pg_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/pg"
)

// A ctx deadline stops the CALLER waiting. statement_timeout stops the
// SERVER working. These pin the second, and the boundary it is scoped
// to — because a SET that outlives its transaction is a setting the
// next borrower of the pooled connection inherits.

func TestStatementTimeoutIsEstablishedInsideTheTransaction(t *testing.T) {
	drv := dropstest.New()
	db := pg.New(drv)

	err := db.InTxWithTimeout(context.Background(), 2*time.Second, func(tx *pg.DB) error {
		_, err := tx.Exec(context.Background(), "SELECT 1")
		return err
	})
	if err != nil {
		t.Fatalf("InTxWithTimeout: %v", err)
	}

	sqls := drv.SQL()
	if len(sqls) != 2 {
		t.Fatalf("statements = %v, want the SET and the caller's one", sqls)
	}
	if sqls[0] != "SET LOCAL statement_timeout = 2000" {
		t.Errorf("first statement = %q, want the SET LOCAL in milliseconds", sqls[0])
	}
	// LOCAL is the whole design: without it the setting is the
	// session's, and a session is a pooled connection.
	if !strings.Contains(sqls[0], "LOCAL") {
		t.Errorf("the timeout is not transaction-scoped: %q", sqls[0])
	}
	// And it is established BEFORE the caller's work, not beside it.
	if sqls[1] != "SELECT 1" {
		t.Errorf("second statement = %q, want the caller's", sqls[1])
	}
}

// A sub-millisecond bound rounds UP. Zero is statement_timeout's own
// spelling of "no limit", so rounding down would turn the tightest
// bound a caller can ask for into none at all.
func TestASubMillisecondTimeoutDoesNotRoundToNoLimit(t *testing.T) {
	drv := dropstest.New()
	db := pg.New(drv)

	if err := db.InTxWithTimeout(context.Background(), 100*time.Microsecond,
		func(*pg.DB) error { return nil }); err != nil {
		t.Fatalf("InTxWithTimeout: %v", err)
	}
	if got := drv.SQL(); len(got) == 0 || got[0] != "SET LOCAL statement_timeout = 1" {
		t.Errorf("statements = %v, want a timeout of 1ms rather than 0", got)
	}
}

// A duration that is not positive is refused before a transaction is
// opened: 0 means "no limit" to the server, and asking for one that way
// reads at the call site as asking for a bound.
func TestANonPositiveTimeoutIsRefusedWithoutOpeningATransaction(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		drv := dropstest.New()
		db := pg.New(drv)
		err := db.InTxWithTimeout(context.Background(), d, func(*pg.DB) error {
			t.Error("the body ran for a refused timeout")
			return nil
		})
		if !errors.Is(err, pg.ErrTimeoutOutsideTx) {
			t.Errorf("%v: got %v, want ErrTimeoutOutsideTx", d, err)
		}
		if got := drv.Statements(); len(got) != 0 {
			t.Errorf("%v: a refused call still sent %v", d, got)
		}
	}
}

// The server aborting a statement is SQLSTATE 57014, which reaches the
// caller as a typed refusal rather than as a driver string.
func TestATimedOutStatementIsErrQueryCanceled(t *testing.T) {
	drv := dropstest.New()
	db := pg.New(drv)

	err := db.InTxWithTimeout(context.Background(), time.Second, func(tx *pg.DB) error {
		// After the SET, so it is the caller's statement that the
		// server aborts rather than the one establishing the bound.
		drv.FailNext(&pgxLikeError{code: "57014", msg: "canceling statement due to statement timeout"})
		_, err := tx.Exec(context.Background(), "SELECT pg_sleep(10)")
		return err
	})
	if !errors.Is(err, pg.ErrQueryCanceled) {
		t.Fatalf("got %v, want ErrQueryCanceled", err)
	}
}
