package mysql

import (
	"context"

	"github.com/bernardoforcillo/drops"
)

// SelectBuilder composes a SELECT statement.
type SelectBuilder struct {
	db       *DB
	columns  []drops.Expression
	from     *Table
	joins    []joinClause
	wheres   []drops.Expression
	groupBys []drops.Expression
	havings  []drops.Expression
	orderBys []drops.Expression
	limit    *int64
	offset   *int64
	distinct bool
	forShare bool
	forUpd   string
	unscoped bool
}

type joinKind string

const (
	innerJoin joinKind = "INNER JOIN"
	leftJoin  joinKind = "LEFT JOIN"
	rightJoin joinKind = "RIGHT JOIN"
)

type joinClause struct {
	kind  joinKind
	table *Table
	on    drops.Expression
}

// From sets the FROM table. Required before execution.
func (s *SelectBuilder) From(t *Table) *SelectBuilder { s.from = t; return s }

// Distinct toggles SELECT DISTINCT.
func (s *SelectBuilder) Distinct() *SelectBuilder { s.distinct = true; return s }

// Join / LeftJoin / RightJoin append joins. MySQL has no FULL OUTER
// JOIN, so there is no FullJoin here — emulate it with a UNION of the
// two outer joins if you need one.
func (s *SelectBuilder) Join(t *Table, on drops.Expression) *SelectBuilder {
	s.joins = append(s.joins, joinClause{innerJoin, t, on})
	return s
}

func (s *SelectBuilder) LeftJoin(t *Table, on drops.Expression) *SelectBuilder {
	s.joins = append(s.joins, joinClause{leftJoin, t, on})
	return s
}

func (s *SelectBuilder) RightJoin(t *Table, on drops.Expression) *SelectBuilder {
	s.joins = append(s.joins, joinClause{rightJoin, t, on})
	return s
}

// Where appends predicates joined by AND.
func (s *SelectBuilder) Where(preds ...drops.Expression) *SelectBuilder {
	s.wheres = append(s.wheres, preds...)
	return s
}

// GroupBy / Having / OrderBy.
func (s *SelectBuilder) GroupBy(exprs ...drops.Expression) *SelectBuilder {
	s.groupBys = append(s.groupBys, exprs...)
	return s
}

func (s *SelectBuilder) Having(preds ...drops.Expression) *SelectBuilder {
	s.havings = append(s.havings, preds...)
	return s
}

func (s *SelectBuilder) OrderBy(exprs ...drops.Expression) *SelectBuilder {
	s.orderBys = append(s.orderBys, exprs...)
	return s
}

// Limit / Offset bound the result window.
func (s *SelectBuilder) Limit(n int64) *SelectBuilder  { s.limit = &n; return s }
func (s *SelectBuilder) Offset(n int64) *SelectBuilder { s.offset = &n; return s }

// ForUpdate appends FOR UPDATE row locking. It outranks
// [SelectBuilder.ForShare] in either order — see there.
func (s *SelectBuilder) ForUpdate() *SelectBuilder { s.forUpd = " FOR UPDATE"; return s }

// ForUpdateSkipLocked appends FOR UPDATE SKIP LOCKED — the queue-worker
// pattern. MySQL 8.0+ / MariaDB 10.6+.
func (s *SelectBuilder) ForUpdateSkipLocked() *SelectBuilder {
	s.forUpd = " FOR UPDATE SKIP LOCKED"
	return s
}

// ForShare appends a shared read lock, rendered as LOCK IN SHARE MODE.
//
// MySQL 8.0 introduced FOR SHARE as the modern spelling and kept the
// older one; MariaDB has never accepted FOR SHARE at all, answering a
// syntax error. LOCK IN SHARE MODE is the form both servers take, and
// drops targets the intersection.
//
// A builder that asks for both locks renders FOR UPDATE, whichever
// order the calls came in: the two clauses cannot both be written, and
// of the two the exclusive lock is the one that cannot be too weak.
// Silently downgrading to a shared lock because ForShare happened to
// be called second is how a read-modify-write loses a row.
func (s *SelectBuilder) ForShare() *SelectBuilder { s.forShare = true; return s }

// Unscoped opts out of the FROM table's DefaultFilter predicates.
func (s *SelectBuilder) Unscoped() *SelectBuilder { s.unscoped = true; return s }

// WriteSQL renders the SELECT.
func (s *SelectBuilder) WriteSQL(b *drops.Builder) {
	b.WriteString("SELECT ")
	if s.distinct {
		b.WriteString("DISTINCT ")
	}
	if len(s.columns) == 0 {
		b.WriteByte('*')
	} else {
		b.AppendList(", ", s.columns)
	}
	if s.from != nil {
		b.WriteString(" FROM ")
		s.from.writeFrom(b)
	}
	for _, j := range s.joins {
		b.WriteByte(' ')
		b.WriteString(string(j.kind))
		b.WriteByte(' ')
		j.table.writeFrom(b)
		b.WriteString(" ON ")
		b.Append(j.on)
	}
	wheres := s.wheres
	if !s.unscoped && s.from != nil && len(s.from.defaultFilters) > 0 {
		wheres = append(append([]drops.Expression(nil), s.from.defaultFilters...), wheres...)
	}
	if len(wheres) > 0 {
		b.WriteString(" WHERE ")
		writeAnd(b, wheres)
	}
	if len(s.groupBys) > 0 {
		b.WriteString(" GROUP BY ")
		b.AppendList(", ", s.groupBys)
	}
	if len(s.havings) > 0 {
		b.WriteString(" HAVING ")
		writeAnd(b, s.havings)
	}
	if len(s.orderBys) > 0 {
		b.WriteString(" ORDER BY ")
		b.AppendList(", ", s.orderBys)
	}
	if s.limit != nil {
		b.WriteString(" LIMIT ")
		b.AddArg(*s.limit)
	}
	if s.offset != nil {
		// MySQL rejects OFFSET without LIMIT, so an offset alone
		// gets the largest limit the grammar accepts — the documented
		// idiom for "skip N, take the rest".
		if s.limit == nil {
			b.WriteString(" LIMIT 18446744073709551615")
		}
		b.WriteString(" OFFSET ")
		b.AddArg(*s.offset)
	}
	if s.forUpd != "" {
		b.WriteString(s.forUpd)
	} else if s.forShare {
		b.WriteString(" LOCK IN SHARE MODE")
	}
}

// ToSQL renders the statement and its arguments.
func (s *SelectBuilder) ToSQL() (string, []any) { return render(s) }

// Rows executes the SELECT and returns the raw cursor.
func (s *SelectBuilder) Rows(ctx context.Context) (drops.Rows, error) {
	sql, args := s.ToSQL()
	return s.db.Query(ctx, sql, args...)
}

// All scans every row into dest, a pointer to a slice of structs.
func (s *SelectBuilder) All(ctx context.Context, dest any) error {
	rows, err := s.Rows(ctx)
	if err != nil {
		return err
	}
	return drops.ScanAll(rows, dest)
}

// One scans the first row into dest, returning ErrNoRows when empty.
func (s *SelectBuilder) One(ctx context.Context, dest any) error {
	rows, err := s.Rows(ctx)
	if err != nil {
		return err
	}
	return drops.ScanOne(rows, dest)
}

// AsSubquery returns a parenthesised, aliased form for use inside
// another statement.
func (s *SelectBuilder) AsSubquery(alias string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		s.WriteSQL(b)
		b.WriteString(") AS ")
		b.WriteIdent(alias)
	})
}

// writeAnd joins predicates with AND, without the outer parentheses.
func writeAnd(b *drops.Builder, preds []drops.Expression) {
	for i, p := range preds {
		if i > 0 {
			b.WriteString(" AND ")
		}
		b.Append(p)
	}
}
