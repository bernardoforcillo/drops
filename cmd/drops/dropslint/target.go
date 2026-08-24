package dropslint

import (
	"go/ast"
	"go/types"

	"golang.org/x/tools/go/analysis"
)

// target is what a chain reads or writes, as far as the analyzer can
// name it. Both objects are package-level vars, because that is where
// a drops schema lives — a *Table built by NewTable and an *Entity
// built by NewEntity, declared once and referenced everywhere — and
// because only a package-level object can carry an analysis fact
// across an import edge.
type target struct {
	table  types.Object
	entity types.Object
}

// named reports whether the analyzer resolved the target to a
// declaration it could have learned something about.
func (t target) named() bool { return t.table != nil || t.entity != nil }

// label is what to call the target in a diagnostic. The Go name is
// used rather than the SQL one: it is what the reader will search for,
// and it is the name the fix has to be written next to.
func (t target) label() string {
	switch {
	case t.table != nil:
		return t.table.Name()
	case t.entity != nil:
		return t.entity.Name()
	}
	return "the table"
}

// targetOf resolves the table or entity a chain addresses: the *Table
// handed to db.Delete / db.Update / db.Find, the one handed to a
// SelectBuilder's From, or the *Entity whose Query opened the chain.
func targetOf(pass *analysis.Pass, origin *ast.CallExpr, calls []*ast.CallExpr) target {
	var t target
	consider := func(args []ast.Expr) {
		for _, arg := range args {
			if t.table != nil {
				return
			}
			if isDialectType(pass.TypesInfo.TypeOf(arg), "Table") {
				t.table = pkgLevelObj(pass, arg)
			}
		}
	}
	consider(origin.Args)
	for _, c := range calls {
		if sel, ok := ast.Unparen(c.Fun).(*ast.SelectorExpr); ok && sel.Sel.Name == "From" {
			consider(c.Args)
		}
	}
	if sel, ok := ast.Unparen(origin.Fun).(*ast.SelectorExpr); ok {
		if isDialectType(pass.TypesInfo.TypeOf(sel.X), "Entity") {
			t.entity = pkgLevelObj(pass, sel.X)
		}
	}
	return t
}

// pkgLevelObj returns the package-level variable an expression names,
// or nil. A table reached through a field or a parameter is a table
// the analyzer cannot carry a fact about, so it answers nil rather
// than something it cannot check later.
func pkgLevelObj(pass *analysis.Pass, e ast.Expr) types.Object {
	var id *ast.Ident
	switch x := ast.Unparen(e).(type) {
	case *ast.Ident:
		id = x
	case *ast.SelectorExpr:
		// schema.Users — a qualified name from another package.
		if pkgID, ok := ast.Unparen(x.X).(*ast.Ident); ok {
			if _, isPkg := pass.TypesInfo.ObjectOf(pkgID).(*types.PkgName); isPkg {
				id = x.Sel
			}
		}
	}
	if id == nil {
		return nil
	}
	v, ok := pass.TypesInfo.ObjectOf(id).(*types.Var)
	if !ok || v.Pkg() == nil || v.Parent() != v.Pkg().Scope() {
		return nil
	}
	return v
}
