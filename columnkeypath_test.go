package drops_test

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The tenant-scoping phase found the same defect three rounds running,
// and each time it was found by it BITING. The question behind all of
// them was only made precise at the end:
//
//	every place that compares or indexes columns by Column.key while
//	the renderer writes a bare name is a candidate.
//
// [TestEveryColumnKeyComparisonIsAnsweredFor] asks that question of
// every `.key()` call in every dialect and requires a recorded verdict.
// It cannot decide the verdicts, because it does not know what a
// statement renders: the half of the question that says "while the
// renderer writes a bare name" is answered there by a human, once, in
// prose.
//
// This file derives that half. It reads each dialect's source for the
// places the renderer writes a column name with no qualifier, follows
// what feeds them, and reports every comparison by [Column.key] that
// sits on the way in. Both halves are then derived: which comparisons
// exist, and which of them are on a path a bare name comes out of. What
// is left to a human is the exemption — a site that IS on such a path
// and is still safe, with the mechanism that makes it safe written
// beside it.
//
// Why the distinction is a leak, in one paragraph. A handle names a
// column two ways at once. [Column.key] answers "which declared column
// is this", collapsing alias copies onto their origin and calling
// everything else a stranger. The renderer answers with a NAME, and an
// INSERT column list, an UPDATE SET target and an upsert assignment
// write that name bare. Where the two disagree, a handle for the
// same-named column of ANOTHER table object is a stranger to the guard
// and the same column to the server. alignRow, the last of the three,
// did not merely fail to check such a binding: it DISCARDED it, so the
// row went out with the column defaulted and, on the tenant axis,
// belonging to nobody.
//
// The derivation is a backward slice and it OVER-approximates on
// purpose. Where it cannot resolve a type it assumes the value is a
// column, and where it cannot tell which function a call reaches it
// taints all of them. Over-approximating can only demand another
// exemption; under-approximating would let a real site through in
// silence, which is the failure this file exists to stop. A reader who
// finds an exemption that looks unnecessary is reading that trade.

// keyPathExemptions records every comparison by [Column.key] that the
// derivation below finds on a path where the renderer writes a bare
// column name, together with the mechanism that keeps it safe.
//
// It is an exemption list rather than a census: a site the derivation
// does not reach needs no entry, and an entry for a site the derivation
// does not reach is deleted. Both directions are checked. A stale entry
// is not harmless — a list of places that used to matter reads like
// coverage and is none.
//
// The key carries no line number on purpose. What is recorded is which
// comparison is exempt, and that must not churn when one moves down a
// file; the failure message carries the line.
//
// Each reason names a MECHANISM, not an intention: the guard that
// refuses, the handles that are the only ones that arrive, the
// qualifier that makes two same-named columns different SQL. "It is
// fine" is not a reason. Three of these were written by the census in
// the same words and are kept in them.
var keyPathExemptions = map[string]string{
	// --- pg ---
	"pg/table.go:As": "declaration time, and it is the wiring that MAKES the identity: an alias copy's " +
		"origin is set to the column it was copied from, so key has an answer to give later",
	"pg/table.go:ScopeWritesByTenant": "a handle this table does not own panics at declaration time, so " +
		"nothing that renders as the axis can arrive here as a stranger",
	"pg/tenant.go:ScopeByTenant": "a handle with no matching struct field panics at declaration time; a " +
		"stranger has none",
	"pg/insert.go:columnsOf": "a stranger becomes a SECOND entry in the column list, and stampTenantColumn " +
		"checks every occurrence of the axis by rendered name before the duplicate reaches the server",
	"pg/entity.go:CreateCols": "the column-list loop refuses another table's handle by table name, and a " +
		"same-table case variant has no struct field for fieldFor, so nothing that renders as the axis " +
		"reaches this question",
	"pg/entity.go:Update": "compares the entity's own colFields against its own version column; both sides " +
		"are handles this package took off the entity",
	"pg/ddl.go:writeTableBody": "the CREATE TABLE body reads the table's own declared columns and its own " +
		"primary-key list; a caller supplies neither",

	// --- mysql ---
	"mysql/table.go:As":                       "declaration time, the same wiring as pg's",
	"mysql/tablescope.go:ScopeWritesByTenant": "a handle this table does not own panics at declaration time",
	"mysql/tablescope.go:setTenantAxis": "normalises an alias copy onto the declared column before storing " +
		"it, so the axis is the declared handle whichever spelling declared it",
	"mysql/tenant.go:ScopeByTenant": "a handle with no matching struct field panics at declaration time",
	"mysql/page.go:rebindSpec": "moves a cursor key onto the entity's own handle; a stranger is left as it " +
		"came and renders qualified, which is different SQL the server can see",

	// --- clickhouse ---
	"clickhouse/table.go:As":                       "declaration time, the same wiring as pg's",
	"clickhouse/tablescope.go:ScopeWritesByTenant": "a handle this table does not own panics at declaration time",
	"clickhouse/tenant.go:ScopeByTenant":           "a handle with no matching struct field panics at declaration time",
	"clickhouse/insert.go:columnsOf": "a stranger becomes a SECOND entry in the column list, and " +
		"stampTenantColumn checks every occurrence of the axis by rendered name before the duplicate " +
		"reaches the server",

	// --- sqlite ---
	"sqlite/table.go:As":                  "declaration time, the same wiring as pg's",
	"sqlite/table.go:ScopeWritesByTenant": "a handle this table does not own panics at declaration time",
	"sqlite/table.go:setTenantAxis":       "normalises an alias copy onto the declared column before storing it",
	"sqlite/tenant.go:ScopeByTenant":      "a handle with no matching struct field panics at declaration time",
	"sqlite/entity.go:alignBindings": "the one alignment sqlite has, and every binding it indexes was built " +
		"by this package from the entity's own colFields — CreateMany takes rows of T, not handles, and " +
		"sqlite has no CreateCols for a caller to name a column through. It widens rather than selects, " +
		"so no binding is dropped either",
}

// TestNoColumnKeyComparisonOnARenderedNamePath is the rule, enforced.
//
// A failure here is one of two things. Either a new comparison by key
// sits on a path a bare column name comes out of, in which case the
// question to ask is the one in the message — can a caller's handle
// reach it, and does the statement it feeds write the column bare? — or
// an exemption has gone stale, in which case the entry goes.
func TestNoColumnKeyComparisonOnARenderedNamePath(t *testing.T) {
	found := map[string][]keyPathSite{}
	for _, dir := range dialectPackages(t) {
		for _, site := range renderedNamePathSites(t, dir) {
			found[site.key()] = append(found[site.key()], site)
		}
	}
	// A derivation that finds nothing asserts nothing. The three
	// dialects that align a row and the one that does not all have at
	// least the declaration-time wiring, so an empty result is the
	// analysis breaking rather than the packages going quiet.
	if len(found) == 0 {
		t.Fatal("the slice found no comparison by key on any rendering path at all: " +
			"the derivation is broken, not the code")
	}

	var keys []string
	for k := range found {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, ok := keyPathExemptions[k]; ok {
			continue
		}
		s := found[k][0]
		t.Errorf("%s:%d: %s compares %s by Column.key, and what it decides reaches a column name the "+
			"renderer writes BARE.\n"+
			"A handle for the same-named column of another table object is a stranger to key and the same "+
			"column to the server, so this comparison answers no where the statement answers yes. Ask what "+
			"reaches here: if a caller's handle can, index by identKey(name) as alignRow does. If none can, "+
			"or if the statement refuses a stranger earlier, record it in keyPathExemptions with the "+
			"mechanism that says why.",
			s.file, s.line, s.fn, s.expr)
	}

	for k, why := range keyPathExemptions {
		if len(found[k]) > 0 {
			if strings.TrimSpace(why) == "" {
				t.Errorf("%s is exempt and the exemption names no mechanism", k)
			}
			continue
		}
		t.Errorf("keyPathExemptions records %s, which the slice does not reach: either the comparison is "+
			"gone or it no longer feeds a bare column name. Delete the entry — an exemption for a site "+
			"that is not there reads like coverage and is none.", k)
	}
}

// keyPathSite is one `.key()` call the slice reached.
type keyPathSite struct {
	dir, file, fn, expr string
	line                int
}

// key is the identity an exemption is recorded under: the file and the
// function, without the line.
func (s keyPathSite) key() string { return s.file + ":" + s.fn }

// renderedNamePathSites returns every comparison by key in dir that the
// backward slice reaches from a bare column-name write.
//
// The three steps are: find the writes that put a column name into the
// statement with no qualifier; taint backwards from them, through
// assignment, return, range and call argument, everything that decides
// what those writes will see; then report the `.key()` calls whose
// receiver reads one of the tainted places.
func renderedNamePathSites(t *testing.T, dir string) []keyPathSite {
	t.Helper()
	p := newNamePathPkg(t, dir)
	p.seedBareNameWrites()
	p.propagate()
	return p.keySitesOnTaint()
}

// --- the analysis ---------------------------------------------------
//
// It is a syntactic analysis with a small local type inference, rather
// than a typed one, because the root module has no dependencies and
// go/types would need export data for the package under test. What the
// inference is for is one question — is this expression a column? — and
// it answers "yes" when it cannot tell.

// parseDirFilesFset parses the non-test sources of one directory into
// the caller's file set, which the analysis needs because it reports
// positions. A directory that does not parse fails the run here rather
// than contributing nothing: it was found by [dialectPackages], so it
// is a dialect, and a dialect this rule cannot read is a dialect this
// rule does not cover.
func parseDirFilesFset(t *testing.T, dir string, fset *token.FileSet) []*ast.File {
	t.Helper()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}
	var out []*ast.File
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			out = append(out, f)
		}
	}
	// The map iteration above has no order, and the slice reports
	// positions: two runs must name the same site the same way.
	sort.Slice(out, func(i, j int) bool {
		return fset.Position(out[i].Pos()).Filename < fset.Position(out[j].Pos()).Filename
	})
	return out
}

// namePathSlot is a place a column value can rest: a struct field, or a
// variable, parameter or result of one function.
type namePathSlot struct {
	owner string // "InsertBuilder" for a field, "InsertBuilder.Row" for a function
	name  string // field or variable name, or resultSlot
}

// resultSlot stands for "whatever this function returns". A tainted
// result taints the expressions the function returns.
const resultSlot = "#result"

// namePathFunc is one function declaration, with the environment its
// identifiers were declared in.
type namePathFunc struct {
	decl     *ast.FuncDecl
	file     string
	recv     string
	id       string
	env      map[string]string
	fset     *token.FileSet
	paramSet []string
}

type namePathPkg struct {
	t       *testing.T
	dir     string
	fset    *token.FileSet
	fields  map[string]map[string]string // struct or interface type -> member -> type
	results map[string]string            // "Recv.Name", ".Name" or "Name" -> first result type
	funcs   []*namePathFunc
	byName  map[string][]*namePathFunc
	tainted map[namePathSlot]bool
}

func newNamePathPkg(t *testing.T, dir string) *namePathPkg {
	t.Helper()
	p := &namePathPkg{
		t:       t,
		dir:     dir,
		fset:    token.NewFileSet(),
		fields:  map[string]map[string]string{},
		results: map[string]string{},
		byName:  map[string][]*namePathFunc{},
		tainted: map[namePathSlot]bool{},
	}
	files := parseDirFilesFset(t, dir, p.fset)
	for _, f := range files {
		p.collectTypes(f)
	}
	for _, f := range files {
		p.collectFuncs(f)
	}
	if len(p.funcs) == 0 {
		t.Fatalf("%s: parsed no functions — the derivation is broken, not the code", dir)
	}
	return p
}

// collectTypes records the member types of every struct and interface
// in the package, so that a selector or a method call can be resolved
// without a type checker.
func (p *namePathPkg) collectTypes(f *ast.File) {
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, s := range gd.Specs {
			spec, ok := s.(*ast.TypeSpec)
			if !ok {
				continue
			}
			m := map[string]string{}
			switch typ := spec.Type.(type) {
			case *ast.StructType:
				for _, fl := range typ.Fields.List {
					for _, n := range fl.Names {
						m[n.Name] = p.typeString(fl.Type)
					}
				}
			case *ast.InterfaceType:
				// An interface method is how a binding answers which
				// column it names — ColumnValue.column. Without them
				// the slice loses the INSERT column list of every
				// dialect that renders it off the first row.
				for _, fl := range typ.Methods.List {
					ft, ok := fl.Type.(*ast.FuncType)
					if !ok || len(fl.Names) == 0 {
						continue
					}
					var res string
					if ft.Results != nil && len(ft.Results.List) > 0 {
						res = p.typeString(ft.Results.List[0].Type)
					}
					p.results[spec.Name.Name+"."+fl.Names[0].Name] = res
				}
				continue
			default:
				continue
			}
			p.fields[spec.Name.Name] = m
		}
	}
}

// collectFuncs records every function, its identifier environment and
// its result type.
func (p *namePathPkg) collectFuncs(f *ast.File) {
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		fn := &namePathFunc{
			decl: fd,
			file: filepath.ToSlash(p.fset.Position(fd.Pos()).Filename),
			env:  map[string]string{},
			fset: p.fset,
		}
		var res string
		if fd.Type.Results != nil && len(fd.Type.Results.List) > 0 {
			res = p.typeString(fd.Type.Results.List[0].Type)
		}
		if fd.Recv != nil && len(fd.Recv.List) > 0 {
			fn.recv = receiverTypeName(fd.Recv.List[0].Type)
			p.results[fn.recv+"."+fd.Name.Name] = res
			// A method name shared by two types resolves to neither:
			// guessing there would give the slice a type the code does
			// not have.
			if _, seen := p.results["."+fd.Name.Name]; seen {
				p.results["."+fd.Name.Name] = ""
			} else {
				p.results["."+fd.Name.Name] = res
			}
			fn.id = fn.recv + "." + fd.Name.Name
		} else {
			p.results[fd.Name.Name] = res
			fn.id = fd.Name.Name
		}
		for _, fl := range fieldLists(fd) {
			for _, field := range fl.List {
				for _, n := range field.Names {
					fn.env[n.Name] = p.typeString(field.Type)
				}
			}
		}
		if fd.Type.Params != nil {
			for _, field := range fd.Type.Params.List {
				if len(field.Names) == 0 {
					fn.paramSet = append(fn.paramSet, "")
					continue
				}
				for _, n := range field.Names {
					fn.paramSet = append(fn.paramSet, n.Name)
				}
			}
		}
		p.declareLocals(fn)
		p.funcs = append(p.funcs, fn)
		p.byName[fd.Name.Name] = append(p.byName[fd.Name.Name], fn)
	}
}

func fieldLists(fd *ast.FuncDecl) []*ast.FieldList {
	var out []*ast.FieldList
	for _, fl := range []*ast.FieldList{fd.Recv, fd.Type.Params, fd.Type.Results} {
		if fl != nil {
			out = append(out, fl)
		}
	}
	return out
}

// declareLocals walks the body once and records the type of every
// identifier a short declaration, a var declaration or a range
// introduces. One pass in source order is enough for the shapes this
// package writes; an identifier the pass cannot type stays unknown,
// which the slice reads as "may be a column".
func (p *namePathPkg) declareLocals(fn *namePathFunc) {
	ast.Inspect(fn.decl.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			if v.Tok != token.DEFINE || len(v.Lhs) != len(v.Rhs) {
				return true
			}
			for i := range v.Lhs {
				id, ok := v.Lhs[i].(*ast.Ident)
				if !ok {
					continue
				}
				if typ := p.typeOf(v.Rhs[i], fn.env); typ != "" {
					fn.env[id.Name] = typ
				}
			}
		case *ast.RangeStmt:
			id, ok := v.Value.(*ast.Ident)
			if !ok || id.Name == "_" {
				return true
			}
			if elem := elemType(p.typeOf(v.X, fn.env)); elem != "" {
				fn.env[id.Name] = elem
			}
		case *ast.DeclStmt:
			gd, ok := v.Decl.(*ast.GenDecl)
			if !ok {
				return true
			}
			for _, s := range gd.Specs {
				vs, ok := s.(*ast.ValueSpec)
				if !ok || vs.Type == nil {
					continue
				}
				for _, n := range vs.Names {
					fn.env[n.Name] = p.typeString(vs.Type)
				}
			}
		}
		return true
	})
}

// typeString renders a type expression as the source spells it. The
// analysis compares these as strings: what it needs to know is whether
// a name contains "Col", not what the type is.
func (p *namePathPkg) typeString(e ast.Expr) string {
	if e == nil {
		return ""
	}
	var b bytes.Buffer
	if err := printer.Fprint(&b, p.fset, e); err != nil {
		return ""
	}
	return b.String()
}

// elemType returns the element type of a slice or map type spelling.
func elemType(t string) string {
	if strings.HasPrefix(t, "[]") {
		return t[2:]
	}
	if strings.HasPrefix(t, "map[") {
		if i := strings.Index(t, "]"); i > 0 {
			return t[i+1:]
		}
	}
	return ""
}

// typeOf answers what an expression's type is spelled as, or "" when it
// cannot tell.
func (p *namePathPkg) typeOf(e ast.Expr, env map[string]string) string {
	switch v := e.(type) {
	case *ast.Ident:
		return env[v.Name]
	case *ast.ParenExpr:
		return p.typeOf(v.X, env)
	case *ast.UnaryExpr:
		if v.Op == token.AND {
			return "*" + p.typeOf(v.X, env)
		}
	case *ast.StarExpr:
		return strings.TrimPrefix(p.typeOf(v.X, env), "*")
	case *ast.IndexExpr:
		return elemType(p.typeOf(v.X, env))
	case *ast.SelectorExpr:
		base := strings.TrimPrefix(p.typeOf(v.X, env), "*")
		return p.fields[base][v.Sel.Name]
	case *ast.CallExpr:
		switch fun := v.Fun.(type) {
		case *ast.Ident:
			return p.results[fun.Name]
		case *ast.SelectorExpr:
			base := strings.TrimPrefix(p.typeOf(fun.X, env), "*")
			if base != "" {
				if r, ok := p.results[base+"."+fun.Sel.Name]; ok {
					return r
				}
			}
			return p.results["."+fun.Sel.Name]
		}
	case *ast.TypeAssertExpr:
		return p.typeString(v.Type)
	}
	return ""
}

// mayBeColumn reports whether a type spelling could be a column
// reference. Unknown counts as yes: the slice must not lose a path
// because the inference ran out of source to read.
func mayBeColumn(t string) bool {
	t = strings.TrimPrefix(strings.TrimPrefix(t, "[]"), "*")
	return t == "" || strings.Contains(t, "Col")
}

// readSlots returns the places an expression reads from — the field,
// variable or function result its value comes out of. Taint flows to
// these.
//
// A method call contributes its receiver as well as its result: an
// accessor is how a binding answers with the column it names, and a
// slice that stopped at the result would lose the INSERT column list of
// every dialect that renders one off a row of bindings.
func (p *namePathPkg) readSlots(e ast.Expr, fn *namePathFunc) []namePathSlot {
	var out []namePathSlot
	var walk func(ast.Expr)
	walk = func(e ast.Expr) {
		switch v := e.(type) {
		case *ast.Ident:
			if isPredeclared(v.Name) {
				return
			}
			out = append(out, namePathSlot{fn.id, v.Name})
		case *ast.ParenExpr:
			walk(v.X)
		case *ast.StarExpr:
			walk(v.X)
		case *ast.UnaryExpr:
			walk(v.X)
		case *ast.IndexExpr:
			walk(v.X)
		case *ast.SliceExpr:
			walk(v.X)
		case *ast.SelectorExpr:
			base := strings.TrimPrefix(p.typeOf(v.X, fn.env), "*")
			if _, ok := p.fields[base][v.Sel.Name]; ok {
				out = append(out, namePathSlot{base, v.Sel.Name})
				return
			}
			walk(v.X)
		case *ast.CallExpr:
			switch fun := v.Fun.(type) {
			case *ast.Ident:
				// The builtins that carry a value through rather than
				// producing one of their own.
				if fun.Name == "append" || fun.Name == "copy" {
					for _, a := range v.Args {
						walk(a)
					}
					return
				}
				out = append(out, namePathSlot{fun.Name, resultSlot})
			case *ast.SelectorExpr:
				base := strings.TrimPrefix(p.typeOf(fun.X, fn.env), "*")
				if base != "" {
					out = append(out, namePathSlot{base + "." + fun.Sel.Name, resultSlot})
				} else {
					out = append(out, namePathSlot{fun.Sel.Name, resultSlot})
				}
				walk(fun.X)
			}
		case *ast.CompositeLit:
			for _, el := range v.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					walk(kv.Value)
					continue
				}
				walk(el)
			}
		}
	}
	walk(e)
	return out
}

func isPredeclared(name string) bool {
	switch name {
	case "nil", "true", "false", "len", "cap", "make", "new", "append", "copy", "_":
		return true
	}
	return false
}

func (p *namePathPkg) taint(s namePathSlot) bool {
	if s.name == "" || p.tainted[s] {
		return false
	}
	p.tainted[s] = true
	return true
}

func (p *namePathPkg) isTainted(e ast.Expr, fn *namePathFunc) bool {
	for _, s := range p.readSlots(e, fn) {
		if p.tainted[s] {
			return true
		}
	}
	return false
}

// seedBareNameWrites taints the places a column name is written with no
// qualifier.
//
// The write to look for is drops.Builder.WriteIdent handed a column's
// NAME: taking the name is the only way a column reaches SQL unqualified,
// because Column.WriteSQL writes the table reference in front of it
// everywhere else. A qualified reference is not this rule's business —
// two same-named columns of two tables are different SQL there, and the
// server sees the difference.
func (p *namePathPkg) seedBareNameWrites() {
	for _, fn := range p.funcs {
		ast.Inspect(fn.decl, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "WriteIdent" || len(call.Args) != 1 {
				return true
			}
			ast.Inspect(call.Args[0], func(n ast.Node) bool {
				name, ok := n.(*ast.SelectorExpr)
				if !ok || (name.Sel.Name != "Name" && name.Sel.Name != "name") {
					return true
				}
				if !mayBeColumn(strings.TrimPrefix(p.typeOf(name.X, fn.env), "*")) {
					return true
				}
				for _, s := range p.readSlots(name.X, fn) {
					p.taint(s)
				}
				return true
			})
			return true
		})
	}
	if len(p.tainted) == 0 {
		p.t.Fatalf("%s: no bare column-name write found — every dialect renders an INSERT column list, "+
			"so this is the derivation breaking rather than the package", p.dir)
	}
}

// propagate runs the backward slice to a fixed point: whatever feeds a
// tainted place is tainted too.
//
// The four ways a value arrives somewhere are all covered — it is
// assigned, it is returned, it is ranged over, or it is passed as an
// argument — and each is followed in the direction that answers "what
// decided this?".
func (p *namePathPkg) propagate() {
	// The slice converges in a handful of rounds on these packages;
	// the bound is here so a cycle the rules did not anticipate fails
	// the run rather than hanging it.
	const maxRounds = 64
	for round := 0; ; round++ {
		if round == maxRounds {
			p.t.Fatalf("%s: the slice did not settle in %d rounds", p.dir, maxRounds)
		}
		changed := false
		for _, fn := range p.funcs {
			changed = p.propagateInto(fn) || changed
		}
		if !changed {
			return
		}
	}
}

func (p *namePathPkg) propagateInto(fn *namePathFunc) bool {
	changed := false
	spread := func(e ast.Expr) {
		for _, s := range p.readSlots(e, fn) {
			if p.taint(s) {
				changed = true
			}
		}
	}
	if p.tainted[namePathSlot{fn.id, resultSlot}] {
		ast.Inspect(fn.decl.Body, func(n ast.Node) bool {
			ret, ok := n.(*ast.ReturnStmt)
			if !ok {
				return true
			}
			for _, r := range ret.Results {
				// Only the column-carrying results: a function that
				// returns a column list and an error does not make its
				// error a column.
				if mayBeColumn(p.typeOf(r, fn.env)) {
					spread(r)
				}
			}
			return true
		})
	}
	ast.Inspect(fn.decl.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range v.Lhs {
				var rhs ast.Expr
				switch {
				case len(v.Rhs) == len(v.Lhs):
					rhs = v.Rhs[i]
				case len(v.Rhs) == 1:
					// One call filling several results: the call is
					// what decided all of them.
					rhs = v.Rhs[0]
				}
				if rhs != nil && p.isTainted(lhs, fn) {
					spread(rhs)
				}
			}
		case *ast.RangeStmt:
			if v.Value != nil && p.isTainted(v.Value, fn) {
				spread(v.X)
			}
		case *ast.CallExpr:
			// A tainted parameter taints the argument every caller
			// hands it. Which function a call reaches is resolved by
			// name only, so every function of that name is asked:
			// tainting one too many costs an exemption, tainting one
			// too few loses a path.
			var name string
			switch fun := v.Fun.(type) {
			case *ast.Ident:
				name = fun.Name
			case *ast.SelectorExpr:
				name = fun.Sel.Name
			default:
				return true
			}
			for _, callee := range p.byName[name] {
				for i, arg := range v.Args {
					if i >= len(callee.paramSet) {
						break
					}
					if p.tainted[namePathSlot{callee.id, callee.paramSet[i]}] {
						spread(arg)
					}
				}
			}
		}
		return true
	})
	return changed
}

// keySitesOnTaint reports every `.key()` call whose receiver reads a
// tainted place.
//
// The receiver is what the question is about: `c.key()` where c is on
// its way into an INSERT column list is the comparison this rule
// governs, and `t.key()` on a table is not — mysql compares two tables
// that way to decide whose relation an upsert assignment restates, and
// a relation is not a column. A receiver the inference cannot type
// counts as a column.
func (p *namePathPkg) keySitesOnTaint() []keyPathSite {
	var out []keyPathSite
	for _, fn := range p.funcs {
		ast.Inspect(fn.decl, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "key" || len(call.Args) != 0 {
				return true
			}
			if !mayBeColumn(strings.TrimPrefix(p.typeOf(sel.X, fn.env), "*")) {
				return true
			}
			if !p.isTainted(sel.X, fn) {
				return true
			}
			out = append(out, keyPathSite{
				dir:  p.dir,
				file: fn.file,
				fn:   fn.decl.Name.Name,
				expr: p.typeString(sel.X) + ".key()",
				line: p.fset.Position(call.Pos()).Line,
			})
			return true
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		return out[i].line < out[j].line
	})
	return out
}
