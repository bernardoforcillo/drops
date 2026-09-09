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
	ctxJoins [][]drops.Expression

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

// UnscopedDefaults drops the table's declaration-time filters and keeps
// its request-time ones — see filterScope.keepCtx. It is what the typed
// entity surface means by Unscoped, and it is not what the raw builder
// means: here the blunt instrument stays blunt.
func (s *SelectBuilder) UnscopedDefaults() *SelectBuilder {
	s.scope.unscoped, s.scope.keepCtx = true, true
	return s
}

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
	// Through hoistCTEs, so a CTE declared on an operand reaches the
	// single WITH clause a set operation is allowed. Rendering only
	// s.ctes left the operand referring to a name the statement never
	// declared — and hoistCTEs, which already existed, was called by
	// the name check and by nothing that renders.
	ctes, recursive := s.hoistCTEs()
	writeCTEs(b, ctes, recursive)
	s.writeCore(b)
	for _, op := range s.setOps {
		b.WriteByte(' ')
		b.WriteString(op.kind)
		b.WriteByte(' ')
		op.right.writeSetOperand(b)
	}
}

// writeSetOperand renders one operand of a set operation.
//
// writeCore alone was wrong for two shapes and silently so.
// A.Union(B.Union(C)) sent "A UNION B" — C simply missing from the
// result, no error anywhere — because writeCore renders no set
// operations of its own. And an operand carrying LIMIT rendered it
// bare, where it binds to the whole set operation rather than to the
// operand: "A UNION B LIMIT 10" caps the union, which is a different
// query from the one that caps B.
//
// Parentheses are written only when there is something to contain, so
// the ordinary two-operand union renders exactly as it always did.
func (s *SelectBuilder) writeSetOperand(b *drops.Builder) {
	if len(s.setOps) == 0 && s.limit == nil && s.offset == nil {
		s.writeCore(b)
		return
	}
	b.WriteByte('(')
	s.writeCore(b)
	for _, op := range s.setOps {
		b.WriteByte(' ')
		b.WriteString(op.kind)
		b.WriteByte(' ')
		op.right.writeSetOperand(b)
	}
	b.WriteByte(')')
}

// ErrFullJoinScoped is returned when a FULL JOIN would have to carry a
// table's context filters, and there is nowhere correct to put them.
//
// A FULL JOIN preserves both sides. Put the predicate in the ON clause
// and the joined table's unmatched rows come through unfiltered — the
// rows the guard exists to hide. Put it in the WHERE clause and the
// FROM table's unmatched rows are deleted from the result — the rows
// the FULL JOIN exists to keep. There is no third place, so the
// statement is refused rather than rendered one of the two wrong ways.
//
// The ways through are both explicit: join a subquery that is already
// filtered, or say Unscoped and write the predicates at the query,
// where a reviewer reads them next to the join.
var ErrFullJoinScoped = errors.New("drops/pg: a FULL JOIN has nowhere to put a table's context filters")

// fromTables is the FROM side of the statement: the table From named,
// and every table comma-joined to it through FromExpr.
//
// A comma join is an inner join written another way, so a table there
// carries its predicates exactly as the FROM table does — and takes the
// same placement, since under a RIGHT JOIN the whole comma-joined
// product is the preserved side. A FromExpr holding anything else — a
// subquery, a CTE reference — is a statement or an opaque expression
// and is reached by the resolver rather than by this.
func (s *SelectBuilder) fromTables() []*Table {
	out := make([]*Table, 0, len(s.fromExprs)+1)
	if s.from != nil {
		out = append(out, s.from)
	}
	for _, e := range s.fromExprs {
		if t, ok := e.(*Table); ok {
			out = append(out, t)
		}
	}
	return out
}

// checkFullJoins refuses the shape ErrFullJoinScoped describes. Only
// context filters count: a default filter is rendered by WriteSQL and
// has been landing in the WHERE clause since long before this, so
// changing where it goes belongs to the round that can verify it.
func (s *SelectBuilder) checkFullJoins() error {
	for _, j := range s.joins {
		if j.kind != fullJoin {
			continue
		}
		for _, t := range append(s.fromTables(), j.table) {
			if !t.hasContextFilters() {
				continue
			}
			return fmt.Errorf("%w: %q carries them; join a pre-filtered subquery, or say Unscoped and write the predicate at the query",
				ErrFullJoinScoped, t.relRef())
		}
	}
	return nil
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
	// The named filters this statement bypasses, for the length of
	// this resolution: a nested statement installs its own at the top
	// of its own resolveCtx, so IgnoreFilters reaches no further than
	// the statement that said it.
	ctx = withIgnoredFilters(ctx, s.scope)

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
	// see [SelectBuilder.Unscoped], which is the blunt instrument. It
	// is also the documented way past the FULL JOIN refusal, which is
	// why that check lives inside this branch.
	if !s.scope.dropsContextFilters() {
		if err := s.checkFullJoins(); err != nil {
			return nil, err
		}
		tables := s.fromTables()
		var ctxFrom []drops.Expression
		for _, t := range tables {
			// Every table on the FROM side, in the order the statement
			// names them: From's own and each one comma-joined to it.
			preds, err := t.resolveContextFilters(ctx)
			if err != nil {
				return nil, err
			}
			ctxFrom = append(ctxFrom, preds...)
		}
		if len(ctxFrom) > 0 {
			cp.ctxFrom, changed = ctxFrom, true
		}
		ctxJoins, any, err := s.resolveJoins(ctx)
		if err != nil {
			return nil, err
		}
		for _, j := range s.joins {
			tables = append(tables, j.table)
		}
		if any {
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

// resolveJoins builds the context filters of every joined table, kept
// one slice per join rather than flattened.
//
// The shape is the point: where a joined table's predicates may be
// written depends on the kind of ITS join — a LEFT JOIN's must go in
// that join's ON clause, because in the WHERE clause they would delete
// the very unmatched rows the LEFT JOIN exists to keep. Flattened, the
// renderer no longer knows which join each predicate came from. See
// filterPlacement.
func (s *SelectBuilder) resolveJoins(ctx context.Context) ([][]drops.Expression, bool, error) {
	if len(s.joins) == 0 {
		return nil, false, nil
	}
	out := make([][]drops.Expression, len(s.joins))
	any := false
	for i, j := range s.joins {
		preds, err := j.table.resolveContextFilters(ctx)
		if err != nil {
			return nil, false, err
		}
		out[i] = preds
		any = any || len(preds) > 0
	}
	return out, any, nil
}

// filterPlacement decides where each table's automatic predicates are
// written, and it is a correctness question rather than a stylistic
// one.
//
// An outer join preserves rows that did not match. A predicate on the
// preserved side, written in the WHERE clause, is evaluated after the
// join has NULL-extended those rows — so it is false for exactly the
// rows the outer join exists to keep, and the join silently becomes an
// inner one. The predicate has to move into the ON clause, where it
// filters the joined relation before the preservation happens.
//
// So: a LEFT-joined table's predicates go in that join's ON, since the
// LEFT JOIN preserves the FROM side and its own rows are the ones a
// WHERE would delete. The FROM table's go in the first RIGHT JOIN's ON
// for the mirror-image reason, because from there on it is the FROM
// side that is preserved. Everything else stays in the WHERE clause,
// which is where all of it has always been.
//
// A table joined at position i takes its placement from its own join
// kind alone, and nothing looks at the kinds after it — a LEFT-joined
// table standing before a RIGHT JOIN keeps its predicates in the WHERE
// clause, where the RIGHT JOIN's NULL-extension then falsifies them.
// That LOSES rows rather than leaking them, and closing it is a change
// to where a guard lands, which belongs to a round that can verify it
// in its own right. See the package doc.
func (s *SelectBuilder) filterPlacement() (fromOn int) {
	for i, j := range s.joins {
		if j.kind == rightJoin {
			return i
		}
	}
	return -1
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
	// Where each table's automatic predicates go — see filterPlacement.
	fromOn := s.filterPlacement()
	var fromDefaults []drops.Expression
	for _, t := range s.fromTables() {
		fromDefaults = append(fromDefaults, s.scope.filtersOf(t, s.defaults)...)
	}
	fromPreds := append(append([]drops.Expression(nil), fromDefaults...), s.ctxFrom...)
	var defaults, ctxPreds []drops.Expression
	if fromOn < 0 {
		defaults = append(defaults, fromDefaults...)
		ctxPreds = append(ctxPreds, s.ctxFrom...)
	}
	for i, j := range s.joins {
		b.WriteByte(' ')
		b.WriteString(string(j.kind))
		b.WriteByte(' ')
		j.table.writeFrom(b)
		b.WriteString(" ON ")
		var onExtra []drops.Expression
		if i == fromOn {
			onExtra = append(onExtra, fromPreds...)
		}
		joinPredsOf := s.scope.filtersOf(j.table, s.defaults)
		if i < len(s.ctxJoins) {
			joinPredsOf = append(joinPredsOf, s.ctxJoins[i]...)
		}
		if j.kind == leftJoin {
			onExtra = append(onExtra, joinPredsOf...)
		} else {
			defaults = append(defaults, s.scope.filtersOf(j.table, s.defaults)...)
			if i < len(s.ctxJoins) {
				ctxPreds = append(ctxPreds, s.ctxJoins[i]...)
			}
		}
		// A nil condition is the empty conjunction, rendered where the
		// grammar insists on a predicate: writing nothing produced SQL
		// no server would parse. When the join carries predicates of
		// its own, they ARE the condition — a leading TRUE would be
		// noise in the one place a reviewer reads closely.
		switch {
		case j.on == nil && len(onExtra) == 0:
			b.Append(orTrue(nil))
		case j.on == nil:
			b.Append(And(onExtra...))
		case len(onExtra) == 0:
			b.Append(j.on)
		default:
			b.Append(And(append([]drops.Expression{j.on}, onExtra...)...))
		}
	}
	// Defaults first, then what the caller asked for, then the
	// request-time predicates: the order the WHERE clause has always
	// read in, scope first.
	wheres := make([]drops.Expression, 0, len(defaults)+len(s.wheres)+len(ctxPreds))
	wheres = append(wheres, defaults...)
	wheres = append(wheres, s.wheres...)
	wheres = append(wheres, ctxPreds...)
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
//
// Through ToSQLCtx, not ToSQL: what an executor sends is the statement
// the ctx names. Rendering here and resolving nowhere is what made the
// context filters a thing the caller had to remember — every one of
// them was in the builder and none of them was in the statement.
func (s *SelectBuilder) Rows(ctx context.Context) (drops.Rows, error) {
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
	// A node holding the statement rather than a closure rendering it:
	// wrapped in ExprFunc, the SELECT was invisible to resolveExpr, so
	// a scoped table read through AsSubquery rendered with none of its
	// context filters — the FROM clause of a report is exactly where
	// that goes unnoticed. See opExpr.
	return &opExpr{parts: []string{"(", ")"}, operands: []drops.Expression{s}, alias: alias}
}
