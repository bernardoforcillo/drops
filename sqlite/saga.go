package sqlite

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Saga implements the saga pattern: a sequence of steps where each step
// commits its own transaction and provides a compensating action. If
// step N fails, the compensations of steps [0, N-1] run in reverse
// order to undo prior work.
//
//	saga := sqlite.NewSaga("checkout").
//	    Step("charge", chargeFn, refundFn).
//	    Step("ship",   shipFn,   cancelShipmentFn).
//	    Step("email",  emailFn,  nil)
//
//	state := &sqlite.SagaState{}
//	state.Set("orderId", orderID)
//	if err := saga.Run(db, ctx, state); err != nil { /* compensated */ }
//
// Caveats: compensations are best-effort; this implementation is
// in-memory (a crash loses state — pair with idempotency keys for
// crash-resilient flows); and step idempotency is the author's
// responsibility.

// SagaStepFn is the signature for forward and compensating actions.
type SagaStepFn func(ctx context.Context, tx *DB, state *SagaState) error

// SagaState is the typed bag flowing between steps.
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

// Get returns the value under key and ok=true when present.
func (s *SagaState) Get(key string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[key]
	return v, ok
}

// SagaStateGet is the type-safe getter — zero value + ok=false when the
// key is missing or the stored value is the wrong type.
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

// NewSaga returns a saga with the supplied name.
func NewSaga(name string) *Saga { return &Saga{name: name} }

// Step appends a step. compensate may be nil (skipped on rollback).
func (s *Saga) Step(name string, forward, compensate SagaStepFn) *Saga {
	if forward == nil {
		panic("drops/sqlite: Saga.Step requires a non-nil forward function")
	}
	s.steps = append(s.steps, sagaStep{name: name, forward: forward, compensate: compensate})
	return s
}

// SagaError is returned by Saga.Run when a step fails.
type SagaError struct {
	SagaName      string
	FailedStep    string
	FailedStepIdx int
	Cause         error
	CompFailures  []SagaCompensationFailure
}

// SagaCompensationFailure is one compensation that itself errored.
type SagaCompensationFailure struct {
	StepName string
	StepIdx  int
	Err      error
}

// Error implements error.
func (e *SagaError) Error() string {
	msg := fmt.Sprintf("drops/sqlite: saga %q step %d (%s) failed: %v",
		e.SagaName, e.FailedStepIdx, e.FailedStep, e.Cause)
	if len(e.CompFailures) > 0 {
		msg += fmt.Sprintf("; %d compensation(s) also failed", len(e.CompFailures))
	}
	return msg
}

// Unwrap exposes the failing-step cause to errors.Is / errors.As.
func (e *SagaError) Unwrap() error { return e.Cause }

// Run executes the saga against db. Each step runs in its own
// transaction; on failure, compensations of completed steps run in
// reverse order.
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

// runCompensations runs compensate for every completed step in reverse
// order, collecting (not aborting on) failures.
func (s *Saga) runCompensations(db *DB, ctx context.Context, state *SagaState, completed []int) []SagaCompensationFailure {
	var failures []SagaCompensationFailure
	for i := len(completed) - 1; i >= 0; i-- {
		idx := completed[i]
		step := s.steps[idx]
		if step.compensate == nil {
			continue
		}
		// Detached context so a cancelled parent doesn't block cleanup.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
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
