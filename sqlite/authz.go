package sqlite

import (
	"context"
	"errors"

	"github.com/bernardoforcillo/drops"
)

// Relationship-based authorization wired into Entity. A Guard returns a
// WHERE predicate AND-ed into every Get / Query / Update / Delete on the
// guarded entity. The subject lives on ctx via WithSubject; a missing
// subject errors out so the bad code path fails closed.
//
//	Invoices := sqlite.NewAutoEntity[Invoice]("invoices").
//	    AuthorizeWith(sqlite.OwnerGuard{Owner: invoicesTable.Col("createdBy")})
//	ctx = sqlite.WithSubject(ctx, currentUserID)
//	inv, err := Invoices.Get(db, ctx, invoiceID)

// Guard materialises an authorization predicate. Implementations are
// stateless — every query rebuilds the expression.
type Guard interface {
	Predicate(ctx context.Context) (drops.Expression, error)
}

type subjectCtxKey int

const subjectKey subjectCtxKey = 1

// WithSubject annotates ctx with the acting subject (typically the
// caller's user id). Distinct from WithActor, which records who
// performed the mutation for audit.
func WithSubject(ctx context.Context, subject any) context.Context {
	return context.WithValue(ctx, subjectKey, subject)
}

// SubjectFrom returns the subject on ctx and ok=true when set.
func SubjectFrom(ctx context.Context) (any, bool) {
	v := ctx.Value(subjectKey)
	return v, v != nil
}

// ErrSubjectMissing is returned when a guarded entity is used with a ctx
// lacking WithSubject.
var ErrSubjectMissing = errors.New("drops/sqlite: entity is guarded but ctx has no subject")

// ErrUnauthorized signals a guard explicitly denied the request.
var ErrUnauthorized = errors.New("drops/sqlite: unauthorized")

// OwnerGuard authorises when the subject matches an ownership column.
type OwnerGuard struct {
	Owner *Column
}

// Predicate implements Guard.
func (g OwnerGuard) Predicate(ctx context.Context) (drops.Expression, error) {
	if g.Owner == nil {
		return nil, errors.New("drops/sqlite: OwnerGuard.Owner is nil")
	}
	s, ok := SubjectFrom(ctx)
	if !ok {
		return nil, ErrSubjectMissing
	}
	return Eq(g.Owner, s), nil
}

// MembershipGuard authorises when the subject is a member of the
// resource's containing group, expressed via a junction table.
type MembershipGuard struct {
	Junction         *Table
	JunctionSubject  *Column
	JunctionResource *Column
	ResourceOwner    *Column
}

// Predicate implements Guard.
func (g MembershipGuard) Predicate(ctx context.Context) (drops.Expression, error) {
	if g.Junction == nil || g.JunctionSubject == nil || g.JunctionResource == nil || g.ResourceOwner == nil {
		return nil, errors.New("drops/sqlite: MembershipGuard is missing one of Junction / JunctionSubject / JunctionResource / ResourceOwner")
	}
	s, ok := SubjectFrom(ctx)
	if !ok {
		return nil, ErrSubjectMissing
	}
	return drops.ExprFunc(func(b *drops.Builder) {
		g.ResourceOwner.WriteSQL(b)
		b.WriteString(" IN (SELECT ")
		b.WriteIdent(g.JunctionResource.Name())
		b.WriteString(" FROM ")
		g.Junction.writeName(b)
		b.WriteString(" WHERE ")
		b.WriteIdent(g.JunctionSubject.Name())
		b.WriteString(" = ")
		b.AddArg(s)
		b.WriteByte(')')
	}), nil
}

// CustomGuard wraps a function as a Guard.
type CustomGuard func(ctx context.Context) (drops.Expression, error)

// Predicate implements Guard.
func (g CustomGuard) Predicate(ctx context.Context) (drops.Expression, error) { return g(ctx) }

// AnyOf authorises when any guard does (OR composition).
func AnyOf(guards ...Guard) Guard { return anyGuard{guards: guards} }

type anyGuard struct{ guards []Guard }

func (g anyGuard) Predicate(ctx context.Context) (drops.Expression, error) {
	if len(g.guards) == 0 {
		return nil, errors.New("drops/sqlite: AnyOf with no guards")
	}
	exprs := make([]drops.Expression, 0, len(g.guards))
	for _, sub := range g.guards {
		e, err := sub.Predicate(ctx)
		if err != nil {
			return nil, err
		}
		exprs = append(exprs, e)
	}
	if len(exprs) == 1 {
		return exprs[0], nil
	}
	return Or(exprs...), nil
}

// AllOf authorises only when every guard does (AND composition).
func AllOf(guards ...Guard) Guard { return allGuard{guards: guards} }

type allGuard struct{ guards []Guard }

func (g allGuard) Predicate(ctx context.Context) (drops.Expression, error) {
	if len(g.guards) == 0 {
		return nil, errors.New("drops/sqlite: AllOf with no guards")
	}
	exprs := make([]drops.Expression, 0, len(g.guards))
	for _, sub := range g.guards {
		e, err := sub.Predicate(ctx)
		if err != nil {
			return nil, err
		}
		exprs = append(exprs, e)
	}
	if len(exprs) == 1 {
		return exprs[0], nil
	}
	return And(exprs...), nil
}

// AuthorizeWith installs g on the entity. Pass nil to clear.
func (e *Entity[T]) AuthorizeWith(g Guard) *Entity[T] {
	e.guard = g
	return e
}

// guardPredicate resolves the active guard's predicate, or (nil, nil)
// when no guard is installed.
func (e *Entity[T]) guardPredicate(ctx context.Context) (drops.Expression, error) {
	if e.guard == nil {
		return nil, nil
	}
	return e.guard.Predicate(ctx)
}
