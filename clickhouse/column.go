package clickhouse

import "github.com/bernardoforcillo/drops"

// Column is the type-erased AST node for a column reference. Most user
// code holds a *Col[T] (returned by every type constructor) which
// embeds *Column and adds type-safe builder + operator methods.
type Column struct {
	name     string
	table    *Table
	typ      ColumnType
	nullable bool   // for Nullable(T) marker on user-built columns
	codec    string // raw CODEC(...) clause
	ttl      string // raw TTL clause
	comment  string // COMMENT '…'
	defSQL   string // DEFAULT <expr>
	hasDef   bool
}

// Name returns the column's unqualified identifier.
func (c *Column) Name() string { return c.name }

// Table returns the table this column was registered with, or nil
// before registration.
func (c *Column) Table() *Table { return c.table }

// Type returns the column's SQL type.
func (c *Column) Type() ColumnType { return c.typ }

// IsNullable reports whether the column was wrapped in Nullable().
func (c *Column) IsNullable() bool { return c.nullable }

// HasDefault reports whether a DEFAULT clause was set.
func (c *Column) HasDefault() bool { return c.hasDef }

// DefaultSQL returns the raw DEFAULT expression.
func (c *Column) DefaultSQL() string { return c.defSQL }

// Codec returns the CODEC(...) clause, or empty.
func (c *Column) Codec() string { return c.codec }

// TTL returns the per-column TTL expression, or empty.
func (c *Column) TTL() string { return c.ttl }

// Comment returns the column comment, or empty.
func (c *Column) Comment() string { return c.comment }

// col is the ColRef implementation; *Col[T] inherits via embedding.
func (c *Column) col() *Column { return c }

// ColRef is implemented by *Column and *Col[T]. Use it where the value
// type doesn't matter (engine ORDER BY / PARTITION BY, index columns,
// SELECT projections).
type ColRef interface {
	drops.Expression
	col() *Column
}

// WriteSQL writes a qualified reference to the column.
func (c *Column) WriteSQL(b *drops.Builder) {
	if c.table != nil {
		c.table.writeRef(b)
		b.WriteByte('.')
	}
	b.WriteIdent(c.name)
}

// As / Asc / Desc helpers.
func (c *Column) As(alias string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		c.WriteSQL(b)
		b.WriteString(" AS ")
		b.WriteIdent(alias)
	})
}

func (c *Column) Asc() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		c.WriteSQL(b)
		b.WriteString(" ASC")
	})
}

func (c *Column) Desc() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		c.WriteSQL(b)
		b.WriteString(" DESC")
	})
}

// Col is the typed handle for a column whose Go value type is T.
type Col[T any] struct {
	*Column
}

func newCol[T any](name string, typ ColumnType) *Col[T] {
	mustIdent("column", name)
	return &Col[T]{Column: &Column{name: name, typ: typ}}
}

// Builder methods — preserve T through the chain.

// Nullable wraps the column type in Nullable(...).
func (c *Col[T]) Nullable() *Col[T] {
	c.Column.typ = TypeNullable(c.Column.typ)
	c.Column.nullable = true
	return c
}

// LowCardinality wraps the column type in LowCardinality(...).
func (c *Col[T]) LowCardinality() *Col[T] {
	c.Column.typ = TypeLowCardinality(c.Column.typ)
	return c
}

// Default sets a raw SQL default expression (e.g. "0", "now()",
// "'pending'", "uuidv4()"). DEFAULT expressions cannot be parameterised.
func (c *Col[T]) Default(sqlExpr string) *Col[T] {
	c.Column.defSQL = sqlExpr
	c.Column.hasDef = true
	return c
}

// Codec sets the CODEC(...) clause — e.g. Codec("ZSTD(3)"),
// Codec("Delta, LZ4"). Pass the inner text without parentheses.
func (c *Col[T]) Codec(spec string) *Col[T] {
	c.Column.codec = spec
	return c
}

// TTL sets a per-column TTL expression.
func (c *Col[T]) TTL(expr string) *Col[T] {
	c.Column.ttl = expr
	return c
}

// Comment attaches a free-form comment that ends up in the CREATE
// TABLE column definition.
func (c *Col[T]) Comment(text string) *Col[T] {
	c.Column.comment = text
	return c
}

// Comparison operators (type-safe).

func (c *Col[T]) Eq(v T) drops.Expression  { return Eq(c.Column, v) }
func (c *Col[T]) Ne(v T) drops.Expression  { return Ne(c.Column, v) }
func (c *Col[T]) Gt(v T) drops.Expression  { return Gt(c.Column, v) }
func (c *Col[T]) Gte(v T) drops.Expression { return Gte(c.Column, v) }
func (c *Col[T]) Lt(v T) drops.Expression  { return Lt(c.Column, v) }
func (c *Col[T]) Lte(v T) drops.Expression { return Lte(c.Column, v) }

// Column-to-column comparisons.

func (c *Col[T]) EqCol(o *Col[T]) drops.Expression  { return Eq(c.Column, o.Column) }
func (c *Col[T]) NeCol(o *Col[T]) drops.Expression  { return Ne(c.Column, o.Column) }
func (c *Col[T]) GtCol(o *Col[T]) drops.Expression  { return Gt(c.Column, o.Column) }
func (c *Col[T]) GteCol(o *Col[T]) drops.Expression { return Gte(c.Column, o.Column) }
func (c *Col[T]) LtCol(o *Col[T]) drops.Expression  { return Lt(c.Column, o.Column) }
func (c *Col[T]) LteCol(o *Col[T]) drops.Expression { return Lte(c.Column, o.Column) }

// Set / null tests.

func (c *Col[T]) In(values ...T) drops.Expression {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return In(c.Column, out...)
}

func (c *Col[T]) NotIn(values ...T) drops.Expression {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return NotIn(c.Column, out...)
}

func (c *Col[T]) IsNull() drops.Expression          { return IsNull(c.Column) }
func (c *Col[T]) IsNotNull() drops.Expression       { return IsNotNull(c.Column) }
func (c *Col[T]) Between(lo, hi T) drops.Expression { return Between(c.Column, lo, hi) }

// Pattern matching.

func (c *Col[T]) Like(pattern string) drops.Expression  { return Like(c.Column, pattern) }
func (c *Col[T]) ILike(pattern string) drops.Expression { return ILike(c.Column, pattern) }

// Val binds a typed value as the column's payload in an INSERT row.
func (c *Col[T]) Val(v T) ColumnValue { return &valueBinding[T]{col: c.Column, val: v} }

// Expr binds an arbitrary SQL expression to the column in an INSERT.
func (c *Col[T]) Expr(e drops.Expression) ColumnValue {
	return &exprBinding{col: c.Column, expr: e}
}
