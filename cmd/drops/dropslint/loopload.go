package dropslint

import (
	"go/ast"

	"golang.org/x/tools/go/analysis"
)

// nameLoopLoad is the rule's name; see nameUnfilteredWrite.
const nameLoopLoad = "loopload"

// LoopLoad reports a query that eager-loads a relation and executes
// inside a loop. That is an N+1 by construction: the parent query runs
// once per iteration and drags a relation query along behind it.
//
// It is the build-time half of what pg.WithN1Detector reports at run
// time, and it asks a narrower question so it can answer without
// running anything: not "did the same SQL fire repeatedly", which only
// the traffic knows, but "is a relation loaded on every pass of a
// loop", which the source knows.
//
// Both halves of the shape have to be inside the loop — the eager load
// and the call that executes it. Assembling the load list in a loop
//
//	for _, name := range wanted {
//	    q = q.With(name)
//	}
//	return q.All(ctx)
//
// runs one query, and the rule says nothing about it.
var LoopLoad = &analysis.Analyzer{
	Name: nameLoopLoad,
	Doc:  "report a query that eager-loads a relation and executes inside a loop",
	URL:  "https://pkg.go.dev/github.com/bernardoforcillo/drops/cmd/drops/dropslint",
	Run:  runLoopLoad,
}

// eagerLoads are the methods that attach a relation to a query.
var eagerLoads = map[string]bool{
	"With":    true,
	"WithRel": true,
	"Load":    true,
	"LoadRel": true,
}

func runLoopLoad(pass *analysis.Pass) (any, error) {
	rep := newReporter(pass, nameLoopLoad)
	for _, body := range bodies(pass) {
		fi := analyzeFunc(pass, body)
		ast.Inspect(body, func(n ast.Node) bool {
			loop, ok := loopBody(n)
			if !ok {
				return true
			}
			ast.Inspect(loop, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
				if !ok || !executes[sel.Sel.Name] {
					return true
				}
				if !loadable(kindOf(pass.TypesInfo.TypeOf(sel.X))) {
					return true
				}
				_, methods, ok := fi.chainFacts(call)
				if !ok || !loads(methods) {
					return true
				}
				// A loop that pages is walking the table, not the rows:
				// the relation load runs once per page, which is what
				// paging is, not an N+1. Offset says so outright; a
				// Limit says it too, because keyset paging — which is
				// how drops' own cursor API walks a table — moves a
				// Where rather than an Offset and would otherwise be
				// indistinguishable from the mistake. The cost is a
				// per-iteration fetch that caps itself with Limit, the
				// quiet direction the other rules also take.
				if methods["Offset"] || methods["Limit"] {
					return true
				}
				rep.report(call.Pos(),
					"this eager-loads a relation inside a loop: the relation queries run "+
						"again on every pass. Load once outside the loop — this is the N+1 "+
						"pg.WithN1Detector reports at run time.")
				return true
			})
			// Do not descend: the sweep just above already covered
			// every nested loop's body, and the reporter is keyed by
			// the executing call, so one query stays one finding.
			return false
		})
	}
	return nil, nil
}

func loadable(k builderKind) bool { return k == findBuilder || k == entityQuery }

func loads(methods map[string]bool) bool {
	for m := range eagerLoads {
		if methods[m] {
			return true
		}
	}
	return false
}

// loopBody returns the body of n when n is a loop.
func loopBody(n ast.Node) (*ast.BlockStmt, bool) {
	switch s := n.(type) {
	case *ast.ForStmt:
		return s.Body, true
	case *ast.RangeStmt:
		return s.Body, true
	}
	return nil, false
}
