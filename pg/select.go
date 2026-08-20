package pg

import (
	"context"
	"errors"
	"fmt"

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

	// ctxOn carries the joined table's resolved context filters for a
	// join kind whose filters belong in the ON clause. It is set by
	// resolveJoins on a per-execution copy and read by writeCore, which
	// is what keeps the ON clause composed exactly once — a resolver
	// that AND-ed its own predicates in would nest a second layer of
	// parentheses around whatever writeCore then added.
	ctxOn []drops.Expression
}

// filterPlacement says where a joined table's automatic predicates —
// its DefaultFilters and its resolved ContextFilters — have to be
// written for the join to keep meaning what it says.
//
// The choice is not cosmetic and it is not the same for every kind,
// which is why it is a function of the kind rather than one branch
// somewhere in writeCore.
//
// An INNER JOIN emits only matched pairs, so a predicate on the joined
// table selects the same rows in the ON clause and in the WHERE clause.
// The WHERE clause is where it goes, next to the FROM table's own, so
// one reader sees every restriction the statement carries in one place.
//
// A LEFT JOIN preserves the FROM table. Its joined side is the nullable
// one, and a WHERE predicate on a nullable side is false for every
// unmatched row — so a tenant guard written there deletes exactly the
// parent rows the LEFT JOIN exists to keep, turning it into an INNER
// JOIN and dropping parents that have no child. In the ON clause the
// same predicate restricts which children match and leaves the parents
// alone, which is what was meant.
//
// A RIGHT JOIN inverts that: the joined table is the preserved side, so
// its predicate belongs in the WHERE clause again — and belongs there
// emphatically, because in the ON clause an unmatched row of the joined
// table survives the join with a NULL left side, predicate and all, and
// a tenant guard in the ON clause of a RIGHT JOIN is therefore a leak
// rather than an over-filter.
//
// A FULL JOIN preserves both sides, so both readings above apply at
// once and neither placement is right: in the ON clause the unmatched
// rows of the joined table survive unfiltered (the RIGHT JOIN leak),
// and in the WHERE clause the unmatched rows of the FROM table are
// dropped (the LEFT JOIN degeneration). There is no third place, so a
// FULL JOIN carries nothing automatically — see [SelectBuilder.FullJoin]
// for what that means for each kind of filter.
func (k joinKind) filterPlacement() joinPlacement {
	switch k {
	case leftJoin:
		return placeInOn
	case fullJoin:
		return placeNowhere
	default:
		return placeInWhere
	}
}

type joinPlacement int

const (
	placeInWhere joinPlacement = iota
	placeInOn
	placeNowhere
)

// ErrFullJoinScoped is returned when a SELECT full-joins a table that
// carries context filters.
//
// A FULL JOIN has nowhere correct to put them — see
// joinKind.filterPlacement — and a tenant axis or an authz guard names
// a row-visibility boundary, so the statement is refused rather than
// sent with the predicate in a place that either leaks the joined
// table's unmatched rows or drops the FROM table's. Join a pre-filtered
// subquery (a CTE, or FromExpr over a scoped SELECT), or say Unscoped()
// and write the predicates at the query where a reviewer can see them.
var ErrFullJoinScoped = errors.New("drops/pg: a FULL JOIN cannot carry the joined table's context filters")

// andWith returns on AND every predicate in extra, for an ON clause
// that has to carry a joined table's automatic filters. A nil on — a
// join written with no condition at all — yields the filters alone
// rather than a dangling AND.
func andWith(on drops.Expression, extra []drops.Expression) drops.Expression {
	if len(extra) == 0 {
		return on
	}
	if on == nil {
		return And(extra...)
	}
	return And(append([]drops.Expression{on}, extra...)...)
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

// Unscoped opts out of automatic predicates for this SELECT — the
// DefaultFilter and ContextFilter lists of the FROM table and of every
// joined table alike. Use to bypass a soft-delete guard, and to run the
// administrative query that has to see every tenant's rows.
//
// It is statement-wide rather than per table on purpose: a caller who
// says Unscoped is describing this query's authority, and a flag that
// unscoped the FROM table while a joined one kept its tenant axis would
// answer with a silently narrowed slice of the rows that were asked
// for. A CTE body is a statement of its own and keeps its own scoping —
// which is also how to unscope one relation and no other.
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
//
// t is joined as a scoped table, not as a bare relation name: its
// DefaultFilters and its ContextFilters are carried by the statement
// like the FROM table's own, in the WHERE clause. Without that, joining
// a tenant-scoped table read every tenant's rows through it, and
// whether a foreign row actually came back depended on the join key
// happening to be unique per tenant — which is the assumption
// ScopeByTenant exists so that nobody has to make. Say Unscoped() to
// opt the whole statement out.
func (s *SelectBuilder) Join(t *Table, on drops.Expression) *SelectBuilder {
	s.joins = append(s.joins, joinClause{kind: innerJoin, table: t, on: on})
	return s
}

// LeftJoin appends a LEFT JOIN.
//
// t's automatic predicates go into the ON clause rather than the WHERE
// clause, and the difference is the whole join: a predicate on the
// nullable side of an outer join is false for every unmatched row, so a
// tenant guard in the WHERE clause would drop exactly the FROM-table
// rows a LEFT JOIN exists to keep and quietly turn it into an INNER
// JOIN. In the ON clause it restricts which rows of t match and leaves
// the parents alone. See joinKind.filterPlacement.
func (s *SelectBuilder) LeftJoin(t *Table, on drops.Expression) *SelectBuilder {
	s.joins = append(s.joins, joinClause{kind: leftJoin, table: t, on: on})
	return s
}

// RightJoin appends a RIGHT JOIN.
//
// t is the preserved side here, so its automatic predicates go into the
// WHERE clause — in the ON clause an unmatched row of t survives with a
// NULL left side and the predicate never removes it, which for a tenant
// axis is a leak rather than an over-filter.
//
// The FROM table is the nullable side of this join and its own
// predicates stay in the WHERE clause, so a scoped FROM table narrows a
// RIGHT JOIN to the rows that matched. That is a shortfall of rows and
// never rows that should not have been visible; the alternative —
// moving the FROM table's guard into the ON clause — is the leak.
// Write such a query as a LEFT JOIN with the tables swapped.
func (s *SelectBuilder) RightJoin(t *Table, on drops.Expression) *SelectBuilder {
	s.joins = append(s.joins, joinClause{kind: rightJoin, table: t, on: on})
	return s
}

// FullJoin appends a FULL OUTER JOIN.
//
// This is the one join drops does not scope for you, because a FULL
// JOIN preserves both sides and so has nowhere correct to put the
// joined table's predicates: the ON clause lets t's unmatched rows
// through unfiltered, the WHERE clause drops the FROM table's unmatched
// rows. See joinKind.filterPlacement for the derivation.
//
// The two kinds of filter are therefore treated differently, deliberately:
//
//   - Context filters — the tenant axis, an authz guard — describe which
//     rows a request may see at all, so the statement is refused with
//     [ErrFullJoinScoped] rather than sent with the predicate in a place
//     that is wrong in one direction or the other.
//   - DefaultFilters — a soft-delete guard and the like — are a default
//     scope rather than a boundary, and refusing would leave no way to
//     full-join a soft-deleted table at all, since Unscoped() is
//     statement-wide and would take the FROM table's scoping with it. So
//     they are left off this one join shape, which means a FULL JOIN can
//     reach soft-deleted rows of t. That is the documented gap.
//
// To full-join a scoped table, join it pre-filtered: put the scoped
// SELECT in a CTE (its body is resolved for the ctx, see
// [SelectBuilder.With]) and full-join that, or hand-write the exact
// form, which is the filter in the ON clause plus "OR <t's key> IS NULL"
// in the WHERE clause to let the FROM table's unmatched rows back
// through.
func (s *SelectBuilder) FullJoin(t *Table, on drops.Expression) *SelectBuilder {
	s.joins = append(s.joins, joinClause{kind: fullJoin, table: t, on: on})
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
	// autoWheres are the automatic predicates that belong in the WHERE
	// clause. The joined tables' are gathered here, as the joins
	// render, so that the placement decision lives in one place with
	// its ON-clause half.
	var autoWheres []drops.Expression
	for _, j := range s.joins {
		b.WriteByte(' ')
		b.WriteString(string(j.kind))
		b.WriteByte(' ')
		j.table.writeFrom(b)
		b.WriteString(" ON ")
		on := j.on
		if !s.unscoped {
			switch dfs := j.table.resolveDefaultFilters(); j.kind.filterPlacement() {
			case placeInOn:
				on = andWith(on, append(append([]drops.Expression(nil), dfs...), j.ctxOn...))
			case placeInWhere:
				autoWheres = append(autoWheres, dfs...)
			}
		}
		b.Append(on)
	}
	// The FROM table's go in front of them, and the whole lot in front
	// of the caller's own predicates — which resolveCtx has already
	// followed with the ones that needed a ctx. So a statement reads
	// scoping first, intent second, and a query log shows at a glance
	// whether a query was scoped at all.
	wheres := s.wheres
	if !s.unscoped && s.from.hasDefaultFilters() {
		autoWheres = append(append([]drops.Expression(nil), s.from.resolveDefaultFilters()...), autoWheres...)
	}
	if len(autoWheres) > 0 {
		wheres = append(autoWheres, wheres...)
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
// Every table the statement names is resolved, not just the FROM one,
// because a context filter is a property of the table and the promise
// in tenant.go is about statements rather than about clauses. A set
// operation's operands each carry their own FROM table. A joined table
// carries its own filters into the clause its join kind allows — see
// joinKind.filterPlacement. A CTE whose body is a *SelectBuilder is
// resolved as the statement it is, so that
// WITH recent AS (SELECT ... FROM posts) is scoped the way a bare
// SELECT from posts would be; a CTE built from a raw drops.Expression
// cannot be, and stays the caller's to scope.
//
// What is still not resolved is a SELECT reached as a subquery
// expression, since it is rendered by WriteSQL from inside another
// statement with no ctx in hand. Resolve such a builder yourself — the
// per-parent-limit rewrite in find.go does exactly that — before
// embedding it. [SelectBuilder.AsSubquery] says so at the call site.
func (s *SelectBuilder) resolveCtx(ctx context.Context) (*SelectBuilder, error) {
	var preds []drops.Expression
	if !s.unscoped && s.from.hasContextFilters() {
		var err error
		preds, err = s.from.resolveContextFilters(ctx)
		if err != nil {
			return nil, err
		}
	}
	joins, joinPreds, err := s.resolveJoins(ctx)
	if err != nil {
		return nil, err
	}
	preds = append(preds, joinPreds...)
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
	ctes, err := resolveCTEs(ctx, s.ctes)
	if err != nil {
		return nil, err
	}
	if len(preds) == 0 && ops == nil && ctes == nil && joins == nil {
		return s, nil
	}
	cp := *s
	if len(preds) > 0 {
		cp.wheres = append(append([]drops.Expression(nil), s.wheres...), preds...)
	}
	if ops != nil {
		cp.setOps = ops
	}
	if ctes != nil {
		cp.ctes = ctes
	}
	if joins != nil {
		cp.joins = joins
	}
	return &cp, nil
}

// resolveJoins resolves the context filters of every joined table.
//
// It returns two things because the answer lands in two clauses: a
// rebuilt join list when some ON clause had to grow (nil when none
// did), and the predicates bound for the WHERE clause. Which of the two
// a join contributes to is joinKind.filterPlacement's decision, and a
// FULL JOIN — where neither clause is correct — is refused here rather
// than answered wrongly.
//
// A self-join of a scoped table resolves its filters once per instance
// and so carries the predicate twice, once qualified per side. That is
// the conservative reading and the right one: each side is a separate
// relation in the statement and each has to be restricted.
func (s *SelectBuilder) resolveJoins(ctx context.Context) ([]joinClause, []drops.Expression, error) {
	if s.unscoped || len(s.joins) == 0 {
		return nil, nil, nil
	}
	var joins []joinClause
	var preds []drops.Expression
	for i, j := range s.joins {
		if !j.table.hasContextFilters() {
			continue
		}
		if j.kind.filterPlacement() == placeNowhere {
			return nil, nil, fmt.Errorf("drops/pg: %w: %q; join it pre-filtered, or say Unscoped",
				ErrFullJoinScoped, j.table.Name())
		}
		jp, err := j.table.resolveContextFilters(ctx)
		if err != nil {
			return nil, nil, err
		}
		if len(jp) == 0 {
			continue
		}
		if j.kind.filterPlacement() == placeInOn {
			if joins == nil {
				joins = append([]joinClause(nil), s.joins...)
			}
			joins[i].ctxOn = jp
			continue
		}
		preds = append(preds, jp...)
	}
	return joins, preds, nil
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
