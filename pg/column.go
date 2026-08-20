package pg

import (
	"database/sql"

	"github.com/bernardoforcillo/drops"
)

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
	nullStated bool // NotNull, PrimaryKey or Nullable was called
	primary    bool
	unique     bool
	defaultSQL string
	hasDefault bool
	ref        *FK
	version    bool // marked via (*Col[T]).OptimisticLock()
	pii        bool // marked via (*Col[T]).AsPII()
	managed    bool // drops writes this column, not the application

	// origin is the column this one was copied from by (*Table).As,
	// and nil on a column as declared. An alias copy is a second
	// handle on one column of one table, so everything that asks
	// which column a handle *is* has to see the two as equal — see
	// key.
	origin *Column
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

// IsNullable reports whether the column admits NULL.
//
// It is the complement of IsNotNull, named for the question that
// matters at bind and scan time rather than for the DDL keyword, and
// spelled the same in all four dialects so the shared checker asks
// exactly one question. A PostgreSQL column admits NULL unless it
// says otherwise, so a column that stated nothing answers true.
func (c *Column) IsNullable() bool { return !c.notNull }

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

// IsManaged reports whether drops writes this column rather than the
// application — the soft-delete marker, the timestamps a mixin keeps
// current. Such a column legitimately has no struct field, so
// NewEntity's drift check skips it.
func (c *Column) IsManaged() bool { return c.managed }

// IsOptimisticVersion reports whether the column is the version
// column used for optimistic locking. Marked via
// (*Col[T]).OptimisticLock().
func (c *Column) IsOptimisticVersion() bool { return c.version }

// col returns c. It is the implementation of ColRef for *Column itself;
// *Col[T] inherits the method via embedding.
func (c *Column) col() *Column { return c }

// key returns the identity a column is recognised by, collapsing every
// alias copy onto the column it was declared as.
//
// Aliasing is a query-scope rename: it changes how a reference renders
// and nothing else. So a handle taken off an alias has to answer the
// same as the declared handle everywhere the question is "which column
// is this" — the INSERT column list, a hook's Has, an Entity's key
// columns, the tenant axis, a cursor's ordering column, the CREATE
// TABLE body's key set. Comparing the two by pointer instead makes
// them strangers, and most of the resulting failures are silent:
// the row loses its values to the DEFAULT fill in alignRow, the
// CREATE TABLE loses its PRIMARY KEY, a snapshot records a key column
// as nullable.
//
// The identity is the declared column rather than the name because two
// tables can both have a "name" column and they are not the same
// column.
func (c *Column) key() *Column {
	if c.origin != nil {
		return c.origin
	}
	return c
}

// ColRef is implemented by *Column and *Col[T]. It is the type-erased
// column reference used by APIs that don't depend on the column's Go
// value type — JOIN ON wiring, ON CONFLICT targets, EXCLUDED references.
type ColRef interface {
	drops.Expression
	col() *Column
}

// WriteSQL writes a qualified reference to the column.
func (c *Column) WriteSQL(b *drops.Builder) {
	if b.BareIdents() {
		// DDL that defines this very table cannot qualify the
		// reference — see (*drops.Builder).BareIdents.
		b.WriteIdent(c.name)
		return
	}
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
	c.Column.nullStated = true
	return c
}

// Nullable states that the column admits NULL.
//
// It changes nothing in the DDL — a PostgreSQL column is nullable
// unless it says otherwise — and everything in what drops will let
// you bind it to: NewEntity requires the struct field bound to a
// nullable column to be one that can receive NULL, so writing this is
// how a schema says the field must be a *T or an sql.Null[T]. It is
// the counterpart of NotNull, and the two are last-writer-wins.
func (c *Col[T]) Nullable() *Col[T] {
	c.Column.notNull = false
	c.Column.nullStated = true
	return c
}

func (c *Col[T]) PrimaryKey() *Col[T] {
	c.Column.primary = true
	c.Column.notNull = true
	// A primary key states NOT NULL implicitly, and a schema that
	// said PrimaryKey has decided about NULL as much as one that
	// spelled it out.
	c.Column.nullStated = true
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

// Managed marks the column as written by drops rather than by the
// application: the soft-delete marker a mixin flips, the timestamps it
// keeps current. NewEntity's drift check skips managed columns, since
// a struct field for them would be redundant rather than missing.
func (c *Col[T]) Managed() *Col[T] {
	c.Column.managed = true
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

// SetNull binds SQL NULL as the column's value.
//
// It is the typed counterpart of SetDefault: a parameter placeholder
// bound to nil, not the literal token, so an INSERT that sometimes
// writes NULL is the same statement as one that writes a value — one
// entry in the server's plan cache rather than two — and the NULL
// travels through AddArg like every other bound value, where hooks,
// tracers and PII redaction can see it.
//
// It does not refuse a NOT NULL column. This is called on a request
// path, and the database reports that violation precisely; turning it
// into a panic would trade a FieldError that names the column for a
// process failure.
func (c *Col[T]) SetNull() ColumnValue {
	return &valueBinding[any]{col: c.Column, val: nil}
}

// ValPtr binds *p, or NULL when p is nil. It is the shape an optional
// struct field already has, and the one AutoTable and dropsgen read
// as "this column is nullable".
func (c *Col[T]) ValPtr(p *T) ColumnValue {
	if p == nil {
		return c.SetNull()
	}
	// Binding *p rather than p keeps the bound argument's Go type
	// identical to what Val would have bound, so a hook, tracer or
	// PII formatter sees one thing rather than two.
	return c.Val(*p)
}

// ValNull binds v.V, or NULL when v is not Valid.
func (c *Col[T]) ValNull(v sql.Null[T]) ColumnValue {
	if !v.Valid {
		return c.SetNull()
	}
	// The wrapper is unwrapped rather than handed to the driver:
	// sql.Null[T].Value did not convert its payload before Go 1.25,
	// so passing one through would make the parameter's fate depend
	// on the toolchain.
	return c.Val(v.V)
}

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
