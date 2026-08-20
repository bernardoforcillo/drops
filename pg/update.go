package pg

import (
	"context"

	"github.com/bernardoforcillo/drops"
)

// UpdateBuilder composes an UPDATE statement.
type UpdateBuilder struct {
	db        *DB
	table     *Table
	sets      []ColumnValue
	from      []*Table
	wheres    []drops.Expression
	returning []drops.Expression
	unscoped  bool
}

// Set adds one or more assignments. Use (*Col[T]).Val(v) to bind a typed
// value or (*Col[T]).Expr(e) to bind an expression.
func (u *UpdateBuilder) Set(values ...ColumnValue) *UpdateBuilder {
	u.sets = append(u.sets, values...)
	return u
}

// From adds tables to a PostgreSQL UPDATE ... FROM clause for joins.
func (u *UpdateBuilder) From(tables ...*Table) *UpdateBuilder {
	u.from = append(u.from, tables...)
	return u
}

// Where appends predicates joined by AND.
func (u *UpdateBuilder) Where(preds ...drops.Expression) *UpdateBuilder {
	u.wheres = append(u.wheres, preds...)
	return u
}

// Returning sets a RETURNING clause.
func (u *UpdateBuilder) Returning(cols ...drops.Expression) *UpdateBuilder {
	u.returning = append(u.returning, cols...)
	return u
}

// Unscoped opts out of the table's automatic predicates for this
// UPDATE — both its DefaultFilter list and its ContextFilter list. Use
// when an administrative job must bypass a soft-delete guard, or write
// across every tenant.
//
// On an UPDATE the widening is the dangerous direction: an unscoped
// statement does not read another tenant's rows, it writes them. Prefer
// restating the scope you meant — Unscoped().Where(...) — over dropping
// it wholesale.
func (u *UpdateBuilder) Unscoped() *UpdateBuilder {
	u.unscoped = true
	return u
}

// WriteSQL renders the UPDATE.
func (u *UpdateBuilder) WriteSQL(b *drops.Builder) {
	sets := u.sets
	if u.table.hasUpdateHooks() {
		sets = u.applyUpdateHooks()
	}
	wheres := u.wheres
	if !u.unscoped && len(u.table.defaultFilters) > 0 {
		wheres = append(append([]drops.Expression(nil), u.table.defaultFilters...), wheres...)
	}
	b.WriteString("UPDATE ")
	u.table.writeFrom(b)
	b.WriteString(" SET ")
	for j, s := range sets {
		if j > 0 {
			b.WriteString(", ")
		}
		b.WriteIdent(s.column().Name())
		b.WriteString(" = ")
		s.writeValue(b)
	}
	if len(u.from) > 0 {
		b.WriteString(" FROM ")
		for j, t := range u.from {
			if j > 0 {
				b.WriteString(", ")
			}
			t.writeFrom(b)
		}
	}
	if len(wheres) > 0 {
		b.WriteString(" WHERE ")
		writeAnd(b, wheres)
	}
	if len(u.returning) > 0 {
		b.WriteString(" RETURNING ")
		b.AppendList(", ", u.returning)
	}
}

// applyUpdateHooks runs every UpdateHook registered on the table and
// returns the (possibly extended) SET list.
func (u *UpdateBuilder) applyUpdateHooks() []ColumnValue {
	ctx := &UpdateHookCtx{bound: make(map[*Column]bool, len(u.sets))}
	for _, s := range u.sets {
		ctx.bound[s.column().key()] = true
	}
	for _, h := range u.table.updateHooks {
		h.BeforeUpdate(ctx)
	}
	if len(ctx.add) == 0 {
		return u.sets
	}
	out := append([]ColumnValue(nil), u.sets...)
	out = append(out, ctx.add...)
	return out
}

// ToSQL renders the statement.
//
// A table's context filters are resolved by the executors and do not
// appear here — see [SelectBuilder.ToSQL] for why, and use
// [UpdateBuilder.ToSQLCtx] for the statement a given ctx would send.
func (u *UpdateBuilder) ToSQL() (sql string, args []any) {
	b := drops.NewBuilder()
	u.WriteSQL(b)
	return b.SQL()
}

// ToSQLCtx renders the complete statement for ctx, with every context
// filter on the target table resolved into the WHERE clause.
func (u *UpdateBuilder) ToSQLCtx(ctx context.Context) (sql string, args []any, err error) {
	r, err := u.resolveCtx(ctx)
	if err != nil {
		return "", nil, err
	}
	sql, args = r.ToSQL()
	return sql, args, nil
}

// resolveCtx returns the builder to render for one execution — this one
// when the table has no context filters, otherwise a shallow copy whose
// WHERE list carries the resolved predicates. The copy is what keeps
// the same builder executable twice without accumulating a predicate
// per run; see [SelectBuilder.resolveCtx].
func (u *UpdateBuilder) resolveCtx(ctx context.Context) (*UpdateBuilder, error) {
	if u.unscoped || !u.table.hasContextFilters() {
		return u, nil
	}
	preds, err := u.table.resolveContextFilters(ctx)
	if err != nil {
		return nil, err
	}
	if len(preds) == 0 {
		return u, nil
	}
	cp := *u
	cp.wheres = append(append([]drops.Expression(nil), u.wheres...), preds...)
	return &cp, nil
}

// Exec runs the UPDATE.
func (u *UpdateBuilder) Exec(ctx context.Context) (drops.Result, error) {
	if len(u.sets) == 0 && !u.table.hasUpdateHooks() {
		return nil, ErrNoUpdateAssignments
	}
	sql, args, err := u.ToSQLCtx(ctx)
	if err != nil {
		return nil, err
	}
	return u.db.Exec(ctx, sql, args...)
}

// All executes the UPDATE and scans the RETURNING rows into dest.
func (u *UpdateBuilder) All(ctx context.Context, dest any) error {
	if len(u.returning) == 0 {
		return ErrReturningRequired
	}
	sql, args, err := u.ToSQLCtx(ctx)
	if err != nil {
		return err
	}
	rows, err := u.db.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	return scanAll(rows, dest)
}

// One executes the UPDATE and scans the first RETURNING row into dest.
func (u *UpdateBuilder) One(ctx context.Context, dest any) error {
	if len(u.returning) == 0 {
		return ErrReturningRequired
	}
	sql, args, err := u.ToSQLCtx(ctx)
	if err != nil {
		return err
	}
	rows, err := u.db.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	return scanOne(rows, dest)
}
