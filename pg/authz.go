package pg

import (
	"context"
	"errors"

	"github.com/bernardoforcillo/drops"
)

// Relationship-based authorization wired into Entity. A Guard
// returns a WHERE predicate that drops AND-s into every Get /
// Query / Update / Delete on the guarded entity. The subject
// (the "who is asking") lives on ctx via WithSubject; missing
// subject errors out, so the bad code path fails closed.
//
//	var Invoices = pg.NewAutoEntity[Invoice]("invoices").
//	    AuthorizeWith(pg.AnyOf(
//	        pg.OwnerGuard{Owner: InvoicesTable.Col("createdBy")},
//	        pg.MembershipGuard{
//	            Junction:         OrgMembersTable,
//	            JunctionSubject:  OrgMembersTable.Col("userId"),
//	            JunctionResource: OrgMembersTable.Col("organizationId"),
//	            ResourceOwner:    InvoicesTable.Col("organizationId"),
//	        },
//	    ))
//
//	ctx = pg.WithSubject(ctx, currentUserID)
//	inv, err := Invoices.Get(db, ctx, invoiceID)
//	// SELECT ... WHERE id = $1 AND (
//	//     "invoices"."createdBy" = $2
//	//     OR "invoices"."organizationId" IN (
//	//         SELECT "organizationId" FROM "org_members"
//	//         WHERE "userId" = $2
//	//     )
//	// )
//
// Composition primitives AnyOf / AllOf let policy authors model
// "owner OR org member", "tenant AND role", etc. without
// recoding the SQL fan-out.

// Guard is the interface drops calls to materialise an
// authorization predicate. Implementations are stateless — every
// query rebuilds the expression so changes to the subject (e.g.
// admin impersonating a user) take effect immediately.
type Guard interface {
	// Predicate returns the SQL expression to AND into the
	// guarded query. ctx carries the subject (set via
	// WithSubject); the guard is free to inspect any other ctx
	// values (e.g. role, tenant) to compose richer rules.
	Predicate(ctx context.Context) (drops.Expression, error)
}

// ----------------------------------------------------------------------
// Subject context
// ----------------------------------------------------------------------

type subjectCtxKey int

const subjectKey subjectCtxKey = 1

// WithSubject annotates ctx with the acting subject — typically
// the user id of the caller. Distinct from WithActor (which
// records "who did this" for audit): an admin impersonating a
// user has the user as subject (so authz checks apply to the
// user) but the admin as actor (so the audit trail captures the
// real actor).
func WithSubject(ctx context.Context, subject any) context.Context {
	return context.WithValue(ctx, subjectKey, subject)
}

// SubjectFrom returns the subject on ctx and ok=true when set.
func SubjectFrom(ctx context.Context) (any, bool) {
	v := ctx.Value(subjectKey)
	return v, v != nil
}

// ErrSubjectMissing is returned when a guarded entity is
// invoked on a ctx without WithSubject.
var ErrSubjectMissing = errors.New("drops/pg: entity is guarded but ctx has no subject")

// ErrUnauthorized signals that an operation was rejected by the
// guard pipeline. Compared to a "no rows" outcome, this means
// the guard explicitly denied the request (e.g. a custom guard
// returned the error directly).
var ErrUnauthorized = errors.New("drops/pg: unauthorized")

// ----------------------------------------------------------------------
// Built-in guards
// ----------------------------------------------------------------------

// OwnerGuard authorises when the subject matches an ownership
// column on the resource — the simplest possible rule.
//
//	OwnerGuard{Owner: PostsTable.Col("authorId")}
//	// emits:  WHERE "posts"."authorId" = $subject
type OwnerGuard struct {
	Owner *Column
}

// Predicate implements Guard.
func (g OwnerGuard) Predicate(ctx context.Context) (drops.Expression, error) {
	if g.Owner == nil {
		return nil, errors.New("drops/pg: OwnerGuard.Owner is nil")
	}
	s, ok := SubjectFrom(ctx)
	if !ok {
		return nil, ErrSubjectMissing
	}
	return Eq(g.Owner, s), nil
}

// MembershipGuard authorises when the subject is a member of the
// resource's containing group, expressed as a junction table.
// Implements the M-N relationship pattern: "user can access
// invoice if user is in invoice's organization".
//
//	MembershipGuard{
//	    Junction:         OrgMembersTable,
//	    JunctionSubject:  OrgMembersTable.Col("userId"),
//	    JunctionResource: OrgMembersTable.Col("organizationId"),
//	    ResourceOwner:    InvoicesTable.Col("organizationId"),
//	}
//	// emits:  WHERE "invoices"."organizationId" IN (
//	//             SELECT "organizationId" FROM "org_members"
//	//             WHERE "userId" = $subject
//	//         )
type MembershipGuard struct {
	// Junction is the table that proves membership (e.g.
	// organization_members, project_collaborators).
	Junction *Table
	// JunctionSubject is the column of Junction holding the
	// subject identifier (the "who").
	JunctionSubject *Column
	// JunctionResource is the column of Junction pointing at the
	// resource's containing group (the "what").
	JunctionResource *Column
	// ResourceOwner is the column on the GUARDED table that
	// matches JunctionResource — e.g. invoices.organizationId
	// when invoices belong to an organization.
	ResourceOwner *Column
}

// Predicate implements Guard.
func (g MembershipGuard) Predicate(ctx context.Context) (drops.Expression, error) {
	if g.Junction == nil || g.JunctionSubject == nil || g.JunctionResource == nil || g.ResourceOwner == nil {
		return nil, errors.New("drops/pg: MembershipGuard is missing one of Junction / JunctionSubject / JunctionResource / ResourceOwner")
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

// CustomGuard wraps a function so application code can compose
// arbitrary authorization rules without implementing the
// interface explicitly.
type CustomGuard func(ctx context.Context) (drops.Expression, error)

// Predicate implements Guard.
func (g CustomGuard) Predicate(ctx context.Context) (drops.Expression, error) { return g(ctx) }

// ----------------------------------------------------------------------
// Composition
// ----------------------------------------------------------------------

// AnyOf returns a Guard that authorises when any of guards does
// (OR composition). Empty disjunction errors at Predicate time.
func AnyOf(guards ...Guard) Guard { return anyGuard{guards: guards} }

type anyGuard struct{ guards []Guard }

func (g anyGuard) Predicate(ctx context.Context) (drops.Expression, error) {
	if len(g.guards) == 0 {
		return nil, errors.New("drops/pg: AnyOf with no guards")
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

// AllOf returns a Guard that authorises only when every guard
// does (AND composition). Empty conjunction errors at Predicate
// time.
func AllOf(guards ...Guard) Guard { return allGuard{guards: guards} }

type allGuard struct{ guards []Guard }

func (g allGuard) Predicate(ctx context.Context) (drops.Expression, error) {
	if len(g.guards) == 0 {
		return nil, errors.New("drops/pg: AllOf with no guards")
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

// ----------------------------------------------------------------------
// Entity wiring
// ----------------------------------------------------------------------

// AuthorizeWith installs g on the entity. Subsequent Get /
// Query / Update / Delete AND the guard's predicate into the
// WHERE clause; ctx-less subject failures surface as
// ErrSubjectMissing. Pass nil to clear an existing guard.
func (e *Entity[T]) AuthorizeWith(g Guard) *Entity[T] {
	e.guard = g
	return e
}

// guardPredicate is the shared helper Entity methods call to
// resolve the active guard's predicate. Returns (nil, nil) when
// no guard is installed.
func (e *Entity[T]) guardPredicate(ctx context.Context) (drops.Expression, error) {
	if e.guard == nil {
		return nil, nil
	}
	return e.guard.Predicate(ctx)
}
