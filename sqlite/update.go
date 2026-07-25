package sqlite

import (
	"context"

	"github.com/bernardoforcillo/drops"
)

// UpdateBuilder builds an UPDATE statement. Create one via DB.Update.
type UpdateBuilder struct {
	db       *DB
	table    *Table
	sets     []ColumnValue
	wheres   []drops.Expression
	unscoped bool
}

// Set adds a column assignment.
func (u *UpdateBuilder) Set(vals ...ColumnValue) *UpdateBuilder {
	u.sets = append(u.sets, vals...)
	return u
}

// Where AND-s the given predicates onto the statement.
func (u *UpdateBuilder) Where(preds ...drops.Expression) *UpdateBuilder {
	u.wheres = append(u.wheres, preds...)
	return u
}

// Unscoped opts out of the table's DefaultFilter predicates for this
// UPDATE (e.g. an admin job bypassing a soft-delete or tenant guard).
func (u *UpdateBuilder) Unscoped() *UpdateBuilder {
	u.unscoped = true
	return u
}

// WriteSQL implements drops.Expression.
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
	u.table.writeName(b)
	b.WriteString(" SET ")
	for i, cv := range sets {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteIdent(cv.column().Name())
		b.WriteString(" = ")
		cv.writeValue(b)
	}
	if len(wheres) > 0 {
		b.WriteString(" WHERE ")
		b.AppendList(" AND ", wheres)
	}
}

// applyUpdateHooks runs every UpdateHook on the table and returns the
// (possibly extended) SET list.
func (u *UpdateBuilder) applyUpdateHooks() []ColumnValue {
	ctx := &UpdateHookCtx{bound: make(map[*Column]bool, len(u.sets))}
	for _, s := range u.sets {
		ctx.bound[s.column()] = true
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

// ToSQL renders the statement with SQLite placeholders.
func (u *UpdateBuilder) ToSQL() (sql string, args []any) { return ToSQL(u) }

// Exec runs the UPDATE.
func (u *UpdateBuilder) Exec(ctx context.Context) (drops.Result, error) {
	sql, args := u.ToSQL()
	return u.db.Exec(ctx, sql, args...)
}
