package sqlite

import (
	"context"
	"errors"
	"fmt"

	"github.com/bernardoforcillo/drops"
)

// InsertBuilder builds an INSERT statement. Create one via DB.Insert.
type InsertBuilder struct {
	db        *DB
	table     *Table
	rows      [][]ColumnValue
	returning []ColRef
	orIgnore  bool
	orReplace bool
	unscoped  bool

	// hooked records that the table's InsertHooks have already been
	// applied to rows — which they are by resolveCtx, so that what a
	// hook binds is walked for subqueries and checked against the tenant
	// axis like any other value. WriteSQL runs them itself only when it
	// is reached without a ctx.
	hooked bool

	// resolved marks a builder resolveCtx has already produced; see
	// [UpdateBuilder.resolved].
	resolved bool
}

// Values appends one row of column bindings. Every row must bind the
// same set of columns, in the same order.
func (i *InsertBuilder) Values(vals ...ColumnValue) *InsertBuilder {
	i.rows = append(i.rows, vals)
	return i
}

// OrIgnore emits INSERT OR IGNORE (SQLite's conflict-skip form).
func (i *InsertBuilder) OrIgnore() *InsertBuilder { i.orIgnore = true; return i }

// OrReplace emits INSERT OR REPLACE (upsert-by-replace).
//
// On a table that declares a tenant write axis this is refused with
// [ErrReplaceScoped] unless the statement says [InsertBuilder.Unscoped]
// — see there for why, and for what to write instead.
func (i *InsertBuilder) OrReplace() *InsertBuilder { i.orReplace = true; return i }

// Unscoped opts out of the tenant axis for this INSERT: the ctx tenant
// is neither stamped onto the rows nor required, and OR REPLACE is
// permitted. Use it for a migration or an import that writes rows for
// tenants other than the ctx one — and say which tenant each row
// belongs to by binding the column yourself.
//
// It does not reach into a statement written inside this one: a
// subquery bound as a value is a statement of its own and keeps its own
// scoping.
func (i *InsertBuilder) Unscoped() *InsertBuilder { i.unscoped = true; return i }

// Returning adds a RETURNING clause.
//
// SQLite has had RETURNING since 3.35 (2021), and this package has
// depended on it since Entity.Create started reading a server-assigned
// key back in the same statement — [drops.Dialect.SupportsReturning]
// reports true for this dialect. It is not a clause the scoping has to
// reason about on an INSERT: what a RETURNING term names is the row
// this statement just wrote, which the stamping has already decided the
// owner of.
func (i *InsertBuilder) Returning(cols ...ColRef) *InsertBuilder {
	i.returning = append(i.returning, cols...)
	return i
}

// ErrReplaceScoped is returned when an INSERT OR REPLACE targets a
// table that declares a tenant write axis.
//
// OR REPLACE is not an update. When the row being inserted collides on
// a PRIMARY KEY or UNIQUE constraint, SQLite DELETES the existing rows
// and inserts the new one — and a primary key is unique across the
// whole table, not per tenant, so the row it collides with may well
// belong to somebody else. Tenant A guessing an id therefore destroys
// tenant B's row and takes ownership of the key, silently, reported as
// one row affected. The delete also fires that row's ON DELETE
// triggers and cascades, so the damage is not confined to the one
// table.
//
// PostgreSQL's dialect answers the equivalent shape — ON CONFLICT DO
// UPDATE — by keeping the statement and gating the branch: it drops the
// assignment to the tenant column and adds
// WHERE tenant = EXCLUDED.tenant, so a collision with another tenant's
// row updates nothing. That answer does not port. OR REPLACE has no
// SET list to prune and no WHERE clause to gate — the conflict
// resolution is a keyword, and everything about which row it destroys
// is decided by the constraint rather than by anything the statement
// says. So the only honest options are to refuse or to permit a
// cross-tenant delete, and this package refuses.
//
// Three ways forward, in the order they are usually right:
//
//   - Use [InsertBuilder.OrIgnore], which skips the colliding row
//     instead of destroying it, and read rows-affected to learn that it
//     did.
//   - Write the update as an UPDATE, which carries the tenant predicate
//     and so touches only rows this tenant owns.
//   - Say [InsertBuilder.Unscoped] when replacing across tenants is
//     genuinely what is meant — a restore, an import — where a reviewer
//     can see it.
var ErrReplaceScoped = errors.New("drops/sqlite: INSERT OR REPLACE would delete a colliding row of any tenant")

// ErrNoRowsToInsert lives in errors.go; Exec returns it for a builder
// with no rows.

// WriteSQL implements drops.Expression.
func (i *InsertBuilder) WriteSQL(b *drops.Builder) {
	b.WriteString("INSERT ")
	switch {
	case i.orIgnore:
		b.WriteString("OR IGNORE ")
	case i.orReplace:
		b.WriteString("OR REPLACE ")
	}
	b.WriteString("INTO ")
	i.table.writeName(b)
	rows := i.rows
	if !i.hooked && i.table.hasInsertHooks() {
		rows = i.applyInsertHooks()
	}
	if len(rows) == 0 {
		// Degenerate: nothing to insert. DEFAULT VALUES keeps it valid.
		b.WriteString(" DEFAULT VALUES")
		return
	}
	cols := rows[0]
	b.WriteString(" (")
	for j, cv := range cols {
		if j > 0 {
			b.WriteString(", ")
		}
		b.WriteIdent(cv.column().Name())
	}
	b.WriteString(") VALUES ")
	for r, row := range rows {
		if r > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('(')
		for j, cv := range row {
			if j > 0 {
				b.WriteString(", ")
			}
			cv.writeValue(b)
		}
		b.WriteByte(')')
	}
	if len(i.returning) > 0 {
		b.WriteString(" RETURNING ")
		for j, c := range i.returning {
			if j > 0 {
				b.WriteString(", ")
			}
			b.WriteIdent(c.col().Name())
		}
	}
}

// applyInsertHooks runs every InsertHook on the table and returns the
// rows with hook-supplied bindings appended to each (uniformly, so the
// column list derived from the first row stays aligned).
func (i *InsertBuilder) applyInsertHooks() [][]ColumnValue {
	if len(i.rows) == 0 {
		return i.rows
	}
	hctx := &InsertHookCtx{bound: make(map[*Column]bool, len(i.rows[0]))}
	for _, cv := range i.rows[0] {
		hctx.bound[cv.column()] = true
	}
	for _, h := range i.table.insertHookList() {
		h.BeforeInsert(hctx)
	}
	if len(hctx.adds) == 0 {
		return i.rows
	}
	out := make([][]ColumnValue, len(i.rows))
	for r, row := range i.rows {
		nr := make([]ColumnValue, 0, len(row)+len(hctx.adds))
		nr = append(nr, row...)
		nr = append(nr, hctx.adds...)
		out[r] = nr
	}
	return out
}

// ToSQL renders the statement with SQLite placeholders.
//
// It renders what the builder knows without a context, which on a table
// that carries a tenant axis is not the statement that would be sent:
// the stamped tenant column is resolved against a ctx. Use
// [InsertBuilder.ToSQLCtx] for the statement a given ctx would send;
// that is the one to assert on in a test, and the one to log.
func (i *InsertBuilder) ToSQL() (sql string, args []any) { return ToSQL(i) }

// ToSQLCtx renders the complete statement for ctx.
//
// What a ctx does to an INSERT is not what it does to the statements
// that carry a WHERE clause, and the asymmetry is worth stating rather
// than discovering. A SELECT, an UPDATE and a DELETE are scoped by a
// predicate: the tenant axis becomes "tenantId = ?" AND-ed into the
// WHERE clause, and a row belonging to somebody else is simply not
// among the rows the statement names. An INSERT has no WHERE for such a
// predicate to reach. It does not choose rows, it produces one — so the
// only two things a ctx can do here are to STAMP the tenant onto the
// row and to REFUSE when the ctx cannot say which tenant that is.
//
// Both matter, and the second is the one that used to be missing. Until
// the raw builder took a ctx, db.Insert(t).Values(…) on a tenant-scoped
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
// rather than filtering, and the refusal of OR REPLACE — see
// [ErrReplaceScoped] for why that conflict clause cannot be gated the
// way PostgreSQL's DO UPDATE branch is.
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

	rows := i.rows
	if i.table.hasInsertHooks() {
		rows, cp.hooked, changed = i.applyInsertHooks(), true, true
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

	if axis := i.writeAxis(); axis != nil {
		if i.orReplace {
			return nil, fmt.Errorf("%w: %q is tenant-scoped on %s; use OrIgnore, an UPDATE, or say Unscoped",
				ErrReplaceScoped, i.table.Name(), tenantAxisName(axis))
		}
		var err error
		// Stamping rebuilds every row, so an INSERT into a table with a
		// tenant axis has always changed by the time it gets here.
		rows, err = stampTenantColumn(ctx, axis, rows)
		if err != nil {
			return nil, err
		}
		changed = true
	}

	if !changed {
		return i, nil
	}
	cp.rows = rows
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

// stampTenantColumn returns the rows with the tenant column bound to
// the ctx tenant on every one of them.
//
// It is [Entity.stampTenant]'s rule, applied to bindings instead of to
// struct fields: no tenant on ctx is [ErrTenantMissing] and no
// statement at all, an unbound tenant column is filled in from ctx, and
// a tenant column already bound to a different tenant is
// [ErrTenantMismatch]. Either refusal aborts the whole statement before
// a single row is rendered, because a partially stamped INSERT is a
// batch nobody can reason about.
//
// Every row is widened, not only the first. This builder derives the
// column list from row zero, so a stamp applied to one row and not the
// rest would bind values under the wrong names — see
// Entity.alignBindings for the same hazard from the other direction.
func stampTenantColumn(ctx context.Context, axis *Column, rows [][]ColumnValue) ([][]ColumnValue, error) {
	t, ok := TenantFrom(ctx)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTenantMissing, tenantAxisName(axis))
	}
	out := make([][]ColumnValue, len(rows))
	for r, row := range rows {
		at := -1
		for j, cv := range row {
			if cv.column() == axis {
				at = j
				break
			}
		}
		if at < 0 {
			next := make([]ColumnValue, 0, len(row)+1)
			next = append(next, row...)
			out[r] = append(next, columnValue{col: axis, val: t})
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
	// bindingOpaque is everything else — an expression only the engine
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
// expression binding, a PII-wrapped param, whatever a hook or a patch
// operator binds — and a kind added later would silently classify as
// "not a literal" and start refusing valid INSERTs, or worse, if the
// switch had a permissive default, stop checking. What the statement
// will bind is what gets compared, because it is produced the same way
// the statement produces it.
func classifyBinding(v ColumnValue) (any, int) {
	b := drops.NewBuilder(drops.WithDialect(Dialect))
	v.writeValue(b)
	sql, args := b.SQL()
	if len(args) == 1 && sql == onePlaceholder() {
		return unwrapPIIArg(args[0]), bindingLiteral
	}
	return nil, bindingOpaque
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
