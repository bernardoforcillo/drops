package sqlite_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/sqlite"
)

// execErrDriver returns a fixed error from Exec (for the concurrency
// path); Query/Begin behave like entDriver.
type execErrDriver struct {
	entDriver
	execErr error
}

func (d *execErrDriver) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	if d.execErr != nil {
		return nil, d.execErr
	}
	return d.entDriver.Exec(ctx, sql, args...)
}
func (d *execErrDriver) Begin(context.Context) (drops.Tx, error) { return &execErrTx{d}, nil }

type execErrTx struct{ *execErrDriver }

func (*execErrTx) Commit(context.Context) error   { return nil }
func (*execErrTx) Rollback(context.Context) error { return nil }

func TestEventStoreAppendSQL(t *testing.T) {
	drv := &entDriver{}
	db := sqlite.New(drv)
	store := sqlite.NewEventStore(db, "events")

	err := store.Append(db, context.Background(), "match", "abc", -1,
		sqlite.EventInput{Type: "started", Payload: map[string]int{"n": 1}},
		sqlite.EventInput{Type: "joined", Payload: map[string]int{"n": 2}},
	)
	if err != nil {
		t.Fatal(err)
	}
	q := drv.queries[0]
	if !strings.Contains(q, `INSERT INTO "events"`) || !strings.Contains(q, "(?, ?, ?, ?, ?, ?), (?, ?, ?, ?, ?, ?)") {
		t.Errorf("append SQL:\n%s", q)
	}
	if len(drv.args[0]) != 12 {
		t.Errorf("want 12 args, got %d", len(drv.args[0]))
	}
}

func TestEventStoreConcurrencyConflict(t *testing.T) {
	drv := &execErrDriver{execErr: errors.New("UNIQUE constraint failed: events.version")}
	db := sqlite.New(drv)
	store := sqlite.NewEventStore(db, "events")
	err := store.Append(db, context.Background(), "match", "abc", 0,
		sqlite.EventInput{Type: "x", Payload: nil})
	if !errors.Is(err, sqlite.ErrConcurrencyConflict) {
		t.Errorf("want ErrConcurrencyConflict, got %v", err)
	}
}

func TestEventStoreTableDDL(t *testing.T) {
	tbl := sqlite.NewEventStoreTable("events")
	sql, _ := sqlite.ToSQL(sqlite.CreateTable(tbl))
	for _, want := range []string{`CREATE TABLE "events"`, `"aggregateType" TEXT NOT NULL`, `UNIQUE ("aggregateType", "aggregateID", "version")`} {
		if !strings.Contains(sql, want) {
			t.Errorf("DDL missing %q:\n%s", want, sql)
		}
	}
}

func TestSagaRunsAllSteps(t *testing.T) {
	db := sqlite.New(&entDriver{})
	var order []string
	saga := sqlite.NewSaga("checkout").
		Step("a", func(context.Context, *sqlite.DB, *sqlite.SagaState) error {
			order = append(order, "a")
			return nil
		}, nil).
		Step("b", func(context.Context, *sqlite.DB, *sqlite.SagaState) error {
			order = append(order, "b")
			return nil
		}, nil)
	if err := saga.Run(db, context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "a,b" {
		t.Errorf("order: %v", order)
	}
}

func TestSagaCompensatesOnFailure(t *testing.T) {
	db := sqlite.New(&entDriver{})
	var log []string
	saga := sqlite.NewSaga("checkout").
		Step("charge",
			func(context.Context, *sqlite.DB, *sqlite.SagaState) error { log = append(log, "charge"); return nil },
			func(context.Context, *sqlite.DB, *sqlite.SagaState) error { log = append(log, "refund"); return nil }).
		Step("ship",
			func(context.Context, *sqlite.DB, *sqlite.SagaState) error {
				log = append(log, "ship")
				return errors.New("out of stock")
			}, nil)

	err := saga.Run(db, context.Background(), nil)
	if !sqlite.IsSagaError(err) {
		t.Fatalf("want SagaError, got %v", err)
	}
	var se *sqlite.SagaError
	if errors.As(err, &se); se.FailedStep != "ship" {
		t.Errorf("failed step: %s", se.FailedStep)
	}
	// charge ran, ship ran and failed, then charge was compensated.
	if strings.Join(log, ",") != "charge,ship,refund" {
		t.Errorf("compensation order: %v", log)
	}
}

func TestSagaStateTyped(t *testing.T) {
	st := &sqlite.SagaState{}
	st.Set("orderId", int64(99))
	if v, ok := sqlite.SagaStateGet[int64](st, "orderId"); !ok || v != 99 {
		t.Errorf("typed get: %v %v", v, ok)
	}
	if _, ok := sqlite.SagaStateGet[string](st, "orderId"); ok {
		t.Error("wrong-type get should fail")
	}
}
