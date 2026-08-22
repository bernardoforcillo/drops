package drops_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
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
// The four tenant.go files now carry a delimited block that is
// byte-identical in all four, and this test is what keeps it that way.
// It fails when one of the four drifts by a word, by whitespace, or by
// reordering, because it compares the extracted bytes rather than
// anything normalised.
//
// The block also names what each dialect cannot do — clickhouse models
// neither UPDATE nor DELETE, RelConfig.Unscoped is pg's alone — so
// that the same words are true in all four rather than four sets of
// words each true in one. Those sentences are claims about the code,
// and TestTenantPolicyBlockSurfaceClaimsHold below checks them: a
// dialect that grows the surface the block says it lacks makes the
// block false, and the block is the thing four packages point at.

const (
	policyStartMarker = "// ==== THE TENANT POLICIES — NORMATIVE ===="
	policyEndMarker   = "// ==== END OF THE TENANT POLICIES ===="
)

// policyFiles are the four files that must carry the same block. The
// list is spelled out rather than globbed: a dialect that stops
// carrying the block should fail this test, and a glob over whatever
// tenant.go files happen to exist would quietly stop checking it.
var policyFiles = []string{
	"pg/tenant.go",
	"sqlite/tenant.go",
	"mysql/tenant.go",
	"clickhouse/tenant.go",
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

// TestTenantPolicyBlockIsIdenticalInAllFourDialects is the mechanical
// check that keeps the policies from drifting apart again.
//
// pg is the reference only because the comparison needs one; the block
// is not pg's to change alone, and a deliberate edit means editing all
// four in the same commit.
func TestTenantPolicyBlockIsIdenticalInAllFourDialects(t *testing.T) {
	const ref = "pg/tenant.go"
	want := extractPolicyBlock(t, ref)

	if strings.TrimSpace(want) == "" {
		t.Fatalf("%s: the policy block is empty", ref)
	}
	// A block that shrank to its markers would compare equal in all
	// four and assert nothing. The policies do not fit in ten lines.
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

	for _, path := range policyFiles {
		if path == ref {
			continue
		}
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

// TestTenantPolicyBlockSurfaceClaimsHold checks the sentences in the
// block that are claims about a dialect's surface rather than about
// the policy.
//
// Naming the differences inside the shared text is what lets one block
// be true in four packages, but it means the block goes stale when a
// dialect grows the surface it is described as lacking — and a stale
// sentence in a block four packages point at is worse than four
// sentences each stale in one. So each claim is asserted here.
func TestTenantPolicyBlockSurfaceClaimsHold(t *testing.T) {
	// "clickhouse models neither UPDATE nor DELETE ... so it has no
	// Update and no Patch". A mutation in ClickHouse is an
	// asynchronous, non-transactional ALTER TABLE … UPDATE/DELETE that
	// this package does not model.
	for _, method := range []string{"Update", "Patch", "PatchKey", "Delete"} {
		if entityHasMethod(t, "clickhouse", method) {
			t.Errorf("clickhouse grew Entity.%s: the policy block says this dialect has no %s, "+
				"and that sentence is now false in all four tenant.go files", method, method)
		}
	}
	// The three that do carry the write half must actually carry it,
	// or the block's "the other three carry all of it" is false.
	for _, pkg := range []string{"pg", "sqlite", "mysql"} {
		for _, method := range []string{"Update", "PatchKey"} {
			if !entityHasMethod(t, pkg, method) {
				t.Errorf("%s lost Entity.%s: the policy block says the three non-clickhouse "+
					"dialects stamp on Update and refuse an axis op on Patch", pkg, method)
			}
		}
	}
	// "RelConfig.Unscoped is pg's alone."
	for _, pkg := range []string{"sqlite", "mysql", "clickhouse"} {
		if pkgHasFunc(t, pkg, "RelConfig", "Unscoped") {
			t.Errorf("%s grew RelConfig.Unscoped: the policy block says the relation-level "+
				"opt-out is pg's alone", pkg)
		}
	}
	if !pkgHasFunc(t, "pg", "RelConfig", "Unscoped") {
		t.Error("pg lost RelConfig.Unscoped: the policy block names it as the relation-level opt-out")
	}
}

// entityHasMethod reports whether pkg declares a method with the given
// name on Entity[T].
func entityHasMethod(t *testing.T, pkg, method string) bool {
	t.Helper()
	return pkgHasFunc(t, pkg, "Entity", method)
}

// pkgHasFunc reports whether pkg declares a method named method on the
// named receiver type, ignoring pointer-ness and type parameters.
func pkgHasFunc(t *testing.T, pkg, recv, method string) bool {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, pkg, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", pkg, err)
	}
	for _, p := range pkgs {
		for _, f := range p.Files {
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
	}
	return false
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
