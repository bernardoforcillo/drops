package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Row and insert structs, generated from the table declaration.
//
// Schema mode runs struct -> table. This is the inverse, and it is
// the direction drizzle-orm gets praised for: $inferSelect derives
// the row type from the column table so it cannot drift, and
// $inferInsert drops the columns the database fills. Go cannot infer
// a struct from a value, so the equivalent has to be generated.
//
// Nullability is why it is worth generating rather than writing.
// pg.NewEntity now refuses a column that admits NULL bound to a field
// that cannot receive one, and it refuses it at package-var init —
// the failure a hand-written struct earns is a panic at process
// start, or a scan error in production on the first row that happens
// to be NULL. A generator that reads nullability off the declaration
// cannot emit that pairing, so the struct is correct by construction.
//
// Three decisions this mode had to make, and why:
//
// How it finds the table. A CLI cannot read a Go variable, and a
// table declaration is one. Parsing pg.Add calls out of the source
// would recover the column names but not the Go types — a column's T
// lives in the *Col[T] handle, appears in no field of it, and is
// exactly what this mode has to know. So it does what `drops` already
// does for migrations: writes a throwaway program that imports the
// schema package, calls its Schema function, and prints what it finds
// as JSON. The real compiler answers, against the real package, which
// is the only answer that matches the one the application gets.
//
// What happens when the struct already exists. It is skipped, and the
// generated file's header says which name was skipped and where the
// existing declaration is. This is not a preference: two declarations
// of one name in one package do not compile, so emitting a second one
// is not an option, and silently overwriting a struct somebody
// maintains is not a generator's call to make.
//
// Determinism. Tables are sorted by schema-qualified name in the
// bridge, columns keep the table's declaration order, and every set
// the generator carries — imports, skip notes — is sorted before it
// is written. Nothing in the output names the temporary directory,
// the clock or the host. TestRowsGenerationIsByteStable runs the
// whole thing twice and compares bytes.

// rowsTable is one table as the bridge program reports it. The field
// names are the JSON keys: encoding/json matches them on both sides,
// so the bridge needs no struct tags and the template needs no
// backticks.
type rowsTable struct {
	Name    string
	Schema  string
	Columns []rowsColumn
}

// rowsImport is one import a column's Go type needs, by path and by
// the name its package actually goes by. The two differ for every
// module past v1: github.com/gofrs/uuid/v5 is package uuid, so a file
// that took the qualifier from the path would say v5.UUID.
type rowsImport struct {
	Path string
	Name string
}

// rowsColumn is one column as the bridge reports it. GoType is the
// source spelling of the Go type the column's handle carries, and
// Imports are the packages that spelling names; both are empty for a
// column with no typed handle — see (*pg.Column).GoType.
type rowsColumn struct {
	Name       string
	SQLType    string
	GoType     string
	Imports    []rowsImport
	Nullable   bool
	PrimaryKey bool
	HasDefault bool
	Managed    bool
}

// rowsPackage is a located Go package that declares a schema.
type rowsPackage struct {
	ImportPath string
	Dir        string
	Name       string
	ModuleDir  string
}

// schemaFuncHint is the convention the bridge compiles against. It is
// the same one `drops` reads for migrations, deliberately: a project
// that already has the function gets this mode for free.
const schemaFuncHint = `add a Schema function to the package:

    func Schema() *pg.Schema {
        return pg.NewSchema(Users, Posts)  // every table to generate for
    }`

// runRows is the CLI entry point for row/insert generation.
func runRows(pattern, outName string) error {
	if outName != "" && filepath.Base(outName) != outName {
		// The generated file has to live in the schema package: a
		// column typed with a type that package declares is written
		// without a qualifier, which is only true there.
		return fmt.Errorf("-o takes a file name, not a path: the generated file belongs in the schema package's own directory")
	}
	ctx := context.Background()
	pkg, err := locateRowsPackage(ctx, pattern)
	if err != nil {
		return err
	}
	if outName == "" {
		outName = pkg.Name + "_drops_rows.go"
	}
	out := filepath.Join(pkg.Dir, outName)

	// The previous output is moved aside before the bridge compiles
	// the package, and put back if anything fails. Otherwise a
	// generated file that has gone stale — a dropped column leaving
	// its import unused, which is a compile error in Go — would break
	// the very run that would have fixed it.
	restore, err := stash(out)
	if err != nil {
		return err
	}
	// The eager-load shapes nest these row structs, so a shape file
	// cannot survive their absence either — and it is in the same
	// package, which the bridge is about to compile. It is put back
	// either way: regenerating it is -rels mode's job, not this
	// one's.
	restoreShapes, err := stashShapes(pkg)
	if err != nil {
		restore()
		return err
	}
	defer restoreShapes()
	tables, err := runRowsBridge(ctx, pkg)
	if err != nil {
		restore()
		return err
	}
	taken, err := declaredNames(pkg.Dir, pkg.Name)
	if err != nil {
		restore()
		return err
	}
	src, err := emitRows(pkg, tables, taken)
	if err != nil {
		restore()
		return err
	}
	return os.WriteFile(out, src, 0o644)
}

// stash takes path's contents out of the way and returns the
// function that puts them back. A path that does not exist stashes
// fine and restores to nothing.
func stash(path string) (restore func(), err error) {
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return func() {}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil {
		return nil, err
	}
	return func() { _ = os.WriteFile(path, body, 0o644) }, nil
}

// stashShapes takes every generated shape file in the package out of
// the way and returns the one function that puts them all back.
func stashShapes(pkg *rowsPackage) (func(), error) {
	paths, err := filepath.Glob(filepath.Join(pkg.Dir, "*"+relsFileSuffix))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	restores := make([]func(), 0, len(paths))
	// Idempotent, because rels mode puts these back as soon as the
	// bridge is done rather than at the end, and the deferred call is
	// still there to cover the paths that return early.
	done := false
	undo := func() {
		if done {
			return
		}
		done = true
		for _, r := range restores {
			r()
		}
	}
	for _, path := range paths {
		restore, err := stash(path)
		if err != nil {
			undo()
			return nil, err
		}
		restores = append(restores, restore)
	}
	return undo, nil
}

// locateRowsPackage resolves a package pattern to the package on disk
// and checks the convention before anything is compiled, so the usual
// mistake is reported as itself rather than as a compiler error
// inside a file the user never wrote.
func locateRowsPackage(ctx context.Context, pattern string) (*rowsPackage, error) {
	if pattern == "" {
		return nil, fmt.Errorf("-rows takes the Go package that declares your tables, e.g. -rows ./db/schema")
	}
	var out, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "go", "list", "-json", pattern)
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go list %s: %v\n%s\nrun dropsgen from inside the Go module that declares the schema, and pass -rows as a package pattern such as ./db/schema", pattern, err, strings.TrimSpace(stderr.String()))
	}
	var listed struct {
		ImportPath string
		Dir        string
		Name       string
		Module     struct{ Dir string }
	}
	if err := json.Unmarshal(out.Bytes(), &listed); err != nil {
		return nil, fmt.Errorf("parse go list output for %s: %w", pattern, err)
	}
	if listed.Module.Dir == "" {
		return nil, fmt.Errorf("package %s is not inside a Go module; dropsgen needs one so it can compile a program that imports your schema", pattern)
	}
	if err := checkSchemaFunc(listed.Dir, listed.ImportPath); err != nil {
		return nil, err
	}
	return &rowsPackage{
		ImportPath: listed.ImportPath,
		Dir:        listed.Dir,
		Name:       listed.Name,
		ModuleDir:  listed.Module.Dir,
	}, nil
}

// checkSchemaFunc looks for the convention in the package's source. It
// reads names, not types — the compiler stays the authority on whether
// the function returns what it should — but it turns the usual mistake
// into a sentence that says what to write.
func checkSchemaFunc(dir, importPath string) error {
	files, _, err := packageFiles(dir)
	if err != nil {
		return err
	}
	for _, af := range files {
		for _, decl := range af.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != "Schema" {
				continue
			}
			if fn.Type.Params.NumFields() != 0 {
				return fmt.Errorf("%s.Schema takes arguments; dropsgen calls it with none — %s", importPath, schemaFuncHint)
			}
			return nil
		}
	}
	return fmt.Errorf("package %s declares no Schema function, so dropsgen has nothing to read — %s", importPath, schemaFuncHint)
}

// packageFiles parses the non-test Go files in dir, sorted by name so
// two runs walk them in the same order.
func packageFiles(dir string) ([]*ast.File, *token.FileSet, error) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", dir, err)
	}
	names := make([]string, 0, len(pkgs))
	for name := range pkgs {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []*ast.File
	for _, name := range names {
		pkg := pkgs[name]
		files := make([]string, 0, len(pkg.Files))
		for f := range pkg.Files {
			files = append(files, f)
		}
		sort.Strings(files)
		for _, f := range files {
			out = append(out, pkg.Files[f])
		}
	}
	return out, fset, nil
}

// runRowsBridge writes the generated program, runs it with `go run`,
// and decodes its reply.
//
// The program is written inside the schema package's module because
// that is the only place an import of the schema package resolves.
func runRowsBridge(ctx context.Context, pkg *rowsPackage) ([]rowsTable, error) {
	dir, err := os.MkdirTemp(pkg.ModuleDir, ".dropsgen-rows-")
	if err != nil {
		return nil, fmt.Errorf("create the directory for the generated program: %w", err)
	}
	defer os.RemoveAll(dir)

	main := filepath.Join(dir, "main.go")
	src := strings.ReplaceAll(rowsBridgeTemplate, "$IMPORT$", pkg.ImportPath)
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
	var tables []rowsTable
	if err := json.Unmarshal(stdout.Bytes(), &tables); err != nil {
		return nil, fmt.Errorf("the generated program printed something that is not a table listing: %w", err)
	}
	return tables, nil
}

// declaredNames collects every package-level identifier the package
// in dir already declares, mapped to the file that declares it.
//
// Types are the collision that matters, but a var or a func of the
// same name collides just as hard, and a generator that only looked
// at types would emit a file that does not compile.
func declaredNames(dir, pkgName string) (map[string]string, error) {
	files, fset, err := packageFiles(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, af := range files {
		if af.Name.Name != pkgName {
			continue
		}
		file := filepath.Base(fset.Position(af.Pos()).Filename)
		record := func(name string) {
			if name != "_" {
				out[name] = file
			}
		}
		for _, decl := range af.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					record(d.Name.Name)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						record(s.Name.Name)
					case *ast.ValueSpec:
						for _, n := range s.Names {
							record(n.Name)
						}
					}
				}
			}
		}
	}
	return out, nil
}

// omitReason says why a column is not the caller's to supply, or ""
// when it is. The wording ends up in the insert struct's doc comment,
// so a reader who wonders where a column went finds the answer next
// to the struct rather than in this file.
func omitReason(c rowsColumn) string {
	switch {
	case isSerialType(c.SQLType):
		// The sequence is in the type, not in a DEFAULT clause, so
		// HasDefault does not see it — the same distinction
		// pg.Entity draws with isImplicitDefault.
		return c.SQLType
	case strings.Contains(strings.ToLower(c.SQLType), "generated always as"):
		return "generated"
	case c.HasDefault:
		return "has a DEFAULT"
	case c.Managed:
		// A managed column is one drops writes: the soft-delete
		// marker a mixin flips, the timestamps it keeps current.
		return "written by drops"
	}
	return ""
}

// rowFieldType is the field type for a column in the row struct: the
// value type its handle carries, made a pointer when the column
// admits NULL.
//
// The pointer is not decoration. pg.NewEntity refuses a nullable
// column bound to a field that cannot receive NULL, so a struct
// without it does not survive its own init — and the pointer is also
// what a schema generator reads back as "nullable", so a struct
// generated from a table and a table generated from that struct agree.
func rowFieldType(c rowsColumn) string {
	if c.Nullable && !inferNullable(c.GoType) {
		return "*" + c.GoType
	}
	return c.GoType
}

// emitRows renders the generated file.
func emitRows(pkg *rowsPackage, tables []rowsTable, taken map[string]string) ([]byte, error) {
	type structDef struct {
		Name string
		Doc  []string
		Body []string
	}
	var (
		defs    []structDef
		skipped []string
		imports = map[string]string{} // path -> package name
		claimed = map[string]string{}
	)

	for _, tbl := range tables {
		if len(tbl.Columns) == 0 {
			continue
		}
		base := goIdentFor(tbl.Name)
		qualified := tbl.Name
		if tbl.Schema != "" {
			qualified = tbl.Schema + "." + tbl.Name
		}
		// A SQL identifier is not a Go one — "2fa" and "first name"
		// are both legal columns — and the source that would carry
		// one does not parse. Said here, the message names the table
		// or column; left to format.Source it is a line number in a
		// file the user never saw.
		if !token.IsIdentifier(base) {
			return nil, fmt.Errorf("table %q becomes %q, which is not a Go type name; rename the table or generate its structs by hand", qualified, base)
		}

		// Field names first: two columns that yield one Go
		// identifier would produce a struct that does not compile,
		// and the column names are the thing to change.
		fields := map[string]string{}
		for _, c := range tbl.Columns {
			if c.GoType == "" {
				return nil, fmt.Errorf("table %q column %q has no typed handle, so its Go type is unknown: declare it with pg.Add and a pg constructor (a table pg.AutoTable derived from a struct already has the struct this mode generates)", qualified, c.Name)
			}
			name := goIdentFor(c.Name)
			if !token.IsIdentifier(name) || token.IsKeyword(name) {
				return nil, fmt.Errorf("table %q: column %q becomes %q, which is not a Go field name; rename the column or generate this table's structs by hand", qualified, c.Name, name)
			}
			if prev, ok := fields[name]; ok {
				return nil, fmt.Errorf("table %q: columns %q and %q both become the Go field %s; rename one", qualified, prev, c.Name, name)
			}
			fields[name] = c.Name
		}

		rowName, insName := base+"Row", base+"Insert"
		for _, n := range []string{rowName, insName} {
			if prev, ok := claimed[n]; ok {
				return nil, fmt.Errorf("tables %q and %q both generate %s; generate one schema at a time", prev, qualified, n)
			}
			claimed[n] = qualified
		}

		var rowBody, insBody []string
		var omitted []string
		for _, c := range tbl.Columns {
			field := fmt.Sprintf("\t%s %s %s", goIdentFor(c.Name), rowFieldType(c), tagLiteral(c.Name))
			rowBody = append(rowBody, field)
			for _, imp := range c.Imports {
				imports[imp.Path] = imp.Name
			}
			if why := omitReason(c); why != "" {
				omitted = append(omitted, fmt.Sprintf("%s — %s", c.Name, why))
				continue
			}
			insBody = append(insBody, field)
		}

		rowDoc := []string{
			fmt.Sprintf("// %s is one row of the %q table, derived from its", rowName, qualified),
			"// declaration. Every column is bound, so pg.NewEntity needs no",
			"// exemption; a column that admits NULL is a pointer, because that",
			"// is the field type pg.NewEntity will let it bind to.",
		}
		insDoc := []string{
			fmt.Sprintf("// %s is %s without the columns a caller must not", insName, rowName),
			"// supply.",
		}
		if len(omitted) > 0 {
			insDoc = append(insDoc, "//", "// Omitted:")
			for _, o := range omitted {
				// A gofmt doc list: "  - x" survives the comment
				// reformatter as a list, where a plain indented line
				// would be turned into a code block.
				insDoc = append(insDoc, "//   - "+o)
			}
		} else {
			insDoc = append(insDoc, "//", "// The database fills none of this table's columns, so it omits nothing.")
		}
		insDoc = append(insDoc,
			"//",
			"// A pointer field is a column that admits NULL. Bind it with",
			"// (*pg.Col[T]).ValPtr: a nil writes NULL rather than a zero, and",
			"// the column is still in the INSERT — a column the database is",
			"// the one to fill is not in this struct at all.")

		if prev, ok := taken[rowName]; ok {
			skipped = append(skipped, skipNote(rowName, prev))
		} else {
			defs = append(defs, structDef{Name: rowName, Doc: rowDoc, Body: rowBody})
		}
		if prev, ok := taken[insName]; ok {
			skipped = append(skipped, skipNote(insName, prev))
		} else {
			defs = append(defs, structDef{Name: insName, Doc: insDoc, Body: insBody})
		}
	}

	var b bytes.Buffer
	b.WriteString("// Code generated by dropsgen. DO NOT EDIT.\n//\n")
	b.WriteString("// One row struct and one insert struct per table the schema\n")
	b.WriteString("// registers. Edit the table declaration, not this file: a change\n")
	b.WriteString("// here is reverted the next time dropsgen runs, and a struct that\n")
	b.WriteString("// disagrees with its table is exactly the drift this generator\n")
	b.WriteString("// removes.\n")
	if len(skipped) > 0 {
		sort.Strings(skipped)
		b.WriteString("//\n// Not generated, because the package already declares the name:\n")
		for _, s := range skipped {
			fmt.Fprintf(&b, "//   %s\n", s)
		}
		b.WriteString("// Two declarations of one name in one package do not compile, so a\n")
		b.WriteString("// generator that meets a hand-written struct can only stand aside.\n")
		b.WriteString("// Rename yours, or delete it and let this file own the name.\n")
	}
	fmt.Fprintf(&b, "\npackage %s\n", pkg.Name)

	paths := make([]rowsImport, 0, len(imports))
	for p, name := range imports {
		paths = append(paths, rowsImport{Path: p, Name: name})
	}
	sort.Slice(paths, func(i, j int) bool { return paths[i].Path < paths[j].Path })
	// An import is only needed by a struct that survived the skip
	// rule, so collect them from the bodies that were kept.
	bodies := make([]string, 0, len(defs))
	for _, d := range defs {
		bodies = append(bodies, strings.Join(d.Body, "\n"))
	}
	used := usedImports(paths, bodies)
	// Two packages of one name cannot both be imported unqualified,
	// and nothing here has a basis for inventing an alias a reader
	// would recognise. Say which two rather than emit a
	// redeclaration.
	byName := map[string]string{}
	for _, im := range used {
		if prev, ok := byName[im.Name]; ok {
			return nil, fmt.Errorf("packages %q and %q are both named %s, so the generated file cannot import both; the struct for one has to be hand-written", prev, im.Path, im.Name)
		}
		byName[im.Name] = im.Path
	}
	if len(used) == 1 {
		fmt.Fprintf(&b, "\nimport %s\n", importLine(used[0]))
	} else if len(used) > 1 {
		b.WriteString("\nimport (\n")
		for _, im := range used {
			fmt.Fprintf(&b, "\t%s\n", importLine(im))
		}
		b.WriteString(")\n")
	}

	for _, d := range defs {
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

// skipNote words one skipped struct.
func skipNote(name, file string) string {
	if file == "" {
		return name
	}
	return name + " (" + file + ")"
}

// usedImports keeps the imports some surviving field still mentions.
// The whole point of the skip rule is that a hand-written struct
// replaces a generated one, and the import it needed goes with it —
// an unused import does not compile.
func usedImports(paths []rowsImport, bodies []string) []rowsImport {
	out := make([]rowsImport, 0, len(paths))
	for _, im := range paths {
		q := im.Name + "."
		for _, body := range bodies {
			if strings.Contains(body, q) {
				out = append(out, im)
				break
			}
		}
	}
	return out
}

// importLine writes one import, named only when the name is not the
// one the compiler would infer from the path. Naming every import
// would be noise; naming none of them is wrong the moment a path ends
// in a version element.
func importLine(im rowsImport) string {
	if im.Name == importPathBase(im.Path) {
		return fmt.Sprintf("%q", im.Path)
	}
	return fmt.Sprintf("%s %q", im.Name, im.Path)
}

// importPathBase is path.Base for an import path. Import paths are
// slash-separated whatever the host separator is, so filepath.Base is
// the wrong tool even where it happens to agree.
func importPathBase(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}
