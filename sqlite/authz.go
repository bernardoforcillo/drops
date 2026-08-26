package sqlite

import (
	"context"
	"errors"
	"reflect"

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
//
//	MembershipGuard{
//	    Junction:         orgMembers,
//	    JunctionSubject:  orgMembers.Col("userId"),
//	    JunctionResource: orgMembers.Col("organizationId"),
//	    ResourceOwner:    invoices.Col("organizationId"),
//	}
//	// emits:  WHERE ("invoices"."organizationId" IN (
//	//             SELECT "org_members"."organizationId" FROM "org_members"
//	//             WHERE ("org_members"."deletedAt" IS NULL)
//	//               AND ("org_members"."userId" = ?)
//	//         ))
//
// Junction is the table handle and not a name, because the junction's
// own DefaultFilters have to apply to the membership check — see
// [MembershipGuard.Predicate].
type MembershipGuard struct {
	// Junction is the table that proves membership (e.g.
	// organization_members, project_collaborators).
	Junction *Table
	// JunctionSubject is the column of Junction holding the subject
	// identifier (the "who").
	JunctionSubject *Column
	// JunctionResource is the column of Junction pointing at the
	// resource's containing group (the "what").
	JunctionResource *Column
	// ResourceOwner is the column on the GUARDED table that matches
	// JunctionResource — e.g. invoices.organizationId when invoices
	// belong to an organization.
	ResourceOwner *Column
}

// Predicate implements Guard.
//
// The membership check is composed as the statement it is — a
// [SelectBuilder] over the junction table — rather than written out as
// SQL text, and that is the whole of what keeps it honest.
//
// It used to be a drops.ExprFunc that wrote "<owner> IN (SELECT <res>
// FROM <junction> WHERE <subj> = ?)" by hand, naming the junction table
// as a string. A membership table is precisely the kind that carries a
// DefaultFilter of its own — a soft-delete column, so a revoked
// membership is kept for audit rather than deleted — and none of it
// reached a subquery drops had never been told was a subquery. So a
// soft-deleted membership row still authorised. Everywhere else in the
// package a widened read returns rows the caller should not see; on a
// guard it also grants a permission the subject does not have.
//
// Held as a statement, the subquery is rendered by [SelectBuilder] like
// any other, so the junction's DefaultFilters land in its WHERE clause
// and a revoked membership stops authorising. The scoping is
// statement-local, which is the property the caller needs: Unscoped on
// the query being guarded widens that query and not the membership
// check inside it.
//
// The builder is composed directly rather than through [DB.Select]
// because a guard is asked for a predicate, not for a result set, and
// has no *DB to ask. A SelectBuilder needs one only to execute; as an
// operand it renders without one.
//
// The subject is read before any of that, so a ctx with no subject
// still fails closed with [ErrSubjectMissing] and builds nothing.
func (g MembershipGuard) Predicate(ctx context.Context) (drops.Expression, error) {
	if g.Junction == nil || g.JunctionSubject == nil || g.JunctionResource == nil || g.ResourceOwner == nil {
		return nil, errors.New("drops/sqlite: MembershipGuard is missing one of Junction / JunctionSubject / JunctionResource / ResourceOwner")
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
//
// The guard is registered as a [Table.ContextFilter], so it is applied
// by whichever executor runs the statement rather than by the Entity
// methods — which is what puts it on an eager-loaded relation edge, on
// a subquery over the guarded table and on a bare db.Select().From(t),
// none of which has an Entity to ask.
//
// One entity registers at most one guard filter, under a key naming its
// row type (see rowScopeFilterKey), and registering under a key that is
// already taken REPLACES what is there. So two entities over one table
// compose or collide depending on their row types, and the rule is
// worth knowing before it decides something:
//
//   - Entity[Invoice] and Entity[InvoiceWrite] over "invoices" hold two
//     keys, so both guards apply and the rows are visible only to a
//     subject both of them admit.
//   - Two Entity[Invoice] values over "invoices" — the ordinary shape
//     where a read path and a write path each build their own — hold
//     one key, and the second AuthorizeWith silently replaces the
//     first's guard.
//
// The second is not the conservative outcome, and calling it one would
// be worse than the behaviour: a reader who believed the two guards
// were AND-ed would read a narrowing that is not there. The key cannot
// be made finer without making it too fine — an entity built inside a
// request handler is a shape this method documents as supported, and
// keying on anything per-value would stack one guard per request onto a
// shared table, growing the WHERE clause without bound. Two guards that
// must both hold go into one AuthorizeWith with AllOf, which says so at
// the call site.
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
// reflect.Type.String, which prints the short package name. Two Invoice
// types from two packages both ending in "billing" are
// "billing.Invoice" to String, so they would share one key and one of
// the two guards would vanish — a row-visibility filter lost to a
// package-name collision, which is not a failure anybody would think to
// look for. A type with no name — an anonymous struct, which is legal
// as an entity's row type in a test — has no path either, so those fall
// back to String.
func rowScopeFilterKey(rowType reflect.Type, kind string) string {
	if rowType == nil {
		return kind + ":"
	}
	if rowType.Name() == "" || rowType.PkgPath() == "" {
		return kind + ":" + rowType.String()
	}
	return kind + ":" + rowType.PkgPath() + "." + rowType.Name()
}

// guardPredicate resolves the active guard's predicate, or (nil, nil)
// when no guard is installed.
//
// It is registered on the table as a context filter by AuthorizeWith
// and called by the executors; the Entity methods no longer inject it
// themselves.
func (e *Entity[T]) guardPredicate(ctx context.Context) (drops.Expression, error) {
	if e.guard == nil {
		return nil, nil
	}
	return e.guard.Predicate(ctx)
}
