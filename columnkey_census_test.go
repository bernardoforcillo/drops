package drops_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The tenant-scoping phase kept finding the same defect, three rounds
// running, and each time it was found by it BITING. The question that
// generated all of them was only made precise at the end:
//
//	every place that compares or indexes columns by Column.key while
//	the renderer writes a bare name is a candidate.
//
// A handle names a column two ways at once. [Column.key] answers "which
// declared column is this", collapsing alias copies onto their origin
// and calling everything else a stranger. The renderer answers with a
// NAME — and an INSERT column list, an UPDATE SET target, an upsert
// assignment and an ORDER BY term write that name bare. Where the two
// disagree, a handle for the same-named column of another table object
// is a stranger to the guard and the same column to the server. Every
// leak this phase closed had that shape, and the last of them was
// alignRow, which did not merely fail to check such a binding: it
// DISCARDED it, so the row went out with the column defaulted and, on
// the tenant axis, belonging to nobody.
//
// This file is that question asked mechanically, so the next one
// cannot be found by biting. It censuses every `.key()` call in every
// dialect and requires each to carry a recorded verdict. A site nobody
// has answered for fails the test; a recorded site that no longer
// exists fails it too, so the record cannot rot into a list of places
// that used to matter.
//
// It does not decide the verdicts — a syntactic census cannot know
// what a statement renders. What it guarantees is that a new
// comparison by key cannot be written without somebody answering the
// question in the same commit, which is the part that failed three
// times.
//
// The dialect set is DERIVED, by the same rule and for the same reason
// as [TestTenantPolicyBlockIsIdenticalInEveryDialect]: a fifth dialect
// added tomorrow is censused because it declares WithTenant, not
// because a path was written down here.

// The three verdicts. They are the answers to "what happens to a
// handle that renders as this column and is NOT key-equal to it?".
const (
	// keyOwnHandles: no such handle can arrive. Both sides are
	// handles this package produced from the table or the entity —
	// the declared columns, the primary-key list, the colFields — so
	// there is no caller-supplied stranger for the comparison to
	// mistake.
	keyOwnHandles = "own-handles"

	// keyQualified: what the site decides is a QUALIFIER, or feeds a
	// reference that renders qualified. Two same-named columns of two
	// tables are different SQL there, the server sees the difference,
	// and leaving a stranger alone is the documented rule rather than
	// a miss.
	keyQualified = "qualified"

	// keyFailsClosed: the site does feed a bare name and a caller's
	// handle can reach it, but a stranger is REFUSED — the why says
	// where. Fail-closed by drops, not by luck of the server.
	keyFailsClosed = "fails-closed"
)

// keyComparison records one answer.
type keyComparison struct {
	verdict string
	// why names the mechanism, not the intent: the guard that
	// refuses, the handles that are the only ones to arrive, the
	// qualifier that makes the two columns different SQL.
	why string
}

// columnKeySites is the answer for every `.key()` call in the dialect
// packages, keyed by "dir/file.go:function".
//
// The key carries no line number on purpose — this record is about
// which comparisons exist, and it must not churn when one moves down a
// file.
var columnKeySites = map[string]keyComparison{
	// --- pg ---
	"pg/table.go:As":                  {keyOwnHandles, "wires an alias copy's origin, which is what key then reads; it establishes the identity rather than trusting one"},
	"pg/table.go:ScopeWritesByTenant": {keyFailsClosed, "a handle this table does not own panics at declaration time, so no stranger becomes the axis"},
	"pg/tenant.go:ScopeByTenant":      {keyFailsClosed, "a handle with no matching struct field panics at declaration time; a stranger has none"},
	"pg/insert.go:columnsOf":          {keyFailsClosed, "a stranger becomes a SECOND entry in the column list, and stampTenantColumn checks every occurrence of the axis before the duplicate reaches the server"},
	"pg/entity.go:CreateCols":         {keyFailsClosed, "the column-list loop refuses another table's handle by table name, and a same-table case variant has no struct field for fieldFor"},
	"pg/entity.go:fieldFor":           {keyFailsClosed, "resolves a struct field rather than a name; a stranger resolves to none and its caller refuses"},
	"pg/entity.go:isKeyColumn":        {keyOwnHandles, "compares the entity's own colFields against its own primary-key handles"},
	"pg/entity.go:Update":             {keyOwnHandles, "compares the entity's own colFields against its own version column"},
	"pg/patch.go:ownsColumn":          {keyFailsClosed, "looks the name up on the entity's table FIRST and then asks key, so a same-name stranger is found and refused as ErrForeignColumn"},
	"pg/page.go:resolveOrderBys":      {keyFailsClosed, "an ordering handle with no matching struct field is refused before the query runs; ORDER BY renders qualified besides"},
	"pg/page.go:encodeCursor":         {keyFailsClosed, "same refusal on the way back out: a cursor is built from struct fields, and a stranger has none"},
	"pg/snapshot.go:BuildSnapshot":    {keyOwnHandles, "reads the key set off the table's own declared columns; no caller handle is involved"},
	"pg/ddl.go:writeTableBody":        {keyOwnHandles, "same, for the CREATE TABLE body"},

	// --- mysql ---
	"mysql/table.go:As":                       {keyOwnHandles, "wires an alias copy's origin, for the table and for its columns"},
	"mysql/tablescope.go:ScopeWritesByTenant": {keyFailsClosed, "a handle this table does not own panics at declaration time"},
	"mysql/tablescope.go:setTenantAxis":       {keyOwnHandles, "normalises an alias copy onto the declared column before storing it"},
	"mysql/tenant.go:ScopeByTenant":           {keyFailsClosed, "a handle with no matching struct field panics at declaration time"},
	"mysql/tenant.go:tenantWriteAxis":         {keyOwnHandles, "matches the table's own axis handle against the entity's own colFields"},
	"mysql/insert.go:OnDuplicateKeyUpdate":    {keyQualified, "picks the declared TABLE, so an assignment restates against the relation the INSERT names"},
	"mysql/column.go:rebindValue":             {keyQualified, "decides whose qualifier an assignment renders with; a stranger deliberately keeps its own"},
	"mysql/entity.go:isKeyColumn":             {keyOwnHandles, "compares the entity's own colFields against its own primary-key handles"},
	"mysql/patch.go:ownsColumn":               {keyFailsClosed, "name lookup first, then key, so a same-name stranger is refused as ErrForeignColumn"},
	"mysql/page.go:rebindSpec":                {keyOwnHandles, "moves a cursor key onto the entity's own handle; a stranger is left as it came and renders qualified"},
	"mysql/cursor.go:rebindSpec":              {keyQualified, "same, plus the join test that decides whether the FROM table's handle may be substituted at all"},
	"mysql/snapshot.go:AddIndex":              {keyOwnHandles, "compares two table objects while recording a schema snapshot"},

	// --- clickhouse ---
	"clickhouse/table.go:As":                       {keyOwnHandles, "wires an alias copy's origin"},
	"clickhouse/tablescope.go:ScopeWritesByTenant": {keyFailsClosed, "a handle this table does not own panics at declaration time"},
	"clickhouse/tenant.go:ScopeByTenant":           {keyFailsClosed, "a handle with no matching struct field panics at declaration time"},
	"clickhouse/tenant.go:tenantWriteAxis":         {keyOwnHandles, "matches the table's own axis handle against the entity's own colFields"},
	"clickhouse/insert.go:columnsOf":               {keyFailsClosed, "a stranger becomes a SECOND entry in the column list, and stampTenantColumn checks every occurrence of the axis before the duplicate reaches the server"},

	// --- sqlite ---
	"sqlite/table.go:As":                  {keyOwnHandles, "wires an alias copy's origin"},
	"sqlite/table.go:ScopeWritesByTenant": {keyFailsClosed, "a handle this table does not own panics at declaration time"},
	"sqlite/table.go:setTenantAxis":       {keyOwnHandles, "normalises an alias copy onto the declared column before storing it"},
	"sqlite/tenant.go:ScopeByTenant":      {keyFailsClosed, "a handle with no matching struct field panics at declaration time"},
	"sqlite/tenant.go:tenantWriteAxis":    {keyOwnHandles, "matches the table's own axis handle against the entity's own colFields"},
	"sqlite/entity.go:isKeyColumn":        {keyOwnHandles, "compares the entity's own colFields against its own primary-key handles"},
	"sqlite/entity.go:alignBindings":      {keyOwnHandles, "widens an entity batch whose every binding this package built from the entity's own colFields"},
	"sqlite/patch.go:ownsColumn":          {keyFailsClosed, "name lookup first, then key, so a same-name stranger is refused as ErrForeignColumn"},
	"sqlite/page.go:encodeCursor":         {keyFailsClosed, "an ordering handle with no matching struct field is refused; a cursor is built from struct fields"},
}

// censusKeyComparisons returns every "dir/file.go:function" in dir
// holding at least one `.key()` call.
//
// It matches on the syntax rather than on a resolved type, so a
// Table.key lands in the record beside a Column.key. That is the right
// side to err on: the question is asked of everything spelled this
// way, and a site whose answer is "this is not a column at all" is one
// line to record and one line to read.
func censusKeyComparisons(t *testing.T, dir string) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range pkgs {
		for _, f := range p.Files {
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok {
					continue
				}
				ast.Inspect(fd, func(n ast.Node) bool {
					ce, ok := n.(*ast.CallExpr)
					if !ok || len(ce.Args) != 0 {
						return true
					}
					se, ok := ce.Fun.(*ast.SelectorExpr)
					if !ok || se.Sel.Name != "key" {
						return true
					}
					file := filepath.ToSlash(fset.Position(ce.Pos()).Filename)
					site := file + ":" + fd.Name.Name
					if !seen[site] {
						seen[site] = true
						out = append(out, site)
					}
					return true
				})
			}
		}
	}
	sort.Strings(out)
	return out
}

// TestEveryColumnKeyComparisonIsAnsweredFor is the census.
func TestEveryColumnKeyComparisonIsAnsweredFor(t *testing.T) {
	found := map[string]bool{}
	for _, dir := range dialectPackages(t) {
		for _, site := range censusKeyComparisons(t, dir) {
			found[site] = true
		}
	}
	if len(found) == 0 {
		t.Fatal("the census found no comparison by key at all: the derivation is broken, not the code")
	}

	var missing []string
	for site := range found {
		if _, ok := columnKeySites[site]; !ok {
			missing = append(missing, site)
		}
	}
	sort.Strings(missing)
	for _, site := range missing {
		t.Errorf("%s compares by key and nothing here answers for it.\n"+
			"Ask what the statement it feeds RENDERS: if the column goes out as a bare name and a "+
			"caller's handle can reach this comparison, a handle for the same-named column of another "+
			"table object is a stranger here and the same column to the server — index by "+
			"identKey(name) as alignRow does. Then record the site in columnKeySites with the verdict "+
			"that says why it is safe.", site)
	}

	var stale []string
	for site := range columnKeySites {
		if !found[site] {
			stale = append(stale, site)
		}
	}
	sort.Strings(stale)
	for _, site := range stale {
		t.Errorf("columnKeySites records %s, which compares by key nowhere. Delete the entry: a "+
			"record of places that used to matter reads like coverage and is none.", site)
	}

	for site, ans := range columnKeySites {
		switch ans.verdict {
		case keyOwnHandles, keyQualified, keyFailsClosed:
		default:
			t.Errorf("%s: verdict %q is not one of the three answers", site, ans.verdict)
		}
		if strings.TrimSpace(ans.why) == "" {
			t.Errorf("%s: the verdict names no mechanism", site)
		}
	}
}
