package clickhouse

import (
	"context"
	"errors"
	"fmt"

	"github.com/bernardoforcillo/drops"
)

// InsertBuilder composes an INSERT INTO …(cols) VALUES (…), (…), …
// statement. ClickHouse-optimal bulk loads use the native columnar
// protocol via clickhouse-go's Prepare/Exec loop; for that path drop
// down to the driver directly. This builder is the convenient form
// for small batches and one-off rows.
type InsertBuilder struct {
	db    *DB
	table *Table
	cols  []*Column
	rows  [][]ColumnValue

	// unscoped opts out of the tenant write axis for this statement.
	unscoped bool

	// hooked marks a builder whose rows already carry what the table's
	// InsertHooks bind, so a second render does not run them again.
	hooked bool

	// resolved marks a builder resolveCtx has already produced; see
	// resolveCtx for why resolution must happen once.
	resolved bool
}

// Row appends a single row. The first Row fixes the column list.
func (i *InsertBuilder) Row(values ...ColumnValue) *InsertBuilder {
	if i.cols == nil {
		i.cols = columnsOf(values)
	}
	i.rows = append(i.rows, values)
	return i
}

// Rows appends multiple rows in one call.
func (i *InsertBuilder) Rows(rows ...[]ColumnValue) *InsertBuilder {
	for _, r := range rows {
		i.Row(r...)
	}
	return i
}

// Columns explicitly fixes the column list (and order) before any
// Row call. Useful when the first row in your batch omits columns
// you want present in the SQL.
func (i *InsertBuilder) Columns(cols ...ColRef) *InsertBuilder {
	if i.cols != nil {
		return i
	}
	out := make([]*Column, len(cols))
	for j, c := range cols {
		out[j] = c.col()
	}
	i.cols = out
	return i
}

// Unscoped opts out of the tenant axis for this INSERT: the ctx tenant
// is neither stamped nor checked, and a ctx with no tenant is not an
// error. It is for the statement that legitimately writes rows for
// tenants other than the ctx one — a backfill, a replay — and says
// which tenant each row belongs to by binding the column itself.
//
// Everything else about the statement is unchanged, including the
// table's InsertHooks.
func (i *InsertBuilder) Unscoped() *InsertBuilder { i.unscoped = true; return i }

// columnsOf picks columns from the first row, in the table's declared
// order for determinism.
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

// alignRow aligns the row's values with the chosen column order;
// missing columns default to NULL (CH uses NULL as the "no value"
// marker — there's no DEFAULT keyword inside VALUES the way PG has).
func alignRow(cols []*Column, values []ColumnValue) []drops.Expression {
	idx := map[*Column]ColumnValue{}
	for _, v := range values {
		idx[v.column().key()] = v
	}
	out := make([]drops.Expression, len(cols))
	for j, c := range cols {
		if v, ok := idx[c.key()]; ok {
			out[j] = bindingExpr(v)
		} else {
			out[j] = drops.Raw("NULL")
		}
	}
	return out
}

func bindingExpr(v ColumnValue) drops.Expression {
	return &opExpr{parts: []string{"", ""}, operands: []drops.Expression{valueExpr{v}}}
}

// valueExpr renders a binding's value. It is an ordinary expression
// node rather than a closure so that the operand [bindingExpr] holds is
// reachable to the resolver walk — see resolveRow for what a caller can
// write into an INSERT's value position.
type valueExpr struct{ v ColumnValue }

func (e valueExpr) WriteSQL(b *drops.Builder) { e.v.writeValue(b) }

// WriteSQL renders the INSERT statement.
//
// It renders what the builder knows without a context, which on a table
// carrying a tenant axis is not the statement that would be sent — see
// [InsertBuilder.ToSQLCtx].
func (i *InsertBuilder) WriteSQL(b *drops.Builder) {
	cols, rows := i.cols, i.rows
	if !i.hooked && i.table.hasInsertHooks() {
		cols, rows = i.applyInsertHooks()
	}
	b.WriteString("INSERT INTO ")
	i.table.writeName(b)
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
		for j, v := range alignRow(cols, row) {
			if j > 0 {
				b.WriteString(", ")
			}
			b.Append(v)
		}
		b.WriteByte(')')
	}
}

// applyInsertHooks runs every InsertHook registered on the table and
// returns the (possibly extended) column list and rows.
func (i *InsertBuilder) applyInsertHooks() ([]*Column, [][]ColumnValue) {
	ctx := &InsertHookCtx{bound: make(map[*Column]bool, len(i.cols))}
	for _, c := range i.cols {
		ctx.bound[c.key()] = true
	}
	for _, h := range i.table.insertHookList() {
		h.BeforeInsert(ctx)
	}
	if len(ctx.added) == 0 {
		return i.cols, i.rows
	}
	cols := append([]*Column(nil), i.cols...)
	for _, v := range ctx.added {
		cols = append(cols, v.column())
	}
	rows := make([][]ColumnValue, len(i.rows))
	for r, row := range i.rows {
		extended := make([]ColumnValue, 0, len(row)+len(ctx.added))
		extended = append(extended, row...)
		extended = append(extended, ctx.added...)
		rows[r] = extended
	}
	return cols, rows
}

// ToSQL renders the statement.
//
// It renders what the builder knows without a context, which on a table
// that carries a tenant axis is not the statement that would be sent:
// the stamped tenant column is resolved against a ctx. Use
// [InsertBuilder.ToSQLCtx] for the statement a given ctx would send;
// that is the one to assert on in a test, and the one to log.
func (i *InsertBuilder) ToSQL() (sql string, args []any) {
	b := drops.NewBuilder(Placeholder)
	i.WriteSQL(b)
	return b.SQL()
}

// ToSQLCtx renders the complete statement for ctx.
//
// What a ctx does to an INSERT is not what it does to a SELECT, and the
// asymmetry is worth stating rather than discovering. A SELECT is
// scoped by a predicate: the tenant axis becomes "tenantId = ?" AND-ed
// into the WHERE clause, and a row belonging to somebody else is simply
// not among the rows the statement names. An INSERT has no WHERE for
// such a predicate to reach. It does not choose rows, it produces
// them — so the only two things a ctx can do here are to STAMP the
// tenant onto every row and to REFUSE when the ctx cannot say which
// tenant that is.
//
// In this dialect that is the WHOLE of the write side, because there is
// no UPDATE and no DELETE to carry a predicate: append is the shape of
// a ClickHouse workload. It is also the whole of the bulk path.
// [Entity.CreateMany] and [InsertBuilder.Rows] go through this method
// before a byte is rendered, and the native columnar protocol — which
// the package doc points very large batches at — is a driver API drops
// is not in, so a caller who takes that route is stamping their own
// rows and should be told so rather than assume otherwise.
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
//     rendered. Such a table keeps its guarantee on every read, and its
//     INSERTs stay the caller's to bind — or become drops' by naming
//     the column with ScopeWritesByTenant.
//
// A statement written inside this one is resolved as the statement it
// is: a subquery bound as a value does carry a WHERE clause and so does
// take the predicate.
func (i *InsertBuilder) ToSQLCtx(ctx context.Context) (sql string, args []any, err error) {
	r, err := i.resolveCtx(ctx)
	if err != nil {
		return "", nil, err
	}
	sql, args = r.ToSQL()
	return sql, args, nil
}

// resolveCtx returns the builder to render for one execution: a shallow
// copy carrying the hooks' bindings, the resolved subqueries and the
// stamped tenant column.
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
// InsertHookCtx takes bindings of any shape — so a hook is an operand
// position a *SelectBuilder can be handed to, and one no call site
// shows. Running them here puts what they bind through the same walk
// and the same tenant check as a value the caller bound, and stops a
// hook that binds the tenant column from being stamped over: the
// stamping sees the column as bound and verifies it instead.
//
// Then the statements hiding in the operands are resolved — every row's
// bindings. INSERT … VALUES ((SELECT id FROM accounts WHERE …)) reads
// as much as any SELECT does, and rendered through WriteSQL it read
// every tenant's accounts to compute a value stored in this tenant's
// row.
//
// Then the tenant axis, which on an INSERT is stamping and refusal
// rather than filtering — and, before any of it, the engine check that
// [ErrTenantNotInSortingKey] describes, because a stamped row is not
// enough when the engine itself will merge it into somebody else's.
func (i *InsertBuilder) resolveCtx(ctx context.Context) (*InsertBuilder, error) {
	if i.resolved {
		return i, nil
	}
	cp := *i
	// changed is the discipline the other builder keeps, and it is not
	// only about this statement: resolveStatement reports "resolved
	// differs from the original" from the pointer, so an INSERT that had
	// nothing to resolve would otherwise tell every enclosing
	// resolveExprs it had changed.
	changed := false

	cols, rows := i.cols, i.rows
	if !i.hooked && i.table.hasInsertHooks() {
		cols, rows = i.applyInsertHooks()
		cp.cols, cp.hooked, changed = cols, true, true
	}

	rowsCopied := false
	for r, row := range rows {
		resolved, err := resolveRow(ctx, row)
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

	if axis := i.writeAxis(); axis != nil {
		if err := checkTenantSortingKey(i.table, axis); err != nil {
			return nil, err
		}
		stamped, err := stampTenantColumn(ctx, axis, rows)
		if err != nil {
			return nil, err
		}
		// Stamping rebuilds every row, so an INSERT into a table with a
		// tenant axis has always changed by the time it gets here.
		rows, changed = stamped, true
		// The column list is widened whether or not stamping had to add
		// the binding, because the list is what decides which bindings
		// are rendered at all: it is derived from row zero (or fixed by
		// [InsertBuilder.Columns]), so a tenant bound on every row but
		// absent from the list would be dropped by alignRow and the
		// batch would land owned by nobody.
		if !containsColumn(cols, axis) {
			cp.cols = append(append([]*Column(nil), cols...), axis)
		}
	}

	if !changed {
		return i, nil
	}
	cp.rows = rows
	cp.resolved = true
	return &cp, nil
}

// resolveStatement implements [ctxResolvable]: it is resolveCtx behind
// the interface resolveExpr dispatches on, so an INSERT written into an
// expression is stamped with the ctx tenant like any other INSERT
// instead of writing a row that belongs to nobody.
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

func containsColumn(cols []*Column, c *Column) bool {
	for _, x := range cols {
		if x.key() == c.key() {
			return true
		}
	}
	return false
}

// ErrTenantNotInSortingKey is returned when a tenant-scoped table
// declares a merging engine whose sorting key does not begin with — or
// at least contain — the tenant column.
//
// This is where ClickHouse puts the hazard PostgreSQL puts in
// ON CONFLICT DO UPDATE, and it is worth spelling out because the shape
// is so different that a port from another dialect will not look for
// it. In PostgreSQL a cross-tenant overwrite needs a statement: an
// upsert whose conflict target is the primary key can rewrite another
// tenant's row, which is why drops gates its SET list. Here no
// statement is involved at all. A MergeTree of the Replacing,
// Collapsing, VersionedCollapsing, Summing, Aggregating or Graphite
// family folds rows that share a SORTING KEY together, in the
// background, whenever it merges parts:
//
//   - ReplacingMergeTree keeps one of them and destroys the rest, so
//     tenant A inserting a row whose sorting key happens to equal one of
//     tenant B's deletes tenant B's row, minutes later, with nothing to
//     see in any query log;
//   - Collapsing and VersionedCollapsing cancel a +1 against a -1, so
//     tenant A can annihilate tenant B's row;
//   - Summing and Aggregating do something worse than destroy it: they
//     ADD tenant A's measures into tenant B's row, and the result is a
//     number that belongs to neither.
//
// Stamping the tenant onto the row does not help, because the tenant
// column is not part of what the engine compares. Putting it in the
// sorting key does: two rows of different tenants then never share a
// key and are never folded together.
//
// So the check is on the table's declaration, and it is made at INSERT
// time rather than at declaration time because a schema is assembled in
// pieces — Engine, OrderBy and ScopeWritesByTenant are three calls in
// whatever order the file happens to make them, and a check that ran on
// the first of them would report on a table that was not finished
// being declared. Reads are unaffected and are not refused: the guard
// on a SELECT is correct whatever the engine does, and refusing reads
// would take a working query away over a hazard that only writes carry.
//
// Say Unscoped() on the INSERT to write into such a table deliberately.
var ErrTenantNotInSortingKey = errors.New("drops/clickhouse: a merging engine would fold rows of different tenants together")

// checkTenantSortingKey refuses an INSERT into a tenant-scoped table
// whose engine merges rows by sorting key and whose sorting key does
// not name the tenant column. See [ErrTenantNotInSortingKey].
//
// A table with no ORDER BY at all is not exempt, it is the worst case:
// a ReplacingMergeTree without a sorting key folds every row of every
// tenant into one. The message says so rather than reporting an empty
// list.
func checkTenantSortingKey(t *Table, axis *Column) error {
	if t == nil || axis == nil || !t.engineMergesBySortingKey() {
		return nil
	}
	for _, c := range t.orderBy {
		if c.col().key() == axis.key() {
			return nil
		}
	}
	where := "its sorting key is empty"
	if len(t.orderBy) > 0 {
		where = "its sorting key is (" + joinNames(t.OrderByColumns()) + ")"
	}
	return fmt.Errorf("%w: %q uses %s and %s, which does not include %s; add the tenant column to ORDER BY, or say Unscoped",
		ErrTenantNotInSortingKey, t.Name(), engineName(t.engine), where, tenantAxisName(axis))
}

func joinNames(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}

// stampTenantColumn returns the rows with the tenant column bound to
// the ctx tenant on every one of them.
//
// It is [Entity.stampTenant]'s rule, applied to bindings instead of to
// struct fields: no tenant on ctx is [ErrTenantMissing] and no
// statement at all, an unbound tenant column is filled in from ctx, and
// a tenant column already bound to a different tenant is
// [ErrTenantMismatch]. Either refusal aborts the whole statement before
// a single row is rendered, because a partially stamped INSERT is a
// batch nobody can reason about — and in this dialect a batch is the
// normal size of a write, so "the first eight hundred rows landed" is
// the outcome being avoided.
//
// Every row is widened, not only the first. This builder derives the
// column list from row zero, so a stamp applied to one row and not the
// rest would bind values under the wrong names.
func stampTenantColumn(ctx context.Context, axis *Column, rows [][]ColumnValue) ([][]ColumnValue, error) {
	t, ok := TenantFrom(ctx)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTenantMissing, tenantAxisName(axis))
	}
	out := make([][]ColumnValue, len(rows))
	for r, row := range rows {
		at := -1
		for j, cv := range row {
			if cv.column().key() == axis.key() {
				at = j
				break
			}
		}
		if at < 0 {
			next := make([]ColumnValue, 0, len(row)+1)
			next = append(next, row...)
			out[r] = append(next, Bind(axis, t))
			continue
		}
		bound, kind := classifyBinding(row[at])
		switch kind {
		case bindingLiteral:
			if !sameTenant(bound, t) {
				return nil, fmt.Errorf("%w: %s is bound to another tenant's value",
					ErrTenantMismatch, tenantAxisName(axis))
			}
			out[r] = row
		default:
			return nil, fmt.Errorf("%w: %s is bound to an expression drops cannot compare with the ctx tenant; bind a value, leave the column out, or say Unscoped",
				ErrTenantMismatch, tenantAxisName(axis))
		}
	}
	return out, nil
}

// The two shapes a binding can take as far as the tenant check is
// concerned.
const (
	// bindingLiteral binds exactly one argument, so what the row says
	// about the column is a Go value the check can compare.
	bindingLiteral = iota
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
// expression binding, whatever a hook binds — and a kind added later
// would silently classify as "not a literal" and start refusing valid
// INSERTs, or worse, if the switch had a permissive default, stop
// checking. What the statement will bind is what gets compared, because
// it is produced the same way the statement produces it.
func classifyBinding(v ColumnValue) (any, int) {
	b := drops.NewBuilder(Placeholder)
	v.writeValue(b)
	sql, args := b.SQL()
	if len(args) == 1 && sql == "?" {
		return args[0], bindingLiteral
	}
	return nil, bindingOpaque
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
