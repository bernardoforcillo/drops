package pg

import (
	"context"

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
	rows      [][]drops.Expression
	returning []drops.Expression
	conflict  *conflictClause
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
func columnsOf(values []ColumnValue) []*Column {
	if len(values) == 0 {
		return nil
	}
	tbl := values[0].column().table
	seen := map[*Column]bool{}
	for _, v := range values {
		seen[v.column()] = true
	}
	out := make([]*Column, 0, len(values))
	if tbl != nil {
		for _, c := range tbl.Columns() {
			if seen[c] {
				out = append(out, c)
				delete(seen, c)
			}
		}
	}
	for _, v := range values {
		c := v.column()
		if seen[c] {
			out = append(out, c)
			delete(seen, c)
		}
	}
	return out
}

// alignRow returns a slice of expressions aligned with cols. Missing
// columns are filled with DEFAULT.
func alignRow(cols []*Column, values []ColumnValue) []drops.Expression {
	idx := make(map[*Column]ColumnValue, len(values))
	for _, v := range values {
		idx[v.column()] = v
	}
	out := make([]drops.Expression, len(cols))
	for j, c := range cols {
		if v, ok := idx[c]; ok {
			out[j] = bindingExpr(v)
		} else {
			out[j] = sqlDefault{}
		}
	}
	return out
}

// bindingExpr adapts a ColumnValue into a write-only expression.
func bindingExpr(v ColumnValue) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { v.writeValue(b) })
}

// Returning sets a RETURNING clause.
func (i *InsertBuilder) Returning(cols ...drops.Expression) *InsertBuilder {
	i.returning = append(i.returning, cols...)
	return i
}

// OnConflictDoNothing adds ON CONFLICT [(target...)] DO NOTHING.
func (i *InsertBuilder) OnConflictDoNothing(target ...ColRef) *InsertBuilder {
	i.conflict = &conflictClause{target: refColumns(target), doNoth: true}
	return i
}

// OnConflictUpdate begins an ON CONFLICT (target...) DO UPDATE SET ...
// clause. Pair with Set / Where to populate the update.
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
	if i.table.hasInsertHooks() {
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
			b.Append(v)
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
func (i *InsertBuilder) applyInsertHooks() ([]*Column, [][]drops.Expression) {
	ctx := &InsertHookCtx{bound: make(map[*Column]bool, len(i.cols))}
	for _, c := range i.cols {
		ctx.bound[c] = true
	}
	for _, h := range i.table.insertHooks {
		h.BeforeInsert(ctx)
	}
	if len(ctx.addCols) == 0 {
		return i.cols, i.rows
	}
	cols := append([]*Column(nil), i.cols...)
	cols = append(cols, ctx.addCols...)
	rows := make([][]drops.Expression, len(i.rows))
	for r, row := range i.rows {
		extended := make([]drops.Expression, 0, len(row)+len(ctx.addExprs))
		extended = append(extended, row...)
		extended = append(extended, ctx.addExprs...)
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
func (i *InsertBuilder) ToSQL() (string, []any) {
	b := drops.NewBuilder()
	i.WriteSQL(b)
	return b.SQL()
}

// Exec runs the INSERT.
func (i *InsertBuilder) Exec(ctx context.Context) (drops.Result, error) {
	if len(i.rows) == 0 {
		return nil, ErrNoRowsToInsert
	}
	sql, args := i.ToSQL()
	return i.db.Exec(ctx, sql, args...)
}

// All executes the INSERT and scans the RETURNING rows into dest.
func (i *InsertBuilder) All(ctx context.Context, dest any) error {
	if len(i.returning) == 0 {
		return ErrReturningRequired
	}
	sql, args := i.ToSQL()
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
	sql, args := i.ToSQL()
	rows, err := i.db.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	return scanOne(rows, dest)
}
