package pg

import (
	"strings"

	"github.com/bernardoforcillo/drops"
)

// PgEnum describes a PostgreSQL enum type. Use it to declare the type
// once and then reference it from one or more columns via EnumCol.
type PgEnum struct {
	name   string
	values []string
}

// NewEnum declares a PostgreSQL enum type with the given values.
func NewEnum(name string, values ...string) *PgEnum {
	return &PgEnum{name: name, values: append([]string(nil), values...)}
}

// Name returns the enum's type name.
func (e *PgEnum) Name() string { return e.name }

// Values returns the labels in declaration order.
func (e *PgEnum) Values() []string { return e.values }

// EnumCol returns a column of this enum type. The Go value type is
// string — drizzle-orm uses the same mapping. Use Custom[Foo] if you
// have a typed string wrapper you want preserved instead.
func (e *PgEnum) Col(name string) *Col[string] {
	return newCol[string](name, simpleType(e.name))
}

// CreateEnum returns a CREATE TYPE ... AS ENUM (...) statement.
func CreateEnum(e *PgEnum) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("CREATE TYPE ")
		b.WriteIdent(e.name)
		b.WriteString(" AS ENUM (")
		for i, v := range e.values {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(quoteLiteral(v))
		}
		b.WriteByte(')')
	})
}

// DropEnum returns DROP TYPE "name".
func DropEnum(name string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("DROP TYPE ")
		b.WriteIdent(name)
	})
}

// DropEnumIfExists is the IF EXISTS variant.
func DropEnumIfExists(name string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("DROP TYPE IF EXISTS ")
		b.WriteIdent(name)
	})
}

// AlterEnumAddValue appends a new label to an existing enum (PG 9.1+).
// before/after are optional anchors; if both are empty the value is
// appended at the end.
func AlterEnumAddValue(enumName, value string, before, after string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("ALTER TYPE ")
		b.WriteIdent(enumName)
		b.WriteString(" ADD VALUE ")
		b.WriteString(quoteLiteral(value))
		switch {
		case before != "":
			b.WriteString(" BEFORE ")
			b.WriteString(quoteLiteral(before))
		case after != "":
			b.WriteString(" AFTER ")
			b.WriteString(quoteLiteral(after))
		}
	})
}

// AlterEnumRenameValue renames an existing enum label (PG 10+).
func AlterEnumRenameValue(enumName, oldValue, newValue string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("ALTER TYPE ")
		b.WriteIdent(enumName)
		b.WriteString(" RENAME VALUE ")
		b.WriteString(quoteLiteral(oldValue))
		b.WriteString(" TO ")
		b.WriteString(quoteLiteral(newValue))
	})
}

// quoteLiteral wraps a string as a SQL single-quoted literal, doubling
// any embedded single quotes.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
