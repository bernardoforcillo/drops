package pg

import (
	"context"
	"errors"
	"reflect"

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
//	//     ("invoices"."createdBy" = $2)
//	//     OR ("invoices"."organizationId" IN (
//	//         SELECT "org_members"."organizationId" FROM "org_members"
//	//         WHERE ("org_members"."userId" = $2)
//	//     ))
//	// )
//
// Composition primitives AnyOf / AllOf let policy authors model
// "owner OR org member", "tenant AND role", etc. without
// recoding the SQL fan-out.
//
// A guard's predicate is a statement's worth of SQL and is treated as
// one: every operand it holds is walked before the statement is
// rendered, so a subquery inside a guard carries the scoping of the
// table it reads. That is why the membership subquery above is built as
// a [SelectBuilder] and not as text — the junction table's own
// DefaultFilters and ContextFilters have to apply to it, or a revoked
// membership row goes on authorising. See [MembershipGuard.Predicate].

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
//	// emits:  WHERE ("invoices"."organizationId" IN (
//	//             SELECT "org_members"."organizationId" FROM "org_members"
//	//             WHERE ("org_members"."userId" = $subject)
//	//         ))
//
// The subquery is a statement drops composed, so the junction table's
// own automatic predicates are resolved into it: a soft-delete filter
// on the membership table means a revoked membership stops authorising,
// and a tenant filter on it means one customer's membership rows cannot
// authorise another customer's. Junction is therefore the table handle
// and not a name — the filters live on the handle.
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
//
// The membership check is composed as the statement it is — a
// *SelectBuilder over the junction table — rather than written out as
// SQL text, and that is the whole of what keeps it honest.
//
// It used to be a drops.ExprFunc that wrote "<owner> IN (SELECT <res>
// FROM <junction> WHERE <subj> = $1)" by hand, naming the junction
// table as a string. A membership table is precisely the kind that
// carries automatic predicates of its own — a tenant column on a shared
// schema, a soft-delete column so a revoked membership is kept for
// audit rather than deleted — and none of them reached a subquery drops
// had never been told was a subquery. So a soft-deleted membership row
// still authorised, and a membership row belonging to another tenant
// authorised as well. Everywhere else in the package a widened read
// returns rows the caller should not see; on a guard it also grants a
// permission the subject does not have.
//
// Being a closure made it unreachable from the outside as well. A
// table's filters now have the predicates they answer with walked for
// the ctx being resolved — see [Table.resolveContextFilters] — so a
// policy shaped as "the rows this subject may see are the ones some
// other scoped table names" gets that other table scoped. The walk
// arrived here and stopped at the drops.ExprFunc, which made this guard
// the one predicate in the package that the fix for exactly this
// failure mode could not reach. Held as a statement, it is walked like
// any other: the junction table's DefaultFilters and ContextFilters are
// resolved into the subquery, and a junction table whose axis cannot be
// resolved refuses the whole statement instead of authorising without
// it. TestNoAutomaticPredicateIsOpaqueToTheResolver enforces the shape
// against the package source so the next predicate producer written as
// a closure fails on the day it is written.
//
// The builder is composed directly rather than through [DB.Select]
// because a guard is asked for a predicate, not for a result set, and
// has no *DB to ask. A SelectBuilder needs one only to execute; as an
// operand it renders and resolves without one.
//
// The subject is read before any of that, so a ctx with no subject
// still fails closed with [ErrSubjectMissing] and builds nothing.
func (g MembershipGuard) Predicate(ctx context.Context) (drops.Expression, error) {
	if g.Junction == nil || g.JunctionSubject == nil || g.JunctionResource == nil || g.ResourceOwner == nil {
		return nil, errors.New("drops/pg: MembershipGuard is missing one of Junction / JunctionSubject / JunctionResource / ResourceOwner")
	}
	s, ok := SubjectFrom(ctx)
	if !ok {
		return nil, ErrSubjectMissing
	}
	memberships := (&SelectBuilder{columns: []drops.Expression{g.JunctionResource}}).
		From(g.Junction).
		Where(Eq(g.JunctionSubject, s))
	return In(g.ResourceOwner, memberships), nil
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
//
// Like the tenant axis, the guard is delivered as a
// [Table.ContextFilter] on the entity's table rather than injected by
// each Entity method, and here the mismatch is worth naming: a guard is
// declared per Entity while a context filter lives on a Table. It is
// resolved in favour of the table, because the row visibility a guard
// describes is a property of the rows — an eager-loaded relation
// reaches those same rows with no Entity anywhere in the call, and a
// guard that stopped at the entity's own queries was a straight
// row-visibility bypass for anybody who loaded the table through a
// relation instead.
//
// What follows from that is worth planning for: the guard now applies
// to every statement against the table, including a raw
// db.Select().From(t) and every relation edge that lands on it, and a
// ctx with no subject makes those fail with ErrSubjectMissing instead
// of returning rows. An entity that must see everything asks with
// Unscoped().
//
// # Two entities over one table
//
// A guard is registered under a key built from the entity's row type
// (see rowScopeFilterKey), and registering under a key that is already
// taken REPLACES what is there. So two entities over one table compose
// or collide depending on their row types, and the rule is worth
// knowing before it decides something:
//
//   - Entity[User] and Entity[UserWrite] over "users" hold two keys, so
//     both guards apply and the rows are visible only to a subject both
//     of them admit.
//   - Two Entity[User] values over "users" — the ordinary shape where a
//     read path and a write path each build their own — hold one key,
//     and the second AuthorizeWith silently replaces the first's guard.
//
// The second is not the conservative outcome, and calling it one would
// be worse than the behaviour: a reader who believed the two guards
// were AND-ed would read a narrowing that is not there. The key cannot
// be made finer without making it too fine — an entity built inside a
// request handler is a shape ScopeByTenant and AuthorizeWith both
// document as supported, and keying on anything per-value would stack
// one guard per request onto a shared table, growing the WHERE clause
// without bound. Two guards that must both hold go into one
// AuthorizeWith with AllOf, which says so at the call site.
func (e *Entity[T]) AuthorizeWith(g Guard) *Entity[T] {
	e.guard = g
	// Registered even for a nil g: the closure reads e.guard when it
	// runs, so clearing a guard clears the predicate, and re-registering
	// under the same key keeps an entity rebuilt per request from
	// stacking one filter per construction.
	e.table.setContextFilter(rowScopeFilterKey(e.rowType, "guard"), e.guardPredicate)
	return e
}

// rowScopeFilterKey names the context filter an entity registers on its
// table for kind ("tenant" or "guard").
//
// The row type is in the key so that two entities over one table with
// different row types each keep their own filter, while the same entity
// declared twice — or rebuilt inside a request handler, which is a
// documented shape — replaces its own rather than stacking a copy per
// call. What the key cannot do is separate two entities of the *same*
// row type: registering replaces, and [Entity.AuthorizeWith] says what
// that costs.
//
// The type is spelled with its full import path rather than through
// reflect.Type.String, which prints the short package name. Two User
// types from two packages both ending in "app" are "app.User" to
// String, so they would share one key and one of the two guards would
// vanish — a row-visibility filter lost to a package-name collision,
// which is not a failure anybody would think to look for. PkgPath plus
// Name costs a string concatenation at declaration time. A type with no
// name — an anonymous struct, which is legal as an entity's row type in
// a test — has no path either, so those fall back to String.
func rowScopeFilterKey(rowType reflect.Type, kind string) string {
	if rowType == nil {
		return kind + ":"
	}
	if rowType.Name() == "" || rowType.PkgPath() == "" {
		return kind + ":" + rowType.String()
	}
	return kind + ":" + rowType.PkgPath() + "." + rowType.Name()
}

// guardPredicate resolves the active guard's predicate. Returns
// (nil, nil) when no guard is installed.
//
// It is registered on the table as a context filter by AuthorizeWith
// and called by the executors; Entity methods no longer inject it
// themselves.
func (e *Entity[T]) guardPredicate(ctx context.Context) (drops.Expression, error) {
	if e.guard == nil {
		return nil, nil
	}
	return e.guard.Predicate(ctx)
}
