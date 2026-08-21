package pg_test

import (
	"go/ast"
	"sort"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------
// The fourth invariant, enforced against the package source
//
//	Every type that renders a caller's expression is a type the
//	resolver can enter — and the resolver decides that by what a
//	value can DO, never by what it is called.
//
// The three rounds before this one each enforced the shape that had
// just bitten: an operand must not be an opaque closure (round 4/5), a
// rendered expression list must not be invisible to the resolver
// (round 6). This one is the same lesson one level up, and it is the
// level the two previous checks were blind to.
//
// resolveExpr is where the package decides what it can walk into, and
// it used to decide by NAMING types: an arm for *SelectBuilder, and
// nothing for *UpdateBuilder, *InsertBuilder or *DeleteBuilder. All
// four satisfy drops.Expression, which is what a CTE body and a
// subquery operand are typed as, so the three writes were invisible to
// the resolver: WITH moved AS (DELETE FROM ax_rows RETURNING name)
// rendered with no WHERE clause on a tenant-scoped table, and refused
// nothing on a ctx with no tenant. A cross-tenant write, through the
// exported API.
//
// A list of type names cannot be kept honest by care. It went stale the
// day the second builder was written, it was copied — staler — into
// resolveCTEs and into renderForCtx's deferred-error check, and every
// copy had to be found by hand. So the two tests below say the rule
// mechanically:
//
//   - TestEveryStatementBearingExpressionIsReachableByTheResolver takes
//     every type in the package that renders an expression a caller
//     supplied and requires resolveExpr to be able to enter it;
//   - TestNoResolutionEntryPointNamesAStatementType requires every
//     resolution path to reach those types through resolveExpr rather
//     than through a type assertion of its own, which is what keeps the
//     first test's answer true everywhere.
//
// Both are checked in both directions, so a stale exemption fails as
// loudly as a missing one — the pattern round 5 established.

// resolverExemptions names the expression-rendering types resolveExpr
// deliberately cannot enter, with the reason.
//
// An entry is a promise that resolving the type would be WRONG, not
// that resolving it is inconvenient. There is one.
var resolverExemptions = map[string]string{
	"ddlBody": "a view body outlives the request that created it: CREATE VIEW v AS <select> stores the statement, and resolving it would bake the creating request's tenant predicate into an object every later request reads through — see TestViewBodiesAreTheCallersToScope, which pins that the body renders as written",
}

// TestEveryStatementBearingExpressionIsReachableByTheResolver is the
// check that would have caught the three write builders on the day the
// second one was written.
func TestEveryStatementBearingExpressionIsReachableByTheResolver(t *testing.T) {
	p := loadPgSyntax(t)
	arms := resolverArms(t, p)
	renders := p.typesRenderingCallerExpressions()

	candidates := map[string]bool{}
	for name := range renders {
		if p.methods[name]["WriteSQL"] == nil {
			// Not a drops.Expression: it is reached as part of one, and
			// whatever holds it is the candidate.
			continue
		}
		candidates[name] = true
	}
	if !candidates["SelectBuilder"] || !candidates["opExpr"] {
		t.Fatalf("the candidate walk no longer finds SelectBuilder and opExpr — it has gone stale; found %v",
			sortedKeys(candidates))
	}

	for _, name := range sortedKeys(candidates) {
		if reason := resolverExemptions[name]; reason != "" {
			continue
		}
		if armFor(arms, p.methods[name]) != "" {
			continue
		}
		t.Errorf("%s renders an expression a caller supplied and implements none of resolveExpr's arms (%v): a statement written into it renders unresolved and is sent",
			name, sortedKeys(arms))
	}

	// The other direction: an exemption for a type that no longer
	// renders a caller's expression, no longer exists, or has since
	// become reachable, is a reason that has stopped being true.
	for name, reason := range resolverExemptions {
		if reason == "" {
			t.Errorf("exempt type %q carries no reason — an exemption without one is a leak with a name", name)
		}
		if !candidates[name] {
			t.Errorf("exempt type %q no longer renders a caller's expression — drop the exemption", name)
			continue
		}
		if arm := armFor(arms, p.methods[name]); arm != "" {
			t.Errorf("exempt type %q implements %s and so IS reachable — drop the exemption or the method",
				name, arm)
		}
	}
}

// armFor returns the name of the resolveExpr arm a type with these
// methods satisfies, or "".
func armFor(arms map[string][]string, have map[string]*ast.FuncDecl) string {
	for _, name := range sortedKeys(arms) {
		if hasAllMethods(have, arms[name]) {
			return name
		}
	}
	return ""
}

// resolverArms reads resolveExpr's own type switch and returns, for
// each arm that can hand back a RESOLVED expression, the methods a type
// needs to be dispatched to it.
//
// Reading the switch rather than listing the interfaces is what makes
// this test say "the resolver can enter it" and keep saying it when the
// switch grows. It also enforces the shape: every arm must be an
// interface. An arm naming a struct is the defect this file exists to
// end, and it fails here before it can be missed anywhere else.
//
// An arm whose methods answer with rendered text rather than with an
// expression does not count as reachable, and that is not a detail. The
// ctxStatement arm exists for a statement drops did not build: it asks
// for the statement's ctx form so a refusing filter still refuses, and
// then renders the statement's own WriteSQL, because finished SQL
// numbered from $1 cannot be spliced into the statement around it. That
// is fail-closed, not resolved — a type of ours that only landed there
// would still send its inner statement unscoped whenever nothing
// refused, which is every request that HAS a tenant.
func resolverArms(t *testing.T, p *pgSyntax) map[string][]string {
	t.Helper()
	var fn *ast.FuncDecl
	for _, f := range p.funcs {
		if f.Name.Name == "resolveExpr" {
			fn = f
		}
	}
	if fn == nil {
		t.Fatalf("resolveExpr is no longer a package-level function — this check has gone stale")
	}
	arms := map[string][]string{}
	ast.Inspect(fn, func(n ast.Node) bool {
		cc, ok := n.(*ast.CaseClause)
		if !ok {
			return true
		}
		for _, e := range cc.List {
			if id, ok := e.(*ast.Ident); ok && id.Name == "nil" {
				continue
			}
			name := typeName(e)
			if name == "" {
				t.Errorf("%s: resolveExpr dispatches on an unnamed type", p.fset.Position(e.Pos()))
				continue
			}
			if p.ifaces[name] == nil {
				t.Errorf("%s: resolveExpr names %s, which is not an interface: the resolver must decide what it can enter by what a value can do, or the next builder is invisible to it exactly as the three writes were",
					p.fset.Position(e.Pos()), name)
				continue
			}
			if !p.interfaceAnswersWithExpression(name) {
				continue
			}
			arms[name] = p.interfaceMethods(name)
		}
		return true
	})
	if len(arms) == 0 {
		t.Fatalf("resolveExpr dispatches on nothing that answers with a resolved expression — this check has gone stale")
	}
	return arms
}

// interfaceAnswersWithExpression reports whether some method of the
// interface hands back a drops.Expression — the difference between an
// arm that resolves a statement and an arm that only inspects one.
func (p *pgSyntax) interfaceAnswersWithExpression(name string) bool {
	it := p.ifaces[name]
	if it == nil || it.Methods == nil {
		return false
	}
	for _, m := range it.Methods.List {
		if len(m.Names) == 0 {
			if embedded := typeName(m.Type); embedded != "" && p.interfaceAnswersWithExpression(embedded) {
				return true
			}
			continue
		}
		ft, ok := m.Type.(*ast.FuncType)
		if !ok || ft.Results == nil {
			continue
		}
		for _, r := range ft.Results.List {
			if isExpressionType(elementType(r.Type)) {
				return true
			}
		}
	}
	return false
}

// interfaceMethods returns every method name a local interface
// requires, following the interfaces it embeds.
//
// An embedded interface from another package — drops.Expression — is
// skipped: every candidate implements it by construction, since having
// a WriteSQL method is how the candidate walk finds them.
func (p *pgSyntax) interfaceMethods(name string) []string {
	it := p.ifaces[name]
	if it == nil || it.Methods == nil {
		return nil
	}
	var out []string
	for _, m := range it.Methods.List {
		if len(m.Names) == 0 {
			if embedded := typeName(m.Type); embedded != "" && p.ifaces[embedded] != nil {
				out = append(out, p.interfaceMethods(embedded)...)
			}
			continue
		}
		for _, n := range m.Names {
			out = append(out, n.Name)
		}
	}
	return out
}

// typesRenderingCallerExpressions returns the package types that render
// an expression a caller supplied.
//
// It is the round-6 question — does the render closure read a field
// that can hold a caller's expression? — asked of every type instead of
// the four builders, and with the fixpoint run through render closures
// rather than through raw field types. That distinction is what keeps
// the answer meaningful: a *Column holds a *Table and a *Table holds
// DefaultFilters, so "reachable field of expression type" makes a
// column reference a statement-bearing expression. It is not one.
// Table.WriteSQL writes a relation name and never renders those
// filters — they are resolved at the statement level, by
// resolveTableDefaults and Table.resolveContextFilters — so neither
// Table nor Column renders a caller's expression and neither is a
// candidate.
//
// A render closure is every method that takes a *drops.Builder: that is
// the structural definition of "renders", and it catches
// exprBinding.writeValue, which renders a caller's expression without
// being a drops.Expression itself.
func (p *pgSyntax) typesRenderingCallerExpressions() map[string]bool {
	renders := map[string]bool{}
	renderRead := map[string]map[string]bool{}
	fieldTypes := map[string]map[string]ast.Expr{}
	for name, st := range p.structs {
		var seeds []string
		for m, fn := range p.methods[name] {
			if takesBuilder(fn.Type) {
				seeds = append(seeds, m)
			}
		}
		if len(seeds) == 0 {
			continue
		}
		sort.Strings(seeds)
		renderRead[name] = p.fieldsRead(name, seeds...)
		fieldTypes[name] = map[string]ast.Expr{}
		for _, f := range st.Fields.List {
			for _, n := range f.Names {
				fieldTypes[name][n.Name] = f.Type
			}
		}
	}

	holds := func(e ast.Expr) bool {
		if isExpressionType(elementType(e)) {
			return true
		}
		name := typeName(e)
		if name == "" {
			return false
		}
		if renders[name] {
			return true
		}
		if it := p.ifaces[name]; it != nil {
			want := p.interfaceMethods(name)
			if len(want) == 0 {
				return false
			}
			for impl := range renders {
				if hasAllMethods(p.methods[impl], want) {
					return true
				}
			}
		}
		return false
	}

	for {
		changed := false
		for name, read := range renderRead {
			if renders[name] {
				continue
			}
			for f := range read {
				if holds(fieldTypes[name][f]) {
					renders[name], changed = true, true
					break
				}
			}
		}
		if !changed {
			return renders
		}
	}
}

// takesBuilder reports whether ft takes a *drops.Builder — which is
// what a method that renders looks like from the outside.
func takesBuilder(ft *ast.FuncType) bool {
	if ft.Params == nil {
		return false
	}
	for _, p := range ft.Params.List {
		star, ok := p.Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		sel, ok := star.X.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "drops" && sel.Sel.Name == "Builder" {
			return true
		}
	}
	return false
}

// TestNoResolutionEntryPointNamesAStatementType is what makes the check
// above stay true.
//
// resolveExpr can dispatch on interfaces and still be bypassed: a
// resolution path that type-asserts a builder by name has made a second
// copy of resolveExpr's decision, and a copy is stale from the moment
// it is written. Both of the leaks this round closed were exactly that.
// resolveCTEs asserted *SelectBuilder, so a CTE body wrapped in
// Subquery — walked by resolveExpr since round 4 — went unresolved, and
// a data-modifying body went unresolved with it. renderForCtx asserted
// *SelectBuilder to check the deferred error a failed cursor leaves
// behind, so a corrupt cursor in a CTE body rendered as the false
// predicate AfterCursor fails closed with: matches nothing, reports
// nothing.
//
// So: no function whose job is resolution may name one of the types the
// resolver enters. Asserting an INTERFACE is not the defect and is not
// flagged — that is dispatching on what a value can do, which is the
// whole point.
func TestNoResolutionEntryPointNamesAStatementType(t *testing.T) {
	p := loadPgSyntax(t)
	renders := p.typesRenderingCallerExpressions()

	entryPoints := map[string]*ast.FuncDecl{}
	for _, fn := range p.funcs {
		if isResolutionEntryPoint(fn.Name.Name) {
			entryPoints[fn.Name.Name] = fn
		}
	}
	for recv, ms := range p.methods {
		for name, fn := range ms {
			if isResolutionEntryPoint(name) {
				entryPoints[recv+"."+name] = fn
			}
		}
	}
	for _, want := range []string{"resolveExpr", "resolveExprs", "resolveCTEs", "resolveSets", "renderForCtx", "SelectBuilder.resolveCtx"} {
		if entryPoints[want] == nil {
			t.Fatalf("%s is no longer a resolution entry point — this check has gone stale", want)
		}
	}

	for _, name := range sortedKeys(entryPoints) {
		fn := entryPoints[name]
		ast.Inspect(fn, func(n ast.Node) bool {
			var named []ast.Expr
			switch v := n.(type) {
			case *ast.TypeAssertExpr:
				if v.Type != nil {
					named = append(named, v.Type)
				}
			case *ast.CaseClause:
				named = append(named, v.List...)
			}
			for _, e := range named {
				tn := typeName(e)
				if tn == "" || !renders[tn] || p.ifaces[tn] != nil {
					continue
				}
				t.Errorf("%s: %s type-asserts %s by name — dispatch on what the value can do, the way resolveExpr does; a named type here is a second copy of the resolver's list and stale from today",
					p.fset.Position(e.Pos()), name, tn)
			}
			return true
		})
	}
}

// isResolutionEntryPoint reports whether a function of this name is
// part of resolving a statement for its ctx. The two names outside the
// resolve* family are the executors' way in: renderForCtx is what
// ExecExpr resolves through, and ToSQLCtx is what every builder
// resolves through.
func isResolutionEntryPoint(name string) bool {
	return strings.HasPrefix(name, "resolve") || name == "renderForCtx" || name == "ToSQLCtx"
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
