package sqlite

import (
	"context"
	"errors"

	"github.com/bernardoforcillo/drops"
)

// SelectBuilder builds a SELECT statement. Create one via DB.Select.
type SelectBuilder struct {
	db       *DB
	columns  []drops.Expression
	table    *Table
	joins    []joinClause
	wheres   []drops.Expression
	orderBy  []drops.Expression
	distinct bool
	limit    *int64
	offset   *int64

	ctes         []*CTE
	recursiveCTE bool
	unscoped     bool
}

// Unscoped opts out of the FROM table's DefaultFilter predicates for
// this SELECT (e.g. to read soft-deleted rows).
func (s *SelectBuilder) Unscoped() *SelectBuilder { s.unscoped = true; return s }

type joinClause struct {
	kind  string // "JOIN", "LEFT JOIN", ...
	table *Table
	on    drops.Expression
}

// From sets the table to select from.
func (s *SelectBuilder) From(t *Table) *SelectBuilder { s.table = t; return s }

// Distinct adds the DISTINCT keyword.
func (s *SelectBuilder) Distinct() *SelectBuilder { s.distinct = true; return s }

// Join adds an INNER JOIN ... ON.
func (s *SelectBuilder) Join(t *Table, on drops.Expression) *SelectBuilder {
	s.joins = append(s.joins, joinClause{"JOIN", t, on})
	return s
}

// LeftJoin adds a LEFT JOIN ... ON.
func (s *SelectBuilder) LeftJoin(t *Table, on drops.Expression) *SelectBuilder {
	s.joins = append(s.joins, joinClause{"LEFT JOIN", t, on})
	return s
}

// Where AND-s the given predicates onto the statement.
func (s *SelectBuilder) Where(preds ...drops.Expression) *SelectBuilder {
	s.wheres = append(s.wheres, preds...)
	return s
}

// OrderBy sets the ORDER BY expressions (use (*Column).WriteSQL refs, or
// raw drops.Raw("col DESC")).
func (s *SelectBuilder) OrderBy(exprs ...drops.Expression) *SelectBuilder {
	s.orderBy = append(s.orderBy, exprs...)
	return s
}

// Limit sets a LIMIT.
func (s *SelectBuilder) Limit(n int64) *SelectBuilder { s.limit = &n; return s }

// Offset sets an OFFSET.
func (s *SelectBuilder) Offset(n int64) *SelectBuilder { s.offset = &n; return s }

// WriteSQL implements drops.Expression.
func (s *SelectBuilder) WriteSQL(b *drops.Builder) {
	writeCTEs(b, s.ctes, s.recursiveCTE)
	b.WriteString("SELECT ")
	if s.distinct {
		b.WriteString("DISTINCT ")
	}
	if len(s.columns) == 0 {
		b.WriteByte('*')
	} else {
		b.AppendList(", ", s.columns)
	}
	if s.table != nil {
		b.WriteString(" FROM ")
		s.table.writeFrom(b)
	}
	for _, j := range s.joins {
		b.WriteByte(' ')
		b.WriteString(j.kind)
		b.WriteByte(' ')
		j.table.writeFrom(b)
		b.WriteString(" ON ")
		b.Append(j.on)
	}
	wheres := s.wheres
	if !s.unscoped && s.table != nil && len(s.table.defaultFilters) > 0 {
		wheres = append(append([]drops.Expression(nil), s.table.defaultFilters...), wheres...)
	}
	if len(wheres) > 0 {
		b.WriteString(" WHERE ")
		b.AppendList(" AND ", wheres)
	}
	if len(s.orderBy) > 0 {
		b.WriteString(" ORDER BY ")
		b.AppendList(", ", s.orderBy)
	}
	if s.limit != nil {
		b.WriteString(" LIMIT ")
		b.AddArg(*s.limit)
	}
	if s.offset != nil {
		// SQLite's grammar only reaches OFFSET through LIMIT, so an
		// offset alone needs one; -1 is the documented spelling of
		// "no limit".
		if s.limit == nil {
			b.WriteString(" LIMIT -1")
		}
		b.WriteString(" OFFSET ")
		b.AddArg(*s.offset)
	}
}

// ToSQL renders the statement with SQLite placeholders.
func (s *SelectBuilder) ToSQL() (sql string, args []any) { return ToSQL(s) }

// All executes the query and scans every row into dest (pointer to
// slice of struct or *struct).
func (s *SelectBuilder) All(ctx context.Context, dest any) error {
	sql, args := s.ToSQL()
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	return drops.ScanAll(rows, dest)
}

// One executes the query and scans the first row into dest (pointer to
// struct), returning ErrNoRows when empty.
func (s *SelectBuilder) One(ctx context.Context, dest any) error {
	sql, args := s.ToSQL()
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	if err := drops.ScanOne(rows, dest); err != nil {
		if errors.Is(err, drops.ErrNoRows) {
			return ErrNoRows
		}
		return err
	}
	return nil
}
