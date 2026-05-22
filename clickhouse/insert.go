package clickhouse

import (
	"context"

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
	rows  [][]drops.Expression
}

// Row appends a single row. The first Row fixes the column list.
func (i *InsertBuilder) Row(values ...ColumnValue) *InsertBuilder {
	if i.cols == nil {
		i.cols = columnsOf(values)
	}
	i.rows = append(i.rows, alignRow(i.cols, values))
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

// columnsOf picks columns from the first row, in the table's declared
// order for determinism.
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

// alignRow aligns the row's values with the chosen column order;
// missing columns default to NULL (CH uses NULL as the "no value"
// marker — there's no DEFAULT keyword inside VALUES the way PG has).
func alignRow(cols []*Column, values []ColumnValue) []drops.Expression {
	idx := map[*Column]ColumnValue{}
	for _, v := range values {
		idx[v.column()] = v
	}
	out := make([]drops.Expression, len(cols))
	for j, c := range cols {
		if v, ok := idx[c]; ok {
			out[j] = bindingExpr(v)
		} else {
			out[j] = drops.Raw("NULL")
		}
	}
	return out
}

func bindingExpr(v ColumnValue) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { v.writeValue(b) })
}

// WriteSQL renders the INSERT statement.
func (i *InsertBuilder) WriteSQL(b *drops.Builder) {
	b.WriteString("INSERT INTO ")
	i.table.writeFrom(b)
	b.WriteString(" (")
	for j, c := range i.cols {
		if j > 0 {
			b.WriteString(", ")
		}
		b.WriteIdent(c.Name())
	}
	b.WriteString(") VALUES ")
	for r, row := range i.rows {
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
}

// ToSQL renders the statement.
func (i *InsertBuilder) ToSQL() (string, []any) {
	b := drops.NewBuilder(Placeholder)
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
