package sqlite

import (
	"context"

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
func (i *InsertBuilder) OrReplace() *InsertBuilder { i.orReplace = true; return i }

// Returning adds a RETURNING clause (SQLite >= 3.35).
func (i *InsertBuilder) Returning(cols ...ColRef) *InsertBuilder {
	i.returning = append(i.returning, cols...)
	return i
}

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
	if len(i.rows) == 0 {
		// Degenerate: nothing to insert. DEFAULT VALUES keeps it valid.
		b.WriteString(" DEFAULT VALUES")
		return
	}
	cols := i.rows[0]
	b.WriteString(" (")
	for j, cv := range cols {
		if j > 0 {
			b.WriteString(", ")
		}
		b.WriteIdent(cv.column().Name())
	}
	b.WriteString(") VALUES ")
	for r, row := range i.rows {
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

// ToSQL renders the statement with SQLite placeholders.
func (i *InsertBuilder) ToSQL() (sql string, args []any) { return ToSQL(i) }

// Exec runs the INSERT.
func (i *InsertBuilder) Exec(ctx context.Context) (drops.Result, error) {
	if len(i.rows) == 0 {
		return nil, ErrNoRowsToInsert
	}
	sql, args := i.ToSQL()
	return i.db.Exec(ctx, sql, args...)
}
