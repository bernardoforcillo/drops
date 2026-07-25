package sqlite

import "github.com/bernardoforcillo/drops"

// ColumnType describes the SQL type of a column as it appears in CREATE
// TABLE — e.g. "INTEGER", "TEXT", "REAL", "BLOB", "NUMERIC".
type ColumnType interface{ TypeSQL() string }

type simpleType string

func (s simpleType) TypeSQL() string { return string(s) }

// Column is the type-erased column AST node. Most user code holds a
// *Col[T]; the untyped Column lets table column lists be heterogeneous.
type Column struct {
	name       string
	table      *Table
	typ        ColumnType
	notNull    bool
	primary    bool
	unique     bool
	autoInc    bool
	defaultSQL string
	hasDefault bool
	ref        *FK
	pii        bool
}

// FK describes a single-column foreign-key reference.
type FK struct {
	Target   *Column
	OnDelete string
	OnUpdate string
}

// OnDelete / OnUpdate configure the referential actions on a FK.
func OnDelete(action string) func(*FK) { return func(fk *FK) { fk.OnDelete = action } }
func OnUpdate(action string) func(*FK) { return func(fk *FK) { fk.OnUpdate = action } }

func (c *Column) Name() string          { return c.name }
func (c *Column) Table() *Table         { return c.table }
func (c *Column) Type() ColumnType      { return c.typ }
func (c *Column) IsNotNull() bool       { return c.notNull }
func (c *Column) IsPrimaryKey() bool    { return c.primary }
func (c *Column) IsUnique() bool        { return c.unique }
func (c *Column) IsAutoIncrement() bool { return c.autoInc }
func (c *Column) HasDefault() bool      { return c.hasDefault }
func (c *Column) DefaultSQL() string    { return c.defaultSQL }
func (c *Column) ForeignKey() *FK       { return c.ref }

// col implements ColRef for *Column; *Col[T] inherits it via embedding.
func (c *Column) col() *Column { return c }

// ColRef is implemented by *Column and *Col[T]: a type-erased column
// reference for APIs that don't depend on the Go value type.
type ColRef interface {
	drops.Expression
	col() *Column
}

// WriteSQL writes a table-qualified column reference. The dialect on
// the Builder controls the quote character.
func (c *Column) WriteSQL(b *drops.Builder) {
	if c.table != nil {
		c.table.writeRef(b)
		b.WriteByte('.')
	}
	b.WriteIdent(c.name)
}

// Col is the typed column handle whose Go value type is T.
type Col[T any] struct{ *Column }

func newCol[T any](name string, typ ColumnType) *Col[T] {
	mustIdent("column", name)
	return &Col[T]{Column: &Column{name: name, typ: typ}}
}

// NotNull marks the column NOT NULL.
func (c *Col[T]) NotNull() *Col[T] { c.Column.notNull = true; return c }

// PrimaryKey marks the column as the (single-column) PRIMARY KEY.
func (c *Col[T]) PrimaryKey() *Col[T] {
	c.Column.primary = true
	c.Column.notNull = true
	return c
}

// AutoIncrement marks an INTEGER PRIMARY KEY as AUTOINCREMENT. SQLite
// only permits AUTOINCREMENT on an INTEGER PRIMARY KEY.
func (c *Col[T]) AutoIncrement() *Col[T] {
	c.Column.autoInc = true
	return c
}

// Unique marks the column UNIQUE.
func (c *Col[T]) Unique() *Col[T] { c.Column.unique = true; return c }

// Default sets a raw SQL default expression (e.g. "0", "CURRENT_TIMESTAMP").
func (c *Col[T]) Default(sqlExpr string) *Col[T] {
	c.Column.hasDefault = true
	c.Column.defaultSQL = sqlExpr
	return c
}

// References declares a single-column foreign key to target.
func (c *Col[T]) References(target *Col[T], opts ...func(*FK)) *Col[T] {
	fk := &FK{Target: target.Column}
	for _, o := range opts {
		o(fk)
	}
	c.Column.ref = fk
	return c
}

// Comparison operators — the rendered SQL is dialect-agnostic (the
// placeholder and quoting come from the Builder's dialect).
func (c *Col[T]) Eq(v T) drops.Expression  { return cmp(c.Column, "=", v) }
func (c *Col[T]) Ne(v T) drops.Expression  { return cmp(c.Column, "<>", v) }
func (c *Col[T]) Gt(v T) drops.Expression  { return cmp(c.Column, ">", v) }
func (c *Col[T]) Gte(v T) drops.Expression { return cmp(c.Column, ">=", v) }
func (c *Col[T]) Lt(v T) drops.Expression  { return cmp(c.Column, "<", v) }
func (c *Col[T]) Lte(v T) drops.Expression { return cmp(c.Column, "<=", v) }

// EqCol compares two columns.
func (c *Col[T]) EqCol(other ColRef) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		c.Column.WriteSQL(b)
		b.WriteString(" = ")
		other.col().WriteSQL(b)
		b.WriteByte(')')
	})
}

// IsNull / IsNotNull.
func (c *Col[T]) IsNull() drops.Expression    { return nullCheck(c.Column, true) }
func (c *Col[T]) IsNotNull() drops.Expression { return nullCheck(c.Column, false) }

// In renders (col IN (?, ?, ...)). Empty renders "(0)" (never matches).
func (c *Col[T]) In(values ...T) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		if len(values) == 0 {
			b.WriteString("(0)")
			return
		}
		b.WriteByte('(')
		c.Column.WriteSQL(b)
		b.WriteString(" IN (")
		for i, v := range values {
			if i > 0 {
				b.WriteString(", ")
			}
			b.AddArg(v)
		}
		b.WriteString("))")
	})
}

// Val binds a typed value for INSERT/UPDATE.
func (c *Col[T]) Val(v T) ColumnValue { return columnValue{col: c.Column, val: v} }

func cmp(c *Column, op string, v any) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		c.WriteSQL(b)
		b.WriteByte(' ')
		b.WriteString(op)
		b.WriteByte(' ')
		b.AddArg(v)
		b.WriteByte(')')
	})
}

func nullCheck(c *Column, isNull bool) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		c.WriteSQL(b)
		if isNull {
			b.WriteString(" IS NULL)")
		} else {
			b.WriteString(" IS NOT NULL)")
		}
	})
}

// And / Or combine predicates.
func And(preds ...drops.Expression) drops.Expression { return boolChain(" AND ", preds) }
func Or(preds ...drops.Expression) drops.Expression  { return boolChain(" OR ", preds) }

func boolChain(sep string, preds []drops.Expression) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		for i, p := range preds {
			if i > 0 {
				b.WriteString(sep)
			}
			b.Append(p)
		}
		b.WriteByte(')')
	})
}

// ColumnValue is a column bound to a value for INSERT/UPDATE.
type ColumnValue interface {
	column() *Column
	writeValue(b *drops.Builder)
}

type columnValue struct {
	col *Column
	val any
}

func (v columnValue) column() *Column { return v.col }
func (v columnValue) writeValue(b *drops.Builder) {
	// PII columns bind a redaction marker so loggers/hooks see
	// "<redacted>"; db.Exec/Query unwrap it before the driver call.
	if v.col != nil && v.col.pii {
		b.AddArg(piiArg{Value: v.val})
		return
	}
	b.AddArg(v.val)
}

// exprValue assigns a raw SQL expression (not a bound value) to a column.
type exprValue struct {
	col  *Column
	expr drops.Expression
}

func (v exprValue) column() *Column             { return v.col }
func (v exprValue) writeValue(b *drops.Builder) { b.Append(v.expr) }
