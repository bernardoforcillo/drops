package pg

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Saga implements the saga pattern: a sequence of steps where each
// step commits its own transaction and provides a compensating
// action. If step N fails, drops runs the compensations of steps
// [0, N-1] in reverse order, attempting to undo what was done.
//
//	saga := pg.NewSaga("checkout").
//	    Step("charge",  chargeFn,  refundFn).
//	    Step("ship",    shipFn,    cancelShipmentFn).
//	    Step("email",   emailFn,   nil)             // no comp needed
//
//	state := &pg.SagaState{}
//	state.Set("orderId", orderID)
//	if err := saga.Run(db, ctx, state); err != nil {
//	    // some step failed; earlier ones have been compensated
//	}
//
// State flows between steps as a typed key/value bag (Get / Set
// / GetTyped). Forward steps write outputs (charge writes the
// payment id); compensations read them (refund needs the payment
// id). Each step runs in its own transaction so a single step's
// rollback doesn't poison later ones.
//
// Caveats:
//
//   - Compensations are best-effort. If a compensation fails, the
//     remaining ones still run, and the original step error is
//     returned wrapped in a SagaError that lists compensation
//     failures.
//
//   - This implementation is in-memory. A process crash during
//     the saga loses state — pair with the outbox or
//     idempotency keys for crash-resilient long-running flows.
//     (Durable saga state is a future commit.)
//
//   - "Idempotency" is the step author's responsibility: a saga
//     can re-run a step after a partial failure (e.g. via
//     pg.Retry around saga.Run), so forward / compensate
//     functions should detect prior application.

// SagaStepFn is the signature for forward and compensating
// actions. The tx is a fresh transaction per step. State is
// shared across the saga; mutations made before an error are
// visible to compensations.
type SagaStepFn func(ctx context.Context, tx *DB, state *SagaState) error

// SagaState is the typed bag flowing between steps. Concurrent
// access is safe because steps run sequentially, but the mutex
// keeps things tidy if a step spawns helpers that touch state.
type SagaState struct {
	mu   sync.Mutex
	data map[string]any
}

// Set stores v under key.
func (s *SagaState) Set(key string, v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		s.data = map[string]any{}
	}
	s.data[key] = v
}

// Get returns the value stored under key and ok=true when
// present.
func (s *SagaState) Get(key string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[key]
	return v, ok
}

// SagaStateGet is the type-safe getter. Returns the zero value
// of T plus ok=false when the key is missing OR the stored value
// is the wrong type.
func SagaStateGet[T any](s *SagaState, key string) (T, bool) {
	var zero T
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[key]
	if !ok {
		return zero, false
	}
	t, ok := v.(T)
	return t, ok
}

// Saga collects a named sequence of steps to run.
type Saga struct {
	name  string
	steps []sagaStep
}

type sagaStep struct {
	name       string
	forward    SagaStepFn
	compensate SagaStepFn
}

// NewSaga returns a saga with the supplied name. Use Step to add
// steps in execution order.
func NewSaga(name string) *Saga { return &Saga{name: name} }

// Step appends a step. compensate may be nil — drops will skip
// the step on rollback in that case (suitable for read-only or
// inherently-idempotent side effects like sending a
// not-already-sent email).
func (s *Saga) Step(name string, forward, compensate SagaStepFn) *Saga {
	if forward == nil {
		panic("drops/pg: Saga.Step requires a non-nil forward function")
	}
	s.steps = append(s.steps, sagaStep{name: name, forward: forward, compensate: compensate})
	return s
}

// SagaError is returned by Saga.Run when a step fails. It exposes
// the failing step's index / name, the original error, and any
// errors that surfaced during compensation. Use errors.As to
// reach it.
type SagaError struct {
	SagaName       string
	FailedStep     string
	FailedStepIdx  int
	Cause          error
	CompFailures   []SagaCompensationFailure
}

// SagaCompensationFailure is one compensation that itself errored.
type SagaCompensationFailure struct {
	StepName string
	StepIdx  int
	Err      error
}

// Error implements error.
func (e *SagaError) Error() string {
	msg := fmt.Sprintf("drops/pg: saga %q step %d (%s) failed: %v",
		e.SagaName, e.FailedStepIdx, e.FailedStep, e.Cause)
	if len(e.CompFailures) > 0 {
		msg += fmt.Sprintf("; %d compensation(s) also failed", len(e.CompFailures))
	}
	return msg
}

// Unwrap exposes the failing-step cause to errors.Is / errors.As.
func (e *SagaError) Unwrap() error { return e.Cause }

// Run executes the saga against db. state may be nil (a fresh
// state is allocated). Each step runs in its own transaction; on
// failure, compensations of completed steps run in reverse
// order. Returns nil on full success, *SagaError on partial
// failure, or the bare error from a step when nothing prior had
// completed.
func (s *Saga) Run(db *DB, ctx context.Context, state *SagaState) error {
	if state == nil {
		state = &SagaState{}
	}
	completed := make([]int, 0, len(s.steps))
	for i, step := range s.steps {
		err := db.InTx(ctx, func(tx *DB) error {
			return step.forward(ctx, tx, state)
		})
		if err != nil {
			compFailures := s.runCompensations(db, ctx, state, completed)
			return &SagaError{
				SagaName:      s.name,
				FailedStep:    step.name,
				FailedStepIdx: i,
				Cause:         err,
				CompFailures:  compFailures,
			}
		}
		completed = append(completed, i)
	}
	return nil
}

// runCompensations runs compensate for every completed step in
// reverse order. Errors are collected and returned; the loop
// does not abort on a single failure.
func (s *Saga) runCompensations(db *DB, ctx context.Context, state *SagaState, completed []int) []SagaCompensationFailure {
	var failures []SagaCompensationFailure
	for i := len(completed) - 1; i >= 0; i-- {
		idx := completed[i]
		step := s.steps[idx]
		if step.compensate == nil {
			continue
		}
		// Use a detached context so a cancelled parent doesn't
		// prevent cleanup — same shape as InTx rollback.
		cctx, cancel := rollbackCtx(ctx)
		cerr := db.InTx(cctx, func(tx *DB) error {
			return step.compensate(cctx, tx, state)
		})
		cancel()
		if cerr != nil {
			failures = append(failures, SagaCompensationFailure{
				StepName: step.name,
				StepIdx:  idx,
				Err:      cerr,
			})
		}
	}
	return failures
}

// IsSagaError reports whether err is a *SagaError.
func IsSagaError(err error) bool {
	var se *SagaError
	return errors.As(err, &se)
}
