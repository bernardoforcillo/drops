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
	// upsertAll records that the assignment list was derived by
	// OnDuplicateKeyUpdateAll rather than supplied by the caller. An
	// empty list means opposite things in the two cases — see the
	// methods' docs — so the renderer has to be able to tell them
	// apart.
	upsertAll bool
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
//
// With no assignments the clause is not rendered at all and the
// statement stays a plain INSERT, so a duplicate still raises error
// 1062. This differs on purpose from the empty case of
// [InsertBuilder.OnDuplicateKeyUpdateAll], which does render a clause:
// there the list is empty because every column the insert names
// belongs to the key, so the colliding row already equals the one
// being inserted and swallowing the collision discards nothing. A list
// the caller assembled and that came out empty carries no such
// guarantee — the row may differ in exactly the columns nobody
// assigned — so drops raises rather than guessing. Spell "do nothing
// on conflict" as [InsertBuilder.Ignore], which says so.
//
// An assignment names its column on the right as well as the left —
// "age = age + ?" — and qualified, so each one is restated against the
// declared table's handle. The declared table is the only qualifier an
// INSERT can accept: the INTO clause is written by writeName and
// carries no AS, so even an insert built entirely from an alias's own
// handles has to name the table there.
func (i *InsertBuilder) OnDuplicateKeyUpdate(assignments ...ColumnValue) *InsertBuilder {
	for _, a := range assignments {
		i.upserts = append(i.upserts, rebindValue(i.table.key(), a))
	}
	return i
}

// OnDuplicateKeyUpdateAll sets every non-key column to the value the
// insert would have written, which is the "upsert this row" shorthand.
//
// On a table whose every column is part of the key there is nothing to
// assign, and the clause degrades to a self-assignment — MySQL's
// spelling of "do nothing on conflict" — rather than vanishing. That
// keeps the method's promise that the row exists afterwards and no
// error was raised, and it costs nothing: on such a table the row
// already in the way is equal, column for column, to the one being
// inserted. [InsertBuilder.OnDuplicateKeyUpdate] does not degrade this
// way, and its doc explains why the two differ.
func (i *InsertBuilder) OnDuplicateKeyUpdateAll() *InsertBuilder {
	i.upsertAll = true
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
//
// The column list is fixed by the first Row and the bindings come from
// whichever handles the caller held, so the two sides are matched on
// Column.key rather than on the pointer: a value bound through an
// alias names the same column as the declared handle. Matching by
// pointer instead drops it, and drops it silently — the column falls
// to the DEFAULT fill, which is well-formed SQL that writes the wrong
// row.
func alignRow(cols []*Column, values []ColumnValue) []drops.Expression {
	byCol := make(map[*Column]ColumnValue, len(values))
	for _, v := range values {
		byCol[v.column().key()] = v
	}
	out := make([]drops.Expression, len(cols))
	for i, c := range cols {
		v, ok := byCol[c.key()]
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
	switch {
	case len(i.upserts) > 0:
		b.WriteString(" ON DUPLICATE KEY UPDATE ")
		for n, a := range i.upserts {
			if n > 0 {
				b.WriteString(", ")
			}
			b.WriteIdent(a.column().name)
			b.WriteString(" = ")
			a.writeValue(b)
		}
	case i.upsertAll && len(i.cols) > 0:
		// Nothing to copy over — every column the insert names
		// belongs to the key. Assigning a column to itself is
		// MySQL's spelling of "do nothing on conflict"; dropping
		// the clause instead would turn the upsert back into a
		// plain INSERT that raises 1062.
		b.WriteString(" ON DUPLICATE KEY UPDATE ")
		b.WriteIdent(i.cols[0].name)
		b.WriteString(" = ")
		b.WriteIdent(i.cols[0].name)
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
