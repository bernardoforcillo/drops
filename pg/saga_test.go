package pg_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// sagaDriver is a trivial commit-everything driver — sagas only
// need Begin/Commit/Rollback to work; the steps run user code.
type sagaDriver struct {
	begins    atomic.Int32
	commits   atomic.Int32
	rollbacks atomic.Int32
}

func (d *sagaDriver) Exec(context.Context, string, ...any) (drops.Result, error) {
	return nil, nil
}
func (d *sagaDriver) Query(context.Context, string, ...any) (drops.Rows, error) {
	return &fakeRows{}, nil
}
func (d *sagaDriver) Begin(context.Context) (drops.Tx, error) {
	d.begins.Add(1)
	return &sagaTx{drv: d}, nil
}

type sagaTx struct{ drv *sagaDriver }

func (tx *sagaTx) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	return tx.drv.Exec(ctx, sql, args...)
}
func (tx *sagaTx) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	return tx.drv.Query(ctx, sql, args...)
}
func (tx *sagaTx) Begin(ctx context.Context) (drops.Tx, error) { return tx.drv.Begin(ctx) }
func (tx *sagaTx) Commit(_ context.Context) error              { tx.drv.commits.Add(1); return nil }
func (tx *sagaTx) Rollback(_ context.Context) error            { tx.drv.rollbacks.Add(1); return nil }

func TestSagaSuccessfulPath(t *testing.T) {
	drv := &sagaDriver{}
	db := pg.New(drv)
	called := []string{}
	saga := pg.NewSaga("checkout").
		Step("charge", func(ctx context.Context, tx *pg.DB, st *pg.SagaState) error {
			called = append(called, "charge")
			st.Set("paymentId", int64(999))
			return nil
		}, func(ctx context.Context, tx *pg.DB, st *pg.SagaState) error {
			called = append(called, "charge-comp")
			return nil
		}).
		Step("ship", func(ctx context.Context, tx *pg.DB, st *pg.SagaState) error {
			called = append(called, "ship")
			return nil
		}, nil)

	if err := saga.Run(db, context.Background(), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Join(called, ",") != "charge,ship" {
		t.Errorf("expected forward order, got: %v", called)
	}
	if drv.commits.Load() != 2 {
		t.Errorf("expected 2 commits, got %d", drv.commits.Load())
	}
}

func TestSagaCompensatesInReverseOnFailure(t *testing.T) {
	drv := &sagaDriver{}
	db := pg.New(drv)
	called := []string{}
	saga := pg.NewSaga("checkout").
		Step("charge",
			func(ctx context.Context, tx *pg.DB, st *pg.SagaState) error {
				called = append(called, "charge")
				st.Set("paymentId", int64(999))
				return nil
			},
			func(ctx context.Context, tx *pg.DB, st *pg.SagaState) error {
				pid, _ := pg.SagaStateGet[int64](st, "paymentId")
				called = append(called, "refund:" + strFromInt(pid))
				return nil
			}).
		Step("ship",
			func(ctx context.Context, tx *pg.DB, st *pg.SagaState) error {
				called = append(called, "ship")
				return nil
			},
			func(ctx context.Context, tx *pg.DB, st *pg.SagaState) error {
				called = append(called, "ship-comp")
				return nil
			}).
		Step("email",
			func(ctx context.Context, tx *pg.DB, st *pg.SagaState) error {
				called = append(called, "email-FAIL")
				return errors.New("smtp down")
			}, nil)

	err := saga.Run(db, context.Background(), nil)
	if err == nil {
		t.Fatal("expected error from failing step")
	}
	var se *pg.SagaError
	if !errors.As(err, &se) {
		t.Fatalf("expected SagaError, got %T", err)
	}
	if se.FailedStep != "email" || se.FailedStepIdx != 2 {
		t.Errorf("failed step: %+v", se)
	}
	// Order should be: charge, ship, email-FAIL, then comps in reverse:
	// ship-comp, refund:999
	wantOrder := []string{"charge", "ship", "email-FAIL", "ship-comp", "refund:999"}
	if strings.Join(called, ",") != strings.Join(wantOrder, ",") {
		t.Errorf("order:\n got:  %v\n want: %v", called, wantOrder)
	}
}

func TestSagaSkipsNilCompensation(t *testing.T) {
	drv := &sagaDriver{}
	db := pg.New(drv)
	called := []string{}
	saga := pg.NewSaga("x").
		Step("nocomp", func(ctx context.Context, tx *pg.DB, st *pg.SagaState) error {
			called = append(called, "nocomp")
			return nil
		}, nil).
		Step("fail", func(ctx context.Context, tx *pg.DB, st *pg.SagaState) error {
			called = append(called, "fail")
			return errors.New("boom")
		}, nil)

	err := saga.Run(db, context.Background(), nil)
	if err == nil {
		t.Fatal("expected error")
	}
	// "nocomp" has no compensation → not invoked again.
	if strings.Join(called, ",") != "nocomp,fail" {
		t.Errorf("nil compensation must be skipped: %v", called)
	}
}

func TestSagaCollectsCompensationFailures(t *testing.T) {
	drv := &sagaDriver{}
	db := pg.New(drv)
	saga := pg.NewSaga("x").
		Step("s1", func(ctx context.Context, tx *pg.DB, st *pg.SagaState) error { return nil },
			func(ctx context.Context, tx *pg.DB, st *pg.SagaState) error {
				return errors.New("comp1 failed")
			}).
		Step("s2", func(ctx context.Context, tx *pg.DB, st *pg.SagaState) error { return nil },
			func(ctx context.Context, tx *pg.DB, st *pg.SagaState) error {
				return errors.New("comp2 failed")
			}).
		Step("boom", func(ctx context.Context, tx *pg.DB, st *pg.SagaState) error {
			return errors.New("boom")
		}, nil)

	err := saga.Run(db, context.Background(), nil)
	var se *pg.SagaError
	if !errors.As(err, &se) {
		t.Fatalf("expected SagaError, got %v", err)
	}
	if len(se.CompFailures) != 2 {
		t.Errorf("expected 2 compensation failures, got %d", len(se.CompFailures))
	}
	if !strings.Contains(se.Error(), "compensation") {
		t.Errorf("error message should mention compensations: %s", se.Error())
	}
}

func TestSagaStateTypedGetter(t *testing.T) {
	st := &pg.SagaState{}
	st.Set("count", 42)
	st.Set("name", "alice")
	if v, ok := pg.SagaStateGet[int](st, "count"); !ok || v != 42 {
		t.Errorf("typed get int: %d, %v", v, ok)
	}
	if v, ok := pg.SagaStateGet[string](st, "name"); !ok || v != "alice" {
		t.Errorf("typed get string: %s, %v", v, ok)
	}
	if _, ok := pg.SagaStateGet[bool](st, "count"); ok {
		t.Errorf("typed get with wrong type should report ok=false")
	}
	if _, ok := pg.SagaStateGet[int](st, "missing"); ok {
		t.Errorf("missing key should report ok=false")
	}
}

func TestSagaIsSagaError(t *testing.T) {
	se := &pg.SagaError{SagaName: "x", FailedStep: "s", Cause: errors.New("c")}
	if !pg.IsSagaError(se) {
		t.Error("IsSagaError must recognise *SagaError")
	}
	if pg.IsSagaError(errors.New("plain")) {
		t.Error("IsSagaError must not match plain errors")
	}
}

func strFromInt(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
