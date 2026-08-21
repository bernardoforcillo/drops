package dropslint

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"
)

// nameUnboundedRead is the rule's name; see nameUnfilteredWrite.
const nameUnboundedRead = "unboundedread"

// UnboundedRead reports a read of every row of a table: no Where, no
// Limit, and nothing that caps the row count at run time either.
//
// This is the rule most likely to cry wolf, and a linter that cries
// wolf is switched off entirely — taking unfilteredwrite with it. So
// it asks for a lot of evidence before it says anything:
//
//   - It looks only at reads of whole rows: an entity query, a Find,
//     or a Select given no projection at all (which renders SELECT *).
//     A hand-written Select with columns is as likely to be an
//     aggregate or a subquery's driving table as a full scan, and the
//     analyzer cannot tell which, so it says nothing about them.
//   - It fires on All alone. One returns a row, Count returns a
//     number, and Stream and Rows hand rows back as they arrive rather
//     than in one slice — which is why Budget lets Stream and Page
//     past MaxRows too. All is the call that pulls a whole table into
//     memory, and it is exactly the call MaxRows renders a LIMIT for.
//   - It stays quiet when the row count is already capped: an entity
//     whose Budget sets MaxRows has its LIMIT written for it.
//   - It counts any predicate, not only Where. The WhereHas family
//     renders a WHERE EXISTS around a subquery over a relation, which
//     narrows the rows returned as surely as a column predicate does.
//   - It stays quiet on a table the schema has declared small, with
//     //drops:lint lookup on the declaration.
//   - It stays quiet unless it can name the table — a package-level
//     *Table or *Entity, which is where a drops schema lives. A table
//     built inside a function is one the analyzer knows nothing about,
//     including whether it is small, and a rule that guesses there is
//     guessing everywhere. It is also the only kind of table nobody
//     can attach a lookup directive or a Budget to, so a finding
//     against one would be a finding with no fix.
//
// The bounding facts travel along import edges, so a package that only
// imports the schema still knows which of its tables are bounded.
var UnboundedRead = &analysis.Analyzer{
	Name:      nameUnboundedRead,
	Doc:       "report a read of a whole table: no Where, no Limit and no Budget.MaxRows",
	URL:       "https://pkg.go.dev/github.com/bernardoforcillo/drops/cmd/drops/dropslint",
	Run:       runUnboundedRead,
	FactTypes: []analysis.Fact{new(lookupFact), new(boundedFact)},
}

// lookupFact marks a *Table the schema has declared small enough to
// read whole, with //drops:lint lookup on its declaration. An *Entity
// over such a table carries it too: a small table stays small however
// it is read.
type lookupFact struct{}

func (*lookupFact) AFact()         {}
func (*lookupFact) String() string { return "lookup table" }

// boundedFact marks an *Entity whose Budget sets MaxRows, so its All
// already renders a LIMIT — see pg/budget.go, which is the run-time
// half of this rule.
type boundedFact struct{}

func (*boundedFact) AFact()         {}
func (*boundedFact) String() string { return "Budget.MaxRows" }

func runUnboundedRead(pass *analysis.Pass) (any, error) {
	exportSchemaFacts(pass)

	rep := newReporter(pass, nameUnboundedRead)
	for _, body := range bodies(pass) {
		fi := analyzeFunc(pass, body)
		ast.Inspect(body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "All" {
				return true
			}
			if !readsWholeRows(kindOf(pass.TypesInfo.TypeOf(sel.X))) {
				return true
			}
			origin, methods, ok := fi.chainFacts(call)
			if !ok || filtered(methods) || methods["Limit"] {
				return true
			}
			if kindOf(pass.TypesInfo.TypeOf(origin)) == selectBuilder && len(origin.Args) > 0 {
				return true // a projection, not a read of whole rows
			}
			t := targetOf(pass, origin, callsOf(call))
			if !t.named() || bounded(pass, t) {
				return true
			}
			rep.report(origin.Pos(), fmt.Sprintf(
				"this reads every row of %s: no Where, no Limit, no Budget.MaxRows. "+
					"Bound it, or mark the table with %s lookup if it is a small one.",
				t.label(), Directive))
			return true
		})
	}
	return nil, nil
}

func readsWholeRows(k builderKind) bool {
	return k == selectBuilder || k == findBuilder || k == entityQuery
}

// callsOf re-walks a chain for the rules that need the intermediate
// calls as well as the origin — From(t), for instance.
func callsOf(call *ast.CallExpr) []*ast.CallExpr {
	_, calls, _ := chainOf(call)
	return calls
}

// bounded reports whether something already caps this read.
func bounded(pass *analysis.Pass, t target) bool {
	if t.table != nil && pass.ImportObjectFact(t.table, new(lookupFact)) {
		return true
	}
	if t.entity != nil {
		if pass.ImportObjectFact(t.entity, new(boundedFact)) {
			return true
		}
		if pass.ImportObjectFact(t.entity, new(lookupFact)) {
			return true
		}
	}
	return false
}

// exportSchemaFacts records what this package's declarations say about
// the size of its tables, so importers can read it too.
//
// Tables are marked before entities are looked at, because an entity
// over a lookup table is bounded by the table and the two are usually
// declared in that order but need not be.
func exportSchemaFacts(pass *analysis.Pass) {
	mark := func(obj types.Object, fact analysis.Fact) {
		if obj != nil && obj.Pkg() == pass.Pkg {
			pass.ExportObjectFact(obj, fact)
		}
	}
	forEachVar(pass, func(obj types.Object, init ast.Expr, lookup bool) {
		if lookup && isDialectType(obj.Type(), "Table") {
			mark(obj, new(lookupFact))
		}
	})
	forEachVar(pass, func(obj types.Object, init ast.Expr, lookup bool) {
		if !isDialectType(obj.Type(), "Entity") {
			return
		}
		if lookup || overLookupTable(pass, init) {
			mark(obj, new(lookupFact))
		}
		if capsRows(pass, init) {
			mark(obj, new(boundedFact))
		}
	})
	// WithBudget is also reachable after the declaration — from an
	// init function, or from whatever wires the schema up.
	for _, f := range pass.Files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "WithBudget" || !maxRowsSet(pass, call) {
				return true
			}
			if obj := pkgLevelObj(pass, sel.X); obj != nil && isDialectType(obj.Type(), "Entity") {
				mark(obj, new(boundedFact))
			}
			return true
		})
	}
}

// forEachVar visits every package-level variable with its initialiser
// and whether a lookup directive covers it. A directive on a var group
// covers every name in it, which is how a table and its columns are
// usually declared.
func forEachVar(pass *analysis.Pass, fn func(obj types.Object, init ast.Expr, lookup bool)) {
	for _, f := range pass.Files {
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			group := hasVerb(gen.Doc, verbLookup)
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				lookup := group || hasVerb(vs.Doc, verbLookup)
				for i, name := range vs.Names {
					obj := pass.TypesInfo.ObjectOf(name)
					if obj == nil {
						continue
					}
					var init ast.Expr
					if i < len(vs.Values) {
						init = vs.Values[i]
					}
					fn(obj, init, lookup)
				}
			}
		}
	}
}

// overLookupTable reports whether an entity is declared over a table
// already marked a lookup table. The entity inherits the mark: a small
// table stays small however it is read.
func overLookupTable(pass *analysis.Pass, init ast.Expr) bool {
	if init == nil {
		return false
	}
	found := false
	ast.Inspect(init, func(n ast.Node) bool {
		e, ok := n.(ast.Expr)
		if !ok || found || !isDialectType(pass.TypesInfo.TypeOf(e), "Table") {
			return true
		}
		if obj := pkgLevelObj(pass, e); obj != nil && pass.ImportObjectFact(obj, new(lookupFact)) {
			found = true
		}
		return true
	})
	return found
}

// capsRows reports whether a declaration's initialiser chain sets a
// Budget with a non-zero MaxRows.
func capsRows(pass *analysis.Pass, init ast.Expr) bool {
	if init == nil {
		return false
	}
	call, ok := ast.Unparen(init).(*ast.CallExpr)
	if !ok {
		return false
	}
	_, calls, sels := chainOf(call)
	for i, c := range calls {
		if sels[i].Sel.Name == "WithBudget" && maxRowsSet(pass, c) {
			return true
		}
	}
	return false
}

// maxRowsSet reads the Budget literal handed to WithBudget. A budget
// the analyzer cannot read — one built elsewhere and passed in — is
// taken at its word: the author wrote a budget, so the read is
// bounded until proven otherwise, which is the quiet direction.
func maxRowsSet(pass *analysis.Pass, call *ast.CallExpr) bool {
	if len(call.Args) != 1 {
		return false
	}
	lit, ok := ast.Unparen(call.Args[0]).(*ast.CompositeLit)
	if !ok {
		return true
	}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "MaxRows" {
			continue
		}
		v := pass.TypesInfo.Types[kv.Value].Value
		if v == nil {
			return true // not a constant; assume the author meant it
		}
		n, ok := constant.Int64Val(constant.ToInt(v))
		return !ok || n > 0
	}
	return false
}
