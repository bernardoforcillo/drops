package pg

import "github.com/bernardoforcillo/drops"

// ColumnType describes the SQL type of a column.
type ColumnType interface {
	// TypeSQL returns the PostgreSQL type expression as it appears in
	// CREATE TABLE — e.g. "text", "integer", "varchar(255)", "uuid".
	TypeSQL() string
}

// Column is the type-erased AST node for a column reference. It is
// registered with a Table and can be written into a drops.Builder.
//
// Most user code holds a *Col[T] instead, which embeds *Column and adds
// type-safe builder and operator methods. The untyped Column exists so
// table column lists can be heterogeneous and so generic methods (Go
// does not allow them on non-generic types) need not be added to Table.
type Column struct {
	name       string
	table      *Table
	typ        ColumnType
	notNull    bool
	primary    bool
	unique     bool
	defaultSQL string
	hasDefault bool
	ref        *FK
	version    bool // marked via (*Col[T]).OptimisticLock()
}

// FK describes a foreign-key reference.
type FK struct {
	Target   *Column
	OnDelete string
	OnUpdate string
}

// Name returns the unqualified column name.
func (c *Column) Name() string { return c.name }

// Table returns the table this column was registered with, or nil before
// registration.
func (c *Column) Table() *Table { return c.table }

// Type returns the column's SQL type.
func (c *Column) Type() ColumnType { return c.typ }

// IsNotNull reports whether the column was declared NOT NULL.
func (c *Column) IsNotNull() bool { return c.notNull }

// IsPrimaryKey reports whether the column was declared PRIMARY KEY.
func (c *Column) IsPrimaryKey() bool { return c.primary }

// IsUnique reports whether the column was declared UNIQUE.
func (c *Column) IsUnique() bool { return c.unique }

// HasDefault reports whether a DEFAULT clause was declared.
func (c *Column) HasDefault() bool { return c.hasDefault }

// DefaultSQL returns the raw SQL DEFAULT expression.
func (c *Column) DefaultSQL() string { return c.defaultSQL }

// ForeignKey returns the foreign-key reference, or nil if none.
func (c *Column) ForeignKey() *FK { return c.ref }

// IsOptimisticVersion reports whether the column is the version
// column used for optimistic locking. Marked via
// (*Col[T]).OptimisticLock().
func (c *Column) IsOptimisticVersion() bool { return c.version }

// col returns c. It is the implementation of ColRef for *Column itself;
// *Col[T] inherits the method via embedding.
func (c *Column) col() *Column { return c }

// ColRef is implemented by *Column and *Col[T]. It is the type-erased
// column reference used by APIs that don't depend on the column's Go
// value type — JOIN ON wiring, ON CONFLICT targets, EXCLUDED references.
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

// As returns an aliased column expression: "<col>" AS "<alias>".
func (c *Column) As(alias string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		c.WriteSQL(b)
		b.WriteString(" AS ")
		b.WriteIdent(alias)
	})
}

// Asc / Desc produce ORDER BY direction expressions.
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
//
// It embeds *Column so it implements drops.Expression and exposes
// Asc/Desc/As; its own methods (NotNull, PrimaryKey, Eq, In, Val, ...)
// preserve the type parameter so the chain stays typed end-to-end.
type Col[T any] struct {
	*Column
}

func newCol[T any](name string, typ ColumnType) *Col[T] {
	mustIdent("column", name)
	return &Col[T]{Column: &Column{name: name, typ: typ}}
}

// Builder methods — overridden so the chain returns *Col[T] instead of
// the embedded *Column.

func (c *Col[T]) NotNull() *Col[T] {
	c.Column.notNull = true
	return c
}

func (c *Col[T]) PrimaryKey() *Col[T] {
	c.Column.primary = true
	c.Column.notNull = true
	return c
}

func (c *Col[T]) Unique() *Col[T] {
	c.Column.unique = true
	return c
}

// Default sets a raw SQL default expression — e.g. "now()", "0",
// "'pending'". PostgreSQL DEFAULT clauses cannot be parameterised.
func (c *Col[T]) Default(sqlExpr string) *Col[T] {
	c.Column.hasDefault = true
	c.Column.defaultSQL = sqlExpr
	return c
}

// OptimisticLock marks the column as the version column for
// optimistic locking. When an Entity bound to the table issues an
// UPDATE, it automatically guards with "AND version = current" and
// bumps the column ("SET version = version + 1"). If no row matches
// the version, ErrStaleObject is returned. Apply this to a single
// integer column per table.
func (c *Col[T]) OptimisticLock() *Col[T] {
	c.Column.version = true
	return c
}

// References declares a foreign-key constraint to a target column. The
// target's value type must match — type inference catches mismatches at
// declaration time.
func (c *Col[T]) References(target *Col[T], opts ...func(*FK)) *Col[T] {
	fk := &FK{Target: target.Column}
	for _, o := range opts {
		o(fk)
	}
	c.Column.ref = fk
	return c
}

// OnDelete configures the referential action for ON DELETE.
func OnDelete(action string) func(*FK) { return func(fk *FK) { fk.OnDelete = action } }

// OnUpdate configures the referential action for ON UPDATE.
func OnUpdate(action string) func(*FK) { return func(fk *FK) { fk.OnUpdate = action } }

// Comparison operators ---------------------------------------------------

func (c *Col[T]) Eq(v T) drops.Expression  { return Eq(c.Column, v) }
func (c *Col[T]) Ne(v T) drops.Expression  { return Ne(c.Column, v) }
func (c *Col[T]) Gt(v T) drops.Expression  { return Gt(c.Column, v) }
func (c *Col[T]) Gte(v T) drops.Expression { return Gte(c.Column, v) }
func (c *Col[T]) Lt(v T) drops.Expression  { return Lt(c.Column, v) }
func (c *Col[T]) Lte(v T) drops.Expression { return Lte(c.Column, v) }

// Column-to-column comparisons. The target's type must match.

func (c *Col[T]) EqCol(other *Col[T]) drops.Expression  { return Eq(c.Column, other.Column) }
func (c *Col[T]) NeCol(other *Col[T]) drops.Expression  { return Ne(c.Column, other.Column) }
func (c *Col[T]) GtCol(other *Col[T]) drops.Expression  { return Gt(c.Column, other.Column) }
func (c *Col[T]) GteCol(other *Col[T]) drops.Expression { return Gte(c.Column, other.Column) }
func (c *Col[T]) LtCol(other *Col[T]) drops.Expression  { return Lt(c.Column, other.Column) }
func (c *Col[T]) LteCol(other *Col[T]) drops.Expression { return Lte(c.Column, other.Column) }

// Pattern matching.

func (c *Col[T]) Like(pattern string) drops.Expression  { return Like(c.Column, pattern) }
func (c *Col[T]) ILike(pattern string) drops.Expression { return ILike(c.Column, pattern) }

// Set membership / null / range.

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

// Insert / Update bindings ---------------------------------------------

// Val binds a typed value as the column's payload in an INSERT row or
// UPDATE assignment.
func (c *Col[T]) Val(v T) ColumnValue { return &valueBinding[T]{col: c.Column, val: v} }

// Expr binds an arbitrary expression to the column.
func (c *Col[T]) Expr(e drops.Expression) ColumnValue {
	return &exprBinding{col: c.Column, expr: e}
}

// SetDefault binds the SQL DEFAULT keyword.
func (c *Col[T]) SetDefault() ColumnValue {
	return &exprBinding{col: c.Column, expr: sqlDefault{}}
}

// Excluded returns an EXCLUDED.<col> reference for use inside an
// ON CONFLICT DO UPDATE clause.
func (c *Col[T]) Excluded() drops.Expression { return Excluded(c.Column) }
