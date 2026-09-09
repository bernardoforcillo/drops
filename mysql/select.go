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
	scope    filterScope

	// fromExprs are arbitrary FROM sources — a CTE reference, a
	// derived table, a JSON_TABLE call — comma-joined after the
	// declared table. ctes / recursiveCTE carry the WITH prefix; see
	// cte.go.
	fromExprs    []drops.Expression
	ctes         []*CTE
	recursiveCTE bool

	// ctxFrom and ctxJoins are the context filters of the FROM tables
	// and of each joined table, built for one execution by resolveCtx.
	// They are held rather than resolved while rendering because
	// WriteSQL has no ctx to build them from — and kept one slice per
	// join rather than flattened, because where a joined table's
	// predicates may be written depends on the kind of ITS join. See
	// filterPlacement.
	ctxFrom  []drops.Expression
	ctxJoins [][]drops.Expression

	// defaults is what this execution resolved for the render-time
	// filters of the tables the statement names, and nil when none of
	// them held a statement.
	defaults resolvedDefaults

	// resolved marks the copy resolveCtx produced, so a statement
	// nested in another is not resolved twice.
	resolved bool
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

// FromExpr appends an arbitrary FROM source — a [CTE] reference, a
// derived table from (*SelectBuilder).AsSubquery, a [JSONTable] call.
// Multiple sources are comma-joined, which is to say cross-joined.
//
// MySQL requires every derived table to carry an alias, so pass one
// through whichever helper produced the expression. Leaving it off
// fails on both servers and says so differently: MySQL answers error
// 1248, "Every derived table must have its own alias", while MariaDB
// answers a bare syntax error, 1064, naming nothing in particular.
func (s *SelectBuilder) FromExpr(e drops.Expression) *SelectBuilder {
	s.fromExprs = append(s.fromExprs, e)
	return s
}

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

// Where appends predicates joined by AND. Nil predicates are ignored,
// so a caller can pass one that is only sometimes present without
// first collecting the non-nil ones into a slice.
func (s *SelectBuilder) Where(preds ...drops.Expression) *SelectBuilder {
	s.wheres = append(s.wheres, dropNilPreds(preds)...)
	return s
}

// GroupBy / Having / OrderBy.
func (s *SelectBuilder) GroupBy(exprs ...drops.Expression) *SelectBuilder {
	s.groupBys = append(s.groupBys, exprs...)
	return s
}

func (s *SelectBuilder) Having(preds ...drops.Expression) *SelectBuilder {
	s.havings = append(s.havings, dropNilPreds(preds)...)
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

// Unscoped opts out of every global filter on the FROM table — named
// and anonymous alike. The blunt instrument: it cannot tell a
// soft-delete guard from a tenancy one. Name what you are stepping
// around with IgnoreFilters instead.
func (s *SelectBuilder) Unscoped() *SelectBuilder { s.scope.unscoped = true; return s }

// IgnoreFilters bypasses the named global filters on the FROM table and
// leaves every other one standing:
//
//	db.Select().From(posts).IgnoreFilters(mysql.FilterSoftDelete)
//
// Names come from [Table.AddFilter]. A name no filter carries is
// ignored — a typo that leaves a filter standing returns too few rows,
// never too many.
func (s *SelectBuilder) IgnoreFilters(names ...string) *SelectBuilder {
	s.scope.ignore(names...)
	return s
}

// WriteSQL renders the SELECT.
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
	onExtra, defaults, ctxPreds := s.placeFilters()
	for i, j := range s.joins {
		b.WriteByte(' ')
		b.WriteString(string(j.kind))
		b.WriteByte(' ')
		j.table.writeFrom(b)
		b.WriteString(" ON ")
		// A nil condition is the empty conjunction, rendered where the
		// grammar insists on a predicate: writing nothing produced SQL
		// no server would parse. When the join carries predicates of
		// its own, they ARE the condition — a leading TRUE would be
		// noise in the one place a reviewer reads closely.
		switch {
		case j.on == nil && len(onExtra[i]) == 0:
			b.Append(orTrue(nil))
		case j.on == nil:
			b.Append(And(onExtra[i]...))
		case len(onExtra[i]) == 0:
			b.Append(j.on)
		default:
			b.Append(And(append([]drops.Expression{j.on}, onExtra[i]...)...))
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

// filterPlacement returns the join whose ON clause a table's automatic
// predicates belong in, and -1 for the WHERE clause.
//
// after is the position the table enters the statement at: -1 for the
// FROM side, and the join's own index for a joined table.
//
// It is a correctness question rather than a stylistic one. An outer
// join preserves rows that did not match, and a predicate on the
// preserved side written in the WHERE clause is evaluated AFTER the
// join has NULL-extended those rows — so it is false for exactly the
// rows the outer join exists to keep, and the join silently becomes an
// inner one. The predicate has to move into an ON clause, where it
// filters its relation before the preservation happens.
//
// A LEFT-joined table's predicates go in its own ON: the LEFT JOIN
// preserves the FROM side, and this table's rows are the ones a WHERE
// would delete. Everything else asks the mirror-image question — is
// there a RIGHT JOIN AFTER this table enters? — because from that
// point on it is everything to the left that is preserved. That covers
// the FROM table, an inner-joined table, and a right-joined one that a
// LATER right join then NULL-extends: its own join made it the
// preserved side, and the second one takes that back.
func (s *SelectBuilder) filterPlacement(after int, kind joinKind) int {
	if kind == leftJoin {
		return after
	}
	for i := after + 1; i < len(s.joins); i++ {
		if s.joins[i].kind == rightJoin {
			return i
		}
	}
	return -1
}

// placeFilters distributes every table's automatic predicates over the
// join ON clauses and the WHERE clause, per filterPlacement.
//
// onExtra[i] is what join i's ON gains; defaults and ctxPreds are what
// the WHERE clause gains, kept apart so the clause still reads
// scope-first, caller, then request-time.
func (s *SelectBuilder) placeFilters() (onExtra [][]drops.Expression, defaults, ctxPreds []drops.Expression) {
	onExtra = make([][]drops.Expression, len(s.joins))
	place := func(slot int, render, ctx []drops.Expression) {
		if slot >= 0 {
			onExtra[slot] = append(onExtra[slot], render...)
			onExtra[slot] = append(onExtra[slot], ctx...)
			return
		}
		defaults = append(defaults, render...)
		ctxPreds = append(ctxPreds, ctx...)
	}

	var fromDefaults []drops.Expression
	for _, t := range s.fromTables() {
		fromDefaults = append(fromDefaults, s.scope.filtersOf(t, s.defaults)...)
	}
	place(s.filterPlacement(-1, innerJoin), fromDefaults, s.ctxFrom)

	for i, j := range s.joins {
		var ctx []drops.Expression
		if i < len(s.ctxJoins) {
			ctx = s.ctxJoins[i]
		}
		place(s.filterPlacement(i, j.kind), s.scope.filtersOf(j.table, s.defaults), ctx)
	}
	return onExtra, defaults, ctxPreds
}

// ToSQLCtx renders the statement ctx would send.
//
// A SELECT rendered through [SelectBuilder.ToSQL] carries the table's
// DefaultFilters and none of its ContextFilters, because a render has
// no ctx to build one from — so on a tenant-scoped table it is not the
// statement that would be sent. This is: the context filters of the
// FROM table and of every joined table are built, walked and AND-ed in,
// and every statement written inside the query — a subquery operand, a
// CTE body — is resolved on the same terms.
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

// UnscopedDefaults opts out of the table's DECLARATION-time filters and
// keeps its request-time ones — "include the soft-deleted rows" without
// also meaning "and every other tenant's". It is what the entity path
// reaches for; [SelectBuilder.Unscoped] stays the blunt instrument.
func (s *SelectBuilder) UnscopedDefaults() *SelectBuilder {
	s.scope.unscoped, s.scope.keepCtx = true, true
	return s
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
func (s *SelectBuilder) resolveCtx(ctx context.Context) (*SelectBuilder, error) {
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
	// be written in any of them.
	if r, err := resolveExprs(ctx, s.columns); err != nil {
		return nil, err
	} else if r != nil {
		cp.columns, changed = r, true
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

	// Unscoped is the statement saying it wants none of the table's
	// automatic predicates, and it means the request-time ones too —
	// see [SelectBuilder.Unscoped], which is the blunt instrument.
	if !s.scope.dropsContextFilters() {
		tables := s.fromTables()
		var ctxFrom []drops.Expression
		for _, t := range tables {
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
// one slice per join rather than flattened — see filterPlacement for
// why the shape is the point.
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

// Rows executes the SELECT and returns the raw cursor, with the tables'
// context filters resolved against ctx first.
func (s *SelectBuilder) Rows(ctx context.Context) (drops.Rows, error) {
	sql, args, err := s.ToSQLCtx(ctx)
	if err != nil {
		return nil, err
	}
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
	// A node holding the statement rather than a closure rendering it:
	// wrapped in ExprFunc, the SELECT was invisible to resolveExpr, so
	// a scoped table read through AsSubquery rendered with none of its
	// context filters — the FROM clause of a report is exactly where
	// that goes unnoticed. See opExpr.
	return &opExpr{parts: []string{"(", ")"}, operands: []drops.Expression{s}, alias: alias}
}
