package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/checker"
	"golang.org/x/tools/go/packages"

	"github.com/bernardoforcillo/drops/cmd/drops/dropslint"
)

// runLint is the front end to the static checks in dropslint. The
// rules themselves are go/analysis analyzers, so they also run under
// any driver that speaks that interface — golangci-lint, a
// unitchecker, `go vet -vettool`. This subcommand exists because most
// people will want them without wiring one up.
//
// It exits 3 when it finds something, which is the same answer drift
// gives: the command ran, and the answer was no.
func runLint(ctx context.Context, args []string) error {
	fs := newFlagSet("lint", "report query mistakes the type checker can see")
	off := fs.String("off", "", "comma-separated rules to skip (see the list below)")
	asJSON := fs.Bool("json", false, "emit findings as JSON instead of text")
	contextLines := fs.Int("context", 0, "lines of source around each finding; -1 prints none")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `drops lint — report query mistakes the type checker can see

Usage:
  drops lint [flags] [packages]     (packages default to ./...)

Rules:
`)
		for _, a := range dropslint.All() {
			fmt.Fprintf(os.Stderr, "  %-16s %s\n", a.Name, a.Doc)
		}
		fmt.Fprintf(os.Stderr, `
A deliberate offender says so in a comment on, or just above, the
reported line:

  %s ignore <rule>

Flags:
`, dropslint.Directive)
		fs.PrintDefaults()
	}
	// Unlike every other subcommand, lint takes operands: the package
	// patterns. So it parses its own flags rather than going through
	// parseFlags, which treats a leftover argument as a mistake.
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usageError{errFlagsReported}
	}

	analyzers, err := enabled(*off)
	if err != nil {
		return err
	}
	patterns := fs.Args()
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}

	pkgs, err := packages.Load(&packages.Config{
		Context: ctx,
		Mode:    loadMode,
		Tests:   true,
	}, patterns...)
	if err != nil {
		return fmt.Errorf("load %s: %w", strings.Join(patterns, " "), err)
	}
	if len(pkgs) == 0 {
		return usagef("no packages match %s", strings.Join(patterns, " "))
	}
	// A package that does not type-check tells the rules nothing —
	// they read types, not text — so say so rather than reporting a
	// clean run over code that was never analysed.
	if n := packages.PrintErrors(pkgs); n > 0 {
		return fmt.Errorf("%d package(s) failed to load; nothing was analysed", n)
	}

	graph, err := checker.Analyze(analyzers, pkgs, nil)
	if err != nil {
		return fmt.Errorf("analyse: %w", err)
	}
	findings, failures := collect(graph)
	// A rule that crashed reported nothing, and printing "clean"
	// afterwards would be the same lie as analysing a package that did
	// not compile.
	if len(failures) > 0 {
		for _, f := range failures {
			fmt.Fprintf(os.Stderr, "drops lint: %v\n", f)
		}
		return fmt.Errorf("%d rule run(s) failed; the report is incomplete", len(failures))
	}
	if *asJSON {
		return emitJSON(findings)
	}
	return emitText(findings, *contextLines)
}

// loadMode is what go/analysis needs of every package it is handed:
// syntax and types for the roots, and the same for their dependencies,
// because facts about a schema travel along the import edges.
const loadMode = packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
	packages.NeedImports | packages.NeedTypes | packages.NeedTypesSizes |
	packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedDeps | packages.NeedModule

// enabled resolves the -off list. An unknown rule name is a usage
// error rather than a silent no-op: a typo in CI that quietly turns
// nothing off is worse than a failed build.
func enabled(off string) ([]*analysis.Analyzer, error) {
	skip := map[string]bool{}
	for _, name := range strings.Split(off, ",") {
		if name = strings.TrimSpace(name); name != "" {
			skip[name] = true
		}
	}
	var out []*analysis.Analyzer
	for _, a := range dropslint.All() {
		if skip[a.Name] {
			delete(skip, a.Name)
			continue
		}
		out = append(out, a)
	}
	if len(skip) > 0 {
		names := make([]string, 0, len(skip))
		for n := range skip {
			names = append(names, n)
		}
		sort.Strings(names)
		return nil, usagef("unknown rule(s) in -off: %s", strings.Join(names, ", "))
	}
	if len(out) == 0 {
		return nil, usagef("-off turned every rule off")
	}
	return out, nil
}

// finding is one diagnostic, flattened so both output modes and the
// sort read the same fields — and comparable, so the de-duplication
// below is a map lookup.
type finding struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Rule    string `json:"rule"`
	Message string `json:"message"`
}

// collect flattens the analysis graph into findings, in the order a
// reader would walk the tree, and returns separately the actions that
// failed outright. Diagnostics are de-duplicated by position: a file
// compiled into both a package and its test variant is analysed twice
// and is still one mistake.
func collect(graph *checker.Graph) (findings []finding, failures []error) {
	seen := map[finding]bool{}
	var out []finding
	for act := range graph.All() {
		if act.Err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", act, act.Err))
			continue
		}
		if !act.IsRoot {
			continue
		}
		for _, diag := range act.Diagnostics {
			pos := act.Package.Fset.Position(diag.Pos)
			f := finding{
				File:    pos.Filename,
				Line:    pos.Line,
				Column:  pos.Column,
				Rule:    act.Analyzer.Name,
				Message: diag.Message,
			}
			if seen[f] {
				continue
			}
			seen[f] = true
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.File != b.File:
			return a.File < b.File
		case a.Line != b.Line:
			return a.Line < b.Line
		case a.Column != b.Column:
			return a.Column < b.Column
		}
		return a.Rule < b.Rule
	})
	return out, failures
}

func emitText(findings []finding, contextLines int) error {
	if len(findings) == 0 {
		fmt.Println("clean: no findings")
		return nil
	}
	for _, f := range findings {
		fmt.Printf("%s:%d:%d: [%s] %s\n", relPath(f.File), f.Line, f.Column, f.Rule, f.Message)
		if contextLines >= 0 {
			printContext(f, contextLines)
		}
	}
	byRule := map[string]int{}
	for _, f := range findings {
		byRule[f.Rule]++
	}
	rules := make([]string, 0, len(byRule))
	for r := range byRule {
		rules = append(rules, r)
	}
	sort.Strings(rules)
	parts := make([]string, 0, len(rules))
	for _, r := range rules {
		parts = append(parts, fmt.Sprintf("%s %d", r, byRule[r]))
	}
	return findingError{fmt.Errorf("%d finding(s): %s", len(findings), strings.Join(parts, ", "))}
}

// printContext echoes the offending line, and n lines either side of
// it, so a finding read out of a CI log still shows the query.
func printContext(f finding, n int) {
	src, err := os.ReadFile(f.File)
	if err != nil {
		return
	}
	lines := strings.Split(string(src), "\n")
	lo, hi := f.Line-1-n, f.Line-1+n
	if lo < 0 {
		lo = 0
	}
	if hi >= len(lines) {
		hi = len(lines) - 1
	}
	for i := lo; i <= hi; i++ {
		marker := "  "
		if i == f.Line-1 {
			marker = "> "
		}
		fmt.Printf("    %s%s\n", marker, lines[i])
	}
}

func emitJSON(findings []finding) error {
	if findings == nil {
		findings = []finding{}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(findings); err != nil {
		return err
	}
	if len(findings) == 0 {
		return nil
	}
	return findingError{fmt.Errorf("%d finding(s)", len(findings))}
}

// relPath shortens an absolute path against the working directory, so
// output is the same length whoever runs it.
func relPath(path string) string {
	wd, err := os.Getwd()
	if err != nil {
		return path
	}
	if rel := strings.TrimPrefix(path, wd+string(os.PathSeparator)); rel != path {
		return rel
	}
	return path
}
