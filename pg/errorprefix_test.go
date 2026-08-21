package pg_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A sentinel in this package spells the package into its own message —
// errors.New("drops/pg: a FULL JOIN cannot carry ...") — so a wrapper
// that writes the prefix again hands the caller
// "drops/pg: drops/pg: a FULL JOIN cannot carry ...". Three of them had
// shipped, in the two places a scoped statement is refused and in the
// CTE name check, which is to say in messages that only ever appear
// when something already went wrong and nobody was reading.
//
// TestPackageErrorsCarryThePrefixExactlyOnce pins the three by running
// them. This one is why there will not be a fourth: it reads the
// package source, and the rule it enforces needs no list — a sentinel
// is recognised by carrying the prefix in its own text, and a wrapper
// by writing the prefix in front of a %w that names one.
func TestNoWrapperDoublesThePackagePrefix(t *testing.T) {
	const prefix = "drops/pg: "

	// Every package-level error whose own message already carries the
	// prefix. Derived from the text, so a sentinel added tomorrow is
	// covered by the same rule that covers today's.
	sentinels := map[string]bool{}
	forEachPgFile(t, func(f *ast.File) {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, sp := range gd.Specs {
				vs, ok := sp.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					if lit := firstStringLit(vs.Values[i]); strings.HasPrefix(lit, prefix) {
						sentinels[name.Name] = true
					}
				}
			}
		}
	})
	for _, want := range []string{"ErrFullJoinScoped", "ErrTenantMissing", "ErrCTENameConflict"} {
		if !sentinels[want] {
			t.Fatalf("%s is no longer a prefixed sentinel — this check has gone stale", want)
		}
	}

	var doubled []string
	forEachPgFile(t, func(f *ast.File) {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Errorf" {
				return true
			}
			format := firstStringLit(call.Args[0])
			if !strings.HasPrefix(format, prefix) || !strings.Contains(format, "%w") {
				return true
			}
			for _, a := range call.Args[1:] {
				id, ok := a.(*ast.Ident)
				if !ok || !sentinels[id.Name] {
					continue
				}
				doubled = append(doubled, prefixFset.Position(call.Pos()).String()+": wraps "+id.Name)
			}
			return true
		})
	})
	sort.Strings(doubled)
	for _, d := range doubled {
		t.Errorf("%s, which already carries %q — drop the prefix from the wrapper", d, strings.TrimSuffix(prefix, " "))
	}
}

// forEachPgFile parses the non-test sources of package pg and hands
// each file to fn. It is a parse of its own rather than a field added
// to pgSyntax because pgSyntax indexes declarations and this needs the
// message text of a var and the format string of a call, which live in
// the files.
func forEachPgFile(t *testing.T, fn func(*ast.File)) {
	t.Helper()
	pkgs, err := parser.ParseDir(prefixFset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	seen := 0
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			seen++
			fn(f)
		}
	}
	if seen == 0 {
		t.Fatal("the package parsed to no files at all — this check has gone stale")
	}
}

// prefixFset is shared by the two walks below so a reported position
// names the file it came from.
var prefixFset = token.NewFileSet()

// firstStringLit returns the value of e when it is a string literal, or
// of the first string literal inside a call such as errors.New(...).
func firstStringLit(e ast.Expr) string {
	var out string
	ast.Inspect(e, func(n ast.Node) bool {
		if out != "" {
			return false
		}
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if v, err := strconv.Unquote(lit.Value); err == nil {
			out = v
		}
		return false
	})
	return out
}
