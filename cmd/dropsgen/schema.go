package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Schema generation.
//
// drops has two ways to get a table declaration and both leave a gap.
// Writing the pg.Add(...) block by hand gives typed *pg.Col[T] handles
// — which is what makes UserAge.Gte(18) a compile-time check — but it
// is a second declaration of the schema that can drift from the
// struct. pg.AutoTable derives the table from the struct at runtime,
// so nothing can drift, but it hands back only untyped columns.
//
// This mode closes it: the struct is the single source of truth, and
// the generated file has the typed handles.

// schemaEntity is one struct flagged with `//drops:schema`.
type schemaEntity struct {
	StructName string
	TableVar   string // Go identifier for the table, e.g. "Users"
	TableName  string // SQL table name, e.g. "users"
	Schema     string // optional SQL schema qualifier
	Columns    []schemaColumn

	// Qualifiers holds the package names the generated declarations
	// mention — "time" for a pg.Custom[time.Time]. The generated file
	// has to import them or it does not compile.
	Qualifiers map[string]bool
}

// schemaColumn is one generated column declaration.
type schemaColumn struct {
	VarName string // Go identifier, e.g. "UserEmail"
	Expr    string // constructor chain, e.g. `pg.Text("email").NotNull()`
}

// colOption is a parsed `drop` tag option.
type colOption struct {
	Key      string
	Value    string
	HasValue bool
}

// knownOptions is the `drop` tag vocabulary schema mode understands:
// pg.AutoTable's, plus the `type=` escape hatch only a generator
// needs. It is closed for the reason AutoTable's is — a mistyped
// `notnull` that quietly generates a nullable column is a schema bug
// with nothing downstream left to notice it.
var knownOptions = map[string]bool{
	"primaryKey":    true,
	"autoIncrement": true,
	"notNull":       true,
	"null":          true,
	"unique":        true,
	"version":       true,
	"pii":           true,
	"default":       true,
	"type":          true,
}

// checkOptions rejects a tag schema mode cannot honour, in the words
// pg.AutoTable already uses for the same tag.
func checkOptions(column string, opts []colOption) error {
	for _, o := range opts {
		if !knownOptions[o.Key] {
			return fmt.Errorf("column %q: unknown drop tag option %q", column, o.Key)
		}
		if !o.HasValue && (o.Key == "default" || o.Key == "type") {
			return fmt.Errorf("column %q: `%s` option requires a value: %s=<sql>", column, o.Key, o.Key)
		}
	}
	if hasOption(opts, "notNull") && hasOption(opts, "null") {
		return fmt.Errorf("column %q is tagged both notNull and null; drop one or the other", column)
	}
	return nil
}

// nullWrappers maps the database/sql null wrappers to the Go type
// they carry. A field declared as one of these says two things — the
// column is optional, and its SQL type is the payload's — and the
// generator has to hear both: emitting pg.Custom[sql.NullString]
// would make the *operators* take the wrapper, which is the mistake
// this whole design exists to avoid.
//
// The list is closed in database/sql and hardcoding it is what an AST
// generator can do, so a test asserts it agrees with what reflection
// sees — see TestNullWrappersAgreeWithReflection.
var nullWrappers = map[string]string{
	"sql.NullBool":    "bool",
	"sql.NullByte":    "byte",
	"sql.NullFloat64": "float64",
	"sql.NullInt16":   "int16",
	"sql.NullInt32":   "int32",
	"sql.NullInt64":   "int64",
	"sql.NullString":  "string",
	"sql.NullTime":    "time.Time",
}

// nullPayload strips a database/sql null wrapper off goType and
// reports whether there was one. sql.Null[T] is read generically; the
// legacy family comes from the table above.
func nullPayload(goType string) (string, bool) {
	if inner, ok := strings.CutPrefix(goType, "sql.Null["); ok {
		if inner, ok := strings.CutSuffix(inner, "]"); ok {
			return inner, true
		}
	}
	if payload, ok := nullWrappers[goType]; ok {
		return payload, true
	}
	return "", false
}

// inferNullable reports the nullability the generator should declare
// for a field of the given type. It is the textual twin of
// drift.InferNullable, which pg.AutoTable applies over reflect.Type:
// a leading "*", or a database/sql null wrapper. Everything else is
// NOT NULL, with the `null` tag as the escape hatch — dropsgen reads
// source, so it cannot see that a user's named type implements
// sql.Scanner.
func inferNullable(goType string) bool {
	if strings.HasPrefix(goType, "*") {
		return true
	}
	_, ok := nullPayload(goType)
	return ok
}

// columnExpr renders the constructor chain for one field.
//
// The Go type picks the constructor and the tag options append the
// builder calls, in a fixed order so regenerating an unchanged struct
// produces an identical file — a generator whose output churns is one
// nobody reruns.
func columnExpr(goType, column string, opts []colOption) (string, error) {
	if err := checkOptions(column, opts); err != nil {
		return "", err
	}
	has := func(k string) bool { return hasOption(opts, k) }
	value := func(k string) (string, bool) {
		for _, o := range opts {
			if o.Key == k {
				return o.Value, true
			}
		}
		return "", false
	}

	// The declared type says two separate things: what the column
	// holds, and whether it is optional. Strip the wrappers that only
	// carry the second — a pointer, an sql.Null[T] — before choosing
	// the constructor, or Col[T]'s T would become the wrapper and
	// every comparison on the column would take one too.
	valueType := strings.TrimPrefix(goType, "*")
	if payload, ok := nullPayload(valueType); ok {
		valueType = payload
	}

	// An explicit type= wins: it is the escape hatch for columns the
	// Go type cannot describe (uuid, citext, a domain type).
	var ctor string
	if t, ok := value("type"); ok {
		ctor = fmt.Sprintf("pg.Custom[%s](%q, %q)", valueType, column, t)
	} else {
		c, err := constructorFor(valueType, column, has("autoIncrement"))
		if err != nil {
			return "", err
		}
		ctor = c
	}

	var b strings.Builder
	b.WriteString(ctor)
	if has("primaryKey") {
		b.WriteString(".PrimaryKey()")
	}
	// A pointer field is nullable by nature, so notNull on one is a
	// contradiction worth refusing rather than silently resolving.
	if has("notNull") && strings.HasPrefix(goType, "*") {
		return "", fmt.Errorf("column %q is a pointer field tagged notNull; drop one or the other", column)
	}
	// Every generated column states its nullability, so nothing the
	// generator emits can be the unstated shape pg.NewEntity now
	// rejects. PrimaryKey already said NOT NULL, so saying it again
	// would be noise.
	if !has("primaryKey") {
		if has("null") || (inferNullable(goType) && !has("notNull")) {
			b.WriteString(".Nullable()")
		} else {
			b.WriteString(".NotNull()")
		}
	}
	if has("unique") {
		b.WriteString(".Unique()")
	}
	if d, ok := value("default"); ok {
		fmt.Fprintf(&b, ".Default(%q)", d)
	}
	if has("version") {
		b.WriteString(".OptimisticLock()")
	}
	if has("pii") {
		b.WriteString(".AsPII()")
	}
	return b.String(), nil
}

// constructorFor maps a Go type to the pg constructor that declares
// it. The mapping mirrors pg.AutoTable exactly — the two must agree,
// or a project that uses both would get two different schemas from one
// struct.
func constructorFor(goType, column string, autoInc bool) (string, error) {
	base := goType
	q := fmt.Sprintf("%q", column)
	switch base {
	case "bool":
		return "pg.Boolean(" + q + ")", nil
	case "int16":
		if autoInc {
			return "pg.SmallSerial(" + q + ")", nil
		}
		return "pg.SmallInt(" + q + ")", nil
	case "int32", "int":
		if autoInc {
			return "pg.Serial(" + q + ")", nil
		}
		return "pg.Integer(" + q + ")", nil
	case "int64":
		if autoInc {
			return "pg.BigSerial(" + q + ")", nil
		}
		return "pg.BigInt(" + q + ")", nil
	case "float32":
		return "pg.Real(" + q + ")", nil
	case "float64":
		return "pg.DoublePrecision(" + q + ")", nil
	case "string":
		return "pg.Text(" + q + ")", nil
	case "[]byte":
		return "pg.Bytea(" + q + ")", nil
	case "time.Time":
		return "pg.Timestamp(" + q + ", true)", nil
	case "json.RawMessage":
		return "pg.JSONB(" + q + ")", nil
	default:
		return "", fmt.Errorf("column %q: no constructor for Go type %s — give it an explicit `type=` in the drop tag", column, goType)
	}
}

// hasOption reports whether the tag carried key.
func hasOption(opts []colOption, key string) bool {
	for _, o := range opts {
		if o.Key == key {
			return true
		}
	}
	return false
}

// typeQualifiers records the package qualifiers a type expression
// mentions — "time" for time.Time or *time.Time, nothing for a type
// declared in the same package.
func typeQualifiers(e ast.Expr, into map[string]bool) {
	switch t := e.(type) {
	case *ast.SelectorExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			into[id.Name] = true
		}
	case *ast.StarExpr:
		typeQualifiers(t.X, into)
	case *ast.ArrayType:
		typeQualifiers(t.Elt, into)
	}
}

// importsFor resolves qualifiers against the input file's own import
// block and returns the import lines the generated file needs, sorted.
// An unresolvable qualifier is an error rather than an omission: the
// alternative is a generated file that does not compile, discovered
// one build later with nothing pointing back here.
func importsFor(af *ast.File, qualifiers map[string]bool) ([]string, error) {
	names := make([]string, 0, len(qualifiers))
	for q := range qualifiers {
		names = append(names, q)
	}
	sort.Strings(names)

	out := make([]string, 0, len(names))
	for _, q := range names {
		spec, ok := importSpecFor(af, q)
		if !ok {
			return nil, fmt.Errorf("cannot tell which import the qualifier %q names; give that import an explicit alias", q)
		}
		out = append(out, spec)
	}
	return out, nil
}

func importSpecFor(af *ast.File, q string) (string, bool) {
	for _, spec := range af.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		name := path.Base(p)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		// gopkg.in/yaml.v3 is imported as yaml: the last path element
		// carries the major version, the package name does not.
		if base, _, ok := strings.Cut(path.Base(p), ".v"); ok && spec.Name == nil {
			name = base
		}
		if name != q {
			continue
		}
		if path.Base(p) == q {
			return strconv.Quote(p), true
		}
		return q + " " + strconv.Quote(p), true
	}
	return "", false
}

// varName builds the Go identifier for a column handle:
// struct name + field name, so User.Email becomes UserEmail and two
// tables with an "id" column do not collide.
func varName(structName, fieldName string) string { return structName + fieldName }

// emitSchema renders the generated schema file. imports names the
// packages the column declarations reach for beyond drops/pg.
func emitSchema(pkg string, ents []schemaEntity, imports []string) ([]byte, error) {
	for _, e := range ents {
		if e.TableName == "" {
			return nil, fmt.Errorf("%s: //drops:schema needs a name= key naming the SQL table", e.StructName)
		}
	}
	sort.SliceStable(ents, func(i, j int) bool { return ents[i].StructName < ents[j].StructName })

	var b bytes.Buffer
	b.WriteString("// Code generated by dropsgen. DO NOT EDIT.\n//\n")
	b.WriteString("// The schema below is derived from the `drop` struct tags in the\n")
	b.WriteString("// source file. Edit the struct, not this file: a change here is\n")
	b.WriteString("// reverted the next time dropsgen runs, and a schema that disagrees\n")
	b.WriteString("// with its struct is exactly the drift this generator removes.\n\n")
	const pgImport = `"github.com/bernardoforcillo/drops/pg"`
	fmt.Fprintf(&b, "package %s\n\n", pkg)
	if len(imports) == 0 {
		fmt.Fprintf(&b, "import %s\n", pgImport)
	} else {
		fmt.Fprintf(&b, "import (\n\t%s\n", pgImport)
		for _, imp := range imports {
			fmt.Fprintf(&b, "\t%s\n", imp)
		}
		b.WriteString(")\n")
	}

	for _, e := range ents {
		ctor := fmt.Sprintf("pg.NewTable(%q)", e.TableName)
		if e.Schema != "" {
			ctor = fmt.Sprintf("pg.NewSchemaTable(%q, %q)", e.Schema, e.TableName)
		}
		fmt.Fprintf(&b, "\n// %s is the %q table, derived from %s.\nvar (\n", e.TableVar, e.TableName, e.StructName)
		fmt.Fprintf(&b, "\t%s = %s\n", e.TableVar, ctor)
		for _, c := range e.Columns {
			fmt.Fprintf(&b, "\t%s = pg.Add(%s, %s)\n", c.VarName, e.TableVar, c.Expr)
		}
		b.WriteString(")\n")
	}

	src, err := format.Source(b.Bytes())
	if err != nil {
		return nil, fmt.Errorf("generated schema does not parse: %w", err)
	}
	return src, nil
}

// parseSchemaFile reads filename and returns the structs carrying a
// `//drops:schema` directive, along with the import lines their
// generated declarations need.
//
//	//drops:schema table=Users name=users [schema=public]
//	type User struct {
//	    ID    int64  `drop:"id,primaryKey,autoIncrement"`
//	    Email string `drop:"email,notNull,unique"`
//	}
//
// table= is the Go identifier the generated *pg.Table is bound to,
// name= the SQL table it addresses. They are separate because the Go
// name is plural and exported by convention while the SQL name is
// whatever the database already calls it.
func parseSchemaFile(filename string) ([]schemaEntity, string, []string, error) {
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, filename, nil, parser.ParseComments)
	if err != nil {
		return nil, "", nil, err
	}

	var out []schemaEntity
	qualifiers := map[string]bool{}
	for _, decl := range af.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		groupDoc := commentText(gd.Doc)
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				continue
			}
			doc := groupDoc
			if d := commentText(ts.Doc); d != "" {
				doc = d
			}
			match := schemaDirective.FindString(doc)
			if match == "" {
				continue
			}
			ent, err := schemaEntityFrom(ts.Name.Name, match, st)
			if err != nil {
				return nil, "", nil, fmt.Errorf("%s: %s: %w", filename, ts.Name.Name, err)
			}
			for q := range ent.Qualifiers {
				qualifiers[q] = true
			}
			out = append(out, ent)
		}
	}
	imports, err := importsFor(af, qualifiers)
	if err != nil {
		return nil, "", nil, fmt.Errorf("%s: %w", filename, err)
	}
	return out, af.Name.Name, imports, nil
}

var schemaDirective = regexp.MustCompile(`drops:schema\b[^\n]*`)

// schemaEntityFrom builds one entity from its directive and fields.
func schemaEntityFrom(structName, directive string, st *ast.StructType) (schemaEntity, error) {
	keys := directiveKeys(directive)
	ent := schemaEntity{
		StructName: structName,
		TableVar:   keys["table"],
		TableName:  keys["name"],
		Schema:     keys["schema"],
		Qualifiers: map[string]bool{},
	}
	if ent.TableVar == "" {
		return ent, fmt.Errorf("//drops:schema needs a table= key naming the Go variable")
	}

	for _, f := range st.Fields.List {
		if len(f.Names) == 0 {
			return ent, embeddedFieldError(structName, f)
		}
		if f.Tag == nil {
			continue
		}
		tag := reflect.StructTag(strings.Trim(f.Tag.Value, "`"))
		raw := tag.Get("drop")
		if raw == "" || raw == "-" {
			continue
		}
		parts := strings.Split(raw, ",")
		column := parts[0]
		opts := make([]colOption, 0, len(parts)-1)
		for _, p := range parts[1:] {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			k, v, hasVal := strings.Cut(p, "=")
			opts = append(opts, colOption{Key: k, Value: v, HasValue: hasVal})
		}
		goType := typeString(f.Type)
		expr, err := columnExpr(goType, column, opts)
		if err != nil {
			return ent, err
		}
		// Only `type=` puts the Go type itself in the output, as
		// pg.Custom's type argument; every other constructor takes
		// nothing but the column name.
		if hasOption(opts, "type") {
			typeQualifiers(f.Type, ent.Qualifiers)
		}
		for _, name := range f.Names {
			if !ast.IsExported(name.Name) {
				continue
			}
			ent.Columns = append(ent.Columns, schemaColumn{
				VarName: varName(structName, name.Name),
				Expr:    expr,
			})
		}
	}
	if len(ent.Columns) == 0 {
		return ent, fmt.Errorf("no `drop`-tagged fields to declare")
	}
	return ent, nil
}

// directiveKeys parses the k=v pairs of a directive line.
func directiveKeys(directive string) map[string]string {
	out := map[string]string{}
	for _, p := range strings.Fields(directive)[1:] {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		out[k] = strings.Trim(v, `"`)
	}
	return out
}

// runSchema is the CLI entry point for schema generation.
func runSchema(in, out string) error {
	ents, pkg, imports, err := parseSchemaFile(in)
	if err != nil {
		return err
	}
	if len(ents) == 0 {
		return fmt.Errorf("no `//drops:schema` directives found in %s", in)
	}
	src, err := emitSchema(pkg, ents, imports)
	if err != nil {
		return err
	}
	if out == "" {
		dir := filepath.Dir(in)
		base := strings.TrimSuffix(filepath.Base(in), ".go")
		out = filepath.Join(dir, base+"_drops_schema.go")
	}
	return os.WriteFile(out, src, 0o644)
}
