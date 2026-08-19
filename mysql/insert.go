package mysql

import (
	"context"
	"errors"

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
	upsert  bool
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
func (i *InsertBuilder) OnDuplicateKeyUpdate(assignments ...ColumnValue) *InsertBuilder {
	i.upsert = true
	i.upserts = append(i.upserts, assignments...)
	return i
}

// OnDuplicateKeyUpdateAll sets every non-key column to the value the
// insert would have written, which is the "upsert this row" shorthand.
//
// On a table whose every column is part of the key there is nothing to
// assign, and the clause degrades to "do nothing on conflict" rather
// than vanishing.
func (i *InsertBuilder) OnDuplicateKeyUpdateAll() *InsertBuilder {
	i.upsert = true
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
func NewValueOf(col ColRef) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("VALUES(")
		b.WriteIdent(col.col().name)
		b.WriteByte(')')
	})
}

// alignRow orders a row's values to match cols, leaving DEFAULT where
// a column has no binding.
func alignRow(cols []*Column, values []ColumnValue) []drops.Expression {
	byCol := make(map[*Column]ColumnValue, len(values))
	for _, v := range values {
		byCol[v.column()] = v
	}
	out := make([]drops.Expression, len(cols))
	for i, c := range cols {
		v, ok := byCol[c]
		if !ok {
			out[i] = drops.Raw("DEFAULT")
			continue
		}
		out[i] = drops.ExprFunc(v.writeValue)
	}
	return out
}

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
	if i.upsert && (len(i.upserts) > 0 || len(i.cols) > 0) {
		b.WriteString(" ON DUPLICATE KEY UPDATE ")
		if len(i.upserts) == 0 {
			// Nothing to copy over — every column the insert names
			// belongs to the key. Assigning a column to itself is
			// MySQL's spelling of "do nothing on conflict"; dropping
			// the clause instead would turn the upsert back into a
			// plain INSERT that raises 1062.
			b.WriteIdent(i.cols[0].name)
			b.WriteString(" = ")
			b.WriteIdent(i.cols[0].name)
		}
		for n, a := range i.upserts {
			if n > 0 {
				b.WriteString(", ")
			}
			b.WriteIdent(a.column().name)
			b.WriteString(" = ")
			a.writeValue(b)
		}
	}
}

// ErrNoRows is returned when an INSERT is executed with no rows.
var ErrNoRows = errors.New("drops/mysql: INSERT has no rows")

// ToSQL renders the statement and its arguments.
func (i *InsertBuilder) ToSQL() (string, []any) { return render(i) }

// Exec runs the INSERT.
func (i *InsertBuilder) Exec(ctx context.Context) (drops.Result, error) {
	if len(i.rows) == 0 {
		return nil, ErrNoRows
	}
	sql, args := i.ToSQL()
	return i.db.Exec(ctx, sql, args...)
}
