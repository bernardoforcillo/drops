package mysql

import (
	"fmt"

	"github.com/bernardoforcillo/drops"
)

// ColumnType describes a column's SQL type as it appears in CREATE
// TABLE — "BIGINT", "VARCHAR(255)", "DATETIME(6)".
type ColumnType interface{ TypeSQL() string }

type simpleType string

func (s simpleType) TypeSQL() string { return string(s) }

// Column is the type-erased column AST node. Most user code holds a
// *Col[T]; the untyped Column lets a table's column list be
// heterogeneous.
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
	onUpdate   string
	comment    string
	keyPrefix  int
	ref        *FK
	managed    bool

	// origin is the column this one was copied from by (*Table).As,
	// and nil on a column as declared. An alias copy is a second
	// handle on one column of one table, so everything that asks
	// which column a handle *is* has to see the two as equal — see
	// key.
	origin *Column
}

// FK describes a single-column foreign-key reference.
type FK struct {
	Target   *Column
	OnDelete string
	OnUpdate string
}

// OnDelete / OnUpdate set the referential actions on a foreign key.
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
func (c *Column) Comment() string       { return c.comment }

// KeyPrefixLength returns the prefix length the column carries into a
// PRIMARY KEY or UNIQUE KEY, or zero when it has none.
func (c *Column) KeyPrefixLength() int { return c.keyPrefix }

// IsManaged reports whether drops writes this column rather than the
// application — see (*Col[T]).Managed.
func (c *Column) IsManaged() bool { return c.managed }

// col implements ColRef for *Column; *Col[T] inherits it by embedding.
func (c *Column) col() *Column { return c }

// key returns the identity a column is recognised by, collapsing every
// alias copy onto the column it was declared as.
//
// Aliasing is a query-scope rename: it changes how a reference renders
// and nothing else. So a handle taken off an alias has to answer the
// same as the declared handle everywhere the question is "which column
// is this" — the INSERT column list an aligned row is matched against,
// an Entity's key columns, a page's ordering column, the column a SET
// or upsert assignment names. Comparing the two by pointer makes them
// strangers, and the worst of the resulting failures is silent: the
// row loses the values it bound to the DEFAULT fill in alignRow, and
// on a column that has a DEFAULT no server objects.
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

// ColRef is implemented by *Column and *Col[T]: a type-erased column
// reference for APIs that do not depend on the Go value type.
type ColRef interface {
	drops.Expression
	col() *Column
}

// WriteSQL writes a table-qualified column reference, or a bare one
// inside DDL that defines the table — see (*drops.Builder).BareIdents.
func (c *Column) WriteSQL(b *drops.Builder) {
	if b.BareIdents() {
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
func (c *Col[T]) NotNull() *Col[T] { c.Column.notNull = true; return c }

// PrimaryKey marks the column part of the PRIMARY KEY. Declaring it on
// more than one column declares a composite key, which Entity
// supports.
func (c *Col[T]) PrimaryKey() *Col[T] {
	c.Column.primary = true
	c.Column.notNull = true
	return c
}

// AutoIncrement marks the column AUTO_INCREMENT.
//
// MySQL allows exactly one such column per table and requires it to be
// indexed — in practice, to be the primary key. [CreateTable] does not
// enforce that; the server will.
func (c *Col[T]) AutoIncrement() *Col[T] { c.Column.autoInc = true; return c }

// Unique marks the column UNIQUE.
//
// Worth knowing before you reach for it: MySQL's upsert keys on any
// unique index, so every UNIQUE column you add widens what
// [Entity.UpsertMany] treats as a duplicate.
func (c *Col[T]) Unique() *Col[T] { c.Column.unique = true; return c }

// Managed marks the column as written by drops rather than by the
// application. NewEntity's drift check skips managed columns.
func (c *Col[T]) Managed() *Col[T] { c.Column.managed = true; return c }

// Default sets a raw SQL DEFAULT expression ("0", "CURRENT_TIMESTAMP").
func (c *Col[T]) Default(sqlExpr string) *Col[T] {
	c.Column.hasDefault = true
	c.Column.defaultSQL = sqlExpr
	return c
}

// OnUpdateExpr sets MySQL's per-column ON UPDATE clause — almost
// always CURRENT_TIMESTAMP, which is how MySQL maintains an
// updated_at without a trigger. It has no PostgreSQL equivalent, which
// is why the name does not pretend to be portable.
func (c *Col[T]) OnUpdateExpr(sqlExpr string) *Col[T] {
	c.Column.onUpdate = sqlExpr
	return c
}

// KeyPrefix indexes only the first n characters of the column when it
// takes part in the table's PRIMARY KEY or a UNIQUE KEY.
//
// A TEXT or BLOB column cannot enter a key without one — MySQL answers
// error 1170, "used in key specification without a key length" — and
// [CreateTable] has nowhere else to learn the length from. A secondary
// index says the same thing through (*Index).Prefix, which is the knob
// to reach for when the column is not part of the table definition.
func (c *Col[T]) KeyPrefix(n int) *Col[T] {
	c.Column.keyPrefix = n
	return c
}

// Comment attaches a COMMENT to the column.
func (c *Col[T]) Comment(text string) *Col[T] {
	c.Column.comment = text
	return c
}

// References declares a single-column foreign key to target.
//
// The target must already belong to a table: REFERENCES names a table,
// and a column that was never registered with one cannot supply that
// name. Add the target to its table first.
func (c *Col[T]) References(target *Col[T], opts ...func(*FK)) *Col[T] {
	if target.Column.table == nil {
		panic(fmt.Sprintf(
			"drops/mysql: column %q references %q, which belongs to no table; Add the target to its table first",
			c.Column.name, target.Column.name))
	}
	fk := &FK{Target: target.Column}
	for _, o := range opts {
		o(fk)
	}
	c.Column.ref = fk
	return c
}

// Comparison operators. The rendered SQL is dialect-agnostic; the
// placeholder and quoting come from the Builder's dialect.
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

func (c *Col[T]) IsNull() drops.Expression    { return nullCheck(c.Column, true) }
func (c *Col[T]) IsNotNull() drops.Expression { return nullCheck(c.Column, false) }

// In renders (col IN (?, ?, …)). An empty list renders FALSE: nothing
// is a member of the empty set, and MySQL rejects "IN ()" outright.
func (c *Col[T]) In(values ...T) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		if len(values) == 0 {
			b.WriteString("(FALSE)")
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

// Val binds a typed value for INSERT / UPDATE.
func (c *Col[T]) Val(v T) ColumnValue { return columnValue{col: c.Column, val: v} }

// Expr assigns a raw SQL expression to the column instead of a bound
// value.
func (c *Col[T]) Expr(e drops.Expression) ColumnValue {
	return exprValue{col: c.Column, expr: e}
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

// ColumnValue is a column bound to a value or expression for
// INSERT / UPDATE.
type ColumnValue interface {
	column() *Column
	writeValue(b *drops.Builder)

	// rebind returns the same assignment restated against col, which
	// names the same column through another handle on its table. Its
	// callers are (*UpdateBuilder).Set and
	// (*InsertBuilder).OnDuplicateKeyUpdate, and both pass a handle
	// the statement's own relation hands out — see rebindValue.
	rebind(col *Column) ColumnValue
}

type columnValue struct {
	col *Column
	val any
}

func (v columnValue) column() *Column              { return v.col }
func (v columnValue) writeValue(b *drops.Builder)  { b.AddArg(v.val) }
func (v columnValue) rebind(c *Column) ColumnValue { v.col = c; return v }

type exprValue struct {
	col  *Column
	expr drops.Expression
}

func (v exprValue) column() *Column             { return v.col }
func (v exprValue) writeValue(b *drops.Builder) { b.Append(v.expr) }

// rebind moves the assignment's target but not its expression: that
// one was built by the caller out of the handles the caller held, and
// drops only re-emits it. See (*Table).As.
func (v exprValue) rebind(c *Column) ColumnValue { v.col = c; return v }

// rebindValue restates an assignment against the handle t hands out
// for the column it names, so the reference renders qualified by the
// relation the statement actually names.
//
// It moves only a second handle on one of t's own columns: a column
// belonging to some other table is a deliberate cross-table reference
// and rewriting it would change what the statement means.
func rebindValue(t *Table, v ColumnValue) ColumnValue {
	c := v.column()
	if t == nil || c == nil {
		return v
	}
	own := t.byName[c.name]
	if own == nil || own == c || own.key() != c.key() {
		return v
	}
	return v.rebind(own)
}

// Bind pairs a column with an untyped value, for callers holding a
// *Column and a value whose Go type is only known at runtime. Prefer
// (*Col[T]).Val wherever the type is known.
func Bind(col ColRef, v any) ColumnValue {
	return columnValue{col: col.col(), val: v}
}
