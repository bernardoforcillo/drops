package dropslint

import (
	"go/ast"
	"go/token"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// Directive is the comment form every rule answers to.
//
//	//drops:lint ignore [rule,rule]
//	//drops:lint lookup
//
// It is spelled like Go's own //go: and //lint: directives — no space
// after the slashes — so gofmt keeps it flush against the declaration
// it belongs to instead of re-indenting it as prose.
const Directive = "//drops:lint"

// verbs a directive may carry.
const (
	verbIgnore = "ignore"
	verbLookup = "lookup"
)

// suppressions is a file's directives, indexed so a rule can ask about
// a position without re-reading comments.
type suppressions struct {
	fset *token.FileSet
	// byLine maps a source line to the rules silenced there. The
	// empty string stands for "every rule".
	byLine map[lineKey]map[string]bool
	// funcs are function bodies whose doc comment silences rules
	// throughout them.
	funcs []funcScope
}

type lineKey struct {
	file string
	line int
}

type funcScope struct {
	start, end token.Pos
	rules      map[string]bool
}

func newSuppressions(pass *analysis.Pass) *suppressions {
	s := &suppressions{fset: pass.Fset, byLine: map[lineKey]map[string]bool{}}
	for _, f := range pass.Files {
		for _, group := range f.Comments {
			for _, c := range group.List {
				rules, ok := parseIgnore(c.Text)
				if !ok {
					continue
				}
				pos := pass.Fset.Position(c.Pos())
				k := lineKey{pos.Filename, pos.Line}
				if s.byLine[k] == nil {
					s.byLine[k] = map[string]bool{}
				}
				for r := range rules {
					s.byLine[k][r] = true
				}
			}
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Doc == nil || fn.Body == nil {
				continue
			}
			for _, c := range fn.Doc.List {
				if rules, ok := parseIgnore(c.Text); ok {
					s.funcs = append(s.funcs, funcScope{fn.Body.Pos(), fn.Body.End(), rules})
				}
			}
		}
	}
	return s
}

// parseIgnore reads an ignore directive. A directive naming no rule
// silences all of them, which is recorded as the empty rule name.
//
// The rule list is the first field after the verb; whatever follows is
// the reason, in the author's own words:
//
//	//drops:lint ignore unfilteredwrite — the table is a scratch buffer
//
// which is where a reviewer looks for it, and is the reason the
// directive is preferable to a builder method that could not carry
// one.
func parseIgnore(text string) (map[string]bool, bool) {
	verb, rest, ok := parseDirective(text)
	if !ok || verb != verbIgnore {
		return nil, false
	}
	out := map[string]bool{}
	if rest == "" {
		out[""] = true
		return out, true
	}
	list, _, _ := strings.Cut(rest, " ")
	for _, name := range strings.Split(list, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out[name] = true
		}
	}
	if len(out) == 0 {
		out[""] = true
	}
	return out, true
}

// parseDirective splits a //drops:lint comment into its verb and the
// rest of the line. Anything else answers false.
func parseDirective(text string) (verb, rest string, ok bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, Directive) {
		return "", "", false
	}
	body := strings.TrimSpace(strings.TrimPrefix(text, Directive))
	verb, rest, _ = strings.Cut(body, " ")
	return verb, strings.TrimSpace(rest), true
}

// silenced reports whether a directive covers this position for this
// rule. The directive is honoured on the reported line, on the line
// above it — where a reader naturally writes one — and in the
// enclosing function's doc comment.
func (s *suppressions) silenced(pos token.Pos, rule string) bool {
	p := s.fset.Position(pos)
	for _, line := range [...]int{p.Line, p.Line - 1} {
		if rules := s.byLine[lineKey{p.Filename, line}]; rules[rule] || rules[""] {
			return true
		}
	}
	for _, fn := range s.funcs {
		if pos >= fn.start && pos < fn.end && (fn.rules[rule] || fn.rules[""]) {
			return true
		}
	}
	return false
}

// hasVerb reports whether a doc comment carries the given directive
// verb. Used for //drops:lint lookup on a table declaration.
func hasVerb(doc *ast.CommentGroup, verb string) bool {
	if doc == nil {
		return false
	}
	for _, c := range doc.List {
		if v, _, ok := parseDirective(c.Text); ok && v == verb {
			return true
		}
	}
	return false
}

// reporter drops a diagnostic unless a directive covers it, and never
// reports the same position twice — a builder executed in two places
// is one mistake, not two.
type reporter struct {
	pass  *analysis.Pass
	rule  string
	supp  *suppressions
	seen  map[token.Pos]bool
	count int
}

func newReporter(pass *analysis.Pass, rule string) *reporter {
	return &reporter{pass: pass, rule: rule, supp: newSuppressions(pass), seen: map[token.Pos]bool{}}
}

func (r *reporter) report(pos token.Pos, msg string) {
	if r.seen[pos] || r.supp.silenced(pos, r.rule) {
		return
	}
	r.seen[pos] = true
	r.count++
	r.pass.Report(analysis.Diagnostic{Pos: pos, Category: r.rule, Message: msg})
}
