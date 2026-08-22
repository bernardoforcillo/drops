package drops_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Every divergence the tenant-scoping phase turned up was a policy
// question that no file owned. Each dialect answered it locally, the
// answers disagreed, and the disagreement was found by reading four
// files side by side — which is to say it was found by whoever
// happened to look, rather than by anything that could fail.
//
// resolve.go closed that class for the WALK: normalise the dialect
// name and pg/resolve.go, sqlite/resolve.go, mysql/resolve.go and
// clickhouse/resolve.go are the same file, so a future divergence in
// how a statement is walked shows up as a diff. The POLICIES — what
// counts as the same tenant, what may assign the axis, what Unscoped
// gives up at each level — had no equivalent home, and nothing that
// could catch a fifth answer being written into a fifth file.
//
// The dialects now carry a delimited block that is byte-identical in
// all of them, and this test is what keeps it that way. It fails when
// one of them drifts by a word, by whitespace, or by reordering,
// because it compares the extracted bytes rather than anything
// normalised.
//
// The set it compares is DERIVED and not listed. A list is the thing
// this file exists to replace: with four paths written down here, a
// fifth dialect added tomorrow carried no block, was compared against
// nothing, and passed in silence — the same failure the block itself
// was written to close, one level up. So the dialects are found by
// reading the source for the tenancy entry point, the way the census
// checks in pg/scopefunc_test.go find the constructors they cover.
//
// The block also names what each dialect cannot do — clickhouse models
// neither UPDATE nor DELETE, RelConfig.Unscoped is pg's alone — so
// that the same words are true in all of them rather than one set of
// words per package. Those sentences are claims about the code, and
// TestTenantPolicyBlockSurfaceClaimsHold below checks them: a dialect
// that grows the surface the block says it lacks makes the block
// false, and the block is the thing four packages point at.

const (
	policyStartMarker = "// ==== THE TENANT POLICIES — NORMATIVE ===="
	policyEndMarker   = "// ==== END OF THE TENANT POLICIES ===="
)

// tenantEntryPoint is the function every dialect that has tenancy
// declares, and the marker this file identifies a dialect by.
//
// It is the entry point rather than the block itself, which is the
// whole point: a package found by looking for the block can never be
// reported as missing one. WithTenant is what a caller writes to name
// a tenant, so a package that has it has tenancy and owes the reader
// the policies; a package that does not have it is not a dialect this
// block is about.
const tenantEntryPoint = "WithTenant"

// dialectPackages returns the directory of every package in the
// repository that declares the tenancy entry point, sorted.
//
// The walk covers the whole tree rather than the root's own children,
// so a dialect added under a subdirectory is found too. Directories
// beginning with "." or "_" are skipped: those are Go's own "not part
// of the build" convention, and _examples holds sources that do not
// compile as part of the module.
func dialectPackages(t *testing.T) []string {
	t.Helper()
	var dirs []string
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if base := filepath.Base(p); p != "." && (strings.HasPrefix(base, ".") ||
			strings.HasPrefix(base, "_") || base == "testdata") {
			return filepath.SkipDir
		}
		if declaresFuncIn(t, p, tenantEntryPoint) {
			dirs = append(dirs, filepath.ToSlash(p))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Strings(dirs)
	// A derivation that finds nothing asserts nothing, and would do so
	// quietly for exactly the reason this file distrusts lists: the
	// thing being derived stopped existing and the check went green.
	if len(dirs) == 0 {
		t.Fatalf("no package declares %s: the derivation is broken, not the policies", tenantEntryPoint)
	}
	return dirs
}

// declaresFuncIn reports whether the package in dir declares a
// package-level function with the given name.
func declaresFuncIn(t *testing.T, dir, name string) bool {
	t.Helper()
	for _, f := range parseDirFiles(t, dir) {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if ok && fd.Recv == nil && fd.Name.Name == name {
				return true
			}
		}
	}
	return false
}

// parseDirFiles parses the non-test sources of one directory. A
// directory that does not parse as Go — no .go files, or several
// packages in one folder — contributes nothing rather than failing the
// run: the question here is which packages have tenancy, and a
// directory that is not one package is not one of them.
func parseDirFiles(t *testing.T, dir string) []*ast.File {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	var out []*ast.File
	for _, p := range pkgs {
		for _, f := range p.Files {
			out = append(out, f)
		}
	}
	return out
}

// policyFileIn returns the one source file in dir that carries the
// block.
//
// Exactly one: a dialect with no block is the failure this file was
// rewritten to catch, and a dialect with two has an extraction whose
// answer depends on which the reader looked at first.
func policyFileIn(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var found []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		p := path.Join(dir, e.Name())
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if strings.Contains(string(src), policyStartMarker) {
			found = append(found, p)
		}
	}
	switch len(found) {
	case 1:
		return found[0]
	case 0:
		t.Fatalf("%s declares %s but carries no %q block: a dialect with tenancy owes the reader "+
			"the policies, and a block that is not in every dialect is not normative",
			dir, tenantEntryPoint, policyStartMarker)
	default:
		t.Fatalf("%s carries the policy block in %d files (%v): the extraction has no single answer",
			dir, len(found), found)
	}
	return ""
}

// extractPolicyBlock returns the delimited block from path, marker
// lines included. Including the markers is deliberate: it pins their
// spelling too, so the block cannot be renamed in one file only.
func extractPolicyBlock(t *testing.T, path string) string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(string(src), "\n")
	start, end := -1, -1
	for i, ln := range lines {
		switch strings.TrimRight(ln, "\r") {
		case policyStartMarker:
			if start >= 0 {
				t.Fatalf("%s: start marker appears twice (lines %d and %d)", path, start+1, i+1)
			}
			start = i
		case policyEndMarker:
			if end >= 0 {
				t.Fatalf("%s: end marker appears twice (lines %d and %d)", path, end+1, i+1)
			}
			end = i
		}
	}
	if start < 0 {
		t.Fatalf("%s: no %q line — the normative tenant policy block is missing", path, policyStartMarker)
	}
	if end < 0 {
		t.Fatalf("%s: no %q line — the normative tenant policy block is unterminated", path, policyEndMarker)
	}
	if end <= start {
		t.Fatalf("%s: end marker (line %d) precedes start marker (line %d)", path, end+1, start+1)
	}
	return strings.Join(lines[start:end+1], "\n")
}

// TestTenantPolicyBlockIsIdenticalInEveryDialect is the mechanical
// check that keeps the policies from drifting apart again.
//
// The reference is whichever dialect sorts first only because the
// comparison needs one; the block is not that package's to change
// alone, and a deliberate edit means editing every dialect in the same
// commit.
func TestTenantPolicyBlockIsIdenticalInEveryDialect(t *testing.T) {
	dirs := dialectPackages(t)
	files := make([]string, len(dirs))
	for i, dir := range dirs {
		files[i] = policyFileIn(t, dir)
	}

	ref := files[0]
	want := extractPolicyBlock(t, ref)

	if strings.TrimSpace(want) == "" {
		t.Fatalf("%s: the policy block is empty", ref)
	}
	// A block that shrank to its markers would compare equal
	// everywhere and assert nothing. The policies do not fit in ten
	// lines.
	if got := strings.Count(want, "\n") + 1; got < 40 {
		t.Fatalf("%s: the policy block is %d lines, which is too few to be stating the policies", ref, got)
	}
	// Every line of the block has to be a comment line, or the block
	// has swallowed code and the comparison is pinning more than the
	// prose.
	for i, ln := range strings.Split(want, "\n") {
		if trimmed := strings.TrimSpace(ln); trimmed != "" && !strings.HasPrefix(trimmed, "//") {
			t.Errorf("%s: policy block line %d is not a comment: %q", ref, i+1, ln)
		}
	}

	for _, path := range files[1:] {
		got := extractPolicyBlock(t, path)
		if got == want {
			continue
		}
		// Report the first line that differs rather than dumping two
		// hundred lines of prose twice: the failure a reader of this
		// output has is "which word moved", not "what does the block
		// say".
		wantLines := strings.Split(want, "\n")
		gotLines := strings.Split(got, "\n")
		for i := 0; i < len(wantLines) || i < len(gotLines); i++ {
			var w, g string
			if i < len(wantLines) {
				w = wantLines[i]
			}
			if i < len(gotLines) {
				g = gotLines[i]
			}
			if w == g {
				continue
			}
			t.Errorf("%s: policy block differs from %s at block line %d\n  %s: %q\n  %s: %q",
				path, ref, i+1, ref, w, path, g)
			break
		}
		if len(wantLines) != len(gotLines) {
			t.Errorf("%s: policy block is %d lines, %s has %d",
				path, len(gotLines), ref, len(wantLines))
		}
	}
}

// TestTenantPolicyBlockNamesEveryDialectItIsIn closes the other half of
// the same hole.
//
// The block opens by naming the files it is byte-identical in, and
// counts the dialects in prose further down — "the other three carry
// all of it". A fifth dialect that carries a correct copy of the block
// satisfies the comparison above and leaves both of those sentences
// false, in every copy at once. So the names in the opening sentence
// are compared against the dialects actually found.
func TestTenantPolicyBlockNamesEveryDialectItIsIn(t *testing.T) {
	dirs := dialectPackages(t)
	block := extractPolicyBlock(t, policyFileIn(t, dirs[0]))

	for _, dir := range dirs {
		named := policyFileIn(t, dir)
		if !strings.Contains(block, named) {
			t.Errorf("the policy block does not name %s among the files it is identical in — "+
				"a dialect the block does not count is one its prose is wrong about", named)
		}
	}
}

// TestTenantPolicyBlockSurfaceClaimsHold checks the sentences in the
// block that are claims about a dialect's surface rather than about
// the policy.
//
// Naming the differences inside the shared text is what lets one block
// be true in every dialect, but it means the block goes stale when a
// dialect grows the surface it is described as lacking — and a stale
// sentence in a block four packages point at is worse than four
// sentences each stale in one. So each claim is asserted here.
func TestTenantPolicyBlockSurfaceClaimsHold(t *testing.T) {
	dirs := dialectPackages(t)

	// "clickhouse models neither UPDATE nor DELETE ... so it has no
	// Update and no Patch". A mutation in ClickHouse is an
	// asynchronous, non-transactional ALTER TABLE … UPDATE/DELETE that
	// this package does not model.
	for _, method := range []string{"Update", "Patch", "PatchKey", "Delete"} {
		if entityHasMethod(t, "clickhouse", method) {
			t.Errorf("clickhouse grew Entity.%s: the policy block says this dialect has no %s, "+
				"and that sentence is now false in every tenant.go file", method, method)
		}
	}
	// The same sentence is a claim about the RAW builder too, and
	// section 2's rule for it — "db.Update may only RESTATE the axis"
	// — is about a method clickhouse does not have. A dialect that
	// grows DB.Update owes that rule an implementation, and one that
	// loses it makes the section-3 paragraph about Unscoped and the
	// SET list describe nothing.
	if pkgHasFunc(t, "clickhouse", "DB", "Update") {
		t.Error("clickhouse grew DB.Update: the policy block says this dialect models no UPDATE, " +
			"and the raw builder's axis rule now has to hold here too")
	}
	for _, dir := range dirs {
		if dir == "clickhouse" {
			continue
		}
		if !pkgHasFunc(t, dir, "DB", "Update") {
			t.Errorf("%s lost DB.Update: the policy block states what a raw UPDATE "+
				"may assign to the axis in every dialect that has one", dir)
		}
	}
	// The ones that do carry the write half must actually carry it, or
	// the block's "the other three carry all of it" is false. The set
	// is derived, so a fifth dialect is asked the same question rather
	// than exempted by not being on a list.
	for _, dir := range dirs {
		if dir == "clickhouse" {
			continue
		}
		for _, method := range []string{"Update", "PatchKey"} {
			if !entityHasMethod(t, dir, method) {
				t.Errorf("%s lost Entity.%s: the policy block says the non-clickhouse "+
					"dialects stamp on Update and refuse an axis op on Patch", dir, method)
			}
		}
	}
	// "only pg and clickhouse register validators. sqlite and mysql
	// have no Entity.Validate". That sentence is what makes the
	// stamp-before-validators rule above true in four packages rather
	// than in one, so it is asked of every dialect found and not of a
	// list: a fifth that grows Validate has to be given the ordering
	// and has to be counted here.
	validators := map[string]bool{"pg": true, "clickhouse": true}
	for _, dir := range dirs {
		has := entityHasMethod(t, dir, "Validate")
		switch {
		case has && !validators[dir]:
			t.Errorf("%s grew Entity.Validate: the policy block says only pg and clickhouse "+
				"register validators, and the stamp-before-validators rule now has to hold here too", dir)
		case !has && validators[dir]:
			t.Errorf("%s lost Entity.Validate: the policy block counts it among the dialects "+
				"the stamp-before-validators rule is about", dir)
		}
	}
	// "each dialect answers it in its own ident.go, in identKey". The
	// fold is the one rule in section 4 whose answer differs by
	// dialect, and it sits beside the quoting helpers so that the
	// tenancy rules around the block — sameTenant, namesAxis,
	// columnPath, isNilTenant, TenantFrom — stay byte-identical in
	// four packages. A dialect that moves the fold back into tenant.go
	// makes them diverge again by a function; one that declares it
	// twice has a fold whose answer depends on which declaration the
	// reader found; one that loses it leaves namesAxis comparing bytes
	// where its server compares columns, which is the defect section 4
	// is written about.
	for _, dir := range dirs {
		files := filesDeclaringFunc(t, dir, "identKey")
		switch {
		case len(files) == 0:
			t.Errorf("%s declares no identKey: the policy block says each dialect answers "+
				"\"the same name\" for its own server, and namesAxis has nothing to ask", dir)
		case len(files) > 1:
			t.Errorf("%s declares identKey in %d files (%v): the fold has no single answer",
				dir, len(files), files)
		case filepath.Base(files[0]) != "ident.go":
			t.Errorf("%s declares identKey in %s: the policy block says it lives in ident.go, "+
				"beside the quoting helpers and out of the rules that stay byte-identical",
				dir, filepath.Base(files[0]))
		}
	}
	// "RelConfig.Unscoped is pg's alone."
	for _, dir := range dirs {
		if dir == "pg" {
			continue
		}
		if pkgHasFunc(t, dir, "RelConfig", "Unscoped") {
			t.Errorf("%s grew RelConfig.Unscoped: the policy block says the relation-level "+
				"opt-out is pg's alone", dir)
		}
	}
	if !pkgHasFunc(t, "pg", "RelConfig", "Unscoped") {
		t.Error("pg lost RelConfig.Unscoped: the policy block names it as the relation-level opt-out")
	}
}

// entityHasMethod reports whether the package in dir declares a method
// with the given name on Entity[T].
func entityHasMethod(t *testing.T, dir, method string) bool {
	t.Helper()
	return pkgHasFunc(t, dir, "Entity", method)
}

// pkgHasFunc reports whether the package in dir declares a method named
// method on the named receiver type, ignoring pointer-ness and type
// parameters.
func pkgHasFunc(t *testing.T, dir, recv, method string) bool {
	t.Helper()
	for _, f := range parseDirFiles(t, dir) {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || len(fd.Recv.List) == 0 || fd.Name.Name != method {
				continue
			}
			if receiverTypeName(fd.Recv.List[0].Type) == recv {
				return true
			}
		}
	}
	return false
}

// filesDeclaringFunc returns the non-test files in dir that declare a
// top-level function called name, sorted. Plural on purpose: two
// declarations of one rule is a finding rather than an impossibility,
// since build tags can put a second one behind a constraint this
// parse does not evaluate.
func filesDeclaringFunc(t *testing.T, dir, name string) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}
	var out []string
	for _, p := range pkgs {
		for file, f := range p.Files {
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if ok && fd.Recv == nil && fd.Name.Name == name {
					out = append(out, file)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// receiverTypeName reduces a receiver expression to the bare type
// name: *Entity[T], Entity[T] and *RelConfig all reduce to the name
// the declaration is about.
func receiverTypeName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.StarExpr:
		return receiverTypeName(v.X)
	case *ast.IndexExpr:
		return receiverTypeName(v.X)
	case *ast.IndexListExpr:
		return receiverTypeName(v.X)
	case *ast.Ident:
		return v.Name
	}
	return ""
}
