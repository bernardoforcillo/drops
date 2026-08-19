// Package drift reports schema/struct mismatches when an entity is
// declared.
//
// A table column that no struct field is bound to is not a harmless
// omission: it is dropped from every INSERT and UPDATE the entity
// builds. A renamed field or a mistyped `drop:` tag therefore stops
// persisting a column while the schema, the compiler and every test
// that does not assert on that column all stay happy. Catching it at
// declaration time is the only point where the mistake is cheap.
//
// The check is shared because pg, sqlite and clickhouse all face it
// identically and differ only in their column accessors and in the
// name they put on the error.
package drift

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// FieldKey renders a struct field's index path as a comparable
// string, so a caller can record which paths a column already claimed.
func FieldKey(idx []int) string {
	parts := make([]string, len(idx))
	for i, n := range idx {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ".")
}

// SpareFields returns the exported fields of rt that no column bound
// to, given the set of [FieldKey] values that were bound.
//
// Two kinds of field are excluded rather than reported. A field
// tagged `drop:"-"` was explicitly opted out by its author. A field
// tagged `dropRel` holds an eager-loaded relation and is bound to a
// query, not a column. Reporting either as a suspect would train
// readers to ignore the message.
func SpareFields(rt reflect.Type, bound map[string]bool) []string {
	var out []string
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		if f.Tag.Get("drop") == "-" || f.Tag.Get("dropRel") != "" {
			continue
		}
		if bound[FieldKey([]int{i})] {
			continue
		}
		out = append(out, f.Name)
	}
	return out
}

// Report formats the error for a set of unmapped columns.
//
// pkg is the reporting package ("drops/pg"), exempt the name of the
// option that waives the check. The message names the columns, offers
// the unbound struct fields as the likely cause, and spells out the
// escape hatch — a diagnostic worth reading is one that contains its
// own fix.
func Report(pkg, typeName, tableName string, missing, spare []string, exempt string) error {
	if len(missing) == 0 {
		return nil
	}
	quoted := quoteAll(missing)
	msg := fmt.Sprintf("%s: NewEntity[%s]: table %q has column(s) %s with no matching struct field, so they would be silently dropped from every INSERT and UPDATE",
		pkg, typeName, tableName, strings.Join(quoted, ", "))
	if len(spare) > 0 {
		msg += fmt.Sprintf("; unbound struct field(s): %s — a typo in a field name or `drop:` tag looks like the cause",
			strings.Join(spare, ", "))
	}
	subject := "them"
	if len(missing) == 1 {
		subject = "it"
	}
	msg += fmt.Sprintf("; if the database owns %s, say so with %s(%s)",
		subject, exempt, strings.Join(quoted, ", "))
	return fmt.Errorf("%s", msg)
}

func quoteAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = strconv.Quote(n)
	}
	return out
}
