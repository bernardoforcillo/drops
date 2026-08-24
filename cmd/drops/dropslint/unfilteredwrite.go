package dropslint

import (
	"fmt"
	"go/ast"

	"golang.org/x/tools/go/analysis"
)

// nameUnfilteredWrite is the rule's name, spelled once: it appears in
// the analyzer, in the diagnostic, and in the directive that silences
// it, and those three must agree.
const nameUnfilteredWrite = "unfilteredwrite"

// UnfilteredWrite reports a DELETE or an UPDATE executed with nothing
// to bound it.
//
// A Limit counts as bounding it. MySQL takes one on a write, and
//
//	db.Delete(events).OrderBy(id.Asc()).Limit(1000).Exec(ctx)
//
// is how a large table is emptied in batches instead of in one
// transaction — the statement touches a thousand rows, not every row,
// and reporting it would push the author towards the version that
// holds the whole table locked. The dialects that do not offer Limit
// on a write cannot reach this branch.
var UnfilteredWrite = &analysis.Analyzer{
	Name: nameUnfilteredWrite,
	Doc:  "report a DELETE or UPDATE executed without a Where clause",
	URL:  "https://pkg.go.dev/github.com/bernardoforcillo/drops/cmd/drops/dropslint",
	Run:  runUnfilteredWrite,
}

func runUnfilteredWrite(pass *analysis.Pass) (any, error) {
	rep := newReporter(pass, nameUnfilteredWrite)
	for _, body := range bodies(pass) {
		fi := analyzeFunc(pass, body)
		ast.Inspect(body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
			if !ok || !executes[sel.Sel.Name] {
				return true
			}
			kind := kindOf(pass.TypesInfo.TypeOf(sel.X))
			if !kind.writes() {
				return true
			}
			origin, methods, ok := fi.chainFacts(call)
			if !ok || filtered(methods) || methods["Limit"] {
				return true
			}
			t := targetOf(pass, origin, nil)
			rep.report(origin.Pos(), fmt.Sprintf(
				"%s on %s has no Where: it %s every row. Add a Where, or %s ignore %s if that is the intent.",
				kind, t.label(), damage(kind), Directive, nameUnfilteredWrite))
			return true
		})
	}
	return nil, nil
}

func damage(k builderKind) string {
	if k == deleteBuilder {
		return "removes"
	}
	return "rewrites"
}
