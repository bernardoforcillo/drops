package pg_test

import (
	"fmt"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------
// The checks, checked
//
//	A source check is only worth what it refuses. Every one of them is
//	pointed at a package that contains the shape it exists to catch,
//	and required to name it.
//
// The checks that read the package source were each written the day
// after a leak that matched their shape. That history is also their
// weakness: a check written to see one shape is confirmed by a package
// where that shape is absent, which is the same evidence a check with a
// hole in it produces. Every bypass closed here was found by someone
// constructing the bypass — never by reading the checker.
//
// The four below were the first round of that, three constructed and
// one live in this package's own source:
//
//   - an expression held behind a LOCAL INTERFACE that declares no
//     methods of its own: `type attackHolder interface{ drops.Expression }`.
//     The candidate walk asked "which rendering struct implements it?",
//     the interface demanded nothing to implement, and the answer was
//     "none" — so a field of that type held nothing as far as the check
//     was concerned;
//   - rendering moved out of a method into a PACKAGE-LEVEL HELPER. The
//     closure walk followed methods on the same receiver, so
//     `func (a *attackNode) WriteSQL(b *drops.Builder) { writeAttack(b, a) }`
//     read no field at all and rendered, as far as the checks could
//     see, nothing;
//   - a fast path asking resolveExpr's question IN ADVANCE with the arm
//     list one arm out of date, which no check was reading at all — the
//     one bypass of the four that was live in this package's own source
//     rather than constructed;
//   - a resolution entry point NAMED ANYTHING ELSE. Discovery was by
//     the prefix resolve*, plus two names spelled out, so a function
//     called applyScope could type-assert *SelectBuilder by name and no
//     check was looking at it.
//
// The round after it found that the closures themselves still decided
// by NAME, and three more bypasses were open:
//
//   - a parameter or a field that carries a relation, or an operand
//     that carries a statement, spelled as a LOCAL INTERFACE rather
//     than as *Table or `any`. Five checks compared against the
//     identifier "Table" — the holdsTable and holdsDB walks, the two
//     censuses built on them, and both operand predicates — so one line
//     of declaration took a relation out of every one of them. See
//     bearsRelation, and the five spellings below;
//   - resolveExpr's decision copied as an IF-ASSERTION CHAIN. The
//     arm-copy check inspected type switches, and an if names the same
//     arm with no *ast.TypeSwitchStmt in sight;
//   - the same decision with the arm SPELLED OUT as an inline anonymous
//     interface. The check compared identifiers, so a switch full of
//     inline arms named no arm at all and was skipped whole.
//
// Each is rebuilt below as a synthetic file of package pg, parsed
// alongside the real sources, and the check has to name it. Reverted
// against any of the closures, the matching test here fails — which is
// the only evidence that a check refuses anything at all.
//
// Two of them are also checked in the other direction, because a check
// that fires for the shape rather than for the defect is as useless as
// one that never fires: an interface no caller can implement is not an
// operand, and a fast path with the walk still behind it is not a copy.
//
// The same harness answers the other question this round asked: is the
// list of statement builders really derived? It is proved by adding a
// FIFTH builder, and the proof is not that the derivation mentions it
// but that the invariant CATCHES it: a fifth builder whose rendered
// expression list nothing resolves is reported without a line being
// added anywhere.

// The synthetic sources. Each is a valid file of package pg containing
// exactly the shape under discussion and nothing else, so a failure
// names the shape rather than a side effect of the scenery.
const (
	// bypassInterfaceHolder holds a caller's expression behind a local
	// interface that declares nothing of its own.
	bypassInterfaceHolder = `package pg

import "github.com/bernardoforcillo/drops"

type attackHolder interface{ drops.Expression }

type attackNode struct{ held attackHolder }

func (a *attackNode) WriteSQL(b *drops.Builder) { a.held.WriteSQL(b) }
`

	// bypassRenderHelper renders a caller's expression from a
	// package-level function instead of from the method.
	bypassRenderHelper = `package pg

import "github.com/bernardoforcillo/drops"

type helperNode struct{ held drops.Expression }

func (h *helperNode) WriteSQL(b *drops.Builder) { writeHelperNode(b, h) }

func writeHelperNode(b *drops.Builder, h *helperNode) { h.held.WriteSQL(b) }
`

	// bypassIndirectHelper is the same move again, one step further
	// out: the helper does not render at all, it merely hands the field
	// back, and the method appends what it returns.
	bypassIndirectHelper = `package pg

import "github.com/bernardoforcillo/drops"

type indirectNode struct{ held drops.Expression }

func (h *indirectNode) WriteSQL(b *drops.Builder) { b.Append(pickHeld(h)) }

func pickHeld(h *indirectNode) drops.Expression { return h.held }
`

	// bypassRenamedEntryPoint resolves under a name outside the
	// resolve* family, and hides the type assertion one call deeper in
	// a helper that takes no ctx at all.
	bypassRenamedEntryPoint = `package pg

import (
	"context"

	"github.com/bernardoforcillo/drops"
)

func applyScope(ctx context.Context, e drops.Expression) drops.Expression {
	if s, ok := e.(*SelectBuilder); ok {
		return s
	}
	return peek(ctx, e)
}

func peek(ctx context.Context, e drops.Expression) drops.Expression {
	switch v := e.(type) {
	case *UpdateBuilder:
		return v
	}
	return e
}
`

	// bypassHiddenAssertion is the same assertion in a helper with no
	// ctx of its own, reached from one that has it.
	bypassHiddenAssertion = `package pg

import (
	"context"

	"github.com/bernardoforcillo/drops"
)

func prepareStatement(ctx context.Context, e drops.Expression) drops.Expression {
	if isOurSelect(e) {
		return e
	}
	return e
}

func isOurSelect(e drops.Expression) bool {
	_, ok := e.(*SelectBuilder)
	return ok
}
`

	// bypassStaleArmCopy asks resolveExpr's question in advance and
	// gets the arm list one arm out of date — the shape that was live
	// in this package until round 8.
	bypassStaleArmCopy = `package pg

import "github.com/bernardoforcillo/drops"

func worthWalking(list []drops.Expression) bool {
	for _, e := range list {
		switch e.(type) {
		case subqueryResolver:
			return true
		}
	}
	return false
}
`

	// bypassOperandInterface is an exported helper whose statement
	// operand is spelled as a local interface. The operand census
	// matched `any`, the empty interface literal and drops.Expression,
	// so a parameter of this type took a caller's subquery and no
	// census asked whether the subquery was ever resolved.
	bypassOperandInterface = `package pg

import "github.com/bernardoforcillo/drops"

type mergeOperand interface{ drops.Expression }

func MergeMatch(e mergeOperand) drops.Expression { return e }
`

	// bypassOperandInlineInterface is the same operand written inline,
	// which is the same promise with the declaration deleted.
	bypassOperandInlineInterface = `package pg

import "github.com/bernardoforcillo/drops"

func MergeMatchInline(e interface{ drops.Expression }) drops.Expression { return e }
`

	// operandSealedToCallers is the direction the census must NOT
	// widen into. An interface demanding an unexported method cannot be
	// implemented outside this package, so a parameter of that type
	// cannot be handed a caller's statement — it is handed one of this
	// package's own values, and whether THAT can carry a statement is
	// the question the implementor search answers. ColRef is the live
	// one: every helper taking a column reference would otherwise be
	// read as taking a subquery.
	operandSealedToCallers = `package pg

import "github.com/bernardoforcillo/drops"

func MergeOnColumn(c ColRef) drops.Expression { return c }
`

	// bypassArmCopyIfChain is the same stale copy written as an
	// if-assertion instead of a type switch. No *ast.TypeSwitchStmt
	// appears in it, which is all the first version of the check
	// inspected.
	bypassArmCopyIfChain = `package pg

import "github.com/bernardoforcillo/drops"

func worthWalkingChain(list []drops.Expression) bool {
	for _, e := range list {
		if _, ok := e.(subqueryResolver); ok {
			return true
		}
	}
	return false
}
`

	// bypassArmCopyInlineCase asks with the arm SPELLED OUT: an inline
	// anonymous interface demanding exactly what subqueryResolver
	// demands. Compared by identifier it named no arm at all, so the
	// whole switch was skipped.
	bypassArmCopyInlineCase = `package pg

import (
	"context"

	"github.com/bernardoforcillo/drops"
)

func worthWalkingInline(list []drops.Expression) bool {
	for _, e := range list {
		switch e.(type) {
		case interface {
			resolveSubqueries(ctx context.Context) (drops.Expression, bool, error)
		}:
			return true
		}
	}
	return false
}
`

	// bypassArmCopyInlineAssertion is both moves at once: the inline
	// spelling, asserted in an if.
	bypassArmCopyInlineAssertion = `package pg

import (
	"context"

	"github.com/bernardoforcillo/drops"
)

func worthWalkingBoth(list []drops.Expression) bool {
	for _, e := range list {
		if _, ok := e.(interface {
			resolveSubqueries(ctx context.Context) (drops.Expression, bool, error)
		}); ok {
			return true
		}
	}
	return false
}
`

	// armCopyWithTheWalkBehindIt is the shape the check must stay quiet
	// about, and it is renderForCtx's: a fast path that names one arm
	// and hands everything it did not match to the resolver anyway. It
	// has skipped nothing, so it is not a copy — and a check that
	// reported it would be reporting the syntax rather than the defect.
	armCopyWithTheWalkBehindIt = `package pg

import (
	"context"

	"github.com/bernardoforcillo/drops"
)

func renderFast(ctx context.Context, e drops.Expression) (drops.Expression, error) {
	if st, ok := e.(ctxStatement); ok {
		if _, _, err := st.ToSQLCtx(ctx); err != nil {
			return nil, err
		}
		return e, nil
	}
	resolved, _, err := resolveExpr(ctx, e)
	return resolved, err
}
`

	// fifthBuilder is a fifth statement builder: it implements the arm
	// resolveExpr dispatches on, renders a list of caller expressions,
	// and resolves nothing.
	fifthBuilder = `package pg

import (
	"context"

	"github.com/bernardoforcillo/drops"
)

type MergeBuilder struct {
	table *Table
	when  []drops.Expression
}

func (m *MergeBuilder) WriteSQL(b *drops.Builder) {
	b.WriteString("MERGE INTO ")
	m.table.WriteSQL(b)
	b.AppendList(" ", m.when)
}

func (m *MergeBuilder) ToSQLCtx(ctx context.Context) (string, []any, error) {
	return "", nil, nil
}

func (m *MergeBuilder) resolveStatement(ctx context.Context) (drops.Expression, bool, error) {
	return m, false, nil
}
`

	// fifthBuilderResolved is the same builder with the walk its
	// rendered list needs, so the check has to stay quiet about it.
	fifthBuilderResolved = `package pg

import (
	"context"

	"github.com/bernardoforcillo/drops"
)

type MergeBuilder struct {
	table *Table
	when  []drops.Expression
}

func (m *MergeBuilder) WriteSQL(b *drops.Builder) {
	b.WriteString("MERGE INTO ")
	m.table.WriteSQL(b)
	b.AppendList(" ", m.when)
}

func (m *MergeBuilder) ToSQLCtx(ctx context.Context) (string, []any, error) {
	return "", nil, nil
}

func (m *MergeBuilder) resolveStatement(ctx context.Context) (drops.Expression, bool, error) {
	if _, err := m.table.resolveContextFilters(ctx); err != nil {
		return nil, false, err
	}
	when, err := resolveExprs(ctx, m.when)
	if err != nil {
		return nil, false, err
	}
	if when == nil {
		return m, false, nil
	}
	cp := *m
	cp.when = when
	return &cp, true, nil
}
`
)

// TestReachabilityCheckSeesAnExpressionBehindALocalInterface rebuilds
// the first bypass. attackNode renders whatever a caller put in its
// field and implements no arm of resolveExpr, so a statement written
// there renders unresolved and is sent — and the check said nothing,
// because the interface in front of the field demanded no methods and
// so appeared to hold nothing.
func TestReachabilityCheckSeesAnExpressionBehindALocalInterface(t *testing.T) {
	p := loadPgSyntaxWith(t, bypassInterfaceHolder)
	if !p.expressionRenderingStatementTypes()["attackNode"] {
		t.Fatalf("attackNode renders what its interface-typed field holds and the candidate walk does not see it")
	}
	requireProblemNaming(t, unreachableRenderingTypeProblems(t, p), "attackNode")
}

// TestReachabilityCheckSeesRenderingMovedIntoAHelper rebuilds the
// second. Extracting a render helper is an ordinary edit, and it must
// not be able to take a field out of the checks' sight.
func TestReachabilityCheckSeesRenderingMovedIntoAHelper(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		typ  string
	}{
		{"the helper renders", bypassRenderHelper, "helperNode"},
		{"the helper only hands the field back", bypassIndirectHelper, "indirectNode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := loadPgSyntaxWith(t, tc.src)
			if !p.expressionRenderingStatementTypes()[tc.typ] {
				t.Fatalf("%s renders its field through a package-level function and the candidate walk does not see it", tc.typ)
			}
			requireProblemNaming(t, unreachableRenderingTypeProblems(t, p), tc.typ)
		})
	}
}

// TestEntryPointDiscoverySeesAResolverCalledSomethingElse rebuilds the
// third, in both its shapes: a resolution path whose name is not
// resolve*, and the assertion pushed one call deeper into a helper with
// no ctx of its own. What makes a function part of resolution is that
// it has the ctx in its hands, or that something which does hands it
// the expression — never what it is called.
func TestEntryPointDiscoverySeesAResolverCalledSomethingElse(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want []string
	}{
		{"named anything else", bypassRenamedEntryPoint, []string{"applyScope", "peek"}},
		{"hidden behind a ctx-less helper", bypassHiddenAssertion, []string{"isOurSelect"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := loadPgSyntaxWith(t, tc.src)
			problems := namedStatementTypeProblems(p)
			for _, want := range tc.want {
				requireProblemNaming(t, problems, want)
			}
		})
	}
}

// TestOperandDiscoverySeesAnOperandBehindALocalInterface rebuilds the
// operand-census half of the same bypass the relation census had: a
// parameter that admits a statement, spelled as an interface instead of
// as `any`.
func TestOperandDiscoverySeesAnOperandBehindALocalInterface(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		fn   string
	}{
		{"a local interface", bypassOperandInterface, "MergeMatch"},
		{"an inline interface", bypassOperandInlineInterface, "MergeMatchInline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := loadPgSyntaxWith(t, tc.src)
			if !hasEntry(censusOperandConstructorsIn(p), tc.fn) {
				t.Errorf("%s takes a statement and the operand census does not see it", tc.fn)
			}
		})
	}
}

// TestOperandDiscoveryStopsAtAnInterfaceNoCallerCanImplement is the
// other direction of the same widening, and the reason it is safe.
//
// Admitting every interface would put every helper that takes a column
// reference into a census about subqueries, and an exemption list is
// what this file exists to avoid. The distinction is computed: an
// unexported method in the demand means no caller can satisfy it, so
// the only values are this package's own — and *Column, the only
// ColRef, renders a qualified name and holds no statement.
func TestOperandDiscoveryStopsAtAnInterfaceNoCallerCanImplement(t *testing.T) {
	p := loadPgSyntaxWith(t, operandSealedToCallers)
	if hasEntry(censusOperandConstructorsIn(p), "MergeOnColumn") {
		t.Error("a helper taking a column reference is censused as taking a statement — the census admits an interface no caller can implement")
	}
}

// TestArmCopyCheckSeesAFastPathOneArmOutOfDate rebuilds the shape the
// round-8 audit found in this package's own source: a pre-check that
// decides, without building anything, whether the resolver could do
// something — written as a subset of resolveExpr's arms, and therefore
// skipping the walk for whichever arm it does not mention.
func TestArmCopyCheckSeesAFastPathOneArmOutOfDate(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		fn   string
	}{
		{"a type switch naming the arm", bypassStaleArmCopy, "worthWalking"},
		{"an if-assertion chain", bypassArmCopyIfChain, "worthWalkingChain"},
		{"the arm spelled out inline in a case", bypassArmCopyInlineCase, "worthWalkingInline"},
		{"the arm spelled out inline in an assertion", bypassArmCopyInlineAssertion, "worthWalkingBoth"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := loadPgSyntaxWith(t, tc.src)
			requireProblemNaming(t, partialArmCopyProblems(t, p), tc.fn)
		})
	}
}

// TestArmCopyCheckIsQuietWhenTheWalkIsStillBehindTheFastPath is the
// other direction, and it is what keeps the check honest about what a
// COPY is.
//
// Naming an arm is not the defect. Naming an arm and then answering the
// resolver's question yourself is. A fast path that hands everything it
// did not match to resolveExpr has skipped nothing, and this package's
// own renderForCtx is exactly that shape — a check that reported it
// would have to be exempted by name, which is the thing this file
// exists to stop.
func TestArmCopyCheckIsQuietWhenTheWalkIsStillBehindTheFastPath(t *testing.T) {
	p := loadPgSyntaxWith(t, armCopyWithTheWalkBehindIt)
	for _, problem := range partialArmCopyProblems(t, p) {
		if strings.Contains(problem, "renderFast") {
			t.Errorf("the fast path resolves everything it did not match and the check reports it anyway: %s", problem)
		}
	}
}

// TestTheInvariantCoversABuilderNobodyAddedToAList is the derivation
// proof, and it is the reason statementBuilders is a function rather
// than four names in a slice.
//
// A fifth builder is added to a synthetic package: it implements the
// arm resolveExpr dispatches on, renders a list of caller expressions,
// and resolves nothing. Nothing anywhere is edited to mention it, and
// the invariant reports it — which is the claim "derived" is supposed
// to mean. The second case is the same builder with the walk in place,
// and it has to be silent, or the check would be reporting the shape
// rather than the defect.
func TestTheInvariantCoversABuilderNobodyAddedToAList(t *testing.T) {
	leaky := loadPgSyntaxWith(t, fifthBuilder)
	if !hasEntry(builderNames(statementBuilders(t, leaky)), "MergeBuilder") {
		t.Fatalf("a fifth builder implementing resolveExpr's arm was not derived: got %v",
			builderNames(statementBuilders(t, leaky)))
	}
	requireProblemNaming(t, invisibleExpressionListProblems(t, leaky), "MergeBuilder.when")

	fixed := loadPgSyntaxWith(t, fifthBuilderResolved)
	for _, problem := range invisibleExpressionListProblems(t, fixed) {
		if strings.Contains(problem, "MergeBuilder") {
			t.Errorf("the fifth builder resolves its rendered list and the check reports it anyway: %s", problem)
		}
	}
}

// mergeBuilderSource is the fifth builder again, with the field that
// holds its relation declared however the caller of this function says.
//
// decl is an optional type declaration the spelling needs; spelling is
// the type the field and the exported method that sets it are written
// with. Everything else is fixed, so a difference in what the checks
// report can only be the spelling.
func mergeBuilderSource(decl, spelling string) string {
	return fmt.Sprintf(`package pg

import (
	"context"

	"github.com/bernardoforcillo/drops"
)

%s

type MergeBuilder struct {
	db    *DB
	table %s
	when  []drops.Expression
}

func (m *MergeBuilder) Into(t %s) *MergeBuilder {
	m.table = t
	return m
}

func (m *MergeBuilder) WriteSQL(b *drops.Builder) {
	b.WriteString("MERGE INTO ")
	b.Append(m.table)
	b.AppendList(" ", m.when)
}

func (m *MergeBuilder) ToSQLCtx(ctx context.Context) (string, []any, error) {
	return "", nil, nil
}

func (m *MergeBuilder) resolveStatement(ctx context.Context) (drops.Expression, bool, error) {
	return m, false, nil
}

func (m *MergeBuilder) Exec(ctx context.Context) error {
	_, _, err := m.ToSQLCtx(ctx)
	return err
}
`, decl, spelling, spelling)
}

// fifthBuilderSpellings is the verifier's table, turned into the
// assertion.
//
// Every check with a relation in it used to compare against the name
// "Table", and the bypass was one line: declare the field with a local
// interface and the relation was invisible to the holdsTable walk, to
// takesRelation, and to both censuses built on them. These are the five
// ways to write the same field — the plain name, and interfaceAdmits's
// three answers, twice over where a spelling has both a named and an
// inline form. Each has to produce the same four answers as the first.
func fifthBuilderSpellings() []struct {
	name string
	src  string
} {
	return []struct {
		name string
		src  string
	}{
		{"the type named outright", mergeBuilderSource("", "*Table")},
		{
			"a local interface embedding drops.Expression",
			mergeBuilderSource("type mergeRel interface{ drops.Expression }", "mergeRel"),
		},
		{
			"an inline interface embedding drops.Expression",
			mergeBuilderSource("", "interface{ drops.Expression }"),
		},
		{"an interface demanding nothing", mergeBuilderSource("", "any")},
		{
			"a local interface a *Table happens to satisfy",
			mergeBuilderSource("type mergeNamed interface {\n\tName() string\n\tSchema() string\n}", "mergeNamed"),
		},
	}
}

// TestTheRelationInvariantSeesTheFieldHoweverItIsSpelled runs that
// table through every check that has a relation in it.
//
// The fifth builder holds a relation, renders it, hands a door to it
// through an exported setter, and can send a statement — so it must
// appear as a relation receiver, its setter must be censused as a
// relation entry point, its executor must be censused as an executor,
// and its unwalked expression list must still be reported. None of
// those answers may depend on how the field is declared.
func TestTheRelationInvariantSeesTheFieldHoweverItIsSpelled(t *testing.T) {
	for _, tc := range fifthBuilderSpellings() {
		t.Run(tc.name, func(t *testing.T) {
			p := loadPgSyntaxWith(t, tc.src)

			if !p.relationReceivers()["MergeBuilder"] {
				t.Errorf("MergeBuilder renders the relation it holds and the receiver walk does not see it")
			}
			if !hasEntry(censusRelationEntryPointsIn(p), "MergeBuilder.Into") {
				t.Errorf("MergeBuilder.Into puts a relation into a statement and the relation census does not see it:\n  census %v",
					mergeEntries(censusRelationEntryPointsIn(p)))
			}
			if !hasEntry(censusExecutors(t, p), "MergeBuilder.Exec") {
				t.Errorf("MergeBuilder.Exec sends a statement for the relation it holds and the executor census does not see it:\n  census %v",
					mergeEntries(censusExecutors(t, p)))
			}
			if !hasEntry(builderNames(statementBuilders(t, p)), "MergeBuilder") {
				t.Errorf("a fifth builder implementing resolveExpr's arm was not derived: got %v",
					builderNames(statementBuilders(t, p)))
			}
			requireProblemNaming(t, invisibleExpressionListProblems(t, p), "MergeBuilder.when")
		})
	}
}

// mergeEntries narrows a census to the synthetic builder's own doors,
// so a failure reads as "these are the doors it found" rather than as
// the whole package.
func mergeEntries(census []string) []string {
	var out []string
	for _, name := range census {
		if strings.HasPrefix(name, "MergeBuilder.") {
			out = append(out, name)
		}
	}
	return out
}

// builderNames lists the derived builders, for the messages above.
func builderNames(bs []statementBuilder) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.name)
	}
	return out
}

// requireProblemNaming fails unless some problem names want. It reports
// everything the check found on failure: a check that fires for the
// wrong reason is as much a problem as one that does not fire.
func requireProblemNaming(t *testing.T, problems []string, want string) {
	t.Helper()
	for _, p := range problems {
		if strings.Contains(p, want) {
			return
		}
	}
	t.Errorf("no problem names %s — the check does not refuse the shape it exists to refuse\n  found: %v", want, problems)
}
