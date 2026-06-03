package clickhouse

import (
	"context"

	"github.com/bernardoforcillo/drops"
)

// SelectBuilder composes a ClickHouse SELECT. It mirrors the drops/pg
// surface (Where, OrderBy, GroupBy, Limit, joins) plus CH-specific
// clauses (PREWHERE, FINAL, SAMPLE, SETTINGS).
type SelectBuilder struct {
	db        *DB
	columns   []drops.Expression
	from      *Table
	final     bool
	sampleBy  drops.Expression
	joins     []joinClause
	prewheres []drops.Expression
	wheres    []drops.Expression
	groupBys  []drops.Expression
	havings   []drops.Expression
	orderBys  []drops.Expression
	limit     *int64
	offset    *int64
	distinct  bool
	settings  []string // raw "key = value"
	unscoped  bool
}

type joinKind string

const (
	innerJoin joinKind = "INNER JOIN"
	leftJoin  joinKind = "LEFT JOIN"
	rightJoin joinKind = "RIGHT JOIN"
	fullJoin  joinKind = "FULL JOIN"
	anyJoin   joinKind = "ANY INNER JOIN"
	allJoin   joinKind = "ALL INNER JOIN"
	asofJoin  joinKind = "ASOF JOIN"
)

type joinClause struct {
	kind  joinKind
	table *Table
	on    drops.Expression
}

// From sets the FROM table. Required before execution.
func (s *SelectBuilder) From(t *Table) *SelectBuilder { s.from = t; return s }

// Final appends FINAL after the table, forcing CH to merge parts
// at read time (handy with ReplacingMergeTree / CollapsingMergeTree
// when you accept the cost).
func (s *SelectBuilder) Final() *SelectBuilder { s.final = true; return s }

// SampleBy adds a SAMPLE clause (e.g. SampleBy(0.1) for 10%).
func (s *SelectBuilder) SampleBy(e any) *SelectBuilder {
	if expr, ok := e.(drops.Expression); ok {
		s.sampleBy = expr
		return s
	}
	s.sampleBy = drops.ExprFunc(func(b *drops.Builder) { b.AddArg(e) })
	return s
}

// Distinct toggles SELECT DISTINCT.
func (s *SelectBuilder) Distinct() *SelectBuilder { s.distinct = true; return s }

// Join / LeftJoin / RightJoin / FullJoin / AnyJoin / AllJoin / AsofJoin.
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
func (s *SelectBuilder) FullJoin(t *Table, on drops.Expression) *SelectBuilder {
	s.joins = append(s.joins, joinClause{fullJoin, t, on})
	return s
}
func (s *SelectBuilder) AnyJoin(t *Table, on drops.Expression) *SelectBuilder {
	s.joins = append(s.joins, joinClause{anyJoin, t, on})
	return s
}
func (s *SelectBuilder) AllJoin(t *Table, on drops.Expression) *SelectBuilder {
	s.joins = append(s.joins, joinClause{allJoin, t, on})
	return s
}
func (s *SelectBuilder) AsofJoin(t *Table, on drops.Expression) *SelectBuilder {
	s.joins = append(s.joins, joinClause{asofJoin, t, on})
	return s
}

// Prewhere adds a PREWHERE predicate — evaluated before the main
// WHERE, with the right primary-key columns it can dramatically cut
// scanned data on MergeTree tables.
func (s *SelectBuilder) Prewhere(preds ...drops.Expression) *SelectBuilder {
	s.prewheres = append(s.prewheres, preds...)
	return s
}

// Where appends predicates joined by AND.
func (s *SelectBuilder) Where(preds ...drops.Expression) *SelectBuilder {
	s.wheres = append(s.wheres, preds...)
	return s
}

// Unscoped opts out of the FROM table's DefaultFilter predicates for
// this SELECT.
func (s *SelectBuilder) Unscoped() *SelectBuilder { s.unscoped = true; return s }

// GroupBy / Having / OrderBy / Limit / Offset.
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
func (s *SelectBuilder) Limit(n int64) *SelectBuilder  { s.limit = &n; return s }
func (s *SelectBuilder) Offset(n int64) *SelectBuilder { s.offset = &n; return s }

// Setting appends a "key = value" pair to the SETTINGS clause.
func (s *SelectBuilder) Setting(key, value string) *SelectBuilder {
	s.settings = append(s.settings, key+" = "+value)
	return s
}

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
		if s.final {
			b.WriteString(" FINAL")
		}
	}
	if s.sampleBy != nil {
		b.WriteString(" SAMPLE ")
		b.Append(s.sampleBy)
	}
	for _, j := range s.joins {
		b.WriteByte(' ')
		b.WriteString(string(j.kind))
		b.WriteByte(' ')
		j.table.writeFrom(b)
		b.WriteString(" ON ")
		b.Append(j.on)
	}
	if len(s.prewheres) > 0 {
		b.WriteString(" PREWHERE ")
		writeAnd(b, s.prewheres)
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
		b.WriteString(" OFFSET ")
		b.AddArg(*s.offset)
	}
	if len(s.settings) > 0 {
		b.WriteString(" SETTINGS ")
		first := true
		for _, kv := range s.settings {
			if !first {
				b.WriteString(", ")
			}
			b.WriteString(kv)
			first = false
		}
	}
}

// ToSQL renders the statement using the ClickHouse placeholder style.
func (s *SelectBuilder) ToSQL() (string, []any) {
	b := drops.NewBuilder(Placeholder)
	s.WriteSQL(b)
	return b.SQL()
}

// Rows runs the SELECT and returns the raw cursor.
func (s *SelectBuilder) Rows(ctx context.Context) (drops.Rows, error) {
	sql, args := s.ToSQL()
	return s.db.Query(ctx, sql, args...)
}

// All scans every row into dest.
func (s *SelectBuilder) All(ctx context.Context, dest any) error {
	rows, err := s.Rows(ctx)
	if err != nil {
		return err
	}
	return scanAll(rows, dest)
}

// One scans the first row into dest. Returns ErrNoRows if empty.
func (s *SelectBuilder) One(ctx context.Context, dest any) error {
	rows, err := s.Rows(ctx)
	if err != nil {
		return err
	}
	return scanOne(rows, dest)
}

// Count wraps the current SELECT in `SELECT count() FROM (... )`.
func (s *SelectBuilder) Count(ctx context.Context) (int64, error) {
	inner, args := s.ToSQL()
	sql := "SELECT count() FROM (" + inner + ") AS _drops_count"
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	if !rows.Next() {
		return 0, rows.Err()
	}
	var n int64
	if err := rows.Scan(&n); err != nil {
		return 0, err
	}
	return n, rows.Err()
}

// writeAnd writes predicates joined by AND, no outer parens.
func writeAnd(b *drops.Builder, preds []drops.Expression) {
	for i, p := range preds {
		if i > 0 {
			b.WriteString(" AND ")
		}
		b.Append(p)
	}
}
