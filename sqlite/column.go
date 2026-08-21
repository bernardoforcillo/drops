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
	managed    bool // drops writes this column, not the application

	// origin is the column this one was copied from by (*Table).As,
	// and nil on a column as declared. An alias rebinds its columns so
	// they render under the alias; origin is what lets the copy still
	// BE the declared column everywhere a column is identified rather
	// than rendered — see key.
	origin *Column
}

// key returns the identity a column is recognised by, collapsing every
// alias copy onto the column it was declared as.
//
// Aliasing changes how a reference RENDERS and nothing else, so
// everywhere drops compares columns rather than writing them — an
// entity's key columns, the tenant axis, a hook's Has, a page's
// ordering column — it compares keys. Without it an alias's handle
// looks like a second column that happens to have the same name, and
// the caller who declared the tenant axis over an alias handle gets a
// statement that stamps two of them.
func (c *Column) key() *Column {
	if c.origin != nil {
		return c.origin
	}
	return c
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

// Managed marks the column as written by drops rather than by the
// application — the soft-delete marker, the timestamps a template
// keeps current. NewEntity's drift check skips managed columns.
func (c *Col[T]) Managed() *Col[T] { c.Column.managed = true; return c }

// IsManaged reports whether drops writes this column rather than the
// application.
func (c *Column) IsManaged() bool { return c.managed }

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
	return binOp(c.Column, "=", other.col())
}

// IsNull / IsNotNull.
func (c *Col[T]) IsNull() drops.Expression    { return nullCheck(c.Column, true) }
func (c *Col[T]) IsNotNull() drops.Expression { return nullCheck(c.Column, false) }

// In renders (col IN (?, ?, ...)). Empty renders "(0)" (never matches).
//
// Every value is BOUND, never rendered as an expression, which is the
// rule the whole typed column form follows and the reason it needs
// saying: T can be instantiated as an interface type, so a *Col[any]
// would otherwise render whatever an Expression-valued argument writes
// instead of binding it — a change of meaning decided by the type
// parameter rather than by the call. The package-level [In] is the one
// that takes an operand, and it holds it.
func (c *Col[T]) In(values ...T) drops.Expression {
	if len(values) == 0 {
		return drops.Raw("(0)")
	}
	// One part before the column, one opening the list, one comma per
	// further value, and the two closing parentheses.
	parts := make([]string, len(values)+2)
	parts[0] = "("
	parts[1] = " IN ("
	for i := 2; i <= len(values); i++ {
		parts[i] = ", "
	}
	parts[len(values)+1] = "))"
	operands := make([]drops.Expression, 0, len(values)+1)
	operands = append(operands, c.Column)
	for _, v := range values {
		operands = append(operands, drops.Param{Value: v})
	}
	return &opExpr{parts: parts, operands: operands}
}

// Asc / Desc produce ORDER BY terms.
//
// These exist on the untyped Column so any handle can order a query,
// and are distinct from the package-level Asc / Desc, which build the
// OrderingColumn that keyset pagination needs.
func (c *Column) Asc() drops.Expression  { return orderTerm(c, " ASC") }
func (c *Column) Desc() drops.Expression { return orderTerm(c, " DESC") }

func orderTerm(c *Column, dir string) drops.Expression {
	return &opExpr{parts: []string{"", dir}, operands: []drops.Expression{c}}
}

// As aliases a column in a SELECT projection.
func (c *Column) As(alias string) drops.Expression {
	return aliasExpr(c, alias)
}

// Val binds a typed value for INSERT/UPDATE.
func (c *Col[T]) Val(v T) ColumnValue { return columnValue{col: c.Column, val: v} }

// cmp renders "(<col> <op> ?)". The value is always BOUND — never
// rendered as an expression — because this is the typed column form and
// its argument is a Go value of the column's own type. The untyped
// package-level operators in op.go are the ones that take an operand,
// and they hold it.
func cmp(c *Column, op string, v any) drops.Expression {
	return &opExpr{
		parts:    []string{"(", " " + op + " ", ")"},
		operands: []drops.Expression{c, drops.Param{Value: v}},
	}
}

func nullCheck(c *Column, isNull bool) drops.Expression {
	tail := " IS NOT NULL)"
	if isNull {
		tail = " IS NULL)"
	}
	return &opExpr{parts: []string{"(", tail}, operands: []drops.Expression{c}}
}

// And / Or combine predicates.
//
// Each operand is held rather than closed over, so a conjunct that is a
// statement — And(Exists(sub), guard) — keeps its own scoping, and each
// is bracketed when leaving it bare would let it reassociate its
// neighbours: And(drops.Raw("a OR b"), guard) rendered
// "(a OR b AND guard)", which is "(a OR (b AND guard))" and leaves the
// guard binding nothing. See bracketConjunct.
//
// A single predicate is not bracketed, because there is nothing beside
// it to reassociate, and the enclosing parentheses these render anyway
// are the wrapper the caller sees. That keeps And(p) and Or(p)
// rendering exactly the bytes they always did.
func And(preds ...drops.Expression) drops.Expression { return boolChain(" AND ", preds) }
func Or(preds ...drops.Expression) drops.Expression  { return boolChain(" OR ", preds) }

func boolChain(sep string, preds []drops.Expression) drops.Expression {
	if len(preds) > 1 {
		preds = bracketConjuncts(preds)
	}
	return listOp("(", sep, ")", preds)
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

// boundExpr and withBoundExpr implement exprBound, so the statements
// inside an [UpdateBuilder.SetExpr] assignment are resolved for the
// executing ctx. The assigned value is the operand that decides what
// gets written rather than which rows do; see resolveSets.
func (v exprValue) boundExpr() drops.Expression { return v.expr }

func (v exprValue) withBoundExpr(x drops.Expression) ColumnValue {
	v.expr = x
	return v
}
