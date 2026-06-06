package pg

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Budget caps the cost of each Entity operation so a single typo or
// runaway query can't take down the database. Configure one via
// (*Entity[T]).WithBudget. Zero-valued fields disable that
// individual limit.
//
//	UserEntity.WithBudget(pg.Budget{
//	    MaxArgs:     1000,            // catch huge IN clauses early
//	    MaxRows:     10_000,          // bound result-set size
//	    MaxDuration: 250 * time.Millisecond,
//	})
//
// MaxArgs is checked after SQL rendering; MaxRows is enforced by
// auto-injecting a LIMIT clause on EntityQuery.All when the user
// hasn't already supplied a tighter one; MaxDuration wraps the
// caller's context with a deadline.
//
// Get / One / Update / Delete operate on a single row and so
// bypass MaxRows (they always cap at one). Stream and Page are
// explicitly paginated and ignore MaxRows. Bulk writes
// (CreateMany / UpsertMany) are honoured by MaxArgs and
// MaxDuration but not MaxRows (which is a SELECT concept).
type Budget struct {
	MaxArgs     int
	MaxRows     int
	MaxDuration time.Duration
}

// ErrBudgetExceeded is returned when the request would breach the
// entity's Budget. MaxDuration produces context.DeadlineExceeded
// directly, which wraps to ErrBudgetExceeded via errors.Is so a
// single check covers every budget mode.
var ErrBudgetExceeded = errors.New("drops/pg: query budget exceeded")

// WithBudget attaches the supplied limits to the entity. Returns the
// entity for chaining at declaration time.
func (e *Entity[T]) WithBudget(b Budget) *Entity[T] {
	e.budget = b
	return e
}

// budgetCtx wraps ctx with the budget's MaxDuration timeout when
// configured, returning a cancel func the caller must invoke.
// When MaxDuration is zero the original context is returned unchanged
// and the cancel func is a no-op.
func (e *Entity[T]) budgetCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if e.budget.MaxDuration <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, e.budget.MaxDuration)
}

// checkArgs enforces MaxArgs against an already-rendered statement.
// Returns ErrBudgetExceeded when the budget is set and the count
// exceeds it.
func (e *Entity[T]) checkArgs(args []any) error {
	if e.budget.MaxArgs <= 0 {
		return nil
	}
	if len(args) > e.budget.MaxArgs {
		return fmt.Errorf("%w: %d args > MaxArgs %d", ErrBudgetExceeded,
			len(args), e.budget.MaxArgs)
	}
	return nil
}

// applyBudgetLimit installs an upper-bound LIMIT on sel matching
// max rows. The user's own Limit wins when tighter — the budget is
// a ceiling, not an override.
func applyBudgetLimit(sel *SelectBuilder, maxRows int) {
	if maxRows <= 0 {
		return
	}
	sel.applyLimitCap(int64(maxRows))
}
