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
	scope        filterScope
	err          error // deferred error (e.g. cursor decode failure) surfaced at Rows()

	// ctxFrom and ctxJoins are the context filters of the FROM table
	// and of each joined table, built for one execution by resolveCtx
	// and AND-ed in by writeCore. They are the resolver's own output —
	// resolveContextFilters walks the predicates before handing them
	// back — so resolveCtx does not read them again; see
	// resolverOwnedFields, and resolved for why walking them twice
	// would bind the tenant twice.
	ctxFrom  []drops.Expression
	ctxJoins []drops.Expression

	// defaults are the default filters of the tables this statement
	// names, resolved for one execution. Nil on the ToSQL path and
	// whenever no default filter held a statement, and then the
	// render-time list is used unchanged.
	defaults resolvedDefaults

	// resolved marks a builder resolveCtx has already produced. It is
	// what keeps resolution from happening twice on one statement:
	// resolving is not idempotent — a second pass asks every context
	// filter for a fresh predicate and AND-s it in beside the first,
	// so the tenant is bound twice and a filter that refuses would
	// refuse a statement that had already been answered.
	resolved bool
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

// Unscoped opts out of every global filter registered on the FROM
// table — named and anonymous alike. It is the blunt instrument: it
// cannot tell a soft-delete guard from a tenancy one, so a query that
// only wants deleted rows loses its isolation with them. Reach for it
// when you mean "no scoping at all"; otherwise name what you are
// stepping around with IgnoreFilters.
func (s *SelectBuilder) Unscoped() *SelectBuilder { s.scope.unscoped = true; return s }

// IgnoreFilters bypasses the named global filters on the FROM table
// and leaves every other one in place:
//
//	// soft-deleted rows too, still only this tenant's
//	db.Select().From(Posts).IgnoreFilters(pg.FilterSoftDelete)
//
// Names come from [Table.AddFilter]; drops' own are the FilterSoftDelete
// and FilterTenant constants. A name no filter on the table carries is
// ignored — the builder may not have its FROM yet, and a typo that
// leaves a filter standing returns too few rows rather than too many.
func (s *SelectBuilder) IgnoreFilters(names ...string) *SelectBuilder {
	s.scope.ignore(names...)
	return s
}

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

// Where appends predicates joined by AND. Nil predicates are ignored,
// so a caller can pass one that is only sometimes present without
// first collecting the non-nil ones into a slice.
func (s *SelectBuilder) Where(preds ...drops.Expression) *SelectBuilder {
	s.wheres = append(s.wheres, dropNilPreds(preds)...)
	return s
}

// GroupBy appends GROUP BY expressions.
func (s *SelectBuilder) GroupBy(exprs ...drops.Expression) *SelectBuilder {
	s.groupBys = append(s.groupBys, exprs...)
	return s
}

// Having appends predicates to the HAVING clause (joined by AND).
// Nil predicates are ignored, as in Where.
func (s *SelectBuilder) Having(preds ...drops.Expression) *SelectBuilder {
	s.havings = append(s.havings, dropNilPreds(preds)...)
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

// ToSQLCtx renders the statement ctx would send.
//
// A SELECT rendered through [SelectBuilder.ToSQL] carries the table's
// DefaultFilters and none of its ContextFilters, because a render has
// no ctx to build one from — so on a tenant-scoped table it is not the
// statement that would be sent. This is: the context filters of the
// FROM table and of every joined table are built, walked and AND-ed in,
// and every statement written inside the query — a subquery operand, a
// CTE body, a set-operation branch — is resolved on the same terms.
//
// A filter that cannot decide what the request may see refuses, and the
// refusal is returned instead of a statement: no SQL at all is safer
// than SQL missing the predicate that makes it safe.
func (s *SelectBuilder) ToSQLCtx(ctx context.Context) (sql string, args []any, err error) {
	r, err := s.resolveCtx(ctx)
	if err != nil {
		return "", nil, err
	}
	sql, args = r.ToSQL()
	return sql, args, nil
}

// resolveCtx returns the builder to render for one execution: the
// receiver when there was nothing to resolve, and a shallow copy
// carrying the resolved lists when there was.
//
// Handing back the receiver is not a micro-optimisation.
// resolveStatement reports "this differs from the original" by
// comparing pointers, and resolveExprs copies the whole list it is
// walking the moment any element reports a change — so a builder that
// always answered with a copy would tell every statement it is nested
// in that it had changed, on every execution.
//
// The deferred error is checked here rather than at the executors,
// because both paths into rendering go through this one: a cursor that
// failed to decode must not reach the server as the false predicate
// AfterCursor fails closed with, which matches nothing and reports
// nothing.
func (s *SelectBuilder) resolveCtx(ctx context.Context) (*SelectBuilder, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.resolved {
		return s, nil
	}
	cp := *s
	changed := false

	// Every list the renderer reads is walked, because a statement can
	// be written in any of them — the DISTINCT ON list is the one that
	// went out unscoped when this was a list of the obvious ones.
	if r, err := resolveExprs(ctx, s.columns); err != nil {
		return nil, err
	} else if r != nil {
		cp.columns, changed = r, true
	}
	if r, err := resolveExprs(ctx, s.distinctOn); err != nil {
		return nil, err
	} else if r != nil {
		cp.distinctOn, changed = r, true
	}
	if r, err := resolveExprs(ctx, s.fromExprs); err != nil {
		return nil, err
	} else if r != nil {
		cp.fromExprs, changed = r, true
	}
	if r, err := resolveExprs(ctx, s.wheres); err != nil {
		return nil, err
	} else if r != nil {
		cp.wheres, changed = r, true
	}
	if r, err := resolveExprs(ctx, s.groupBys); err != nil {
		return nil, err
	} else if r != nil {
		cp.groupBys, changed = r, true
	}
	if r, err := resolveExprs(ctx, s.havings); err != nil {
		return nil, err
	} else if r != nil {
		cp.havings, changed = r, true
	}
	if r, err := resolveExprs(ctx, s.orderBys); err != nil {
		return nil, err
	} else if r != nil {
		cp.orderBys, changed = r, true
	}
	if r, err := resolveCTEs(ctx, s.ctes); err != nil {
		return nil, err
	} else if r != nil {
		cp.ctes, changed = r, true
	}

	// A join's ON is a predicate like any other, and a statement in it
	// decides which rows the join admits.
	joins := s.joins
	for i, j := range s.joins {
		on, ch, err := resolveExpr(ctx, j.on)
		if err != nil {
			return nil, err
		}
		if !ch {
			continue
		}
		if &joins[0] == &s.joins[0] {
			joins = append([]joinClause(nil), s.joins...)
		}
		joins[i].on, changed = on, true
	}
	if changed {
		cp.joins = joins
	}

	// A set operation's right-hand side is a statement in its own
	// right: UNION with an unscoped branch reads every tenant's rows
	// into a result the left-hand side scoped.
	setOps := s.setOps
	for i, op := range s.setOps {
		r, err := op.right.resolveCtx(ctx)
		if err != nil {
			return nil, err
		}
		if r == op.right {
			continue
		}
		if &setOps[0] == &s.setOps[0] {
			setOps = append([]setOp(nil), s.setOps...)
		}
		setOps[i].right, changed = r, true
	}
	if changed {
		cp.setOps = setOps
	}

	// Unscoped is the statement saying it wants none of the table's
	// automatic predicates, and it means the request-time ones too —
	// see [SelectBuilder.Unscoped], which is the blunt instrument.
	if !s.scope.unscoped {
		tables := make([]*Table, 0, len(s.joins)+1)
		if s.from != nil {
			tables = append(tables, s.from)
		}
		ctxFrom, err := s.from.resolveContextFilters(ctx)
		if err != nil {
			return nil, err
		}
		if len(ctxFrom) > 0 {
			cp.ctxFrom, changed = ctxFrom, true
		}
		var ctxJoins []drops.Expression
		for _, j := range s.joins {
			tables = append(tables, j.table)
			preds, err := j.table.resolveContextFilters(ctx)
			if err != nil {
				return nil, err
			}
			ctxJoins = append(ctxJoins, preds...)
		}
		if len(ctxJoins) > 0 {
			cp.ctxJoins, changed = ctxJoins, true
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
		return s, nil
	}
	cp.resolved = true
	return &cp, nil
}

// resolveStatement implements [ctxResolvable]: it is resolveCtx behind
// the interface resolveExpr dispatches on, so a SELECT written as a CTE
// body or a subquery operand carries the same predicates a bare one
// would.
func (s *SelectBuilder) resolveStatement(ctx context.Context) (drops.Expression, bool, error) {
	r, err := s.resolveCtx(ctx)
	if err != nil {
		return nil, false, err
	}
	return r, r != s, nil
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
		// A nil condition is the empty conjunction, rendered where
		// the grammar insists on a predicate. Writing nothing here
		// produced SQL no server would parse.
		b.Append(orTrue(j.on))
	}
	wheres := s.scope.apply(s.from, s.wheres, s.defaults)
	if len(s.ctxFrom) > 0 || len(s.ctxJoins) > 0 {
		// Copied rather than appended to: wheres may be the builder's
		// own slice, and an append into its spare capacity would leave
		// this execution's tenant predicate in a statement the caller
		// holds and runs again.
		all := make([]drops.Expression, 0, len(wheres)+len(s.ctxFrom)+len(s.ctxJoins))
		all = append(all, wheres...)
		all = append(all, s.ctxFrom...)
		wheres = append(all, s.ctxJoins...)
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
func (s *SelectBuilder) ToSQL() (sql string, args []any) {
	b := drops.NewBuilder()
	s.WriteSQL(b)
	return b.SQL()
}

// Rows executes the SELECT and returns the raw cursor for manual scanning.
func (s *SelectBuilder) Rows(ctx context.Context) (drops.Rows, error) {
	if s.err != nil {
		return nil, s.err
	}
	sql, args := s.ToSQL()
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
	inner, args := s.ToSQL()
	sql := "SELECT count(*) FROM (" + inner + ") AS _drops_count"
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return 0, err
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
func (s *SelectBuilder) AsSubquery(alias string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		s.WriteSQL(b)
		b.WriteString(") AS ")
		b.WriteIdent(alias)
	})
}
