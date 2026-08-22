package pg

import (
	"context"
	"fmt"

	"github.com/bernardoforcillo/drops"
)

// InsertBuilder composes an INSERT statement.
//
// Rows are supplied via Row (one row at a time) or Rows (a batch). The
// column order across rows is fixed by the first Row call — subsequent
// rows must target the same set of columns; columns not bound on a row
// receive DEFAULT.
type InsertBuilder struct {
	db        *DB
	table     *Table
	cols      []*Column
	rows      [][]ColumnValue
	returning []drops.Expression
	conflict  *conflictClause
	unscoped  bool

	// hooked records that the table's InsertHooks have already been
	// applied to cols/rows — which they are by resolveCtx, so that what
	// a hook binds is walked for subqueries and checked against the
	// tenant axis like any other value. WriteSQL runs them itself only
	// when it is reached without a ctx.
	hooked bool

	// resolved marks a builder resolveCtx has already produced, and is
	// the same discipline [SelectBuilder.resolved] states: resolution is
	// not idempotent — the target table still has its context filters
	// afterwards, so a second pass appends the tenant predicate a second
	// time and binds its value twice. Nothing fails; the rows come back
	// right and only an argument limit or a query log shows it. A
	// resolved statement became reachable a second time the moment
	// resolveExpr learned to walk into the write builders, since a
	// resolved body is what a resolved CTE or operand holds.
	resolved bool
}

type conflictClause struct {
	target  []*Column
	doNoth  bool
	updates []ColumnValue
	where   []drops.Expression
}

// Row appends a single row. The first Row determines the column list.
func (i *InsertBuilder) Row(values ...ColumnValue) *InsertBuilder {
	if i.cols == nil {
		i.cols = columnsOf(values)
	}
	i.rows = append(i.rows, alignRow(i.cols, values))
	return i
}

// Rows appends multiple rows in a single call. Equivalent to calling Row
// once per slice element.
func (i *InsertBuilder) Rows(rows ...[]ColumnValue) *InsertBuilder {
	for _, r := range rows {
		i.Row(r...)
	}
	return i
}

// columnsOf returns the columns referenced by values in declaration
// order. The order matches the table's Columns() listing so SQL is
// deterministic.
//
// The set is keyed by Column.key rather than by the handle, so a value
// bound through a table alias still matches the column as the table
// declares it.
func columnsOf(values []ColumnValue) []*Column {
	if len(values) == 0 {
		return nil
	}
	tbl := values[0].column().table
	seen := map[*Column]bool{}
	for _, v := range values {
		seen[v.column().key()] = true
	}
	out := make([]*Column, 0, len(values))
	if tbl != nil {
		for _, c := range tbl.Columns() {
			if seen[c.key()] {
				out = append(out, c)
				delete(seen, c.key())
			}
		}
	}
	for _, v := range values {
		c := v.column()
		if seen[c.key()] {
			out = append(out, c)
			delete(seen, c.key())
		}
	}
	return out
}

// alignRow returns the row's bindings aligned with cols. Missing
// columns are filled with a DEFAULT binding.
//
// The index is keyed by Column.key: a row bound through an alias while
// the column list was fixed by a row bound through the base handles
// would otherwise match nothing and render as a row of DEFAULTs — a
// well-formed INSERT of the wrong data, and on a nullable column a
// silent one.
//
// The gaps are filled with a binding rather than with a bare expression
// so that a row stays a list of [ColumnValue] all the way to the
// renderer. That is what lets resolveCtx walk a row for the statements
// hiding in it and stamp the tenant column by index — an aligned row
// whose entries were opaque writer closures could do neither.
func alignRow(cols []*Column, values []ColumnValue) []ColumnValue {
	idx := make(map[*Column]ColumnValue, len(values))
	for _, v := range values {
		idx[v.column().key()] = v
	}
	out := make([]ColumnValue, len(cols))
	for j, c := range cols {
		if v, ok := idx[c.key()]; ok {
			out[j] = v
		} else {
			out[j] = defaultBinding(c)
		}
	}
	return out
}

// defaultBinding binds the SQL DEFAULT keyword to c.
func defaultBinding(c *Column) ColumnValue {
	return &exprBinding{col: c, expr: sqlDefault{}}
}

// bindingExpr adapts a ColumnValue into an expression that writes the
// bound value.
//
// It is a named node rather than a closure because a closure is where
// statements go to hide: a hook that binds Col.Expr(db.Select()...) is
// an operand position a *SelectBuilder can be handed to, and wrapped in
// a drops.ExprFunc it would render through WriteSQL — with no ctx, and
// so with none of the target table's context filters. valueExpr keeps
// the binding reachable, so the same walk that resolves a subquery in a
// WHERE clause resolves one a hook assigned.
func bindingExpr(v ColumnValue) drops.Expression { return valueExpr{v: v} }

// valueExpr is the expression form of a ColumnValue.
type valueExpr struct{ v ColumnValue }

// WriteSQL implements drops.Expression.
func (e valueExpr) WriteSQL(b *drops.Builder) { e.v.writeValue(b) }

// resolveSubqueries implements subqueryResolver by delegating to the
// same walk an UPDATE's SET list goes through — there is one rule for
// what a binding may hide, and it lives in resolveSets.
func (e valueExpr) resolveSubqueries(ctx context.Context) (drops.Expression, bool, error) {
	out, err := resolveSets(ctx, []ColumnValue{e.v})
	if err != nil {
		return nil, false, err
	}
	if out == nil {
		return e, false, nil
	}
	return valueExpr{v: out[0]}, true, nil
}

// Returning sets a RETURNING clause.
func (i *InsertBuilder) Returning(cols ...drops.Expression) *InsertBuilder {
	i.returning = append(i.returning, cols...)
	return i
}

// Unscoped opts out of the tenant axis for this INSERT: the ctx tenant
// is neither stamped onto the rows nor required, and a ctx carrying no
// tenant is not an error. An ON CONFLICT DO UPDATE branch is left
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
// bound as a value, or written in a RETURNING term, is a statement of
// its own and keeps its own scoping, exactly as
// [SelectBuilder.Unscoped] leaves a subquery's scoping alone.
// Unscoping one part of a write and no other is done by saying
// Unscoped on that part's builder.
//
// Everything else about the statement is unchanged, including the
// table's InsertHooks.
func (i *InsertBuilder) Unscoped() *InsertBuilder {
	i.unscoped = true
	return i
}

// OnConflictDoNothing adds ON CONFLICT [(target...)] DO NOTHING.
func (i *InsertBuilder) OnConflictDoNothing(target ...ColRef) *InsertBuilder {
	i.conflict = &conflictClause{target: refColumns(target), doNoth: true}
	return i
}

// OnConflictUpdate begins an ON CONFLICT (target...) DO UPDATE SET ...
// clause. Pair with Set / Where to populate the update.
//
// On a table that declared a tenant axis the rendered branch is not
// quite the one written here: the assignment to the tenant column is
// dropped and the update is gated on the tenant matching EXCLUDED's.
// See resolveCtx for why an ungated one is a cross-tenant write.
func (i *InsertBuilder) OnConflictUpdate(target ...ColRef) *ConflictUpdate {
	i.conflict = &conflictClause{target: refColumns(target)}
	return &ConflictUpdate{ins: i, c: i.conflict}
}

func refColumns(refs []ColRef) []*Column {
	out := make([]*Column, len(refs))
	for i, r := range refs {
		out[i] = r.col()
	}
	return out
}

// ConflictUpdate is the configuration handle returned by OnConflictUpdate.
type ConflictUpdate struct {
	ins *InsertBuilder
	c   *conflictClause
}

// Set adds an assignment to the conflict update.
func (cu *ConflictUpdate) Set(values ...ColumnValue) *ConflictUpdate {
	cu.c.updates = append(cu.c.updates, values...)
	return cu
}

// Where adds predicates that gate the conflict update.
func (cu *ConflictUpdate) Where(preds ...drops.Expression) *ConflictUpdate {
	cu.c.where = append(cu.c.where, preds...)
	return cu
}

// Done returns the InsertBuilder for further chaining (e.g. Returning).
func (cu *ConflictUpdate) Done() *InsertBuilder { return cu.ins }

// Excluded refers to a column in the EXCLUDED pseudo-table inside an
// ON CONFLICT update — i.e. the value that would have been inserted.
// Accepts either *Column or *Col[T] via the ColRef interface.
func Excluded(c ColRef) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("EXCLUDED.")
		b.WriteIdent(c.col().Name())
	})
}

// WriteSQL renders the INSERT statement.
func (i *InsertBuilder) WriteSQL(b *drops.Builder) {
	cols, rows := i.cols, i.rows
	if !i.hooked && i.table.hasInsertHooks() {
		cols, rows = i.applyInsertHooks()
	}
	b.WriteString("INSERT INTO ")
	i.table.writeFrom(b)
	b.WriteString(" (")
	for j, c := range cols {
		if j > 0 {
			b.WriteString(", ")
		}
		b.WriteIdent(c.Name())
	}
	b.WriteString(") VALUES ")
	for r, row := range rows {
		if r > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('(')
		for j, v := range row {
			if j > 0 {
				b.WriteString(", ")
			}
			v.writeValue(b)
		}
		b.WriteByte(')')
	}
	if i.conflict != nil {
		writeConflict(b, i.conflict)
	}
	if len(i.returning) > 0 {
		b.WriteString(" RETURNING ")
		b.AppendList(", ", i.returning)
	}
}

// applyInsertHooks runs every InsertHook registered on the table and
// returns the (possibly extended) column list and rows. Hook-supplied
// columns are appended after the user-bound ones and apply to every
// row uniformly.
func (i *InsertBuilder) applyInsertHooks() ([]*Column, [][]ColumnValue) {
	ctx := &InsertHookCtx{bound: make(map[string]bool, len(i.cols))}
	for _, c := range i.cols {
		ctx.bound[boundKey(c)] = true
	}
	for _, h := range i.table.insertHookList() {
		h.BeforeInsert(ctx)
	}
	if len(ctx.addCols) == 0 {
		return i.cols, i.rows
	}
	cols := append([]*Column(nil), i.cols...)
	cols = append(cols, ctx.addCols...)
	add := make([]ColumnValue, len(ctx.addCols))
	for j, c := range ctx.addCols {
		add[j] = &exprBinding{col: c, expr: ctx.addExprs[j]}
	}
	rows := make([][]ColumnValue, len(i.rows))
	for r, row := range i.rows {
		extended := make([]ColumnValue, 0, len(row)+len(add))
		extended = append(extended, row...)
		extended = append(extended, add...)
		rows[r] = extended
	}
	return cols, rows
}

func writeConflict(b *drops.Builder, c *conflictClause) {
	b.WriteString(" ON CONFLICT")
	if len(c.target) > 0 {
		b.WriteString(" (")
		for j, col := range c.target {
			if j > 0 {
				b.WriteString(", ")
			}
			b.WriteIdent(col.Name())
		}
		b.WriteByte(')')
	}
	if c.doNoth {
		b.WriteString(" DO NOTHING")
		return
	}
	b.WriteString(" DO UPDATE SET ")
	for j, u := range c.updates {
		if j > 0 {
			b.WriteString(", ")
		}
		b.WriteIdent(u.column().Name())
		b.WriteString(" = ")
		u.writeValue(b)
	}
	if len(c.where) > 0 {
		b.WriteString(" WHERE ")
		writeAnd(b, c.where)
	}
}

// ToSQL renders the statement.
//
// It renders what the builder knows without a context, which on a table
// that carries a tenant axis is not the statement that would be sent:
// the stamped tenant column, and the gate on an ON CONFLICT DO UPDATE
// branch, are both resolved against a ctx. Use [InsertBuilder.ToSQLCtx]
// for the statement a given ctx would send; that is the one to assert
// on in a test, and the one to log.
func (i *InsertBuilder) ToSQL() (sql string, args []any) {
	b := drops.NewBuilder()
	i.WriteSQL(b)
	return b.SQL()
}

// ToSQLCtx renders the complete statement for ctx.
//
// What a ctx does to an INSERT is not what it does to the statements
// that carry a WHERE clause, and the asymmetry is worth stating rather
// than discovering. A SELECT, an UPDATE and a DELETE are scoped by a
// predicate: the tenant axis becomes "tenantId = $n" AND-ed into the
// WHERE clause, and a row belonging to somebody else is simply not
// among the rows the statement names. An INSERT has no WHERE for such a
// predicate to reach. It does not choose rows, it produces one — so the
// only two things a ctx can do here are to STAMP the tenant onto the
// row and to REFUSE when the ctx cannot say which tenant that is.
//
// Both matter, and the second is the one that used to be missing. Until
// the raw builder took a ctx, db.Insert(t).Row(...) on a tenant-scoped
// table inserted whatever the caller bound — typically nothing at all
// for the tenant column, which lands the row on the column's DEFAULT or
// on NULL. That row belongs to no tenant: the very next SELECT, which
// does carry the predicate, cannot see it, and the INSERT was reported
// as a success. Entity.Create and Entity.CreateMany stamped; the
// builder the readme also exposes did not, and the two spellings of
// "write a row" disagreed about a promise the package makes for the
// table.
//
// Which tables this reaches is exactly the set that named a column:
//
//   - Entity.ScopeByTenant(col) declares the axis and, through it,
//     the table's write axis. Every INSERT into that table — raw
//     builder or entity method — stamps col from ctx and refuses
//     without one.
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
// A statement written inside this one is resolved as the statement it
// is, wherever it is written: a subquery bound as a value, one in a
// RETURNING term, one in the SET list or WHERE of the conflict branch.
// Those do carry a WHERE clause and so do take the predicate.
func (i *InsertBuilder) ToSQLCtx(ctx context.Context) (sql string, args []any, err error) {
	r, err := i.resolveCtx(ctx)
	if err != nil {
		return "", nil, err
	}
	sql, args = r.ToSQL()
	return sql, args, nil
}

// resolveCtx returns the builder to render for one execution: a shallow
// copy carrying the hooks' bindings, the resolved subqueries, the
// stamped tenant column and the scoped conflict branch.
//
// The copy is what keeps a builder executable twice. Stamping into
// i.rows would leave the first request's tenant bound in the second
// request's statement — the one failure in this file that is silent in
// the SQL, correct-looking in a log, and wrong in the row it writes.
//
// Three things happen here, in an order that is itself load-bearing.
//
// The InsertHooks run first, rather than at render time. A hook is
// registered on the table and reaches every INSERT into it, and
// InsertHookCtx.SetExpr takes any expression — so a hook is an operand
// position a *SelectBuilder can be handed to, and one no call site
// shows. Running them here puts what they bind through the same walk
// and the same tenant check as a value the caller bound, and stops a
// hook that binds the tenant column from being stamped over: the
// stamping sees the column as bound and verifies it instead.
//
// Then the statements hiding in the operands are resolved — every row's
// bindings, the conflict SET list, the conflict WHERE, the RETURNING
// terms. INSERT ... VALUES ((SELECT id FROM accounts WHERE ...)) reads
// as much as any SELECT does, and rendered through WriteSQL it read
// every tenant's accounts to compute a value stored in this tenant's
// row.
//
// Then the tenant axis, which is stamping and refusal rather than
// filtering — see [InsertBuilder.ToSQLCtx] for why an INSERT is the odd
// statement out.
//
// The conflict branch is where the refusal has teeth, and the reasoning
// is the one Entity.UpsertMany spelled out: a primary key is unique
// across the whole table and not per tenant, so the row an INSERT
// collides with may well belong to somebody else. A DO UPDATE that sets
// every column from EXCLUDED then rewrites that row's data *and* its
// tenant column, which is to say tenant A takes ownership of tenant B's
// row by guessing an id — silently, reported as one row affected. So
// the assignment to the tenant column is dropped, and the update is
// gated on WHERE tenant = EXCLUDED.tenant: a collision with another
// tenant's row updates nothing at all rather than overwriting it. The
// caller sees that as a shortfall in rows-affected, which is the most
// an ON CONFLICT branch can report — SQL has no way to raise from a
// WHERE that did not match — so ingestion that must know whether every
// row landed compares the count.
//
// Dropping the tenant assignment can empty the SET list, on a table
// whose every non-key column is the tenant axis. "DO UPDATE SET" with
// no assignments is a syntax error, so the branch becomes DO NOTHING,
// which is what an update with nothing left to write means anyway.
func (i *InsertBuilder) resolveCtx(ctx context.Context) (*InsertBuilder, error) {
	if i.resolved {
		return i, nil
	}
	cp := *i
	// changed is the discipline the other three builders keep, and it
	// is not only about this statement: resolveStatement reports
	// "resolved differs from the original" from the pointer, so an
	// INSERT that had nothing to resolve told every enclosing
	// resolveExprs it had changed, and each of them copied a slice to
	// hold a value identical to the one it already had.
	changed := false

	cols, rows := i.cols, i.rows
	if i.table.hasInsertHooks() {
		cols, rows = i.applyInsertHooks()
		cp.hooked, changed = true, true
	}

	rowsCopied := false
	for r, row := range rows {
		resolved, err := resolveSets(ctx, row)
		if err != nil {
			return nil, err
		}
		if resolved == nil {
			continue
		}
		if !rowsCopied {
			rows, rowsCopied, changed = append([][]ColumnValue(nil), rows...), true, true
		}
		rows[r] = resolved
	}

	conflict := i.conflict
	if conflict != nil {
		updates, err := resolveSets(ctx, conflict.updates)
		if err != nil {
			return nil, err
		}
		wheres, err := resolveExprs(ctx, conflict.where)
		if err != nil {
			return nil, err
		}
		if updates != nil || wheres != nil {
			c := *conflict
			if updates != nil {
				c.updates = updates
			}
			if wheres != nil {
				c.where = wheres
			}
			conflict, changed = &c, true
		}
	}

	returning, err := resolveExprs(ctx, i.returning)
	if err != nil {
		return nil, err
	}
	if returning != nil {
		cp.returning, changed = returning, true
	}

	if axis := i.writeAxis(); axis != nil {
		// Stamping rebuilds the column list and every row, so an
		// INSERT into a table with a tenant axis has always changed by
		// the time it gets here.
		cols, rows, err = stampTenantColumn(ctx, axis, cols, rows)
		if err != nil {
			return nil, err
		}
		conflict, changed = scopeConflict(axis, conflict), true
	}

	if !changed {
		return i, nil
	}
	cp.cols, cp.rows, cp.conflict = cols, rows, conflict
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
// out, and the gap is exactly the case stamping exists for.
//
// The column list is searched with [namesAxis] and EVERY match is
// checked, rather than the first one found by [Column.key]. Both halves
// close the same hole. A handle for the same-named column of another
// table object is not key-equal to the axis, so the column list was
// read as not naming the tenant at all and the ctx stamp was appended
// beside the caller's binding — INSERT INTO "zzi" ("title", "tenantId",
// "tenantId") VALUES ($1, $2, $3) with args {"x", "evil", "acme"}.
// PostgreSQL rejects that (42701, column specified more than once) but
// SQLite accepts it and keeps the first occurrence, so the same shape
// wrote a row under "evil" while the ctx said "acme". Matching by
// rendered name finds the caller's binding in place, so nothing is
// appended and the value is compared with the ctx tenant like any
// other; scanning every match means a column bound twice cannot smuggle
// the second binding past a check that stopped at the first.
func stampTenantColumn(ctx context.Context, axis *Column, cols []*Column, rows [][]ColumnValue) ([]*Column, [][]ColumnValue, error) {
	t, ok := TenantFrom(ctx)
	if !ok {
		return nil, nil, fmt.Errorf("%w: %s", ErrTenantMissing, columnPath(axis))
	}
	var at []int
	for j, c := range cols {
		if namesAxis(c, axis) {
			at = append(at, j)
		}
	}
	if len(at) == 0 {
		out := make([][]ColumnValue, len(rows))
		for r, row := range rows {
			next := make([]ColumnValue, 0, len(row)+1)
			next = append(next, row...)
			out[r] = append(next, insertBinding(axis, t))
		}
		return append(append([]*Column(nil), cols...), axis), out, nil
	}
	out, copied := rows, false
	for r, row := range rows {
		next, rowCopied := row, false
		for _, j := range at {
			bound, kind := classifyBinding(row[j])
			switch kind {
			case bindingDefault:
				if !copied {
					out, copied = append([][]ColumnValue(nil), rows...), true
				}
				if !rowCopied {
					next, rowCopied = append([]ColumnValue(nil), row...), true
				}
				next[j] = insertBinding(axis, t)
			case bindingLiteral:
				if !sameTenant(bound, t) {
					return nil, nil, fmt.Errorf("%w: %s is bound to another tenant's value", ErrTenantMismatch, columnPath(axis))
				}
			default:
				return nil, nil, fmt.Errorf("%w: %s is bound to an expression drops cannot compare with the ctx tenant; bind a value, leave the column out, or say Unscoped", ErrTenantMismatch, columnPath(axis))
			}
		}
		if rowCopied {
			out[r] = next
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
// over the binding kinds this package happens to have today. That is
// the difference between a check that covers the shapes somebody listed
// and one that covers the shapes: [ColumnValue] is closed to this
// package, but the implementations are several — a typed value, an
// expression wrapping a drops.Param, a PII-wrapped param, whatever a
// hook or a patch operator binds — and a kind added later would
// silently classify as "not a literal" and start refusing valid
// INSERTs, or worse, if the switch had a permissive default, stop
// checking. What the statement will bind is what gets compared, because
// it is produced the same way the statement produces it.
func classifyBinding(v ColumnValue) (any, int) {
	b := drops.NewBuilder()
	v.writeValue(b)
	sql, args := b.SQL()
	switch {
	case len(args) == 0 && sql == defaultKeyword():
		return nil, bindingDefault
	case len(args) == 1 && sql == onePlaceholder():
		return unwrapPIIArg(args[0]), bindingLiteral
	}
	return nil, bindingOpaque
}

// defaultKeyword renders the DEFAULT token the same way a row's gap
// renders it, so the comparison cannot drift from the renderer.
func defaultKeyword() string {
	b := drops.NewBuilder()
	sqlDefault{}.WriteSQL(b)
	sql, _ := b.SQL()
	return sql
}

// onePlaceholder renders a lone bound argument, so the comparison is
// against the placeholder syntax the builder actually emits rather than
// against a hard-coded "$1".
func onePlaceholder() string {
	b := drops.NewBuilder()
	b.AddArg(nil)
	sql, _ := b.SQL()
	return sql
}

// unwrapPIIArg returns the value behind the redaction marker a
// PII-flagged column's binding wraps its argument in. The marker is a
// logging concern; the tenant check compares what the driver will
// receive.
func unwrapPIIArg(v any) any {
	if p, ok := v.(piiArg); ok {
		return p.Value
	}
	return v
}

// scopeConflict returns the conflict branch an INSERT into a
// tenant-scoped table may run: without the assignment that would hand
// the row to another tenant, and gated on the collided row belonging to
// this one. See [InsertBuilder.resolveCtx] for the whole reasoning.
//
// Which assignment that is, is decided by [namesAxis] and not by
// [Column.key], for the reason stampTenantColumn is: the SET list of a
// DO UPDATE writes the bare column name, so an assignment built from
// another table's handle for a column of the same name rendered
// SET "tenantId" = $1 under a gate that had just confirmed the
// collided row was this tenant's — the gate passing is what made the
// transfer land.
func scopeConflict(axis *Column, c *conflictClause) *conflictClause {
	if c == nil || c.doNoth {
		return c
	}
	cp := *c
	cp.updates = make([]ColumnValue, 0, len(c.updates))
	for _, u := range c.updates {
		if namesAxis(u.column(), axis) {
			continue
		}
		cp.updates = append(cp.updates, u)
	}
	if len(cp.updates) == 0 {
		// Nothing a conflict may legally rewrite is left. DO NOTHING
		// says that in SQL; "DO UPDATE SET" with an empty list is a
		// syntax error the server would report instead.
		cp.updates, cp.where, cp.doNoth = nil, nil, true
		return &cp
	}
	cp.where = append(append([]drops.Expression(nil), c.where...), excludedGate{col: axis})
	return &cp
}

// excludedGate renders "<col> = EXCLUDED.<col>" — the predicate that
// makes a conflict update apply only to a row this tenant already owns.
//
// The column is written unqualified, unlike the one [Eq] would write,
// and that is not cosmetic. Inside ON CONFLICT DO UPDATE an unqualified
// name is the target row's column, which is the same spelling the SET
// list uses two clauses earlier; a qualified "gadgets"."tenantId" names
// a relation that INSERT INTO "gadgets" AS "g" does not have, and
// PostgreSQL answers 42P01. Writing the gate the way the SET list is
// written keeps a scoped table insertable under an alias.
//
// It is a named node rather than a closure for the reason valueExpr is:
// a closure is opaque to every walk this package does. Nothing can hide
// inside this one — it holds a column and no operand — and stating that
// is cheaper than a reader having to check.
type excludedGate struct{ col *Column }

// WriteSQL implements drops.Expression.
func (g excludedGate) WriteSQL(b *drops.Builder) {
	b.WriteIdent(g.col.Name())
	b.WriteString(" = EXCLUDED.")
	b.WriteIdent(g.col.Name())
}

// Exec runs the INSERT.
func (i *InsertBuilder) Exec(ctx context.Context) (drops.Result, error) {
	if len(i.rows) == 0 {
		return nil, ErrNoRowsToInsert
	}
	sql, args, err := i.ToSQLCtx(ctx)
	if err != nil {
		return nil, err
	}
	return i.db.Exec(ctx, sql, args...)
}

// All executes the INSERT and scans the RETURNING rows into dest.
func (i *InsertBuilder) All(ctx context.Context, dest any) error {
	if len(i.returning) == 0 {
		return ErrReturningRequired
	}
	sql, args, err := i.ToSQLCtx(ctx)
	if err != nil {
		return err
	}
	rows, err := i.db.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	return scanAll(rows, dest)
}

// One executes the INSERT and scans the first RETURNING row into dest.
func (i *InsertBuilder) One(ctx context.Context, dest any) error {
	if len(i.returning) == 0 {
		return ErrReturningRequired
	}
	sql, args, err := i.ToSQLCtx(ctx)
	if err != nil {
		return err
	}
	rows, err := i.db.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	return scanOne(rows, dest)
}
