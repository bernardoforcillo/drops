package mysql

import (
	"context"
	"errors"
	"fmt"

	"github.com/bernardoforcillo/drops"
)

// InsertBuilder composes an INSERT statement.
type InsertBuilder struct {
	db      *DB
	table   *Table
	cols    []*Column
	rows    [][]drops.Expression
	ignore  bool
	upserts []ColumnValue

	// upsertAll records that the assignment list was derived by
	// OnDuplicateKeyUpdateAll rather than supplied by the caller. An
	// empty list means opposite things in the two cases — see the
	// methods' docs — so the renderer has to be able to tell them
	// apart.
	upsertAll bool

	// unscoped opts this INSERT out of the table's tenant axis — see
	// [InsertBuilder.Unscoped].
	unscoped bool

	// resolved marks a builder resolveCtx has already produced.
	// Resolution is not idempotent: the table still carries its tenant
	// axis afterwards, and a second pass would stamp a column the first
	// pass already appended.
	resolved bool
}

// Row appends one row of column/value bindings. The first Row fixes
// the column list; later rows are aligned to it, so a row that omits a
// column inserts DEFAULT for it.
func (i *InsertBuilder) Row(values ...ColumnValue) *InsertBuilder {
	if len(i.cols) == 0 {
		for _, v := range values {
			i.cols = append(i.cols, v.column())
		}
	}
	i.rows = append(i.rows, alignRow(i.cols, values))
	return i
}

// Rows appends several rows at once.
func (i *InsertBuilder) Rows(rows ...[]ColumnValue) *InsertBuilder {
	for _, r := range rows {
		i.Row(r...)
	}
	return i
}

// Ignore renders INSERT IGNORE, which turns a duplicate-key collision
// into a warning and a skipped row.
//
// It is blunter than it looks: IGNORE also downgrades several other
// errors — a truncated value, a bad date — from failures into silently
// adjusted data. Reach for OnDuplicateKeyUpdate when you mean "the row
// already exists".
func (i *InsertBuilder) Ignore() *InsertBuilder { i.ignore = true; return i }

// OnDuplicateKeyUpdate appends ON DUPLICATE KEY UPDATE with the given
// assignments, MySQL's upsert.
//
// Unlike PostgreSQL's ON CONFLICT (col), MySQL names no conflict
// target: the clause fires on a collision with *any* unique index on
// the table. A table with a unique email as well as a primary key will
// therefore update on either, which is usually what you want and
// occasionally a surprise.
//
// With no assignments the clause is not rendered at all and the
// statement stays a plain INSERT, so a duplicate still raises error
// 1062. This differs on purpose from the empty case of
// [InsertBuilder.OnDuplicateKeyUpdateAll], which does render a clause:
// there the list is empty because every column the insert names
// belongs to the key, so the colliding row already equals the one
// being inserted and swallowing the collision discards nothing. A list
// the caller assembled and that came out empty carries no such
// guarantee — the row may differ in exactly the columns nobody
// assigned — so drops raises rather than guessing. Spell "do nothing
// on conflict" as [InsertBuilder.Ignore], which says so.
//
// An assignment names its column on the right as well as the left —
// "age = age + ?" — and qualified, so each one is restated against the
// declared table's handle. The declared table is the only qualifier an
// INSERT can accept: the INTO clause is written by writeName and
// carries no AS, so even an insert built entirely from an alias's own
// handles has to name the table there.
func (i *InsertBuilder) OnDuplicateKeyUpdate(assignments ...ColumnValue) *InsertBuilder {
	for _, a := range assignments {
		i.upserts = append(i.upserts, rebindValue(i.table.key(), a))
	}
	return i
}

// OnDuplicateKeyUpdateAll sets every non-key column to the value the
// insert would have written, which is the "upsert this row" shorthand.
//
// On a table whose every column is part of the key there is nothing to
// assign, and the clause degrades to a self-assignment — MySQL's
// spelling of "do nothing on conflict" — rather than vanishing. That
// keeps the method's promise that the row exists afterwards and no
// error was raised, and it costs nothing: on such a table the row
// already in the way is equal, column for column, to the one being
// inserted. [InsertBuilder.OnDuplicateKeyUpdate] does not degrade this
// way, and its doc explains why the two differ.
func (i *InsertBuilder) OnDuplicateKeyUpdateAll() *InsertBuilder {
	i.upsertAll = true
	for _, c := range i.cols {
		if c.primary {
			continue
		}
		i.upserts = append(i.upserts, exprValue{col: c, expr: NewValueOf(c)})
	}
	return i
}

// NewValueOf refers to the value the INSERT would have written for a
// column, for use inside ON DUPLICATE KEY UPDATE.
//
// MySQL 8.0.20 deprecated the VALUES(col) spelling in favour of a row
// alias, but the alias form is not available on MariaDB or on older
// MySQL, and VALUES(col) still works everywhere. drops emits the
// portable one.
//
// The column is written unqualified, which is what the SET list two
// clauses earlier does and what an INSERT INTO — whose target carries
// no AS — can accept.
func NewValueOf(col ColRef) drops.Expression { return valuesRef{col: col.col()} }

// valuesRef renders VALUES(`col`).
//
// It is a named node rather than a closure for the reason [opExpr] is:
// a closure is opaque to every walk this package does. Nothing can hide
// inside this one — it holds a column and no operand — and stating that
// is cheaper than a reader having to check.
type valuesRef struct{ col *Column }

// WriteSQL implements drops.Expression.
func (v valuesRef) WriteSQL(b *drops.Builder) {
	b.WriteString("VALUES(")
	b.WriteIdent(v.col.name)
	b.WriteByte(')')
}

// alignRow orders a row's values to match cols, leaving DEFAULT where
// a column has no binding.
//
// The column list is fixed by the first Row and the bindings come from
// whichever handles the caller held, so the two sides are matched on
// Column.key rather than on the pointer: a value bound through an
// alias names the same column as the declared handle. Matching by
// pointer instead drops it, and drops it silently — the column falls
// to the DEFAULT fill, which is well-formed SQL that writes the wrong
// row.
func alignRow(cols []*Column, values []ColumnValue) []drops.Expression {
	byCol := make(map[*Column]ColumnValue, len(values))
	for _, v := range values {
		byCol[v.column().key()] = v
	}
	out := make([]drops.Expression, len(cols))
	for i, c := range cols {
		v, ok := byCol[c.key()]
		if !ok {
			out[i] = sqlDefault{}
			continue
		}
		// The binding's own expression, not a closure over its
		// writeValue: a value bound as a subquery is a statement, and a
		// statement inside a closure is one the resolver cannot reach.
		// See ColumnValue.valueExpr.
		out[i] = v.valueExpr()
	}
	return out
}

// sqlDefault renders the DEFAULT keyword, for a column a row did not
// bind. It is a named type rather than a drops.Raw so that the tenant
// stamping can recognise the gap it fills — see stampTenantColumn.
type sqlDefault struct{}

// WriteSQL implements drops.Expression.
func (sqlDefault) WriteSQL(b *drops.Builder) { b.WriteString("DEFAULT") }

// WriteSQL renders the INSERT.
func (i *InsertBuilder) WriteSQL(b *drops.Builder) {
	b.WriteString("INSERT ")
	if i.ignore {
		b.WriteString("IGNORE ")
	}
	b.WriteString("INTO ")
	i.table.writeName(b)
	b.WriteString(" (")
	for n, c := range i.cols {
		if n > 0 {
			b.WriteString(", ")
		}
		b.WriteIdent(c.name)
	}
	b.WriteString(") VALUES ")
	for n, row := range i.rows {
		if n > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('(')
		b.AppendList(", ", row)
		b.WriteByte(')')
	}
	switch {
	case len(i.upserts) > 0:
		b.WriteString(" ON DUPLICATE KEY UPDATE ")
		for n, a := range i.upserts {
			if n > 0 {
				b.WriteString(", ")
			}
			b.WriteIdent(a.column().name)
			b.WriteString(" = ")
			a.writeValue(b)
		}
	case i.upsertAll && len(i.cols) > 0:
		// Nothing to copy over — every column the insert names
		// belongs to the key. Assigning a column to itself is
		// MySQL's spelling of "do nothing on conflict"; dropping
		// the clause instead would turn the upsert back into a
		// plain INSERT that raises 1062.
		b.WriteString(" ON DUPLICATE KEY UPDATE ")
		b.WriteIdent(i.cols[0].name)
		b.WriteString(" = ")
		b.WriteIdent(i.cols[0].name)
	}
}

// ErrNoRows is returned when an INSERT is executed with no rows.
var ErrNoRows = errors.New("drops/mysql: INSERT has no rows")

// Unscoped opts out of the tenant axis for this INSERT: the ctx tenant
// is neither stamped onto the rows nor required, and a ctx carrying no
// tenant is not an error. An ON DUPLICATE KEY UPDATE branch is left
// exactly as it was written.
//
// It is the escape hatch a migration, a backfill, a seed loader or an
// admin tool needs — the statements that legitimately write rows for
// tenants other than the one on the ctx, or for no tenant at all. Say
// which tenant each row belongs to by binding the column yourself. It
// says so at the call site, where a reviewer reads it, which is the
// whole difference between this and a package-level switch nobody sees
// in review.
//
// It does not reach the statements written inside this one: a subquery
// bound as a value is a statement of its own and keeps its own
// scoping, exactly as [SelectBuilder.Unscoped] leaves a subquery's
// scoping alone. Unscoping one part of a write and no other is done by
// saying Unscoped on that part's builder.
func (i *InsertBuilder) Unscoped() *InsertBuilder { i.unscoped = true; return i }

// ToSQL renders the statement and its arguments.
//
// It renders what the builder knows without a context, which on a table
// that carries a tenant axis is not the statement that would be sent:
// the stamped tenant column, and the gate on an ON DUPLICATE KEY
// UPDATE, are resolved against a ctx. Use [InsertBuilder.ToSQLCtx] for
// the statement a given ctx would send; that is the one to assert on in
// a test, and the one to log.
func (i *InsertBuilder) ToSQL() (string, []any) { return render(i) }

// ToSQLCtx renders the complete statement for ctx.
//
// What a ctx does to an INSERT is not what it does to the statements
// that carry a WHERE clause, and the asymmetry is worth stating rather
// than discovering. A SELECT, an UPDATE and a DELETE are scoped by a
// predicate: the tenant axis becomes "`tenantId` = ?" AND-ed into the
// WHERE clause, and a row belonging to somebody else is simply not
// among the rows the statement names. An INSERT has no WHERE for such a
// predicate to reach. It does not choose rows, it produces one — so the
// only two things a ctx can do here are to STAMP the tenant onto the
// row and to REFUSE when the ctx cannot say which tenant that is.
//
// Both matter, and the second is the one that used to be missing. Until
// the raw builder took a ctx, db.Insert(t).Row(…) on a tenant-scoped
// table inserted whatever the caller bound — typically nothing at all
// for the tenant column, which lands the row on the column's DEFAULT or
// on NULL. That row belongs to no tenant: the very next SELECT, which
// does carry the predicate, cannot see it, and the INSERT was reported
// as a success.
//
// Which tables this reaches is exactly the set that named a column:
//
//   - Entity.ScopeByTenant(col) declares the axis and, through it, the
//     table's write axis. Every INSERT into that table — raw builder or
//     entity method — stamps col from ctx and refuses without one.
//   - Table.ScopeWritesByTenant(col) is the same declaration without an
//     entity, for a schema that scopes with Table.ContextFilter.
//   - A table scoped ONLY by Table.ContextFilter(TenantFilter(col)) has
//     no named column here. A ContextFilterFunc is a closure that
//     answers with a predicate; nothing can ask it which column it
//     names, and drops will not guess one from the predicate it
//     rendered. Such a table keeps its guarantee on every read and on
//     every UPDATE and DELETE, and its INSERTs stay the caller's to
//     bind — or become drops' by naming the column with
//     ScopeWritesByTenant.
//
// # ON DUPLICATE KEY UPDATE, and why the gate is wider here
//
// The upsert branch is where the refusal has teeth. A primary key is
// unique across the whole table and not per tenant, so the row an
// INSERT collides with may well belong to somebody else, and an
// unguarded assignment list rewrites that row's data — and, if the
// tenant column is among the assignments, its owner. Tenant A takes
// ownership of tenant B's row by guessing an id, silently, reported as
// one row affected.
//
// MySQL makes that broader than PostgreSQL does, in two ways that
// compound. ON DUPLICATE KEY UPDATE names no conflict target, so it
// fires on a collision with ANY unique index: a unique email is enough,
// and the id need not be guessed at all. And there is no WHERE clause
// on the branch — PostgreSQL gates its DO UPDATE with
// WHERE tenant = EXCLUDED.tenant, and MySQL has nowhere to put that
// predicate.
//
// So the gate moves inside each assignment. The assignment to the
// tenant column is dropped, which leaves the existing row's tenant
// readable on the right-hand side, and every surviving assignment
// becomes
//
//	`col` = IF(`tenantId` = VALUES(`tenantId`), <value>, `col`)
//
// — the value when the row already belongs to this tenant, and the
// column's own current value, which is to say no change at all, when it
// does not. A collision with another tenant's row updates nothing
// rather than overwriting it. The caller sees that as a shortfall in
// rows-affected, which is the most an upsert branch can report — SQL
// has no way to raise from a condition that did not match — so
// ingestion that must know whether every row landed compares the count.
//
// Dropping the tenant assignment can empty the list, on a table whose
// every non-key column is the tenant axis. An [OnDuplicateKeyUpdateAll]
// that empties this way still renders its self-assignment, which is
// MySQL's "do nothing on conflict" and rewrites nobody; a list the
// caller wrote that empties renders no clause at all, which is what an
// explicitly-empty list already meant here — the collision raises 1062
// and nothing is silently discarded.
func (i *InsertBuilder) ToSQLCtx(ctx context.Context) (sql string, args []any, err error) {
	r, err := i.resolveCtx(ctx)
	if err != nil {
		return "", nil, err
	}
	sql, args = r.ToSQL()
	return sql, args, nil
}

// resolveCtx returns the builder to render for one execution: a shallow
// copy carrying the resolved subqueries, the stamped tenant column and
// the gated upsert branch.
//
// The copy is what keeps a builder executable twice. Stamping into
// i.rows would leave the first request's tenant bound in the second
// request's statement — the one failure in this file that is silent in
// the SQL, correct-looking in a log, and wrong in the row it writes.
//
// The statements hiding in the operands are resolved first — every
// row's bindings and every upsert assignment. INSERT … VALUES ((SELECT
// `id` FROM `accounts` WHERE …)) reads as much as any SELECT does, and
// rendered through WriteSQL it read every tenant's accounts to compute
// a value stored in this tenant's row. Then the tenant axis, which on
// an INSERT is stamping and refusal rather than filtering.
func (i *InsertBuilder) resolveCtx(ctx context.Context) (*InsertBuilder, error) {
	if i.resolved {
		return i, nil
	}
	cp := *i
	// changed is the discipline the other builders keep, and it is not
	// only about this statement: resolveStatement reports "resolved
	// differs from the original" from the pointer, so an INSERT that had
	// nothing to resolve would otherwise tell every enclosing
	// resolveExprs it had changed.
	changed := false

	cols, rows := i.cols, i.rows
	rowsCopied := false
	for r, row := range rows {
		resolved, err := resolveExprs(ctx, row)
		if err != nil {
			return nil, err
		}
		if resolved == nil {
			continue
		}
		if !rowsCopied {
			rows, rowsCopied, changed = append([][]drops.Expression(nil), rows...), true, true
		}
		rows[r] = resolved
	}

	upserts, err := resolveSets(ctx, i.upserts)
	if err != nil {
		return nil, err
	}
	if upserts == nil {
		upserts = i.upserts
	} else {
		changed = true
	}

	if axis := i.writeAxis(); axis != nil {
		// Stamping rebuilds the column list and every row, so an INSERT
		// into a table with a tenant axis has always changed by the time
		// it gets here.
		cols, rows, err = stampTenantColumn(ctx, axis, cols, rows)
		if err != nil {
			return nil, err
		}
		upserts, changed = gateUpserts(axis, upserts), true
	}

	if !changed {
		return i, nil
	}
	cp.cols, cp.rows, cp.upserts = cols, rows, upserts
	cp.resolved = true
	return &cp, nil
}

// resolveStatement implements [ctxResolvable]: it is resolveCtx behind
// the interface resolveExpr dispatches on, so an INSERT written as a
// CTE body or a subquery operand is stamped with the ctx tenant like
// any other INSERT instead of writing a row that belongs to nobody.
func (i *InsertBuilder) resolveStatement(ctx context.Context) (drops.Expression, bool, error) {
	r, err := i.resolveCtx(ctx)
	if err != nil {
		return nil, false, err
	}
	return r, r != i, nil
}

// writeAxis returns the tenant column this INSERT stamps, or nil when
// the table declared none or the caller said Unscoped.
func (i *InsertBuilder) writeAxis() *Column {
	if i.unscoped {
		return nil
	}
	return i.table.tenantAxis()
}

// stampTenantColumn returns the column list and rows with the tenant
// column bound to the ctx tenant on every row.
//
// It is [Entity.stampTenant]'s rule, applied to bindings instead of to
// struct fields: no tenant on ctx is [ErrTenantMissing] and no
// statement at all, an unbound tenant column is filled in from ctx, and
// a tenant column already bound to a different tenant is
// [ErrTenantMismatch]. Either refusal aborts the whole statement before
// a single row is rendered, because a partially stamped INSERT is a
// batch nobody can reason about.
//
// "Already bound" is decided per row rather than per statement, because
// alignRow fills a column no row bound with DEFAULT: in a multi-row
// INSERT one row may name the tenant column and the next may leave it
// out, and the gap is exactly the case stamping exists for. Every row
// is widened when the column is missing entirely, not only the first —
// this builder derives its column list from row zero, so a stamp
// applied to one row and not the rest would bind values under the wrong
// names.
func stampTenantColumn(ctx context.Context, axis *Column, cols []*Column, rows [][]drops.Expression) ([]*Column, [][]drops.Expression, error) {
	t, ok := TenantFrom(ctx)
	if !ok {
		return nil, nil, fmt.Errorf("%w: %s", ErrTenantMissing, columnPath(axis))
	}
	at := -1
	for j, c := range cols {
		if c.key() == axis.key() {
			at = j
			break
		}
	}
	if at < 0 {
		out := make([][]drops.Expression, len(rows))
		for r, row := range rows {
			next := make([]drops.Expression, 0, len(row)+1)
			next = append(next, row...)
			out[r] = append(next, drops.Param{Value: t})
		}
		return append(append([]*Column(nil), cols...), axis), out, nil
	}
	out, copied := rows, false
	for r, row := range rows {
		bound, kind := classifyBinding(row[at])
		switch kind {
		case bindingDefault:
			if !copied {
				out, copied = append([][]drops.Expression(nil), rows...), true
			}
			next := append([]drops.Expression(nil), row...)
			next[at] = drops.Param{Value: t}
			out[r] = next
		case bindingLiteral:
			if !sameTenant(bound, t) {
				return nil, nil, fmt.Errorf("%w: %s is bound to another tenant's value",
					ErrTenantMismatch, columnPath(axis))
			}
		default:
			return nil, nil, fmt.Errorf("%w: %s is bound to an expression drops cannot compare with the ctx tenant; bind a value, leave the column out, or say Unscoped",
				ErrTenantMismatch, columnPath(axis))
		}
	}
	return cols, out, nil
}

// The three shapes a binding can take as far as the tenant check is
// concerned.
const (
	// bindingDefault renders the DEFAULT keyword: the row says nothing
	// about this column, which is the case stamping fills in.
	bindingDefault = iota
	// bindingLiteral binds exactly one argument, so what the row says
	// about the column is a Go value the check can compare.
	bindingLiteral
	// bindingOpaque is everything else — an expression only the server
	// can evaluate.
	bindingOpaque
)

// classifyBinding reports what a binding will write, and the value it
// binds when that is a single argument.
//
// It decides by rendering the binding rather than by type-switching
// over the shapes this package happens to build today. That is the
// difference between a check that covers the shapes somebody listed and
// one that covers the shapes: a row's cell is whatever
// ColumnValue.valueExpr answered with, and the implementations are
// several — a bound value, an expression assignment, whatever a patch
// operator builds. What the statement will bind is what gets compared,
// because it is produced the same way the statement produces it.
func classifyBinding(e drops.Expression) (any, int) {
	b := drops.NewBuilder(drops.WithDialect(Dialect))
	b.Append(e)
	sql, args := b.SQL()
	switch {
	case len(args) == 0 && sql == defaultKeyword():
		return nil, bindingDefault
	case len(args) == 1 && sql == onePlaceholder():
		return args[0], bindingLiteral
	}
	return nil, bindingOpaque
}

// defaultKeyword renders the DEFAULT token the same way a row's gap
// renders it, so the comparison cannot drift from the renderer.
func defaultKeyword() string {
	b := drops.NewBuilder(drops.WithDialect(Dialect))
	b.Append(sqlDefault{})
	sql, _ := b.SQL()
	return sql
}

// onePlaceholder renders a lone bound argument, so the comparison is
// against the placeholder syntax the builder actually emits rather than
// against a hard-coded "?".
func onePlaceholder() string {
	b := drops.NewBuilder(drops.WithDialect(Dialect))
	b.AddArg(nil)
	sql, _ := b.SQL()
	return sql
}

// gateUpserts drops the assignment to the tenant column and wraps every
// other one so that a collision with another tenant's row rewrites
// nothing. See [InsertBuilder.ToSQLCtx] for why the gate has to live
// inside the assignments in this dialect.
func gateUpserts(axis *Column, upserts []ColumnValue) []ColumnValue {
	if len(upserts) == 0 {
		return upserts
	}
	out := make([]ColumnValue, 0, len(upserts))
	for _, u := range upserts {
		if u.column().key() == axis.key() {
			continue
		}
		out = append(out, tenantGatedValue{inner: u, axis: axis})
	}
	return out
}

// tenantGatedValue is one upsert assignment with the tenant gate around
// it: IF(`tenant` = VALUES(`tenant`), <value>, `col`).
//
// The columns are written unqualified, which is what the SET list of an
// ON DUPLICATE KEY UPDATE does and what its INSERT INTO target — which
// carries no AS — can accept. Unqualified inside this clause, the
// tenant column reads the EXISTING row's value, and VALUES() reads the
// one the INSERT would have written; the tenant assignment having been
// dropped, nothing earlier in the list can have changed the former
// under it.
//
// It is a named node holding its operand rather than a closure, for the
// reason [opExpr] is: it is built after the SET list has been walked,
// and a closure here would put the walked value back out of reach of
// any later walk.
type tenantGatedValue struct {
	inner ColumnValue
	axis  *Column
}

func (v tenantGatedValue) column() *Column { return v.inner.column() }

func (v tenantGatedValue) rebind(c *Column) ColumnValue {
	v.inner = v.inner.rebind(c)
	return v
}

func (v tenantGatedValue) writeValue(b *drops.Builder) { b.Append(v.valueExpr()) }

func (v tenantGatedValue) valueExpr() drops.Expression {
	return &opExpr{
		parts: []string{"IF(", " = ", ", ", ", ", ")"},
		operands: []drops.Expression{
			bareCol{v.axis}, valuesRef{col: v.axis},
			v.inner.valueExpr(), bareCol{v.inner.column()},
		},
	}
}

// bareCol renders a column name with no relation qualifier, for the
// clauses that take one and only one: an ON DUPLICATE KEY UPDATE
// assignment, whose target relation is named once by the INSERT INTO
// and can carry no alias.
type bareCol struct{ col *Column }

// WriteSQL implements drops.Expression.
func (c bareCol) WriteSQL(b *drops.Builder) { b.WriteIdent(c.col.name) }

// Exec runs the INSERT.
func (i *InsertBuilder) Exec(ctx context.Context) (drops.Result, error) {
	if len(i.rows) == 0 {
		return nil, ErrNoRows
	}
	sql, args, err := i.ToSQLCtx(ctx)
	if err != nil {
		return nil, err
	}
	return i.db.Exec(ctx, sql, args...)
}
