package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"regexp"
	"strings"
)

// entity describes a single struct flagged for code generation.
type entity struct {
	StructName string  // Go type name, e.g. "User"
	TableVar   string  // *pg.Table identifier the entity is bound to, e.g. "Users"
	Fields     []field // public, db-tagged fields in declaration order
}

// field bundles a struct field's Go identity with the column name it
// maps to.
type field struct {
	GoName string // identifier, e.g. "Email"
	GoType string // type expression text, e.g. "string", "*int32"
	Column string // drop tag value, e.g. "email"
}

var entityDirective = regexp.MustCompile(`drops:entity\b[^\n]*`)

// parseFile reads path and returns the entity descriptors plus the
// declared package name. Only files declaring `package <name>` parse
// successfully.
func parseFile(path string) ([]entity, string, error) {
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, "", err
	}
	pkg := af.Name.Name

	var out []entity
	for _, decl := range af.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		// Doc comment lives on the GenDecl when there is one type, or
		// on each TypeSpec when grouped under `type ( ... )`. Try both.
		groupDoc := commentText(gd.Doc)
		for _, spec := range gd.Specs {
			ts := spec.(*ast.TypeSpec)
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				continue
			}
			doc := groupDoc
			if d := commentText(ts.Doc); d != "" {
				doc = d
			}
			match := entityDirective.FindString(doc)
			if match == "" {
				continue
			}
			tableVar, err := tableFromDirective(match)
			if err != nil {
				return nil, "", fmt.Errorf("%s: %s: %w", path, ts.Name.Name, err)
			}
			fields, err := collectFields(st)
			if err != nil {
				return nil, "", fmt.Errorf("%s: %s: %w", path, ts.Name.Name, err)
			}
			out = append(out, entity{
				StructName: ts.Name.Name,
				TableVar:   tableVar,
				Fields:     fields,
			})
		}
	}
	return out, pkg, nil
}

// commentText flattens a CommentGroup into a single string.
func commentText(g *ast.CommentGroup) string {
	if g == nil {
		return ""
	}
	var lines []string
	for _, c := range g.List {
		lines = append(lines, strings.TrimPrefix(c.Text, "//"))
	}
	return strings.Join(lines, "\n")
}

// tableFromDirective parses the `table=Name` key from a
// `drops:entity table=Name` directive. Returns an error if absent.
func tableFromDirective(directive string) (string, error) {
	parts := strings.Fields(directive)
	for _, p := range parts[1:] {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		if k == "table" {
			return strings.Trim(v, `"`), nil
		}
	}
	return "", fmt.Errorf("drops:entity directive missing `table=` key")
}

// collectFields walks a struct's fields and returns the bind/scan
// metadata for every exported, drop-tagged field. The first
// comma-separated token is the column name; subsequent tokens
// (pk, autoinc, notnull, …) are ignored here because dropsgen
// itself does not generate the schema declaration, only the
// bind/scan helpers.
func collectFields(st *ast.StructType) ([]field, error) {
	var out []field
	for _, f := range st.Fields.List {
		if f.Tag == nil {
			continue
		}
		tag := reflect.StructTag(strings.Trim(f.Tag.Value, "`"))
		raw := tag.Get("drop")
		if raw == "" || raw == "-" {
			continue
		}
		col := raw
		if j := strings.IndexByte(raw, ','); j >= 0 {
			col = raw[:j]
		}
		for _, name := range f.Names {
			if !ast.IsExported(name.Name) {
				continue
			}
			out = append(out, field{
				GoName: name.Name,
				GoType: typeString(f.Type),
				Column: col,
			})
		}
	}
	return out, nil
}

// typeString renders an ast.Expr back to its source form — sufficient
// for primitive types, pointer types, and named types we expect on
// entity fields. More exotic shapes are not the dropsgen MVP focus.
func typeString(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + typeString(t.X)
	case *ast.SelectorExpr:
		return typeString(t.X) + "." + t.Sel.Name
	case *ast.ArrayType:
		return "[]" + typeString(t.Elt)
	default:
		return fmt.Sprintf("%T", e)
	}
}
