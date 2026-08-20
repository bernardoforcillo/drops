package pg

import (
	"context"

	"github.com/bernardoforcillo/drops"
)

// SelectBuilder composes a SELECT statement.
type SelectBuilder struct {
	db           *DB
	columns      []drops.Expression
	from         *Table
	fromExprs    []drops.Expression // arbitrary FROM sources (subqueries, CTE refs)
	joins        []joinClause
	wheres       []drops.Expression
	groupBys     []drops.Expression
	havings      []drops.Expression
	orderBys     []drops.Expression
	limit        *int64
	offset       *int64
	distinct     bool
	distinctOn   []drops.Expression
	forUpdate    bool
	ctes         []*CTE
	recursiveCTE bool
	setOps       []setOp // UNION / INTERSECT / EXCEPT continuations
	unscoped     bool
	err          error // deferred error (e.g. cursor decode failure) surfaced at Rows()
}

type setOp struct {
	kind  string // "UNION", "UNION ALL", "INTERSECT", "INTERSECT ALL", "EXCEPT", "EXCEPT ALL"
	right *SelectBuilder
}

type joinKind string

const (
	innerJoin joinKind = "INNER JOIN"
	leftJoin  joinKind = "LEFT JOIN"
	rightJoin joinKind = "RIGHT JOIN"
	fullJoin  joinKind = "FULL JOIN"
)

type joinClause struct {
	kind  joinKind
	table *Table
	on    drops.Expression
}

// From sets the FROM clause. Required before execution.
func (s *SelectBuilder) From(t *Table) *SelectBuilder { s.from = t; return s }

// FromExpr appends an arbitrary FROM source — a subquery, CTE
// reference, set-returning function, etc. Multiple FROMs are
// comma-joined (i.e. cross-joined).
func (s *SelectBuilder) FromExpr(e drops.Expression) *SelectBuilder {
	s.fromExprs = append(s.fromExprs, e)
	return s
}

// Distinct toggles SELECT DISTINCT.
func (s *SelectBuilder) Distinct() *SelectBuilder { s.distinct = true; return s }

// DistinctOn renders SELECT DISTINCT ON (exprs...). Mutually exclusive
// with Distinct().
func (s *SelectBuilder) DistinctOn(exprs ...drops.Expression) *SelectBuilder {
	s.distinctOn = append(s.distinctOn, exprs...)
	return s
}

// ForUpdate appends FOR UPDATE row locking.
func (s *SelectBuilder) ForUpdate() *SelectBuilder { s.forUpdate = true; return s }

// Unscoped opts out of the FROM table's automatic predicates for this
// SELECT — both its DefaultFilter list and its ContextFilter list. Use
// to bypass a soft-delete guard, and to run the administrative query
// that has to see every tenant's rows.
//
// It clears the context filters too because a half-scoped statement is
// the worse of the two answers: a caller who reaches for Unscoped to
// read soft-deleted rows and instead gets ErrTenantMissing has learned
// nothing about the row they were after, and one who gets the tenant
// predicate they did not ask for silently reads a subset. Scope such a
// query explicitly — Unscoped().Where(pg.Eq(TenantID, id)) — where the
// intent is on the page.
func (s *SelectBuilder) Unscoped() *SelectBuilder { s.unscoped = true; return s }

// Join appends an INNER JOIN.
func (s *SelectBuilder) Join(t *Table, on drops.Expression) *SelectBuilder {
	s.joins = append(s.joins, joinClause{innerJoin, t, on})
	return s
}

// LeftJoin appends a LEFT JOIN.
func (s *SelectBuilder) LeftJoin(t *Table, on drops.Expression) *SelectBuilder {
	s.joins = append(s.joins, joinClause{leftJoin, t, on})
	return s
}

// RightJoin appends a RIGHT JOIN.
func (s *SelectBuilder) RightJoin(t *Table, on drops.Expression) *SelectBuilder {
	s.joins = append(s.joins, joinClause{rightJoin, t, on})
	return s
}

// FullJoin appends a FULL OUTER JOIN.
func (s *SelectBuilder) FullJoin(t *Table, on drops.Expression) *SelectBuilder {
	s.joins = append(s.joins, joinClause{fullJoin, t, on})
	return s
}

// Where appends predicates joined by AND.
func (s *SelectBuilder) Where(preds ...drops.Expression) *SelectBuilder {
	s.wheres = append(s.wheres, preds...)
	return s
}

// GroupBy appends GROUP BY expressions.
func (s *SelectBuilder) GroupBy(exprs ...drops.Expression) *SelectBuilder {
	s.groupBys = append(s.groupBys, exprs...)
	return s
}

// Having appends predicates to the HAVING clause (joined by AND).
func (s *SelectBuilder) Having(preds ...drops.Expression) *SelectBuilder {
	s.havings = append(s.havings, preds...)
	return s
}

// OrderBy appends ORDER BY expressions. Use Column.Asc / Column.Desc for
// direction.
func (s *SelectBuilder) OrderBy(exprs ...drops.Expression) *SelectBuilder {
	s.orderBys = append(s.orderBys, exprs...)
	return s
}

// Limit sets the LIMIT.
func (s *SelectBuilder) Limit(n int64) *SelectBuilder { s.limit = &n; return s }

// applyLimitCap installs cap as the LIMIT unless an explicit Limit
// has already been set to something tighter. Used by Entity.Budget
// to bound result sets without overriding the caller's narrower
// LIMIT.
func (s *SelectBuilder) applyLimitCap(capt int64) {
	if s.limit == nil || *s.limit > capt {
		v := capt
		s.limit = &v
	}
}

// Offset sets the OFFSET.
func (s *SelectBuilder) Offset(n int64) *SelectBuilder { s.offset = &n; return s }

// Union appends UNION <select>. Multiple set operations are chainable.
func (s *SelectBuilder) Union(other *SelectBuilder) *SelectBuilder {
	s.setOps = append(s.setOps, setOp{kind: "UNION", right: other})
	return s
}

// UnionAll appends UNION ALL <select>.
func (s *SelectBuilder) UnionAll(other *SelectBuilder) *SelectBuilder {
	s.setOps = append(s.setOps, setOp{kind: "UNION ALL", right: other})
	return s
}

// Intersect appends INTERSECT <select>.
func (s *SelectBuilder) Intersect(other *SelectBuilder) *SelectBuilder {
	s.setOps = append(s.setOps, setOp{kind: "INTERSECT", right: other})
	return s
}

// IntersectAll appends INTERSECT ALL <select>.
func (s *SelectBuilder) IntersectAll(other *SelectBuilder) *SelectBuilder {
	s.setOps = append(s.setOps, setOp{kind: "INTERSECT ALL", right: other})
	return s
}

// Except appends EXCEPT <select>.
func (s *SelectBuilder) Except(other *SelectBuilder) *SelectBuilder {
	s.setOps = append(s.setOps, setOp{kind: "EXCEPT", right: other})
	return s
}

// ExceptAll appends EXCEPT ALL <select>.
func (s *SelectBuilder) ExceptAll(other *SelectBuilder) *SelectBuilder {
	s.setOps = append(s.setOps, setOp{kind: "EXCEPT ALL", right: other})
	return s
}

// WriteSQL renders the SELECT into a Builder. Wrapped in parentheses so
// the same builder can be embedded as a subquery.
func (s *SelectBuilder) WriteSQL(b *drops.Builder) {
	writeCTEs(b, s.ctes, s.recursiveCTE)
	s.writeCore(b)
	for _, op := range s.setOps {
		b.WriteByte(' ')
		b.WriteString(op.kind)
		b.WriteByte(' ')
		op.right.writeCore(b)
	}
}

// writeCore renders the SELECT body without any WITH prefix or set-op
// continuation. Set operations call this on each operand.
func (s *SelectBuilder) writeCore(b *drops.Builder) {
	b.WriteString("SELECT ")
	if len(s.distinctOn) > 0 {
		b.WriteString("DISTINCT ON (")
		b.AppendList(", ", s.distinctOn)
		b.WriteString(") ")
	} else if s.distinct {
		b.WriteString("DISTINCT ")
	}
	if len(s.columns) == 0 {
		b.WriteByte('*')
	} else {
		b.AppendList(", ", s.columns)
	}
	if s.from != nil || len(s.fromExprs) > 0 {
		b.WriteString(" FROM ")
		first := true
		if s.from != nil {
			s.from.writeFrom(b)
			first = false
		}
		for _, e := range s.fromExprs {
			if !first {
				b.WriteString(", ")
			}
			b.Append(e)
			first = false
		}
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
		b.WriteString(" OFFSET ")
		b.AddArg(*s.offset)
	}
	if s.forUpdate {
		b.WriteString(" FOR UPDATE")
	}
}

// ToSQL renders the statement to a SQL string and arg list.
//
// It renders what the builder knows without a context, which since
// [Table.ContextFilter] shipped is no longer necessarily the whole
// statement: a table's context filters — the tenant axis installed by
// ScopeByTenant, an authz guard — are resolved by the executors and do
// not appear here. Use [SelectBuilder.ToSQLCtx] to see the statement a
// given ctx would send; that is the one to assert on in a test, and the
// one to log. ToSQL remains the right call where there is no request to
// speak of and never will be: rendering a CREATE VIEW body, or
// embedding the SELECT as a subquery in a statement some other executor
// will run.
func (s *SelectBuilder) ToSQL() (sql string, args []any) {
	b := drops.NewBuilder()
	s.WriteSQL(b)
	return b.SQL()
}

// ToSQLCtx renders the complete statement for ctx, with every context
// filter on the FROM table resolved into the WHERE clause. A filter
// that refuses — [TenantFilter] with no tenant on ctx — returns its
// error and no SQL, because the alternative to refusing is an
// unfiltered query.
func (s *SelectBuilder) ToSQLCtx(ctx context.Context) (sql string, args []any, err error) {
	r, err := s.resolveCtx(ctx)
	if err != nil {
		return "", nil, err
	}
	sql, args = r.ToSQL()
	return sql, args, nil
}

// resolveCtx returns the builder to render for one execution: this one
// when the FROM table has no context filters, otherwise a shallow copy
// whose WHERE list carries the resolved predicates.
//
// The copy is what keeps a builder reusable. Appending the resolved
// predicates to s.wheres would make the second execution of the same
// builder carry two tenant predicates, the third three — with the same
// value bound repeatedly, so the rows come back right and nothing fails
// until an argument limit or a query log makes it visible.
//
// A set operation's operands each carry their own FROM table, so they
// are resolved as well; a SELECT reached as a subquery expression is
// not, since it is rendered by WriteSQL from inside another statement
// with no ctx in hand. Resolve such a builder yourself — the
// per-parent-limit rewrite in find.go does exactly that — before
// embedding it.
func (s *SelectBuilder) resolveCtx(ctx context.Context) (*SelectBuilder, error) {
	var preds []drops.Expression
	if !s.unscoped && s.from.hasContextFilters() {
		var err error
		preds, err = s.from.resolveContextFilters(ctx)
		if err != nil {
			return nil, err
		}
	}
	var ops []setOp
	for i, op := range s.setOps {
		right, err := op.right.resolveCtx(ctx)
		if err != nil {
			return nil, err
		}
		if right == op.right {
			continue
		}
		if ops == nil {
			ops = append([]setOp(nil), s.setOps...)
		}
		ops[i].right = right
	}
	if len(preds) == 0 && ops == nil {
		return s, nil
	}
	cp := *s
	if len(preds) > 0 {
		cp.wheres = append(append([]drops.Expression(nil), s.wheres...), preds...)
	}
	if ops != nil {
		cp.setOps = ops
	}
	return &cp, nil
}

// Rows executes the SELECT and returns the raw cursor for manual scanning.
func (s *SelectBuilder) Rows(ctx context.Context) (drops.Rows, error) {
	if s.err != nil {
		return nil, s.err
	}
	sql, args, err := s.ToSQLCtx(ctx)
	if err != nil {
		return nil, err
	}
	return s.db.Query(ctx, sql, args...)
}

// All executes the SELECT and scans every row into dest, which must be a
// pointer to a slice of structs (or pointer-to-structs).
func (s *SelectBuilder) All(ctx context.Context, dest any) error {
	rows, err := s.Rows(ctx)
	if err != nil {
		return err
	}
	return scanAll(rows, dest)
}

// One executes the SELECT and scans the first row into dest. Returns
// ErrNoRows if no row is produced.
func (s *SelectBuilder) One(ctx context.Context, dest any) error {
	rows, err := s.Rows(ctx)
	if err != nil {
		return err
	}
	return scanOne(rows, dest)
}

// Count returns the number of rows the current SELECT would produce,
// computed as SELECT count(*) FROM (<original>) AS _drops_count. The
// original ORDER BY / LIMIT / OFFSET are kept inside the subquery so
// LIMIT-aware page counts work correctly.
//
// For un-paginated counts on simple SELECTs, this is the natural and
// safe shape — PostgreSQL will optimise the inner query as needed.
func (s *SelectBuilder) Count(ctx context.Context) (int64, error) {
	inner, args, err := s.ToSQLCtx(ctx)
	if err != nil {
		return 0, err
	}
	sql := "SELECT count(*) FROM (" + inner + ") AS _drops_count"
	rows, qerr := s.db.Query(ctx, sql, args...)
	if qerr != nil {
		return 0, qerr
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, err
		}
		return 0, nil
	}
	var n int64
	if err := rows.Scan(&n); err != nil {
		return 0, err
	}
	return n, rows.Err()
}

// writeAnd writes a list of predicates joined by AND, without the outer
// parentheses Or/And would emit when used as a sub-expression.
func writeAnd(b *drops.Builder, preds []drops.Expression) {
	for i, p := range preds {
		if i > 0 {
			b.WriteString(" AND ")
		}
		b.Append(p)
	}
}

// AsSubquery returns a parenthesised, aliased form of the SELECT for use
// as a subquery in another statement.
//
// The result is an Expression, so it renders through WriteSQL and
// carries the FROM table's DefaultFilters but not its context filters —
// nothing hands a ctx to an expression. Resolve the inner builder
// first when it selects from a table with context filters.
func (s *SelectBuilder) AsSubquery(alias string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		s.WriteSQL(b)
		b.WriteString(") AS ")
		b.WriteIdent(alias)
	})
}
