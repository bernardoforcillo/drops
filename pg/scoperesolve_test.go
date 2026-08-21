package pg_test

import (
	"fmt"
	"go/ast"
	"go/token"
	"sort"
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
//     first test's answer true everywhere;
//   - TestNoPartialCopyOfTheResolversArms is the round-8 addition, and
//     it found a live one. Dispatching on interfaces is not enough if
//     something else asks the same question in advance with a shorter
//     list: a fast path that admitted two of resolveExpr's three arms
//     skipped the walk entirely for the third, which is the arm that
//     keeps a foreign statement fail-closed.
//
// What a resolution path IS, is derived as well. It used to be a name
// prefix — resolve*, plus two names spelled out — so a resolver called
// anything else was invisible to the second check. It is now "has the
// ctx in its hands, or is handed the expression by something that
// does", which is a property of the function rather than of its name.
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
	candidates := p.expressionRenderingStatementTypes()
	if !candidates["SelectBuilder"] || !candidates["opExpr"] {
		t.Fatalf("the candidate walk no longer finds SelectBuilder and opExpr — it has gone stale; found %v",
			sortedKeys(candidates))
	}
	for _, problem := range unreachableRenderingTypeProblems(t, p) {
		t.Error(problem)
	}
}

// expressionRenderingStatementTypes returns the package types that
// render an expression a caller supplied AND are expressions
// themselves, so that a statement written into one is reached — or is
// not — through resolveExpr. A type that renders one without being one
// is reached as part of whatever holds it, and that holder is the
// candidate.
func (p *pgSyntax) expressionRenderingStatementTypes() map[string]bool {
	candidates := map[string]bool{}
	for name := range p.typesRenderingCallerExpressions() {
		if p.methods[name]["WriteSQL"] == nil {
			continue
		}
		candidates[name] = true
	}
	return candidates
}

// unreachableRenderingTypeProblems is the check body, returning what it
// found rather than reporting it, so the same code can be pointed at a
// synthetic package built to slip past it — see
// pg/scopecheckself_test.go.
func unreachableRenderingTypeProblems(t *testing.T, p *pgSyntax) []string {
	t.Helper()
	arms := resolverArms(t, p)
	candidates := p.expressionRenderingStatementTypes()
	var problems []string

	for _, name := range sortedKeys(candidates) {
		if reason := resolverExemptions[name]; reason != "" {
			continue
		}
		if armFor(arms, p.methods[name]) != "" {
			continue
		}
		problems = append(problems, fmt.Sprintf("%s renders an expression a caller supplied and implements none of resolveExpr's arms (%v): a statement written into it renders unresolved and is sent",
			name, sortedKeys(arms)))
	}

	// The other direction: an exemption for a type that no longer
	// renders a caller's expression, no longer exists, or has since
	// become reachable, is a reason that has stopped being true.
	for _, name := range sortedKeys(resolverExemptions) {
		reason := resolverExemptions[name]
		if reason == "" {
			problems = append(problems, fmt.Sprintf("exempt type %q carries no reason — an exemption without one is a leak with a name", name))
		}
		if !candidates[name] {
			problems = append(problems, fmt.Sprintf("exempt type %q no longer renders a caller's expression — drop the exemption", name))
			continue
		}
		if arm := armFor(arms, p.methods[name]); arm != "" {
			problems = append(problems, fmt.Sprintf("exempt type %q implements %s and so IS reachable — drop the exemption or the method",
				name, arm))
		}
	}
	sort.Strings(problems)
	return problems
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
		sites := p.renderSites(name)
		if len(sites) == 0 {
			continue
		}
		fields := map[string]bool{}
		for _, f := range st.Fields.List {
			for _, n := range f.Names {
				fields[n.Name] = true
			}
		}
		renderRead[name] = p.fieldsReadAt(name, fields, sites)
		fieldTypes[name] = map[string]ast.Expr{}
		for _, f := range st.Fields.List {
			for _, n := range f.Names {
				fieldTypes[name][n.Name] = f.Type
			}
		}
	}

	// holds asks whether a field of this type can carry a caller's
	// expression. The interface case goes through interfaceHolds, and
	// that is the closure of a bypass someone built: this used to ask
	// only "which rendering struct implements it?", so a field declared
	//
	//	type attackHolder interface{ drops.Expression }
	//
	// held nothing as far as the check was concerned — the interface
	// declares no methods of ITS OWN, so the implementor search had
	// nothing to search for — and the type holding it was not a
	// candidate for the resolver to be able to enter. An interface that
	// embeds drops.Expression holds exactly what the resolver walks.
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
		return p.interfaceHolds(name, renders)
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

// renderSites returns everywhere a value of this type is rendered: its
// own methods that take a *drops.Builder, and the package-level
// functions that take one along with the value itself.
//
// The second half is the closure of the second bypass. The checks
// looked at methods, so a type whose rendering lived in a free function
//
//	func writeAttack(b *drops.Builder, a *attackNode) { a.inner.WriteSQL(b) }
//
// rendered a caller's expression that no check could see it render.
// Where the rendering lives is an implementation detail; whether the
// resolver can reach what it renders is not.
func (p *pgSyntax) renderSites(name string) []walkSite {
	var sites []walkSite
	for _, m := range sortedKeys(p.methods[name]) {
		if takesBuilder(p.methods[name][m].Type) {
			sites = append(sites, walkSite{fn: p.methods[name][m]})
		}
	}
	for _, fn := range p.funcs {
		if fn.Type.Params == nil || !takesBuilder(fn.Type) {
			continue
		}
		for _, param := range fn.Type.Params.List {
			if typeName(param.Type) != name || len(param.Names) == 0 {
				continue
			}
			sites = append(sites, walkSite{fn: fn, self: param.Names[0].Name})
		}
	}
	return sites
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
	entryPoints := p.ctxReachingFuncs()
	for _, want := range []string{"resolveExpr", "resolveExprs", "resolveCTEs", "resolveSets", "renderForCtx", "SelectBuilder.resolveCtx"} {
		if entryPoints[want] == nil {
			t.Fatalf("%s is no longer a resolution entry point — this check has gone stale", want)
		}
	}

	for _, problem := range namedStatementTypeProblems(p) {
		t.Error(problem)
	}
}

// namedStatementTypeProblems is the check body, returning what it found
// rather than reporting it, so the same code can be pointed at a
// synthetic package that hides the assertion where the old discovery
// could not see it — see pg/scopecheckself_test.go.
func namedStatementTypeProblems(p *pgSyntax) []string {
	renders := p.typesRenderingCallerExpressions()
	entryPoints := p.ctxReachingFuncs()
	var problems []string
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
				problems = append(problems, fmt.Sprintf("%s: %s type-asserts %s by name — dispatch on what the value can do, the way resolveExpr does; a named type here is a second copy of the resolver's list and stale from today",
					p.fset.Position(e.Pos()), name, tn))
			}
			return true
		})
	}
	sort.Strings(problems)
	return problems
}

// TestNoPartialCopyOfTheResolversArms is the third of these checks, and
// it exists because the round-8 audit found one.
//
// resolveExpr dispatches on interfaces, and round 7 made that stick.
// What it could not see is a switch somewhere else that asks the same
// question in advance — a fast path deciding whether walking a list can
// do anything at all is a legitimate thing to want, and
// Table.resolveDefaultFilterExprs has one, because the common default
// filter is a soft-delete guard with no statement in it and building a
// resolution chain for it is pure cost.
//
// A pre-check like that is a COPY of resolveExpr's arm list, and the
// copy was stale: it admitted *SelectBuilder and subqueryResolver and
// knew nothing of the ctxStatement arm, so a default filter holding a
// statement drops did not build was never offered to the resolver, and
// the fail-closed answer that arm exists to give was skipped. It
// rendered, and was sent, on a ctx with no tenant.
//
// So: any type switch in this package that names one of resolveExpr's
// arms has to admit them all. "Admit" is by method set rather than by
// name — a case whose interface demands a subset of an arm's methods
// catches everything that arm catches, which is why naming ctxStatement
// covers ctxResolvable and why a pre-check need not repeat the whole
// switch. A switch that names none of the arms is not a copy and is not
// this check's business.
func TestNoPartialCopyOfTheResolversArms(t *testing.T) {
	p := loadPgSyntax(t)
	for _, problem := range partialArmCopyProblems(t, p) {
		t.Error(problem)
	}
}

// partialArmCopyProblems is the check body, returning what it found
// rather than reporting it — see pg/scopecheckself_test.go, which
// rebuilds the stale copy and requires this to name it.
//
// A decision is matched STRUCTURALLY, in two ways the first version was
// not, and both were walked past by someone rewriting the same
// decision in different syntax:
//
//   - a type switch is not the only way to ask. An if-assertion chain,
//     `if _, ok := e.(subqueryResolver); ok`, is the same decision with
//     no *ast.TypeSwitchStmt anywhere in it, and the check inspected
//     nothing else;
//   - an arm is not the same thing as its name. A case that spells the
//     arm out —
//     `case interface{ resolveSubqueries(context.Context) (drops.Expression, bool, error) }:`
//     — dispatches on exactly what the arm dispatches on, and the check
//     compared identifiers, so a switch full of them named no arm at
//     all and was skipped whole.
//
// So an asserted type is reduced to the METHOD SET it demands, from a
// local interface name or from an inline one alike, and the arms are
// too. Naming an arm is demanding exactly what it demands; admitting
// one is demanding no more than it does.
//
// The last part is what a copy IS, as opposed to a dispatch. A decision
// that names an arm and then hands the value to resolveExpr anyway has
// not skipped anything — renderForCtx asks whether e is a ctxStatement
// and resolves it when it is not, which is a fast path with the walk
// still behind it. A decision whose unmatched path never reaches the
// resolver has answered the question on the resolver's behalf, and that
// answer is the one that goes stale.
func partialArmCopyProblems(t *testing.T, p *pgSyntax) []string {
	t.Helper()
	arms := resolverArmSets(t, p)
	resolvers := p.resolverCalls()
	var problems []string
	for _, key := range sortedKeys(p.allFuncs()) {
		fn := p.allFuncs()[key]
		if fn.Name.Name == "resolveExpr" {
			continue
		}
		for _, d := range p.armDecisions(fn) {
			if !namesAnyArm(d.named, arms) {
				continue
			}
			if p.reachesResolver(fn, d.end, resolvers) {
				continue
			}
			for _, arm := range sortedKeys(arms) {
				if armAdmitted(arms[arm], d.named) {
					continue
				}
				problems = append(problems, fmt.Sprintf(
					"%s: %s decides in advance what resolveExpr can enter and admits nothing that catches its %s arm — a partial copy of the arm list skips the walk for exactly the shape the arm was added for",
					p.fset.Position(d.pos), key, arm))
			}
		}
	}
	sort.Strings(problems)
	return problems
}

// armDecision is one place a function decides what a value is, reduced
// to what each branch DEMANDS of it.
type armDecision struct {
	pos   token.Pos
	end   token.Pos
	named [][]string
}

// armDecisions returns the decisions a function makes: one per type
// switch, plus one for the explicit type assertions it makes outside
// any switch — an if-chain asks in several statements what a switch
// asks in one, and both are the same decision.
func (p *pgSyntax) armDecisions(fn *ast.FuncDecl) []armDecision {
	var out []armDecision
	var chain armDecision
	ast.Inspect(fn, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.TypeSwitchStmt:
			d := armDecision{pos: v.Pos(), end: v.End()}
			for _, stmt := range v.Body.List {
				cc, ok := stmt.(*ast.CaseClause)
				if !ok {
					continue
				}
				for _, e := range cc.List {
					if ms, ok := p.demandedMethods(e); ok {
						d.named = append(d.named, ms)
					}
				}
			}
			if len(d.named) > 0 {
				out = append(out, d)
			}
			return false
		case *ast.TypeAssertExpr:
			// Type is nil for the assertion a type switch is written
			// around; those are handled above.
			if v.Type == nil {
				return true
			}
			ms, ok := p.demandedMethods(v.Type)
			if !ok {
				return true
			}
			if chain.pos == token.NoPos {
				chain.pos = v.Pos()
			}
			if v.End() > chain.end {
				chain.end = v.End()
			}
			chain.named = append(chain.named, ms)
		}
		return true
	})
	if len(chain.named) > 0 {
		out = append(out, chain)
	}
	return out
}

// demandedMethods reduces an asserted type to the method set it
// demands, for a local interface named or written inline. Anything else
// — a concrete type, a type from another package — is not this check's
// business: naming a concrete statement type is what
// TestNoResolutionEntryPointNamesAStatementType refuses.
func (p *pgSyntax) demandedMethods(e ast.Expr) ([]string, bool) {
	if it, ok := e.(*ast.InterfaceType); ok {
		return p.interfaceMethodsOf(it), true
	}
	if tn := typeName(e); tn != "" && p.ifaces[tn] != nil {
		return p.interfaceMethods(tn), true
	}
	return nil, false
}

// resolverCalls returns the package-level functions that hand an
// expression to resolveExpr, directly or through one another. It is the
// derived answer to "does this path still reach the walk", so that
// resolveExprs counts without being named.
func (p *pgSyntax) resolverCalls() map[string]bool {
	out := map[string]bool{"resolveExpr": true}
	for {
		changed := false
		for _, fn := range p.funcs {
			if out[fn.Name.Name] {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok && out[id.Name] {
					out[fn.Name.Name], changed = true, true
				}
				return true
			})
		}
		if !changed {
			return out
		}
	}
}

// reachesResolver reports whether the function hands the value to the
// resolver on a path the decision did not match — approximated by the
// calls that come after the decision ends, which is where an unmatched
// if-assertion and a switch with no default both continue.
func (p *pgSyntax) reachesResolver(fn *ast.FuncDecl, after token.Pos, resolvers map[string]bool) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || call.Pos() < after {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && resolvers[id.Name] {
			found = true
		}
		return true
	})
	return found
}

// resolverArmSets returns every interface resolveExpr's type switch
// dispatches on, as the method set each demands, keyed by name for the
// message.
func resolverArmSets(t *testing.T, p *pgSyntax) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, name := range resolveExprArmNames(t, p) {
		out[name] = p.interfaceMethods(name)
	}
	return out
}

// resolveExprArmNames returns every interface resolveExpr's type switch
// dispatches on, in source order.
func resolveExprArmNames(t *testing.T, p *pgSyntax) []string {
	t.Helper()
	fn := p.allFuncs()["resolveExpr"]
	if fn == nil {
		t.Fatalf("resolveExpr is no longer a package-level function — this check has gone stale")
	}
	var out []string
	ast.Inspect(fn, func(n ast.Node) bool {
		cc, ok := n.(*ast.CaseClause)
		if !ok {
			return true
		}
		for _, e := range cc.List {
			if tn := typeName(e); tn != "" && p.ifaces[tn] != nil {
				out = append(out, tn)
			}
		}
		return true
	})
	if len(out) == 0 {
		t.Fatalf("resolveExpr dispatches on no interface at all — this check has gone stale")
	}
	return out
}

// namesAnyArm reports whether a decision demands exactly what one of
// resolveExpr's arms demands, which is what makes it a copy of the
// decision rather than an unrelated switch. Compared as method sets, so
// the arm written out inline is the arm.
func namesAnyArm(named [][]string, arms map[string][]string) bool {
	for _, ms := range named {
		for _, arm := range arms {
			if sameMethodSet(ms, arm) {
				return true
			}
		}
	}
	return false
}

// armAdmitted reports whether one of the demanded method sets catches
// everything the arm catches: it demands no more than the arm does, so
// every value dispatched to the arm satisfies it too.
func armAdmitted(arm []string, named [][]string) bool {
	want := map[string]bool{}
	for _, m := range arm {
		want[m] = true
	}
	for _, ms := range named {
		if len(ms) == 0 {
			continue
		}
		covers := true
		for _, m := range ms {
			if !want[m] {
				covers = false
				break
			}
		}
		if covers {
			return true
		}
	}
	return false
}

// sameMethodSet compares two method sets ignoring order and repetition.
func sameMethodSet(a, b []string) bool {
	set := func(ms []string) map[string]bool {
		out := map[string]bool{}
		for _, m := range ms {
			out[m] = true
		}
		return out
	}
	x, y := set(a), set(b)
	if len(x) != len(y) {
		return false
	}
	for m := range x {
		if !y[m] {
			return false
		}
	}
	return true
}

// allFuncs returns every function and method in the package, keyed by
// name — a package-level function by its own, a method by receiver and
// name.
func (p *pgSyntax) allFuncs() map[string]*ast.FuncDecl {
	out := map[string]*ast.FuncDecl{}
	for _, fn := range p.funcs {
		out[fn.Name.Name] = fn
	}
	for recv, ms := range p.methods {
		for name, fn := range ms {
			out[recv+"."+name] = fn
		}
	}
	return out
}

// ctxReachingFuncs returns every function in the package that resolving
// a statement can pass through, keyed by name — a package-level
// function by its own name, a method by receiver and name.
//
// It answers by what a function CAN SEE, and that is the closure of the
// third bypass. Discovery used to be by name: resolve*, plus
// renderForCtx and ToSQLCtx spelled out because they break the pattern.
// So a resolution path called anything else — prepareStatement,
// applyScope, walk — was not a resolution path as far as the check was
// concerned, and could type-assert *SelectBuilder by name with nothing
// to say so. A naming convention is not an invariant; it is a habit,
// and the check was enforcing the habit rather than the rule.
//
// What makes a function part of resolution is that it has the ctx in
// its hands, or that something which does hands it the expression it
// was resolving:
//
//   - a function or method taking a context.Context is a seed. It can
//     resolve, so a type assertion in it is a copy of resolveExpr's
//     decision;
//   - a function it calls with an expression-bearing parameter is one
//     too, transitively — otherwise the assertion moves one call
//     deeper into a helper that takes no ctx and the check goes quiet
//     again, which is the same bypass wearing a different name.
func (p *pgSyntax) ctxReachingFuncs() map[string]*ast.FuncDecl {
	all := map[string]*ast.FuncDecl{}
	byName := map[string][]*ast.FuncDecl{}
	for _, fn := range p.funcs {
		all[fn.Name.Name] = fn
		byName[fn.Name.Name] = append(byName[fn.Name.Name], fn)
	}
	for recv, ms := range p.methods {
		for name, fn := range ms {
			all[recv+"."+name] = fn
			byName[name] = append(byName[name], fn)
		}
	}

	out := map[string]*ast.FuncDecl{}
	var queue []string
	for key, fn := range all {
		if fn.Type.Params == nil {
			continue
		}
		for _, param := range fn.Type.Params.List {
			if isContextType(param.Type) {
				out[key], queue = fn, append(queue, key)
				break
			}
		}
	}
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		ast.Inspect(out[key], func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				name = fun.Name
			case *ast.SelectorExpr:
				name = fun.Sel.Name
			default:
				return true
			}
			for _, fn := range byName[name] {
				if !p.takesExpression(fn) {
					continue
				}
				k := funcKey(fn)
				if out[k] == nil {
					out[k], queue = fn, append(queue, k)
				}
			}
			return true
		})
	}
	return out
}

// takesExpression reports whether a function is handed an expression to
// work on — the shape of a helper a resolver delegates the decision to.
func (p *pgSyntax) takesExpression(fn *ast.FuncDecl) bool {
	if fn.Type.Params == nil {
		return false
	}
	for _, param := range fn.Type.Params.List {
		if p.bearsExpression(param.Type) {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
