package drops

import (
	"context"
	"errors"
	"fmt"
)

// A Multi is a transaction written down as a value: an ordered list of
// named steps, all of which run inside one transaction, all of which
// are committed together or none at all.
//
//	m := drops.NewMulti().
//	    Run("team", func(ctx context.Context, tx drops.Tx, prior drops.Results) (any, error) {
//	        return insertTeam(ctx, tx)
//	    }).
//	    Run("audit", func(ctx context.Context, tx drops.Tx, prior drops.Results) (any, error) {
//	        team, err := drops.StepResult[*Team](prior, "team")
//	        if err != nil {
//	            return nil, err
//	        }
//	        return nil, audit(ctx, tx, team.ID)
//	    })
//
//	res, err := m.Exec(ctx, db)
//
// The point is what happens when step four fails at three in the
// morning. [InTx] hands back one error and nothing else; a Multi hands
// back a [MultiError] that names the step that failed, carries its
// cause, and carries the results of every step that had already
// succeeded — which is usually the difference between reading the log
// and reproducing the incident.
//
// The second reason to describe a transaction rather than close over
// it: a closure can do anything, and re-running one (as a
// serialization-failure retry does) is safe only by convention.
// Nothing stops a closure from having sent an email on the attempt
// that got rolled back. A Multi is a list of steps that can be
// enumerated with [Multi.Names] before it ever runs, so "this script
// touches these four things, in this order" is a fact a test can
// assert rather than a comment. It does not make a step's body pure —
// only Go can do that — but it puts the shape of the transaction where
// a reviewer and a test can see it.
//
// # Multi is not a saga
//
// A Multi is one transaction. Every step shares it, the database
// guarantees all-or-nothing, and there is nothing to compensate
// because a failure leaves no trace. That bounds what belongs in one:
// work that is finished by the time the transaction commits, and short
// enough that holding it open is reasonable.
//
// A saga (see the Saga type in the pg package) is the opposite trade.
// Each step commits its own transaction, so the work is durable as it
// goes and nothing holds a long transaction open — and in exchange
// undoing is no longer the database's job, so every step needs a
// compensating action and those compensations are best-effort. A saga
// can leave the world half-undone; a Multi cannot. Reach for a saga
// when the work spans services or hours, for a Multi when it does not.
//
// A Multi is safe for concurrent Exec once built; building one (Run,
// Step, Append) is not.
type Multi struct {
	steps []multiStep
	// names is the set of step names, for the duplicate check. A
	// repeated name would make Results.Get ambiguous, so it is
	// refused at construction rather than silently resolved.
	names map[string]struct{}
}

type multiStep struct {
	name string
	fn   StepFunc
}

// StepFunc is one step of a [Multi]. It receives the shared
// transaction and the results of the steps that already ran, and
// returns the value this step contributes under its own name.
//
// Returning an error aborts the Multi: no later step runs and the
// transaction rolls back.
type StepFunc func(ctx context.Context, tx Tx, prior Results) (any, error)

// NewMulti returns an empty Multi. Add steps with [Multi.Run] or
// [Step].
func NewMulti() *Multi { return &Multi{} }

// Run appends a step. It panics on an empty name, a nil function, or a
// name already used — all three are mistakes in the program text, not
// conditions a caller could handle, and a Multi is meant to be built
// once at start-up where a panic is loud and immediate.
func (m *Multi) Run(name string, fn StepFunc) *Multi {
	if name == "" {
		panic("drops: Multi.Run requires a non-empty step name")
	}
	if fn == nil {
		panic("drops: Multi.Run requires a non-nil function for step " + name)
	}
	if _, dup := m.names[name]; dup {
		panic("drops: Multi already has a step named " + name)
	}
	if m.names == nil {
		m.names = make(map[string]struct{})
	}
	m.names[name] = struct{}{}
	m.steps = append(m.steps, multiStep{name: name, fn: fn})
	return m
}

// Step appends a step whose result type is known at compile time. It
// is the typed form of [Multi.Run] and exists because a method cannot
// take a type parameter:
//
//	m := drops.NewMulti()
//	drops.Step(m, "team", func(ctx context.Context, tx drops.Tx, prior drops.Results) (*Team, error) {
//	    return insertTeam(ctx, tx)
//	})
//
// What the type parameter buys is on the reading end. A step declared
// this way can only ever store a T, so the matching
// StepResult[T](res, "team") cannot fail on the type — the compiler
// has already agreed the two halves are talking about the same thing,
// and a later change to the step's return type breaks every reader
// that has not kept up. Run stores an any and defers that check to
// run time.
func Step[T any](m *Multi, name string, fn func(ctx context.Context, tx Tx, prior Results) (T, error)) *Multi {
	if fn == nil {
		panic("drops: Step requires a non-nil function for step " + name)
	}
	return m.Run(name, func(ctx context.Context, tx Tx, prior Results) (any, error) {
		v, err := fn(ctx, tx, prior)
		if err != nil {
			// Discard v: a step that failed contributes nothing,
			// and Results never holds an entry for it.
			return nil, err
		}
		return v, nil
	})
}

// Append copies other's steps onto m, so two scripts written
// separately can be run as one transaction. It panics on a name
// collision, for the reason [Multi.Run] does. other is not modified.
func (m *Multi) Append(other *Multi) *Multi {
	if other == nil {
		return m
	}
	for _, s := range other.steps {
		m.Run(s.name, s.fn)
	}
	return m
}

// Names returns the step names in execution order. This is the
// inspection hook: a test can assert what a transaction script does
// without a database anywhere in sight.
func (m *Multi) Names() []string {
	out := make([]string, len(m.steps))
	for i, s := range m.steps {
		out[i] = s.name
	}
	return out
}

// Len returns the number of steps.
func (m *Multi) Len() int { return len(m.steps) }

// Exec opens a transaction on d, runs every step in order, and commits.
// Any step's error rolls the whole thing back and comes back wrapped in
// a [MultiError].
//
// A Multi with no steps does not touch d at all.
//
// The Results returned alongside an error hold the steps that had
// succeeded before the failure — the same value as MultiError.Completed.
// Their database effects are gone, rolled back with everything else;
// they are there to say how far the script got and with what values.
//
// Only a step's failure is a MultiError. Failing to open the
// transaction, or failing to commit one every step was happy with, is
// the driver's error returned as it came — there is no step to name.
// After a failed commit the Results are complete rather than partial,
// which is the shape to test for if the two must be told apart.
func (m *Multi) Exec(ctx context.Context, d Driver) (Results, error) {
	if len(m.steps) == 0 {
		return Results{}, nil
	}
	var res Results
	err := InTx(ctx, d, func(tx Tx) error {
		var err error
		res, err = m.ExecIn(ctx, tx)
		return err
	})
	return res, err
}

// ExecIn runs the steps against an already-open transaction, without
// committing or rolling back — that stays the caller's decision. It is
// what [Multi.Exec] is built from, and the way to nest a Multi inside a
// larger transaction or inside a dialect's retry loop.
func (m *Multi) ExecIn(ctx context.Context, tx Tx) (Results, error) {
	res := Results{}
	for i, s := range m.steps {
		v, err := s.fn(ctx, tx, res)
		if err != nil {
			return res, &MultiError{
				FailedStep:    s.name,
				FailedStepIdx: i,
				Cause:         err,
				Completed:     res,
			}
		}
		res = res.with(s.name, v)
	}
	return res, nil
}

// Results holds what each step of a [Multi] returned, keyed by step
// name and ordered by execution. The zero value is empty and usable.
//
// Read a value out with [StepResult], which types it; [Results.Get]
// is the untyped escape hatch.
type Results struct {
	order []string
	vals  map[string]any
}

// with returns a copy of r carrying one more entry, leaving r itself
// untouched. Copying rather than appending in place is what makes the
// Results a step was handed a stable snapshot of the world before it,
// and what makes MultiError.Completed still mean "the steps that had
// succeeded" long after the Multi returned.
func (r Results) with(name string, v any) Results {
	out := Results{
		order: make([]string, len(r.order), len(r.order)+1),
		vals:  make(map[string]any, len(r.vals)+1),
	}
	copy(out.order, r.order)
	for k, val := range r.vals {
		out.vals[k] = val
	}
	out.order = append(out.order, name)
	out.vals[name] = v
	return out
}

// Get returns the value a step returned, and ok=false when no step of
// that name has run.
func (r Results) Get(name string) (any, bool) {
	v, ok := r.vals[name]
	return v, ok
}

// Has reports whether the named step ran successfully.
func (r Results) Has(name string) bool {
	_, ok := r.vals[name]
	return ok
}

// Names returns the names of the steps that ran, in execution order.
func (r Results) Names() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Len returns how many steps ran.
func (r Results) Len() int { return len(r.order) }

// ErrNoSuchStep is returned by [StepResult] when no step of that name
// has run — either because the Multi has no such step, or because it
// failed or never got that far.
var ErrNoSuchStep = errors.New("drops: no such step")

// ErrStepResultType is returned by [StepResult] when the step ran but
// returned a value of a different type. Building the step with [Step]
// instead of [Multi.Run] makes this one unreachable.
var ErrStepResultType = errors.New("drops: step result has a different type")

// StepResult returns the named step's result as a T.
//
// It reports [ErrNoSuchStep] and [ErrStepResultType] separately on
// purpose: "the step never ran" and "the step ran and returned
// something else" are different bugs, and collapsing both into a zero
// value — the shape a plain type assertion would take — hides whichever
// one you have.
//
// A step that returned an untyped nil yields the zero value of T and
// no error.
func StepResult[T any](r Results, name string) (T, error) {
	var zero T
	v, ok := r.vals[name]
	if !ok {
		return zero, fmt.Errorf("%w: %q", ErrNoSuchStep, name)
	}
	if v == nil {
		return zero, nil
	}
	t, ok := v.(T)
	if !ok {
		return zero, fmt.Errorf("%w: step %q returned %T, want %T", ErrStepResultType, name, v, zero)
	}
	return t, nil
}

// MultiError is what a failed [Multi] returns: which step failed,
// where in the order it sat, why, and what had already succeeded.
//
// Its field names deliberately match the pg package's SagaError, since
// the two answer the same question about different machinery.
//
// Reach it with errors.As; errors.Is sees through to the step's own
// error.
type MultiError struct {
	// FailedStep is the name given to the step in Run or Step.
	FailedStep string
	// FailedStepIdx is its 0-based position in execution order.
	FailedStepIdx int
	// Cause is the error the step returned.
	Cause error
	// Completed holds the results of the steps before this one. The
	// transaction has been rolled back, so these describe how far
	// the script got, not the state of the database.
	Completed Results
}

// Error implements error.
func (e *MultiError) Error() string {
	return fmt.Sprintf("drops: multi step %d (%s) failed after %d completed step(s): %v",
		e.FailedStepIdx, e.FailedStep, e.Completed.Len(), e.Cause)
}

// Unwrap exposes the failing step's error to errors.Is / errors.As.
func (e *MultiError) Unwrap() error { return e.Cause }

// IsMultiError reports whether err is a *MultiError, mirroring
// IsSagaError in the pg package.
func IsMultiError(err error) bool {
	var me *MultiError
	return errors.As(err, &me)
}
