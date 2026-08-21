package pg_test

import (
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
// Six checks now read the package source, and each was written the day
// after a leak that matched its shape. That history is also their
// weakness: a check written to see one shape is confirmed by a package
// where that shape is absent, which is the same evidence a check with a
// hole in it produces. Every bypass closed in this round was found by
// someone constructing the bypass — never by reading the checker — and
// four of them were open, three constructed and one live in this
// package's own source:
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
// Each is rebuilt below as a synthetic file of package pg, parsed
// alongside the real sources, and the check has to name it. Reverted
// against any of the four closures, the matching test here fails —
// which is the only evidence that a check refuses anything at all.
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

// TestArmCopyCheckSeesAFastPathOneArmOutOfDate rebuilds the shape the
// round-8 audit found in this package's own source: a pre-check that
// decides, without building anything, whether the resolver could do
// something — written as a subset of resolveExpr's arms, and therefore
// skipping the walk for whichever arm it does not mention.
func TestArmCopyCheckSeesAFastPathOneArmOutOfDate(t *testing.T) {
	p := loadPgSyntaxWith(t, bypassStaleArmCopy)
	requireProblemNaming(t, partialArmCopyProblems(t, p), "worthWalking")
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
