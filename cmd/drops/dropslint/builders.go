package dropslint

import (
	"go/ast"
	"go/types"

	"golang.org/x/tools/go/analysis"
)

// dialects are the packages whose builders the rules understand. A
// fork, or a copy vendored under a different path, is invisible — the
// conservative direction, since an unrecognised type produces silence
// rather than a guess.
var dialects = map[string]bool{
	"github.com/bernardoforcillo/drops/pg":         true,
	"github.com/bernardoforcillo/drops/sqlite":     true,
	"github.com/bernardoforcillo/drops/mysql":      true,
	"github.com/bernardoforcillo/drops/clickhouse": true,
}

// builderKind names the drops builder a value's type denotes. Every
// dialect spells these the same way, which is what lets one rule cover
// four packages.
type builderKind int

const (
	notBuilder builderKind = iota
	deleteBuilder
	updateBuilder
	selectBuilder
	findBuilder
	entityQuery
)

func (k builderKind) String() string {
	switch k {
	case deleteBuilder:
		return "DELETE"
	case updateBuilder:
		return "UPDATE"
	case selectBuilder, findBuilder, entityQuery:
		return "SELECT"
	}
	return "?"
}

// writes reports whether the builder mutates rows, which is the pair
// unfilteredwrite cares about.
func (k builderKind) writes() bool {
	return k == deleteBuilder || k == updateBuilder
}

var builderKinds = map[string]builderKind{
	"DeleteBuilder": deleteBuilder,
	"UpdateBuilder": updateBuilder,
	"SelectBuilder": selectBuilder,
	"FindBuilder":   findBuilder,
	"EntityQuery":   entityQuery,
}

// PageBuilder and the cursor builders are deliberately absent: they are
// paginated by construction, the same reason pg.Budget lets Page and
// Stream past MaxRows.

// kindOf classifies a type. It looks through the pointer every builder
// is handed round as, and through the instantiation of the generic
// ones, so EntityQuery[User] and EntityQuery[Post] answer alike.
func kindOf(t types.Type) builderKind {
	name, pkg, ok := namedOf(t)
	if !ok || !dialects[pkg] {
		return notBuilder
	}
	return builderKinds[name]
}

// isDialectType reports whether t is the named type from any dialect
// package — *pg.Table, *mysql.DB and so on.
func isDialectType(t types.Type, want string) bool {
	name, pkg, ok := namedOf(t)
	return ok && dialects[pkg] && name == want
}

// namedOf strips one level of pointer and reports the named type's own
// name and defining package path.
func namedOf(t types.Type) (name, pkg string, ok bool) {
	if t == nil {
		return "", "", false
	}
	t = types.Unalias(t)
	if p, isPtr := t.(*types.Pointer); isPtr {
		t = types.Unalias(p.Elem())
	}
	named, isNamed := t.(*types.Named)
	if !isNamed || named.Obj() == nil || named.Obj().Pkg() == nil {
		return "", "", false
	}
	return named.Obj().Name(), named.Obj().Pkg().Path(), true
}

// executes are the methods that send a statement to the server. ToSQL
// is pointedly not among them: rendering a statement is not running
// it, and the dialect packages' own tests render unfiltered deletes by
// the thousand. Both unfilteredwrite and loopload key off this: a
// statement that never runs is not a mistake either of them has
// anything to say about.
var executes = map[string]bool{
	"Exec":   true,
	"All":    true,
	"One":    true,
	"Rows":   true,
	"Stream": true,
	"Count":  true,
}

// predicates are the methods that put a WHERE on a statement. Where is
// the obvious one; the WhereHas family renders a WHERE EXISTS around a
// subquery over a relation, which narrows the rows returned exactly as
// a column predicate does. A rule that knew only Where reported drops'
// own relation filters as reads of a whole table — the kind of false
// positive that gets a linter switched off.
var predicates = map[string]bool{
	"Where":              true,
	"WhereHas":           true,
	"WhereHasRel":        true,
	"WhereDoesntHave":    true,
	"WhereDoesntHaveRel": true,
}

// carriesPredicate reports whether a predicate-taking call was handed
// anything to filter by.
//
// It exists because the builders drop nil predicates rather than
// panicking on them, which is a convenience on a read and a loaded gun
// on a write: db.Delete(t).Where(nil) is a DELETE with no WHERE, and
// counting the call by name alone would let the one spelling that
// needs this rule most be the one that silences it.
//
// A variadic expansion — Where(preds...) — counts as carrying one.
// The slice's contents are not knowable here, and a rule that fires on
// a caller who accumulated predicates properly is a rule people
// switch off.
func carriesPredicate(pass *analysis.Pass, call *ast.CallExpr) bool {
	if call.Ellipsis.IsValid() {
		return true
	}
	if len(call.Args) == 0 {
		return false
	}
	for _, a := range call.Args {
		b, ok := pass.TypesInfo.TypeOf(a).(*types.Basic)
		if !ok || b.Kind() != types.UntypedNil {
			return true
		}
	}
	return false
}

// filtered reports whether any predicate was put on the statement.
func filtered(methods map[string]bool) bool {
	for m := range predicates {
		if methods[m] {
			return true
		}
	}
	return false
}

// chainOf decomposes a method-call chain into the expression it grows
// out of and the calls along it, outermost first. For
//
//	db.Delete(Users).Where(p).Exec(ctx)
//
// base is the identifier db and calls is [Exec, Where, Delete]; the
// last entry is therefore always the call that started the chain.
func chainOf(call *ast.CallExpr) (base ast.Expr, calls []*ast.CallExpr, sels []*ast.SelectorExpr) {
	cur := ast.Expr(call)
	for {
		c, isCall := cur.(*ast.CallExpr)
		if !isCall {
			break
		}
		sel, isSel := ast.Unparen(c.Fun).(*ast.SelectorExpr)
		if !isSel {
			// A call on something that is not a method — a helper
			// returning a builder, say. That is the function boundary
			// the rules stop at, so the call itself is the base.
			break
		}
		calls = append(calls, c)
		sels = append(sels, sel)
		cur = ast.Unparen(sel.X)
	}
	return cur, calls, sels
}
