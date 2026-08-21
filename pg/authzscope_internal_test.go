package pg

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"sort"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
)

// ----------------------------------------------------------------------
// The third invariant, third check
//
//	No expression list the renderer reads may be invisible to the
//	resolver.
//
// Round 4 said an operand a caller can hand a *SelectBuilder to must not
// be an opaque closure. Round 5 enforced it with a source census of the
// package's exported constructors. The round before this one found two
// leaks that obeyed both and leaked anyway — one on a builder FIELD, one
// on a filter's RETURN value — and answered them with three checks: every
// expression-bearing field the renderer reads must be read by the
// resolver, every builder method that takes an operand must be exercised
// with a scoped subquery, and the constructor census must be run again
// under a wider reading of what an operand is.
//
// A member of the same family survived all of them.
// MembershipGuard.Predicate hand-wrote its junction subquery inside a
// drops.ExprFunc. It is not a constructor, so no census saw it; it is not
// a builder field, so the field check did not either; and it was reached
// through a table's filter list, which the previous round had just taught
// the resolver to walk — so the walk arrived at the guard's predicate,
// found a closure, and stopped. The subquery therefore carried none of
// the junction table's own DefaultFilters and none of its ContextFilters:
// a soft-deleted or wrong-tenant membership row still authorised, on a
// guard, which is the one place in the package where a widened read is a
// widened permission.
//
// What the three leaks have in common is the shape this check names:
//
//	A predicate the automatic-filter pipeline produces must be a node
//	the resolver can enter.
//
// "Can enter" is not a matter of opinion — it is resolveExpr's type
// switch, and mayHoldStatements is that switch asked in advance. So the
// assertion below is made against the resolver's own definition rather
// than against a list of approved types, and a node type the resolver
// learns tomorrow is admitted here on the same day.
//
// The two halves work the way round 5 established. The census is read out
// of the package source, so a producer added next year is in it whether
// or not anybody remembers this file; the table is checked in both
// directions, so a stale entry fails as loudly as a missing one; and the
// behavioural half — what MembershipGuard actually renders now, and what
// it refuses — lives in authzscope_test.go against the exported API.

// authzCensusRow is the row type the census entity is declared over. Its
// columns are the four a guard fixture needs: an identity, a group to be
// a member of, an owner to be, and a tenant axis to be scoped by.
type authzCensusRow struct {
	ID       int64 `drop:"id,primaryKey,autoIncrement"`
	OrgID    int64 `drop:"organizationId,notNull"`
	OwnerID  int64 `drop:"ownerId,notNull"`
	TenantID int64 `drop:"tenantId,notNull"`
}

// authzCensusFixture is the schema every census entry is built against:
// a guarded table with an entity over it, and a junction table to prove
// membership in.
type authzCensusFixture struct {
	res      *Table
	junction *Table
	entity   *Entity[authzCensusRow]
}

func newAuthzCensusFixture(t *testing.T) *authzCensusFixture {
	t.Helper()
	res := AutoTable[authzCensusRow]("azc_res")

	junction := NewTable("azc_members")
	Add(junction, BigSerial("id").PrimaryKey())
	Add(junction, BigInt("userId").NotNull())
	Add(junction, BigInt("organizationId").NotNull())
	Add(junction, BigInt("tenantId").NotNull())

	ent := NewEntity[authzCensusRow](res).
		ScopeByTenant(res.Col("tenantId")).
		AuthorizeWith(OwnerGuard{Owner: res.Col("ownerId")})
	return &authzCensusFixture{res: res, junction: junction, entity: ent}
}

// azcTenant and azcSubject are the values the census puts on ctx. They
// differ from each other and from every other constant in the package's
// tests so a predicate that read the wrong one cannot pass by accident.
const (
	azcTenant  = int64(61)
	azcSubject = int64(62)
)

func azcCtx() context.Context {
	return WithSubject(WithTenant(context.Background(), azcTenant), azcSubject)
}

// predicateProducerCase is one entry of the census: a producer named
// exactly as the source census names it, and the way to make it answer
// with a predicate rather than with nil.
type predicateProducerCase struct {
	name  string
	build func(f *authzCensusFixture) (drops.Expression, error)
}

// predicateProducers is the census. Every producer the package source
// declares has to be here, and everything here has to be a producer the
// source declares — TestEveryPredicateProducerIsCensused checks both
// directions.
//
// Each entry is built so the producer answers with a real predicate: a
// producer that returns (nil, nil) because it was handed nothing to do
// asserts nothing about the shape of what it returns.
func predicateProducers() []predicateProducerCase {
	return []predicateProducerCase{
		{
			name: "OwnerGuard.Predicate",
			build: func(f *authzCensusFixture) (drops.Expression, error) {
				return OwnerGuard{Owner: f.res.Col("ownerId")}.Predicate(azcCtx())
			},
		},
		{
			name: "MembershipGuard.Predicate",
			build: func(f *authzCensusFixture) (drops.Expression, error) {
				return MembershipGuard{
					Junction:         f.junction,
					JunctionSubject:  f.junction.Col("userId"),
					JunctionResource: f.junction.Col("organizationId"),
					ResourceOwner:    f.res.Col("organizationId"),
				}.Predicate(azcCtx())
			},
		},
		{
			// CustomGuard hands back whatever the caller's function
			// answered with, so what this entry pins is that the wrapper
			// adds no wrapper of its own: a Predicate that re-opaqued the
			// caller's tree would undo the walk for every policy anybody
			// writes by hand.
			name: "CustomGuard.Predicate",
			build: func(f *authzCensusFixture) (drops.Expression, error) {
				g := CustomGuard(func(ctx context.Context) (drops.Expression, error) {
					return Eq(f.res.Col("ownerId"), azcSubject), nil
				})
				return g.Predicate(azcCtx())
			},
		},
		{
			name: "anyGuard.Predicate",
			build: func(f *authzCensusFixture) (drops.Expression, error) {
				return AnyOf(
					OwnerGuard{Owner: f.res.Col("ownerId")},
					OwnerGuard{Owner: f.res.Col("organizationId")},
				).Predicate(azcCtx())
			},
		},
		{
			name: "allGuard.Predicate",
			build: func(f *authzCensusFixture) (drops.Expression, error) {
				return AllOf(
					OwnerGuard{Owner: f.res.Col("ownerId")},
					OwnerGuard{Owner: f.res.Col("organizationId")},
				).Predicate(azcCtx())
			},
		},
		{
			name: "Entity.guardPredicate",
			build: func(f *authzCensusFixture) (drops.Expression, error) {
				return f.entity.guardPredicate(azcCtx())
			},
		},
		{
			name: "Entity.tenantPredicate",
			build: func(f *authzCensusFixture) (drops.Expression, error) {
				return f.entity.tenantPredicate(azcCtx())
			},
		},
		{
			name: "TenantFilter",
			build: func(f *authzCensusFixture) (drops.Expression, error) {
				return TenantFilter(f.res.Col("tenantId"))(azcCtx())
			},
		},
	}
}

// TestNoAutomaticPredicateIsOpaqueToTheResolver is the assertion the
// census exists for.
//
// The predicate a guard or a context filter answers with is handed to a
// builder and rendered. Since the previous round it is also walked, which
// is what lets a policy say "the rows this subject may see are the ones
// some other scoped table names" and have that other table's own filters
// apply. A closure defeats the walk without failing anything: it renders,
// the statement is sent, and the read inside it carried no scoping at
// all. MembershipGuard was exactly that, and the guard is the one place
// where the widened read is also the widened permission — a revoked or
// soft-deleted membership row still authorised.
//
// The check is made against mayHoldStatements rather than against a list
// of blessed types, because mayHoldStatements is resolveExpr's own type
// switch asked in advance. So this test says precisely "the resolver can
// enter it", and stays true to that meaning when the resolver's switch
// grows.
func TestNoAutomaticPredicateIsOpaqueToTheResolver(t *testing.T) {
	for _, tc := range predicateProducers() {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuthzCensusFixture(t)
			e, err := tc.build(f)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if e == nil {
				t.Fatalf("%s answered with no predicate; the census entry has to make it answer with one", tc.name)
			}
			if !mayHoldStatements([]drops.Expression{e}) {
				t.Errorf("%s answers with %T, which resolveExpr cannot enter: a statement written inside it renders unscoped and is sent",
					tc.name, e)
			}
		})
	}
}

// TestEveryPredicateProducerIsCensused reads the package source and
// requires the census above to name every function the automatic-filter
// pipeline can call for a predicate.
//
// Enumerating them from the source is the whole point. MembershipGuard
// leaked for as long as it did because every enforcement the previous
// rounds built enumerated something else — package-level constructors,
// builder fields, builder methods — and a Guard implementation is none of
// those. What it is, is a function of ctx that answers with a predicate,
// which is exactly what ContextFilterFunc is; so the two are one family
// and the source is asked for all of them at once.
//
// Checked in both directions: a producer the source declares and the
// census does not name is a producer nobody proved reachable, and a
// census entry with no producer behind it is a check that stopped
// checking anything.
func TestEveryPredicateProducerIsCensused(t *testing.T) {
	declared := predicateProducersInSource(t)
	censused := map[string]bool{}
	for _, tc := range predicateProducers() {
		censused[tc.name] = true
	}
	for _, name := range declared {
		if !censused[name] {
			t.Errorf("%s produces a predicate for the automatic-filter pipeline and is not in the census; add it to predicateProducers", name)
		}
	}
	declaredSet := map[string]bool{}
	for _, name := range declared {
		declaredSet[name] = true
	}
	for name := range censused {
		if !declaredSet[name] {
			t.Errorf("the census names %s, which the package source no longer declares as a predicate producer; drop the entry", name)
		}
	}
}

// predicateProducersInSource lists, from the package's own non-test
// sources, every function and method the automatic-filter pipeline can
// call for a predicate.
//
// Two shapes qualify, and they are the same shape twice:
//
//   - a function of exactly (context.Context) answering exactly
//     (drops.Expression, error) — [ContextFilterFunc]'s signature, which
//     is also [Guard.Predicate]'s, because both answer "what may this
//     request see?" and both have their answer handed to a builder;
//   - a function whose result is a [ContextFilterFunc], which is how a
//     filter is written when it needs a parameter — TenantFilter takes a
//     column and closes over it.
//
// A helper that takes a ctx AND something else is not in the family: it
// cannot be registered as a filter and cannot implement Guard, so
// whatever it returns reaches a builder only through one of these.
func predicateProducersInSource(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	var out []string
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok {
					continue
				}
				if !isPredicateShape(fn.Type) && !returnsContextFilterFunc(fn.Type) {
					continue
				}
				name := fn.Name.Name
				if fn.Recv != nil {
					name = receiverTypeName(fn.Recv.List[0].Type) + "." + name
				}
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// isPredicateShape reports whether ft is
// func(context.Context) (drops.Expression, error).
func isPredicateShape(ft *ast.FuncType) bool {
	if ft.Params == nil || len(ft.Params.List) != 1 || len(ft.Params.List[0].Names) > 1 {
		return false
	}
	if !isContextParam(ft.Params.List[0].Type) {
		return false
	}
	res := flattenFields(ft.Results)
	return len(res) == 2 && isDropsExpression(res[0]) && isErrorType(res[1])
}

// returnsContextFilterFunc reports whether ft answers with a
// ContextFilterFunc — the way a filter that needs a parameter is written.
func returnsContextFilterFunc(ft *ast.FuncType) bool {
	res := flattenFields(ft.Results)
	if len(res) != 1 {
		return false
	}
	id, ok := res[0].(*ast.Ident)
	return ok && id.Name == "ContextFilterFunc"
}

// flattenFields expands a field list into one type per value, so that
// "(drops.Expression, error)" and a grouped "(a, b int)" are counted the
// same way.
func flattenFields(fl *ast.FieldList) []ast.Expr {
	if fl == nil {
		return nil
	}
	var out []ast.Expr
	for _, f := range fl.List {
		n := len(f.Names)
		if n == 0 {
			n = 1
		}
		for i := 0; i < n; i++ {
			out = append(out, f.Type)
		}
	}
	return out
}

func isContextParam(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "context" && sel.Sel.Name == "Context"
}

func isDropsExpression(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "drops" && sel.Sel.Name == "Expression"
}

func isErrorType(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "error"
}

// receiverTypeName names a method's receiver type the way the census
// spells it: without the pointer, and without the type parameters, so
// Entity[T] is "Entity".
func receiverTypeName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.StarExpr:
		return receiverTypeName(v.X)
	case *ast.IndexExpr:
		return receiverTypeName(v.X)
	case *ast.IndexListExpr:
		return receiverTypeName(v.X)
	case *ast.Ident:
		return v.Name
	}
	return ""
}
