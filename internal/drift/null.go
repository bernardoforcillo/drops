package drift

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// Nullability drift — a column the database will hand back as NULL
// bound to a struct field that cannot receive one.
//
// This is the same class of mistake as an unmapped column and it is
// caught in the same place, but it fails differently when it is not
// caught: the entity works until the first row that happens to be
// NULL, at which point database/sql reports "converting NULL to
// string is unsupported" naming a column index, from inside a scan,
// in production. Nothing in the type system can see it — a column's
// T is the *operand* type its comparisons take, and the scan
// destination is a field inside a struct drops only reaches by
// reflection. NewEntity is the one point where both are in scope.

var scannerType = reflect.TypeOf((*sql.Scanner)(nil)).Elem()

// AcceptsNull reports whether a SQL NULL can be scanned into a field
// of type t. The scanner passes &field, so the question is whether *t
// knows how to receive a column value — the same claim the row
// scanner's isScalarDest honours — or whether t is one of the kinds
// database/sql sets to nil.
//
// Accepts *T, sql.Null[T] and the legacy sql.NullString family, any
// type with a Scan method on the pointer receiver, []byte,
// json.RawMessage, maps and interfaces. Rejects string, the numeric
// kinds, bool, time.Time and [N]byte.
func AcceptsNull(t reflect.Type) bool {
	if t == nil {
		return false
	}
	if reflect.PointerTo(t).Implements(scannerType) {
		return true
	}
	switch t.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice:
		return true
	}
	return false
}

// InferNullable reports the nullability a schema generator should
// declare for a field of type t.
//
// Deliberately narrower than [AcceptsNull], because what a generator
// should declare is not what a field can survive. A []byte field can
// receive a NULL, but that is no reason to make the column optional;
// an optional column is one whose author said so, by writing a
// pointer or an sql.Null[T]. The two must keep the invariant
//
//	InferNullable(t) implies AcceptsNull(t)
//
// or a generator would declare a column nullable, bind it to a field
// that cannot receive NULL, and fail its own check.
func InferNullable(t reflect.Type) bool {
	if t == nil {
		return false
	}
	if t.Kind() == reflect.Pointer {
		return true
	}
	// database/sql's Null[T] and the legacy NullString family:
	// writing one is the author saying "this is optional".
	return t.PkgPath() == "database/sql" && strings.HasPrefix(t.Name(), "Null")
}

// NullPayload returns the value type inside a database/sql null
// wrapper — string for sql.NullString, T for sql.Null[T] — and
// reports whether t was one.
//
// A schema generator has to see through the wrapper to pick the
// column's SQL type: the wrapper carries no type of its own, it only
// says the column is optional. The payload is the wrapper's first
// field, which is how every member of the family is laid out.
func NullPayload(t reflect.Type) (reflect.Type, bool) {
	if t == nil || t.Kind() != reflect.Struct || t.NumField() == 0 {
		return nil, false
	}
	if t.PkgPath() != "database/sql" || !strings.HasPrefix(t.Name(), "Null") {
		return nil, false
	}
	return t.Field(0).Type, true
}

// FieldTypeAt resolves a field's index path to its declared type,
// stepping through an embedded pointer the way reflect's own
// FieldByIndex does. It returns nil for a path that does not resolve,
// which a caller reads as "nothing to judge".
func FieldTypeAt(rt reflect.Type, idx []int) reflect.Type {
	t := rt
	for _, i := range idx {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct || i >= t.NumField() {
			return nil
		}
		t = t.Field(i).Type
	}
	return t
}

// FieldPath renders a field's index path as the dotted Go name a
// reader can search for — "Audit.DeletedAt" for a field reached
// through an embedded struct, so the message names something that
// exists in the source rather than a bare leaf shared by three
// mixins.
func FieldPath(rt reflect.Type, idx []int) string {
	parts := make([]string, 0, len(idx))
	t := rt
	for _, i := range idx {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct || i >= t.NumField() {
			break
		}
		f := t.Field(i)
		parts = append(parts, f.Name)
		t = f.Type
	}
	return strings.Join(parts, ".")
}

// NullMismatch is one column that admits NULL bound to a field that
// cannot receive one.
type NullMismatch struct {
	Column    string
	Field     string // the Go field name, dotted through embedding
	FieldType string
	Stated    bool // the column said Nullable, rather than saying nothing
}

// ReportNullable formats the error for a set of nullability
// mismatches.
//
// pkg is the reporting package ("drops/pg"), exempt the name of the
// option that waives the check. Stated picks the wording and nothing
// else: a column that said Nullable is being contradicted, while one
// that said nothing at all is a schema that never decided — and the
// fix differs, because the second one can legitimately be answered by
// tightening the column instead of loosening the field.
func ReportNullable(pkg, typeName, tableName string, m []NullMismatch, exempt string) error {
	if len(m) == 0 {
		return nil
	}
	names := make([]string, len(m))
	var b strings.Builder
	fmt.Fprintf(&b, "%s: NewEntity[%s]: table %q: %d column(s) can hold NULL where the field cannot:",
		pkg, typeName, tableName, len(m))
	loose := false
	for i, x := range m {
		names[i] = x.Column
		if !x.Stated {
			loose = true
		}
		fmt.Fprintf(&b, "\n\t%s", nullClause(x))
	}
	// The explanation goes once, after the list. One clause per column
	// put the same thirty words on the line five times for a table
	// nobody would call unusual, and a diagnostic that has to be
	// re-read is one that gets skipped.
	b.WriteString("\nthe database will accept NULL there and the scan fails on the first such row")
	if loose {
		b.WriteString("; a column that merely omitted NOT NULL can be tightened instead — add .NotNull() to it, with a migration to enforce it")
	}
	fmt.Fprintf(&b, "\nif NULL is genuinely impossible there, say so with %s(%s)",
		exempt, strings.Join(quoteAll(names), ", "))
	return errors.New(b.String())
}

// nullClause words one mismatch: what disagrees, and the replacement
// field type that settles it. The alternative fix — tightening the
// column — is stated once by the caller, because it does not vary.
func nullClause(m NullMismatch) string {
	stated := " (column not declared NOT NULL)"
	if m.Stated {
		stated = " (column declared Nullable)"
	}
	return fmt.Sprintf("column %q -> field %s is %s; make the field *%s or sql.Null[%s]%s",
		m.Column, m.Field, m.FieldType, m.FieldType, m.FieldType, stated)
}
