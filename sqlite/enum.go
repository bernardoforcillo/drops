package sqlite

import "strings"

// SQLite has no native enum type. The idiomatic equivalent is a TEXT
// column constrained by a CHECK ("col" IN ('a', 'b', ...)), which these
// helpers declare in one call. The Go value type is string, matching how
// drops/pg maps PostgreSQL enums.

// Enum describes a fixed set of string labels. Declare it once and add a
// constrained column to any table with AddTo.
type Enum struct {
	name   string
	values []string
}

// NewEnum declares an enum with the given labels. name is used to build
// the CHECK constraint's name; it is not a SQL type (SQLite has none).
func NewEnum(name string, values ...string) *Enum {
	return &Enum{name: name, values: append([]string(nil), values...)}
}

// Name returns the enum's declared name.
func (e *Enum) Name() string { return e.name }

// Values returns the labels in declaration order.
func (e *Enum) Values() []string { return e.values }

// CheckExpr renders the CHECK body constraining col to the enum labels:
// "col" IN ('a', 'b', ...).
func (e *Enum) CheckExpr(col string) string {
	var sb strings.Builder
	sb.WriteString(quoteIdent(col))
	sb.WriteString(" IN (")
	for i, v := range e.values {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(sqlStringLit(v))
	}
	sb.WriteByte(')')
	return sb.String()
}

// sqlStringLit renders s as a single-quoted SQL string literal, doubling
// embedded quotes. (The package's quoteLiteral is a PRAGMA helper that
// double-quotes, which is wrong for a value literal.)
func sqlStringLit(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// AddTo registers a TEXT column named col on t plus a table-level CHECK
// constraint restricting it to the enum's labels, and returns the typed
// column handle.
//
//	status := statusEnum.AddTo(orders, "status")
//	// orders."status" TEXT, CHECK ("status" IN ('open','paid','shipped'))
func (e *Enum) AddTo(t *Table, col string) *Col[string] {
	c := Add(t, Text(col))
	t.AddCheck(col+e.name+"Enum", e.CheckExpr(col))
	return c
}

// EnumCol is the free-function form of Enum.AddTo for one-off columns:
//
//	priority := sqlite.EnumCol(tasks, "priority", "low", "med", "high")
func EnumCol(t *Table, col string, values ...string) *Col[string] {
	return NewEnum(col, values...).AddTo(t, col)
}
