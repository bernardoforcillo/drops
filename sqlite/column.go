package sqlite

import (
	"database/sql"

	"github.com/bernardoforcillo/drops"
)

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
	nullStated bool // NotNull, PrimaryKey or Nullable was called
	primary    bool
	unique     bool
	autoInc    bool
	defaultSQL string
	hasDefault bool
	ref        *FK
	pii        bool
	managed    bool // drops writes this column, not the application

	// renamedFrom is the name this column used to have, set by
	// (*Col[T]).RenamedFrom. It is the one fact about a column that no
	// comparison of two schemas can recover — see rename.go — and the
	// schema is where Push can find it.
	renamedFrom string
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

// IsNullable reports whether the column admits NULL. It is the
// complement of IsNotNull, named for the question that matters at
// bind and scan time and spelled the same in all four dialects so the
// shared checker asks exactly one question. A SQLite column admits
// NULL unless it says otherwise, so one that stated nothing answers
// true.
func (c *Column) IsNullable() bool { return !c.notNull }

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

// Col is the typed column handle whose Go value type is T.
type Col[T any] struct{ *Column }

func newCol[T any](name string, typ ColumnType) *Col[T] {
	mustIdent("column", name)
	return &Col[T]{Column: &Column{name: name, typ: typ}}
}

// NotNull marks the column NOT NULL.
func (c *Col[T]) NotNull() *Col[T] { c.Column.notNull, c.Column.nullStated = true, true; return c }

// Nullable states that the column admits NULL.
//
// It changes nothing in the DDL — a SQLite column is nullable unless
// it says otherwise — and everything in what drops will let you bind
// it to: NewEntity requires the struct field bound to a nullable
// column to be one that can receive NULL. It is the counterpart of
// NotNull, and the two are last-writer-wins.
func (c *Col[T]) Nullable() *Col[T] { c.Column.notNull, c.Column.nullStated = false, true; return c }

// PrimaryKey marks the column as the (single-column) PRIMARY KEY.
func (c *Col[T]) PrimaryKey() *Col[T] {
	c.Column.primary = true
	c.Column.notNull = true
	// A primary key states NOT NULL implicitly.
	c.Column.nullStated = true
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

// Managed marks the column as written by drops rather than by the
// application — the soft-delete marker, the timestamps a template
// keeps current. NewEntity's drift check skips managed columns.
func (c *Col[T]) Managed() *Col[T] { c.Column.managed = true; return c }

// IsManaged reports whether drops writes this column rather than the
// application.
func (c *Column) IsManaged() bool { return c.managed }

// RenamedFrom states that this column is the column that used to be
// called previous — the same column, the same data, a different name.
//
// Nothing in a pair of schemas can tell a rename from a drop and an
// add, so drops asks rather than guesses, and this is the answer
// written where the question is. GenerateMigration can record an
// answer in the migration directory; Push has no migration directory,
// and its refusal is otherwise unanswerable by anything durable. A
// rename is a fact about the schema's history, the schema is what Push
// reads, so the schema is where the fact belongs — and it then travels
// to every database the schema is pushed to, not just to the one
// whoever typed the flag was pointed at.
//
// It matters most here. A SQLite rename that goes unstated is not a
// DROP COLUMN anybody can read in the statement list: the rebuild
// copies the columns both sides name and simply leaves this one out,
// so the data goes with nothing at all in the SQL to say so.
//
// The declaration is inert once the rename has happened: it is applied
// only while the old name is still in the database and the new one is
// not, so it may be left in place, and should be until every database
// the schema is pushed to has moved past it.
//
//	sqlite.Add(Users, sqlite.Text("emailAddress").NotNull().RenamedFrom("email"))
func (c *Col[T]) RenamedFrom(previous string) *Col[T] {
	c.Column.renamedFrom = previous
	return c
}

// PreviousName returns the name the column was declared to have been
// renamed from, or empty when it was not. See (*Col[T]).RenamedFrom.
func (c *Column) PreviousName() string { return c.renamedFrom }

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

// Asc / Desc produce ORDER BY terms.
//
// These exist on the untyped Column so any handle can order a query,
// and are distinct from the package-level Asc / Desc, which build the
// OrderingColumn that keyset pagination needs.
func (c *Column) Asc() drops.Expression  { return orderTerm(c, " ASC") }
func (c *Column) Desc() drops.Expression { return orderTerm(c, " DESC") }

func orderTerm(c *Column, dir string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		c.WriteSQL(b)
		b.WriteString(dir)
	})
}

// As aliases a column in a SELECT projection.
func (c *Column) As(alias string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		c.WriteSQL(b)
		b.WriteString(" AS ")
		b.WriteIdent(alias)
	})
}

// Val binds a typed value for INSERT/UPDATE.
func (c *Col[T]) Val(v T) ColumnValue { return columnValue{col: c.Column, val: v} }

// SetNull binds SQL NULL as the column's value.
//
// It is a parameter placeholder bound to nil, not the literal token,
// so an INSERT that sometimes writes NULL is the same statement as
// one that writes a value, and the NULL travels through the same
// binding every other value does — where PII redaction, hooks and
// tracers can see it. Before this existed there was no way at all to
// write NULL into a SQLite INSERT through the public API.
//
// It does not refuse a NOT NULL column: that violation is one the
// database reports precisely, and turning it into a panic on a
// request path would trade a good error for a process failure.
func (c *Col[T]) SetNull() ColumnValue { return columnValue{col: c.Column, val: nil} }

// ValPtr binds *p, or NULL when p is nil. It is the shape an optional
// struct field already has, and the one AutoTable reads as "this
// column is nullable".
func (c *Col[T]) ValPtr(p *T) ColumnValue {
	if p == nil {
		return c.SetNull()
	}
	// Binding *p rather than p keeps the bound argument's Go type
	// identical to what Val would have bound.
	return c.Val(*p)
}

// ValNull binds v.V, or NULL when v is not Valid.
func (c *Col[T]) ValNull(v sql.Null[T]) ColumnValue {
	if !v.Valid {
		return c.SetNull()
	}
	// Unwrapped rather than handed to the driver: sql.Null[T].Value
	// did not convert its payload before Go 1.25, so passing the
	// wrapper through would make the parameter depend on the
	// toolchain.
	return c.Val(v.V)
}

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

// And / Or combine predicates, ignoring the nil ones. With no
// arguments — or with nothing but nils — And renders TRUE and Or
// renders FALSE, the identity of each. (SQLite has understood the two
// keywords since 3.23.)
func And(preds ...drops.Expression) drops.Expression { return boolChain(" AND ", "TRUE", preds) }
func Or(preds ...drops.Expression) drops.Expression  { return boolChain(" OR ", "FALSE", preds) }

func boolChain(sep, empty string, preds []drops.Expression) drops.Expression {
	preds = dropNilPreds(preds)
	return drops.ExprFunc(func(b *drops.Builder) {
		if len(preds) == 0 {
			b.WriteString(empty)
			return
		}
		if len(preds) == 1 {
			b.Append(preds[0])
			return
		}
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
