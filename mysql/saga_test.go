package mysql_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/mysql"
)

// The saga coordinator. Each step runs in its own transaction and a
// failure runs the completed steps' compensations in reverse, which is
// the whole contract — there is no distributed transaction underneath,
// only an ordering and an undo per step.

func TestSagaRunsEveryStepInOrder(t *testing.T) {
	db := mysql.New(&frDriver{})
	var order []string
	saga := mysql.NewSaga("checkout").
		Step("a", func(context.Context, *mysql.DB, *mysql.SagaState) error {
			order = append(order, "a")
			return nil
		}, nil).
		Step("b", func(context.Context, *mysql.DB, *mysql.SagaState) error {
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

func TestSagaCompensatesInReverseOnFailure(t *testing.T) {
	db := mysql.New(&frDriver{})
	var log []string
	saga := mysql.NewSaga("checkout").
		Step("charge",
			func(context.Context, *mysql.DB, *mysql.SagaState) error { log = append(log, "charge"); return nil },
			func(context.Context, *mysql.DB, *mysql.SagaState) error { log = append(log, "refund"); return nil }).
		Step("ship",
			func(context.Context, *mysql.DB, *mysql.SagaState) error {
				log = append(log, "ship")
				return errors.New("out of stock")
			}, nil)

	err := saga.Run(db, context.Background(), nil)
	if !mysql.IsSagaError(err) {
		t.Fatalf("want SagaError, got %v", err)
	}
	var se *mysql.SagaError
	if !errors.As(err, &se) {
		t.Fatalf("error is not a *mysql.SagaError: %v", err)
	}
	if se.FailedStep != "ship" {
		t.Errorf("failed step: %s", se.FailedStep)
	}
	// charge ran, ship ran and failed, then charge was compensated.
	// The step that failed is NOT compensated: it did not complete, so
	// there is nothing of it to undo.
	if strings.Join(log, ",") != "charge,ship,refund" {
		t.Errorf("compensation order: %v", log)
	}
}

// A compensation that itself fails does not stop the others. The saga
// is already unwinding; abandoning the remaining undos because one of
// them failed would leave more behind, not less.
func TestSagaKeepsCompensatingAfterOneCompensationFails(t *testing.T) {
	db := mysql.New(&frDriver{})
	var log []string
	boom := errors.New("refund declined")
	saga := mysql.NewSaga("checkout").
		Step("reserve",
			func(context.Context, *mysql.DB, *mysql.SagaState) error { log = append(log, "reserve"); return nil },
			func(context.Context, *mysql.DB, *mysql.SagaState) error { log = append(log, "unreserve"); return nil }).
		Step("charge",
			func(context.Context, *mysql.DB, *mysql.SagaState) error { log = append(log, "charge"); return nil },
			func(context.Context, *mysql.DB, *mysql.SagaState) error { return boom }).
		Step("ship",
			func(context.Context, *mysql.DB, *mysql.SagaState) error { return errors.New("out of stock") }, nil)

	err := saga.Run(db, context.Background(), nil)
	var se *mysql.SagaError
	if !errors.As(err, &se) {
		t.Fatalf("error is not a *mysql.SagaError: %v", err)
	}
	if len(se.CompFailures) != 1 || !errors.Is(se.CompFailures[0].Err, boom) {
		t.Errorf("the failed compensation was not reported: %+v", se.CompFailures)
	}
	// The one after it still ran.
	if strings.Join(log, ",") != "reserve,charge,unreserve" {
		t.Errorf("a failed compensation stopped the rest: %v", log)
	}
}

func TestSagaStateIsTypedOnTheWayOut(t *testing.T) {
	st := &mysql.SagaState{}
	st.Set("orderId", int64(99))
	if v, ok := mysql.SagaStateGet[int64](st, "orderId"); !ok || v != 99 {
		t.Errorf("typed get: %v %v", v, ok)
	}
	if _, ok := mysql.SagaStateGet[string](st, "orderId"); ok {
		t.Error("a get at the wrong type must not succeed")
	}
}
