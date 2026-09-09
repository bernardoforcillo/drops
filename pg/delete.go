package pg

import (
	"context"

	"github.com/bernardoforcillo/drops"
)

// DeleteBuilder composes a DELETE statement.
type DeleteBuilder struct {
	db        *DB
	table     *Table
	using     []*Table
	wheres    []drops.Expression
	returning []drops.Expression
	scope     filterScope

	// defaults are the default filters of the tables this statement
	// names, resolved for one execution — see SelectBuilder.defaults.
	defaults resolvedDefaults

	// resolved marks a builder resolveCtx has already produced, so a
	// statement is not scoped twice. See SelectBuilder.resolved.
	resolved bool
}

// Table returns the target table.
func (d *DeleteBuilder) Table() *Table { return d.table }

// Wheres returns a copy of the predicate slice — exposed so custom
// DeleteHooks (e.g. soft-delete rewrites) can read the original WHERE
// clauses when synthesising replacement SQL.
func (d *DeleteBuilder) Wheres() []drops.Expression {
	return append([]drops.Expression(nil), d.wheres...)
}

// UsingTables returns a copy of the USING list — exposed for the same
// reason [DeleteBuilder.Wheres] is: a hook that rewrites the statement
// has to carry them, and a rewrite that drops them leaves a WHERE
// clause naming a relation the new statement no longer has (42P01), or
// worse, one it silently no longer filters.
func (d *DeleteBuilder) UsingTables() []*Table {
	return append([]*Table(nil), d.using...)
}

// ReturningClauses returns a copy of the RETURNING projection list.
func (d *DeleteBuilder) ReturningClauses() []drops.Expression {
	return append([]drops.Expression(nil), d.returning...)
}

// IsUnscoped reports whether the caller opted out of every default
// scope via Unscoped. A DeleteHook reads it to tell a hard DELETE from
// the soft one it would otherwise rewrite. IgnoreFilters does not set
// it: naming a filter drops a predicate, it does not cancel the
// rewrite that turns DELETE into UPDATE.
func (d *DeleteBuilder) IsUnscoped() bool { return d.scope.unscoped }

// DB returns the executing DB. Hooks that need to build a replacement
// statement (an UPDATE for soft-delete, for instance) use it.
func (d *DeleteBuilder) DB() *DB { return d.db }

// Using adds tables to a PostgreSQL DELETE ... USING clause for joins.
func (d *DeleteBuilder) Using(tables ...*Table) *DeleteBuilder {
	d.using = append(d.using, tables...)
	return d
}

// Where appends predicates joined by AND. Nil predicates are ignored,
// so a filter that is only sometimes present can be passed straight in
// — but a DELETE all of whose predicates were nil is a DELETE with no
// WHERE, and removes every row the table's filters still admit.
func (d *DeleteBuilder) Where(preds ...drops.Expression) *DeleteBuilder {
	d.wheres = append(d.wheres, dropNilPreds(preds)...)
	return d
}

// Returning sets a RETURNING clause.
func (d *DeleteBuilder) Returning(cols ...drops.Expression) *DeleteBuilder {
	d.returning = append(d.returning, cols...)
	return d
}

// Unscoped opts out of both DeleteHooks and every global filter on the
// table. On a soft-deleted table it forces a real, hard DELETE that
// bypasses the rewrite-to-UPDATE behaviour — and, being the blunt
// instrument, drops the table's other scoping with it.
func (d *DeleteBuilder) Unscoped() *DeleteBuilder {
	d.scope.unscoped = true
	return d
}

// IgnoreFilters bypasses the named global filters on the table and
// leaves every other one in place — see [SelectBuilder.IgnoreFilters].
// It only drops predicates: the DeleteHooks stay, so on a soft-deleted
// table the statement is still rewritten into an UPDATE. Use Unscoped
// when you want the row gone for good.
func (d *DeleteBuilder) IgnoreFilters(names ...string) *DeleteBuilder {
	d.scope.ignore(names...)
	return d
}

// namedTables is the target and every USING table, in the order the
// statement names them — the tables whose automatic predicates this
// DELETE carries.
func (d *DeleteBuilder) namedTables() []*Table {
	tables := make([]*Table, 0, len(d.using)+1)
	if d.table != nil {
		tables = append(tables, d.table)
	}
	return append(tables, d.using...)
}

// ToSQLCtx renders the statement ctx would send: the context filters of
// the target table and of every USING table, resolved and AND-ed in,
// and every statement written inside the WHERE clause or the RETURNING
// projection resolved with them.
//
// A DELETE that loses its axis removes another tenant's rows and
// reports success, and nothing walks that back — so a filter that
// refuses returns the refusal and no statement at all.
func (d *DeleteBuilder) ToSQLCtx(ctx context.Context) (sql string, args []any, err error) {
	r, err := d.resolveCtx(ctx)
	if err != nil {
		return "", nil, err
	}
	sql, args = r.ToSQL()
	return sql, args, nil
}

// resolveCtx returns the builder to render for one execution — the
// receiver when there was nothing to resolve. See
// [SelectBuilder.resolveCtx] for why the identity matters, and
// [UpdateBuilder.resolveCtx] for why the predicates land in the WHERE
// list rather than in a field of their own.
func (d *DeleteBuilder) resolveCtx(ctx context.Context) (*DeleteBuilder, error) {
	if d.resolved {
		return d, nil
	}
	// The named filters this statement bypasses, for the length of
	// this resolution: a nested statement installs its own at the top
	// of its own resolveCtx, so IgnoreFilters reaches no further than
	// the statement that said it.
	ctx = withIgnoredFilters(ctx, d.scope)

	cp := *d
	changed := false

	wheres := d.wheres
	if r, err := resolveExprs(ctx, d.wheres); err != nil {
		return nil, err
	} else if r != nil {
		wheres, changed = r, true
	}
	if r, err := resolveExprs(ctx, d.returning); err != nil {
		return nil, err
	} else if r != nil {
		cp.returning, changed = r, true
	}

	if !d.scope.dropsContextFilters() {
		tables := d.namedTables()
		var preds []drops.Expression
		for _, t := range tables {
			// A USING table's rows choose which target rows go, so an
			// unfiltered one lets another tenant's rows decide what
			// this DELETE removes.
			p, err := t.resolveContextFilters(ctx)
			if err != nil {
				return nil, err
			}
			preds = append(preds, p...)
		}
		if len(preds) > 0 {
			all := make([]drops.Expression, 0, len(wheres)+len(preds))
			all = append(all, wheres...)
			wheres, changed = append(all, preds...), true
		}
		defaults, err := resolveTableDefaults(ctx, tables...)
		if err != nil {
			return nil, err
		}
		if defaults != nil {
			cp.defaults, changed = defaults, true
		}
	}

	if !changed {
		return d, nil
	}
	cp.wheres = wheres
	cp.resolved = true
	return &cp, nil
}

// resolveStatement implements [ctxResolvable], so a DELETE written as a
// CTE body is scoped like a bare one — WITH moved AS (DELETE FROM
// scoped RETURNING ...) is the shape that made an unscoped one
// reachable through the exported API.
func (d *DeleteBuilder) resolveStatement(ctx context.Context) (drops.Expression, bool, error) {
	r, err := d.resolveCtx(ctx)
	if err != nil {
		return nil, false, err
	}
	return r, r != d, nil
}

// WriteSQL renders the DELETE. If the table has DeleteHooks and the
// caller has not opted out via Unscoped, hooks may replace the
// statement entirely — used by SoftDelete to flip DELETE into UPDATE.
func (d *DeleteBuilder) WriteSQL(b *drops.Builder) {
	if !d.scope.unscoped {
		for _, h := range d.table.deleteHookList() {
			if rep := h.BeforeDelete(d); rep != nil {
				rep.WriteSQL(b)
				return
			}
		}
	}
	wheres := d.scope.applyAll(d.namedTables(), d.wheres, d.defaults)
	b.WriteString("DELETE FROM ")
	d.table.writeFrom(b)
	if len(d.using) > 0 {
		b.WriteString(" USING ")
		for j, t := range d.using {
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
	if len(d.returning) > 0 {
		b.WriteString(" RETURNING ")
		b.AppendList(", ", d.returning)
	}
}

// ToSQL renders the statement.
func (d *DeleteBuilder) ToSQL() (sql string, args []any) {
	b := drops.NewBuilder()
	d.WriteSQL(b)
	return b.SQL()
}

// Exec runs the DELETE.
func (d *DeleteBuilder) Exec(ctx context.Context) (drops.Result, error) {
	// Through ToSQLCtx: a DELETE that loses its context filters removes
	// another tenant's rows and reports success.
	sql, args, err := d.ToSQLCtx(ctx)
	if err != nil {
		return nil, err
	}
	return d.db.Exec(ctx, sql, args...)
}

// All executes the DELETE and scans the RETURNING rows into dest.
func (d *DeleteBuilder) All(ctx context.Context, dest any) error {
	if len(d.returning) == 0 {
		return ErrReturningRequired
	}
	sql, args, err := d.ToSQLCtx(ctx)
	if err != nil {
		return err
	}
	rows, err := d.db.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	return scanAll(rows, dest)
}

// One executes the DELETE and scans the first RETURNING row into dest.
func (d *DeleteBuilder) One(ctx context.Context, dest any) error {
	if len(d.returning) == 0 {
		return ErrReturningRequired
	}
	sql, args, err := d.ToSQLCtx(ctx)
	if err != nil {
		return err
	}
	rows, err := d.db.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	return scanOne(rows, dest)
}
