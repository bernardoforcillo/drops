package dropslint

import (
	"go/ast"
	"go/types"

	"golang.org/x/tools/go/analysis"
)

// builderVar is what one function body has been able to learn about a
// local variable holding a builder.
//
// methods is a union over the whole body rather than the set reachable
// at any one statement: the builders mutate their receiver and return
// it, so q.Where(p) narrows q whether or not the result is assigned,
// and an analysis that ignored a Where under an if would report the
// careful version of the code and not the careless one.
type builderVar struct {
	origin  *ast.CallExpr // the call that produced the value
	methods map[string]bool
	// opaque records that the value's history is not fully visible
	// here: it came from a helper, it was handed to one, or a second
	// name was bound to the same pointer. Every rule declines to
	// report an opaque value.
	opaque bool
}

// funcInfo is the per-function-body view the rules share.
type funcInfo struct {
	pass *analysis.Pass
	vars map[types.Object]*builderVar
}

func (fi *funcInfo) get(obj types.Object) *builderVar {
	v := fi.vars[obj]
	if v == nil {
		v = &builderVar{methods: map[string]bool{}}
		fi.vars[obj] = v
	}
	return v
}

// analyzeFunc builds the view for one function body. It runs three
// sweeps because the answers depend on each other: what each variable
// was assigned, which methods were invoked on it, and whether it ever
// escaped somewhere the analyzer cannot follow.
func analyzeFunc(pass *analysis.Pass, body *ast.BlockStmt) *funcInfo {
	fi := &funcInfo{pass: pass, vars: map[types.Object]*builderVar{}}
	parent := parentMap(body)

	// Sweep 1: every method invoked on a named builder, wherever in
	// the body it happens.
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		base, calls, sels := chainOf(call)
		if len(sels) == 0 {
			return true
		}
		obj := fi.builderObj(base)
		if obj == nil {
			return true
		}
		v := fi.get(obj)
		for i, sel := range sels {
			if predicates[sel.Sel.Name] && !carriesPredicate(pass, calls[i]) {
				continue
			}
			v.methods[sel.Sel.Name] = true
		}
		return true
	})

	// Sweep 2: where each variable's value came from.
	ast.Inspect(body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range s.Lhs {
				if i < len(s.Rhs) {
					fi.bind(lhs, s.Rhs[i])
				}
			}
		case *ast.ValueSpec:
			for i, name := range s.Names {
				if i < len(s.Values) {
					fi.bind(name, s.Values[i])
				}
			}
		}
		return true
	})

	// Sweep 3: escapes. Anything but "receiver of a method call" or
	// "the name being assigned" puts the value beyond this analysis.
	ast.Inspect(body, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		obj := fi.builderObj(id)
		if obj == nil {
			return true
		}
		if !visibleUse(id, parent) {
			fi.get(obj).opaque = true
		}
		return true
	})
	return fi
}

// bind records that lhs takes its value from rhs.
func (fi *funcInfo) bind(lhs, rhs ast.Expr) {
	obj := fi.builderObj(lhs)
	if obj == nil {
		return
	}
	v := fi.get(obj)
	call, ok := ast.Unparen(rhs).(*ast.CallExpr)
	if !ok {
		// x := y, or a value read out of a struct: not traceable.
		v.opaque = true
		return
	}
	origin, methods, ok := fi.chainFacts(call)
	if !ok {
		v.opaque = true
		return
	}
	for m := range methods {
		v.methods[m] = true
	}
	switch {
	case v.origin == nil:
		v.origin = origin
	case v.origin != origin:
		// Rebound from a different statement; the two histories can no
		// longer be told apart at a use site.
		v.opaque = true
	}
}

// builderObj returns the object e names, when e is an identifier bound
// to a builder-typed variable. Package-level names, fields and
// everything else answer nil.
func (fi *funcInfo) builderObj(e ast.Expr) types.Object {
	id, ok := ast.Unparen(e).(*ast.Ident)
	if !ok {
		return nil
	}
	obj := fi.pass.TypesInfo.ObjectOf(id)
	v, ok := obj.(*types.Var)
	if !ok || v.Parent() == nil {
		return nil
	}
	if v.Pkg() != nil && v.Parent() == v.Pkg().Scope() {
		return nil // package-level: not a per-call value
	}
	if kindOf(v.Type()) == notBuilder {
		return nil
	}
	return obj
}

// visibleUse reports whether this occurrence of a builder name is one
// the analysis can account for: the receiver of a method call, or the
// name on the left of an assignment.
func visibleUse(id *ast.Ident, parent map[ast.Node]ast.Node) bool {
	p := parent[ast.Node(id)]
	for {
		par, isParen := p.(*ast.ParenExpr)
		if !isParen {
			break
		}
		p = parent[ast.Node(par)]
	}
	switch n := p.(type) {
	case *ast.SelectorExpr:
		// q.Where(...) — but only when the selector is being called,
		// not when the method value is taken and passed elsewhere.
		if ast.Unparen(n.X) != ast.Expr(id) {
			return false
		}
		call, ok := parent[ast.Node(n)].(*ast.CallExpr)
		return ok && ast.Unparen(call.Fun) == ast.Expr(n)
	case *ast.AssignStmt:
		for _, lhs := range n.Lhs {
			if ast.Unparen(lhs) == ast.Expr(id) {
				return true
			}
		}
		return false
	case *ast.ValueSpec:
		for _, name := range n.Names {
			if name == id {
				return true
			}
		}
		return false
	}
	return false
}

// chainFacts answers what the analyzer knows about the builder that
// call is a method on: the call that created it, and every method
// invoked on it. ok is false when the value's origin is outside this
// function or its history is opaque.
func (fi *funcInfo) chainFacts(call *ast.CallExpr) (origin *ast.CallExpr, methods map[string]bool, ok bool) {
	base, calls, sels := chainOf(call)
	if len(calls) == 0 {
		return nil, nil, false
	}
	methods = make(map[string]bool, len(sels))
	for i, sel := range sels {
		if predicates[sel.Sel.Name] && !carriesPredicate(fi.pass, calls[i]) {
			continue
		}
		methods[sel.Sel.Name] = true
	}

	if obj := fi.builderObj(base); obj != nil {
		v := fi.vars[obj]
		if v == nil || v.opaque || v.origin == nil {
			return nil, nil, false
		}
		for m := range v.methods {
			methods[m] = true
		}
		return v.origin, methods, true
	}

	// Otherwise the chain must start at a call that manufactures a
	// builder out of something that is not one — db.Delete(t),
	// UserEntity.Query(db). The manufacturing method has to be one of
	// the dialects' own, not merely a function that returns a builder:
	// a helper of the caller's that hands one back has already done
	// its Where inside itself, where this analysis cannot see it.
	inner, innerSel := calls[len(calls)-1], sels[len(sels)-1]
	if kindOf(fi.pass.TypesInfo.TypeOf(inner)) == notBuilder {
		return nil, nil, false
	}
	if kindOf(fi.pass.TypesInfo.TypeOf(innerSel.X)) != notBuilder {
		return nil, nil, false
	}
	if !dialectMethod(fi.pass, innerSel) {
		return nil, nil, false
	}
	return inner, methods, true
}

// dialectMethod reports whether a selector names a method one of the
// dialect packages declares. It reads the type checker's resolution
// rather than the receiver's type, so a *pg.DB embedded in somebody's
// Store still answers yes, and a struct field holding a func that
// returns a builder answers no.
func dialectMethod(pass *analysis.Pass, sel *ast.SelectorExpr) bool {
	selection := pass.TypesInfo.Selections[sel]
	if selection == nil || selection.Kind() != types.MethodVal {
		return false
	}
	fn, ok := selection.Obj().(*types.Func)
	if !ok || fn.Pkg() == nil {
		return false
	}
	return dialects[fn.Pkg().Path()]
}

// parentMap indexes each node under root by its parent, which the
// escape sweep needs and go/ast does not keep.
func parentMap(root ast.Node) map[ast.Node]ast.Node {
	m := map[ast.Node]ast.Node{}
	var stack []ast.Node
	ast.Inspect(root, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if len(stack) > 0 {
			m[n] = stack[len(stack)-1]
		}
		stack = append(stack, n)
		return true
	})
	return m
}

// bodies yields the outermost function body of each declaration.
// Closures are not listed separately: a body is walked whole, so a
// builder shared between a function and a literal nested in it is one
// value with one history, which is what it is at run time too.
func bodies(pass *analysis.Pass) []*ast.BlockStmt {
	var out []*ast.BlockStmt
	for _, f := range pass.Files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Body != nil {
					out = append(out, d.Body)
				}
			case *ast.GenDecl:
				// A package-level var may be initialised by a
				// function literal; its body is code like any other.
				ast.Inspect(d, func(n ast.Node) bool {
					if lit, ok := n.(*ast.FuncLit); ok {
						out = append(out, lit.Body)
						return false
					}
					return true
				})
			}
		}
	}
	return out
}
