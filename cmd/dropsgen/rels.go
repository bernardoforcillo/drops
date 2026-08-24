package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Eager-load shapes, generated from the relation declarations.
//
// Rows mode emits what a SELECT of one table hands back. This emits
// what an eager load hands back, which is a different struct and the
// one a reader is most likely to get wrong:
//
//	type UsersWithPosts struct {
//	    UsersRow
//	    Posts []PostsRow `drop:"-" dropRel:"posts"`
//	}
//
// Four things there have to agree with declarations that live
// somewhere else — the field name, the slice-versus-pointer choice,
// the dropRel tag and the nested type — and three of them fail
// silently. A misspelled tag is a relation that does not load, which
// reads exactly like a parent with no children.
//
// The shape follows the loader, not the declaration. Where the two
// could be read differently, pg/find.go is the authority:
//
//   - HasMany, ManyToMany and MorphMany fill a slice, and the loader
//     refuses anything else. A parent with no children is assigned an
//     empty non-nil slice, so len() is the answer and a nil slice
//     means nobody loaded it.
//
//   - HasOne and BelongsTo fill a pointer. The loader leaves the field
//     untouched when no row matches, so a value struct would come back
//     zeroed and a zeroed struct is a row of zeros — the absence a
//     nil pointer states is one it cannot state.
//
//   - MorphTo fills an interface. The loader requires interface kind
//     because the concrete type varies row by row, and it refuses to
//     descend past one for the same reason, so a path through a
//     MorphTo is refused here too rather than generating a struct no
//     query can use.
//
// Three decisions this mode had to make, and why:
//
// How deep. Exactly as deep as the paths the caller asks for, and no
// deeper. "Every relation" is not offered: any schema with a
// back-reference makes it an infinite type, and even truncated at one
// level it is wrong in a way that costs something — a struct that
// declares a relation the query does not load is refused outright
// under StrictLoading, so generating more than was asked for breaks
// the query it was generated for. The paths are the same strings
// With() takes, deliberately: the shape and the query that fills it
// are written from one spelling.
//
// What the structs are called. A shape is named for the table and the
// paths it carries — users plus "posts.comments" is
// UsersWithPostsComments — so the name is derivable by a reader and
// stable across runs. A node with no relations beneath it is not a
// new struct at all: it is the <Table>Row rows mode already emits,
// which is why the two modes belong together. Where the derived name
// is not the one a codebase wants, the shape can be named outright:
// -shape 'AuthorWithBooks=authors:books'.
//
// What happens when the struct already exists. It is skipped and the
// header says which name and which file, because two declarations of
// one name in one package do not compile and overwriting somebody's
// struct is not a generator's call to make — but only when the struct
// already there is the one this run would have written.
//
// That last clause is the whole of the naming scheme's soundness. A
// derived name erases where a path was split: users plus
// "comments.posts" and users plus "comments,posts" are two different
// trees and one string. Asked for both in one run the generator sees
// the clash and refuses. Asked for them in two runs writing two files
// it would see only the second, find the name declared, and stand
// aside — leaving the caller with no error and a name that resolves
// to a struct carrying different relations, which is a query that
// loads the wrong thing or is refused outright under StrictLoading.
//
// The fix is not a longer name. The names are already at the limit of
// what anybody will read — UsersWithManagerPostsPostsCommentsPostsTagsProfile
// is a real output — and encoding the tree unambiguously into one Go
// identifier can only add to that. The package's own declarations are
// the manifest instead: the fields of every struct it already
// declares are read back and compared with the fields this run would
// have written, and a disagreement is the same refusal the in-run
// collision gets, with the same way out. Nothing new to check in, no
// file to keep in sync, and no name a character longer.

// relsFileSuffix names the generated shape file. Rows mode knows it
// too: a shape nests the row structs, so the two files are written
// in an order, and the one that comes first has to move the other
// out of the way while it compiles the package. That is also why -o
// has to end in it: a shape file under some other name is one rows
// mode will not know to move, and the package would lose the ability
// to regenerate its row structs.
const relsFileSuffix = "_drops_rels.go"

// relsTable is one table as the bridge reports it.
type relsTable struct {
	Name      string
	Schema    string
	Columns   []string
	Relations []relsRelation
}

// relsRelation is one declared edge. Target is the schema-qualified
// name of the table it points at, empty for a MorphTo.
type relsRelation struct {
	Name   string
	Kind   string
	Target string
}

// relsSpec is one -shape argument, parsed.
type relsSpec struct {
	Name  string   // explicit struct name, empty to derive one
	Table string   // schema-qualified table name, as the user wrote it
	Paths []string // relation paths, sorted and deduplicated
	Raw   string   // the argument itself, for error messages
}

// rename is the spec as an argument that a struct name can be put in
// front of. It is rebuilt from the parsed parts rather than sliced out
// of Raw, because a spec that already carries a name would otherwise
// suggest an argument with two of them.
func (s relsSpec) rename() string {
	return s.Table + ":" + strings.Join(s.Paths, ",")
}

// relsNode is one node of a shape's tree. The root has no name and no
// kind; every other node is reached by the edge that names it.
type relsNode struct {
	name     string
	kind     string
	table    *relsTable
	children []*relsNode
}

// runRels is the CLI entry point for eager-load shape generation.
func runRels(pattern string, shapes []string, outName string) error {
	if outName != "" && filepath.Base(outName) != outName {
		return fmt.Errorf("-o takes a file name, not a path: the generated file belongs in the schema package's own directory")
	}
	// The suffix is not a convention, it is how rows mode finds these
	// files to move them aside while it compiles the package. A shape
	// file named anything else is one it will not know about, and the
	// package loses the ability to regenerate its row structs at all.
	if outName != "" && !strings.HasSuffix(outName, relsFileSuffix) {
		return fmt.Errorf("-o %s: a shape file's name has to end in %s, because that is what `dropsgen -rows` moves aside while it compiles the package — a shape only compiles while the row structs it names exist, so one named anything else stops -rows from ever running here again", outName, relsFileSuffix)
	}
	ctx := context.Background()
	pkg, err := locateRowsPackage(ctx, pattern)
	if err != nil {
		return err
	}
	specs, err := parseShapeSpecs(shapes)
	if err != nil {
		return err
	}
	if outName == "" {
		outName = pkg.Name + relsFileSuffix
	}
	out := filepath.Join(pkg.Dir, outName)

	// As in rows mode: the previous output is moved aside before the
	// bridge compiles the package, so a generated file that has gone
	// stale cannot break the run that would have fixed it.
	restore, err := stash(out)
	if err != nil {
		return err
	}
	// Any *other* shape file in the package goes the same way while
	// the bridge compiles: it names row structs and shape structs
	// this run may be about to change, and one stale file is enough
	// to stop the package compiling. They are put back either way —
	// this run only owns the one it writes.
	restoreOthers, err := stashShapes(pkg)
	if err != nil {
		restore()
		return err
	}
	defer restoreOthers()
	tables, err := runRelsBridge(ctx, pkg)
	if err != nil {
		restore()
		return err
	}
	// The bridge is done, so the other shape files go back before the
	// package's declarations are read. They only had to be out of the
	// way while it compiled, and the names they declare are exactly
	// the ones this run must not declare a second time — a package
	// may hold more than one shape file, because -o names the output.
	restoreOthers()
	if len(specs) == 0 {
		restore()
		return fmt.Errorf("-rels needs at least one -shape saying which relations to generate a struct for.\n%s", shapeMenu(tables))
	}
	taken, err := declaredNames(pkg.Dir, pkg.Name)
	if err != nil {
		restore()
		return err
	}
	// And what those declarations *are*, not just that they exist. A
	// name already taken is only one to stand aside for when the
	// struct behind it is the struct this run would have written.
	declared, err := declaredShapeFields(pkg.Dir, pkg.Name)
	if err != nil {
		restore()
		return err
	}
	src, err := emitRels(pkg, tables, specs, taken, declared)
	if err != nil {
		restore()
		return err
	}
	return os.WriteFile(out, src, 0o644)
}

// runRelsBridge writes the generated program, runs it with `go run`,
// and decodes its reply. See runRowsBridge — this is the same dance
// against the same package, asking a different question.
func runRelsBridge(ctx context.Context, pkg *rowsPackage) ([]relsTable, error) {
	dir, err := os.MkdirTemp(pkg.ModuleDir, ".dropsgen-rels-")
	if err != nil {
		return nil, fmt.Errorf("create the directory for the generated program: %w", err)
	}
	defer os.RemoveAll(dir)

	main := filepath.Join(dir, "main.go")
	src := strings.ReplaceAll(relsBridgeTemplate, "$IMPORT$", pkg.ImportPath)
	if err := os.WriteFile(main, []byte(src), 0o644); err != nil {
		return nil, fmt.Errorf("write the generated program: %w", err)
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "go", "run", main)
	cmd.Dir = pkg.ModuleDir
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("evaluating the schema in %s failed: %v\n%s", pkg.ImportPath, err, strings.TrimSpace(stderr.String()))
	}
	var tables []relsTable
	if err := json.Unmarshal(stdout.Bytes(), &tables); err != nil {
		return nil, fmt.Errorf("the generated program printed something that is not a table listing: %w", err)
	}
	return tables, nil
}

// shapeMenu lists what could be asked for. A schema is the one place
// that knows which relations exist, and the caller who has to name
// one is the least likely to have it memorised.
func shapeMenu(tables []relsTable) string {
	var b strings.Builder
	b.WriteString("The schema declares:\n")
	any := false
	for _, t := range tables {
		if len(t.Relations) == 0 {
			continue
		}
		any = true
		names := make([]string, 0, len(t.Relations))
		for _, r := range t.Relations {
			names = append(names, r.Name)
		}
		fmt.Fprintf(&b, "    -shape '%s:%s'\n", qualifiedName(t), strings.Join(names, ","))
	}
	if !any {
		return "No table in the schema declares a relation, so there is no shape to generate — declare them with pg.NewRelations first."
	}
	b.WriteString("\nA path may go deeper: 'users:posts.comments' generates the struct a\n")
	b.WriteString("With(\"posts.comments\") query fills.")
	return b.String()
}

// qualifiedName is how a table is identified on the command line and
// in error messages.
func qualifiedName(t relsTable) string {
	if t.Schema != "" {
		return t.Schema + "." + t.Name
	}
	return t.Name
}

// parseShapeSpecs parses the -shape arguments.
//
// It does not order them: the structs are written in name order, so
// the same set of shapes produces the same file however the arguments
// were arranged.
func parseShapeSpecs(shapes []string) ([]relsSpec, error) {
	out := make([]relsSpec, 0, len(shapes))
	for _, raw := range shapes {
		spec, err := parseShapeSpec(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, spec)
	}
	return out, nil
}

// parseShapeSpec parses one "[Name=]table:path[,path...]" argument.
func parseShapeSpec(raw string) (relsSpec, error) {
	const form = `-shape takes '[Name=]table:path[,path...]', e.g. -shape 'users:posts.comments,profile' or -shape 'AuthorWithBooks=authors:books'`
	spec := relsSpec{Raw: raw}
	rest := strings.TrimSpace(raw)
	colon := strings.IndexByte(rest, ':')
	if colon < 0 {
		return spec, fmt.Errorf("shape %q names no relations: %s", raw, form)
	}
	// An '=' only introduces a struct name when it comes before the
	// colon; past it the argument is relation paths, which cannot
	// contain one anyway.
	if eq := strings.IndexByte(rest, '='); eq >= 0 && eq < colon {
		spec.Name = strings.TrimSpace(rest[:eq])
		if !token.IsIdentifier(spec.Name) || token.IsKeyword(spec.Name) {
			return spec, fmt.Errorf("shape %q: %q is not a Go type name", raw, spec.Name)
		}
		rest = rest[eq+1:]
		colon = strings.IndexByte(rest, ':')
	}
	spec.Table = strings.TrimSpace(rest[:colon])
	if spec.Table == "" {
		return spec, fmt.Errorf("shape %q names no table: %s", raw, form)
	}
	if strings.TrimSpace(rest[colon+1:]) == "" {
		return spec, fmt.Errorf("shape %q names no relations: %s", raw, form)
	}
	seen := map[string]bool{}
	for _, p := range strings.Split(rest[colon+1:], ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			return spec, fmt.Errorf("shape %q has an empty relation path: %s", raw, form)
		}
		for _, seg := range strings.Split(p, ".") {
			if strings.TrimSpace(seg) == "" {
				return spec, fmt.Errorf("shape %q: %q is not a relation path — segments are separated by a single dot", raw, p)
			}
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		spec.Paths = append(spec.Paths, p)
	}
	if len(spec.Paths) == 0 {
		return spec, fmt.Errorf("shape %q names no relations: %s", raw, form)
	}
	sort.Strings(spec.Paths)
	return spec, nil
}

// buildRelsTree resolves one spec's paths against the schema graph.
// Every relation name is checked here, before anything is emitted, so
// a typo is reported as a typo rather than as a struct that does not
// compile.
func buildRelsTree(spec relsSpec, byName map[string]*relsTable) (*relsNode, error) {
	root, ok := byName[spec.Table]
	if !ok {
		known := make([]string, 0, len(byName))
		for name := range byName {
			known = append(known, name)
		}
		sort.Strings(known)
		return nil, fmt.Errorf("shape %q: the schema registers no table %q; it registers: %s",
			spec.Raw, spec.Table, strings.Join(known, ", "))
	}
	node := &relsNode{table: root}
	for _, path := range spec.Paths {
		cur := node
		walked := ""
		for _, seg := range strings.Split(path, ".") {
			if walked != "" {
				walked += "."
			}
			walked += seg
			if cur.kind == "morphTo" {
				// pg.validateRelTree refuses this before it runs a
				// query, so a struct for it could never be filled.
				return nil, fmt.Errorf("shape %q: %q goes through the MorphTo relation %q, and the loader refuses to descend past one because the loaded parent's type varies row by row",
					spec.Raw, path, cur.name)
			}
			rel := findRel(cur.table, seg)
			if rel == nil {
				return nil, fmt.Errorf("shape %q: table %q declares no relation %q; it declares: %s",
					spec.Raw, qualifiedName(*cur.table), seg, relNames(cur.table))
			}
			child := findChild(cur, seg)
			if child == nil {
				target := (*relsTable)(nil)
				if rel.Kind != "morphTo" {
					target, ok = byName[rel.Target]
					if !ok {
						return nil, fmt.Errorf("shape %q: relation %q on table %q targets table %q, which Schema() does not register — add it there, or this generator has no row struct to nest",
							spec.Raw, seg, qualifiedName(*cur.table), rel.Target)
					}
				}
				child = &relsNode{name: seg, kind: rel.Kind, table: target}
				cur.children = append(cur.children, child)
				sort.Slice(cur.children, func(i, j int) bool { return cur.children[i].name < cur.children[j].name })
			}
			cur = child
		}
	}
	return node, nil
}

func findRel(t *relsTable, name string) *relsRelation {
	for i := range t.Relations {
		if t.Relations[i].Name == name {
			return &t.Relations[i]
		}
	}
	return nil
}

func findChild(n *relsNode, name string) *relsNode {
	for _, c := range n.children {
		if c.name == name {
			return c
		}
	}
	return nil
}

func relNames(t *relsTable) string {
	if len(t.Relations) == 0 {
		return "none"
	}
	names := make([]string, 0, len(t.Relations))
	for _, r := range t.Relations {
		names = append(names, r.Name)
	}
	return strings.Join(names, ", ")
}

// subPaths returns every relation path beneath n, sorted. It is what
// a node's name and its identity are derived from: two nodes over the
// same table carrying the same paths generate the same struct.
func subPaths(n *relsNode) []string {
	var out []string
	for _, c := range n.children {
		below := subPaths(c)
		if len(below) == 0 {
			out = append(out, c.name)
			continue
		}
		for _, p := range below {
			out = append(out, c.name+"."+p)
		}
	}
	sort.Strings(out)
	return out
}

// shapeTypeName is the type a node is rendered as. A node with no
// relations beneath it is the row struct rows mode already emits;
// anything else is a struct named for its table and its paths.
func shapeTypeName(n *relsNode) string {
	base := goIdentFor(n.table.Name)
	paths := subPaths(n)
	if len(paths) == 0 {
		return base + "Row"
	}
	var b strings.Builder
	b.WriteString(base)
	b.WriteString("With")
	for _, p := range paths {
		for _, seg := range strings.Split(p, ".") {
			b.WriteString(goIdentFor(seg))
		}
	}
	return b.String()
}

// relFieldType is the field type for one relation, and it is the
// single most load-bearing decision in this file. See the notes at
// the top: the loader fills a slice for the many-kinds, a pointer for
// the one-kinds and an interface for a MorphTo, and it errors on
// anything else.
func relFieldType(child *relsNode) (string, error) {
	switch child.kind {
	case "hasMany", "manyToMany", "morphMany":
		return "[]" + shapeTypeName(child), nil
	case "hasOne", "belongsTo":
		return "*" + shapeTypeName(child), nil
	case "morphTo":
		return "any", nil
	}
	return "", fmt.Errorf("relation %q has kind %q, which this generator does not know how to shape", child.name, child.kind)
}

// relTagLiteral writes the two tags a relation field carries.
//
// dropRel is the one the loader looks for. drop:"-" is not
// decoration: without it the field competes for a column name — the
// scanner claims a field's own name and its camelCase form — and it
// competes at depth 0, where it outranks the real column promoted
// from the embedded row struct. A table with a column named like one
// of its relations would otherwise scan that column into a slice.
func relTagLiteral(name string) string {
	tag := `drop:"-" dropRel:` + strconv.Quote(name)
	if strings.Contains(tag, "`") {
		return strconv.Quote(tag)
	}
	return "`" + tag + "`"
}

// relsDef is one struct the generator will write.
type relsDef struct {
	Name string
	Doc  []string
	Body []string
	From string // the shape that produced it, quoted, for collision messages
	// Rename is the shape rewritten as an argument a name can be put
	// in front of: unquoted, and with any name it already carries
	// dropped, so what the collision message suggests is a -shape the
	// generator would accept.
	Rename string
	// Root says whether this struct is the shape's own, or a nested
	// one named for its table and the paths beneath it. Only the root
	// can be renamed on the command line, so only the root gets told
	// to.
	Root bool
}

// emitRels renders the generated file.
func emitRels(pkg *rowsPackage, tables []relsTable, specs []relsSpec, taken map[string]string, declared map[string][]string) ([]byte, error) {
	byName := make(map[string]*relsTable, len(tables))
	for i := range tables {
		byName[qualifiedName(tables[i])] = &tables[i]
	}

	var (
		order   []string
		defs    = map[string]*relsDef{}
		skipped []string
		needRow = map[string]bool{}
	)
	// define records one struct, reconciling a node reached from two
	// shapes. Identical bodies are one struct; different bodies under
	// one name are the collision the naming scheme cannot resolve on
	// the caller's behalf.
	define := func(def *relsDef) error {
		if prev, ok := defs[def.Name]; ok {
			if strings.Join(prev.Body, "\n") == strings.Join(def.Body, "\n") {
				return nil
			}
			// The suggestion is a command line, so the shape goes in
			// unquoted: %q here would nest a second pair of quotes
			// inside the -shape argument and the table name would
			// come out with a quote in it.
			return fmt.Errorf("shapes %s and %s both generate %s, with different fields; name one of them outright, as -shape 'SomeName=%s'",
				prev.From, def.From, def.Name, def.Rename)
		}
		defs[def.Name] = def
		order = append(order, def.Name)
		return nil
	}

	for _, spec := range specs {
		root, err := buildRelsTree(spec, byName)
		if err != nil {
			return nil, err
		}
		name := spec.Name
		if name == "" {
			name = shapeTypeName(root)
		}
		collectRowNames(root, needRow)
		if err := defineRelsNodes(root, name, true, spec, taken, define); err != nil {
			return nil, err
		}
	}

	// Every shape nests a row struct — embedded at a node with
	// relations beneath it, named outright at a leaf — and rows mode
	// is what declares those. Saying so here beats a compile error
	// naming a type the reader never asked for.
	var missing []string
	for row := range needRow {
		if _, ok := taken[row]; !ok {
			missing = append(missing, row)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("this package declares no %s — an eager-load shape nests the row struct of every table it reaches, so run `dropsgen -rows` on this package first",
			strings.Join(missing, ", "))
	}

	sort.Strings(order)
	var kept []string
	for _, name := range order {
		prev, ok := taken[name]
		if !ok {
			kept = append(kept, name)
			continue
		}
		fields, isStruct := declared[name]
		same, err := sameShape(fields, defs[name])
		if err != nil {
			return nil, err
		}
		if !same {
			return nil, collisionWithDeclaration(defs[name], prev, isStruct)
		}
		skipped = append(skipped, skipNote(name, prev))
	}

	var b bytes.Buffer
	b.WriteString("// Code generated by dropsgen. DO NOT EDIT.\n//\n")
	b.WriteString("// One struct per eager-load shape, generated from the relation\n")
	b.WriteString("// declarations. Each struct is what a Find that loads the named\n")
	b.WriteString("// relations fills: the row struct of the table, plus one field per\n")
	b.WriteString("// relation, tagged the way the loader looks it up.\n//\n")
	b.WriteString("// Edit the relation declarations and the -shape arguments, not this\n")
	b.WriteString("// file: a change here is reverted the next time dropsgen runs.\n")
	if len(skipped) > 0 {
		sort.Strings(skipped)
		b.WriteString("//\n// Not generated, because the package already declares the name:\n")
		for _, s := range skipped {
			fmt.Fprintf(&b, "//   %s\n", s)
		}
		b.WriteString("// Two declarations of one name in one package do not compile, so a\n")
		b.WriteString("// generator that meets one already there can only stand aside — and\n")
		b.WriteString("// it only stands aside for a struct carrying the fields it would\n")
		b.WriteString("// have written itself, because that struct is what the shapes below\n")
		b.WriteString("// nest. Edit it and the next run refuses rather than skips.\n")
	}
	fmt.Fprintf(&b, "\npackage %s\n", pkg.Name)

	for _, name := range kept {
		d := defs[name]
		b.WriteString("\n")
		for _, line := range d.Doc {
			b.WriteString(line)
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "type %s struct {\n", d.Name)
		for _, f := range d.Body {
			b.WriteString(f)
			b.WriteString("\n")
		}
		b.WriteString("}\n")
	}

	src, err := format.Source(b.Bytes())
	if err != nil {
		return nil, fmt.Errorf("generated file does not parse: %w", err)
	}
	return src, nil
}

// collectRowNames records the row struct every node of a shape needs.
func collectRowNames(n *relsNode, into map[string]bool) {
	if n.table == nil {
		return // a MorphTo target is not one table
	}
	into[goIdentFor(n.table.Name)+"Row"] = true
	for _, c := range n.children {
		collectRowNames(c, into)
	}
}

// defineRelsNodes walks a shape's tree and hands every struct it
// needs to define. name overrides the derived name for the root only:
// a nested node is named by its table and its paths, because that is
// what makes two shapes that reach the same node share one struct.
func defineRelsNodes(n *relsNode, name string, root bool, spec relsSpec, taken map[string]string, define func(*relsDef) error) error {
	if len(n.children) == 0 {
		return nil // a leaf is the row struct; rows mode declares it
	}
	rowName := goIdentFor(n.table.Name) + "Row"
	body := []string{"\t" + rowName}
	var shadowed []string
	columns := map[string]bool{}
	for _, c := range n.table.Columns {
		columns[goIdentFor(c)] = true
	}
	for _, child := range n.children {
		field := goIdentFor(child.name)
		if !token.IsIdentifier(field) || token.IsKeyword(field) {
			return fmt.Errorf("shape %q: relation %q becomes %q, which is not a Go field name; rename the relation",
				spec.Raw, child.name, field)
		}
		typ, err := relFieldType(child)
		if err != nil {
			return fmt.Errorf("shape %q: %w", spec.Raw, err)
		}
		if columns[field] {
			shadowed = append(shadowed, field)
		}
		body = append(body, fmt.Sprintf("\t%s %s %s", field, typ, relTagLiteral(child.name)))
	}

	doc := relsDoc(n, name, rowName, shadowed, taken)
	if err := define(&relsDef{Name: name, Doc: doc, Body: body, From: fmt.Sprintf("%q", spec.Raw), Rename: spec.rename(), Root: root}); err != nil {
		return err
	}
	for _, child := range n.children {
		if child.table == nil {
			// A MorphTo has no one target table, so there is no
			// struct beneath it to define — the field is an
			// interface and the caller type-switches on it.
			continue
		}
		if err := defineRelsNodes(child, shapeTypeName(child), false, spec, taken, define); err != nil {
			return err
		}
	}
	return nil
}

// relsDoc words one generated struct. Everything it says is something
// the reader would otherwise have to know about the loader.
func relsDoc(n *relsNode, name, rowName string, shadowed []string, taken map[string]string) []string {
	paths := subPaths(n)
	quoted := make([]string, 0, len(paths))
	for _, p := range paths {
		quoted = append(quoted, strconv.Quote(p))
	}
	doc := []string{
		fmt.Sprintf("// %s is one row of the %q table with %s loaded.", name, qualifiedName(*n.table), englishList(paths)),
		"//",
		"// It is what this query fills, and only this query — the relation",
		"// paths below are the ones the struct declares, and under",
		"// StrictLoading a query that loads fewer is refused:",
		"//",
		fmt.Sprintf("//\tvar rows []%s", name),
		fmt.Sprintf("//\tdb.Find(%s).With(%s).All(ctx, &rows)", tableVar(n.table, taken), strings.Join(quoted, ", ")),
		"//",
		fmt.Sprintf("// The columns come from the embedded %s. Each relation field is", rowName),
		"// tagged with the name the loader looks up, and typed the way the",
		"// loader fills it:",
		"//",
	}
	slices := false
	for _, c := range n.children {
		note := relFieldNote(c)
		if below := subPaths(c); len(below) > 0 {
			// Otherwise the reader has to infer from the type name
			// that the field is itself a shape rather than a row.
			note += fmt.Sprintf(" It is a shape of its own, carrying %s.", englishList(below))
		}
		doc = append(doc, wrapDocItem(note)...)
		switch c.kind {
		case "hasMany", "manyToMany", "morphMany":
			slices = true
		}
	}
	if slices {
		doc = append(doc,
			"//",
			"// A nil slice is not an empty relation. The loader assigns an empty",
			"// non-nil slice to a parent with no children, so a nil one means no",
			"// query ever filled it.")
	}
	if len(shadowed) > 0 {
		doc = append(doc,
			"//",
			fmt.Sprintf("// %s shadows a column of the same name; the column is still", englishList(shadowed)),
			fmt.Sprintf("// scanned, and is reached through the embedded %s.", rowName))
	}
	return doc
}

// tableVar names the *pg.Table a Find would be given. The
// convention every drops schema follows — and the one dropsgen's own
// schema mode emits — is the table name as a Go identifier, but a
// hand-written declaration is free to call it anything, and a doc
// comment that names a variable the package does not declare is
// worse than one that does not try.
func tableVar(t *relsTable, taken map[string]string) string {
	name := goIdentFor(t.Name)
	if _, ok := taken[name]; ok {
		return name
	}
	return "/* the *pg.Table for " + strconv.Quote(qualifiedName(*t)) + " */"
}

// relFieldNote is the one-line explanation of a relation field's
// type, worded as the reason rather than the restatement.
func relFieldNote(c *relsNode) string {
	field := goIdentFor(c.name)
	switch c.kind {
	case "hasMany":
		return fmt.Sprintf("%s is a slice: %q is HasMany.", field, c.name)
	case "manyToMany":
		return fmt.Sprintf("%s is a slice: %q is ManyToMany.", field, c.name)
	case "morphMany":
		return fmt.Sprintf("%s is a slice: %q is MorphMany.", field, c.name)
	case "hasOne":
		return fmt.Sprintf("%s is a pointer: %q is HasOne, and a parent with no match keeps a nil, which a zero struct could not tell from a real row of zeros.", field, c.name)
	case "belongsTo":
		return fmt.Sprintf("%s is a pointer: %q is BelongsTo, and a row whose key matches nothing keeps a nil, which a zero struct could not tell from a real row of zeros.", field, c.name)
	case "morphTo":
		return fmt.Sprintf("%s is an interface: %q is MorphTo, and the loaded parent's type varies row by row — the loader stores a pointer to it, so read it with a type switch.", field, c.name)
	}
	return field
}

// wrapDocItem renders one bullet of a gofmt doc list, wrapped. The
// continuation lines are indented under the text rather than under
// the dash, which is what keeps the comment reformatter reading them
// as one item.
func wrapDocItem(text string) []string {
	const width = 68
	var (
		out  []string
		line = "//   - "
		open = false
	)
	for _, word := range strings.Fields(text) {
		if open && len(line)+1+len(word) > width {
			out = append(out, line)
			line, open = "//     ", false
		}
		if open {
			line += " "
		}
		line += word
		open = true
	}
	if open {
		out = append(out, line)
	}
	return out
}

// englishList joins names the way a sentence would.
func englishList(names []string) string {
	switch len(names) {
	case 0:
		return "nothing"
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// collisionWithDeclaration words the refusal for a name the package
// already declares as something other than what this run would write.
//
// It is the cross-run twin of the message define() produces, and it
// says the same two things: which name, and the way out. The way out
// differs, because only a shape's own struct can be renamed on the
// command line — a nested one is named for its table and the paths
// beneath it, which is exactly what makes two shapes that reach one
// node share one struct.
//
// isStruct says whether the name is taken by a struct type at all.
// declaredNames records every top-level declaration, so the thing in
// the way may be a func, a var or an alias — and telling its author
// that their func carries the wrong fields sends them looking for
// fields it does not have.
func collisionWithDeclaration(def *relsDef, file string, isStruct bool) error {
	where := "the package"
	if file != "" {
		where = file
	}
	what := "a " + def.Name + " with different fields"
	if !isStruct {
		what = def.Name + ", and it is not a struct type"
	}
	if def.Root {
		return fmt.Errorf("shape %s generates %s, and %s already declares %s; the declaration that is already there is what a query written against this name would load, so this is a collision and not a struct to stand aside for — name this one outright, as -shape 'SomeName=%s'",
			def.From, def.Name, where, what, def.Rename)
	}
	return fmt.Errorf("shape %s nests %s, and %s already declares %s; a nested shape is named for its table and the paths beneath it and cannot be renamed on the command line, so the two shapes have to be generated into one file — or the relation names they walk have to differ",
		def.From, def.Name, where, what)
}

// declaredShapeFields renders the fields of every struct type the
// package declares, keyed by name, in the form sameShape compares.
//
// declaredNames answers whether a name is taken. This answers what it
// is taken *by*, which is the difference between a struct this
// generator would have written anyway and one that merely arrived at
// the same derived name from a different relation tree.
func declaredShapeFields(dir, pkgName string) (map[string][]string, error) {
	files, fset, err := packageFiles(dir)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, af := range files {
		if af.Name.Name != pkgName {
			continue
		}
		for _, decl := range af.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gen.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Assign.IsValid() {
					continue // an alias is not a struct declaration
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				out[ts.Name.Name] = structFieldSignature(fset, st)
			}
		}
	}
	return out, nil
}

// structFieldSignature renders one struct's fields as one string per
// field: name, type and tag, with the whitespace gofmt distributes
// between them collapsed. What survives is what the compiler and the
// loader see, so two declarations agree here exactly when they are
// the same struct.
func structFieldSignature(fset *token.FileSet, st *ast.StructType) []string {
	out := []string{}
	if st.Fields == nil {
		return out
	}
	for _, f := range st.Fields.List {
		var typ bytes.Buffer
		if err := printer.Fprint(&typ, fset, f.Type); err != nil {
			// An expression that will not print is one the package
			// does not compile with either; treating it as its own
			// spelling keeps this from being the place that reports
			// it.
			typ.WriteString("?")
		}
		tag := ""
		if f.Tag != nil {
			if unquoted, err := strconv.Unquote(f.Tag.Value); err == nil {
				tag = unquoted
			} else {
				tag = f.Tag.Value
			}
		}
		rendered := strings.Join(strings.Fields(typ.String()), " ")
		if len(f.Names) == 0 {
			out = append(out, rendered+"\x00"+tag)
			continue
		}
		for _, n := range f.Names {
			out = append(out, n.Name+" "+rendered+"\x00"+tag)
		}
	}
	return out
}

// sameShape reports whether a struct the package already declares
// carries the fields this run would have written for the same name.
//
// The generated body is parsed rather than string-matched, so the two
// sides go through one renderer: a hand-written struct that gofmt
// aligned differently, or that groups its fields, is still the same
// struct and still gets stood aside for.
func sameShape(existing []string, def *relsDef) (bool, error) {
	if def == nil {
		return false, nil
	}
	if existing == nil {
		// The name is declared by something that is not a struct
		// type — a func, a var, an alias. Whatever it is, it is not
		// this shape.
		return false, nil
	}
	want, err := relsBodySignature(def.Body)
	if err != nil {
		return false, err
	}
	if len(want) != len(existing) {
		return false, nil
	}
	for i := range want {
		if want[i] != existing[i] {
			return false, nil
		}
	}
	return true, nil
}

// relsBodySignature renders a body this generator built through the
// same parser declaredShapeFields reads the package with, so the two
// are compared as declarations rather than as text.
func relsBodySignature(body []string) ([]string, error) {
	src := "package p\ntype T struct {\n" + strings.Join(body, "\n") + "\n}\n"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "shape.go", src, 0)
	if err != nil {
		return nil, fmt.Errorf("generated struct does not parse: %w", err)
	}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			if st, ok := ts.Type.(*ast.StructType); ok {
				return structFieldSignature(fset, st), nil
			}
		}
	}
	return nil, fmt.Errorf("generated struct does not parse: no struct type in %q", src)
}
