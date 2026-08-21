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

	// unscopedDefaults drops the DEFAULT filters of every table this
	// statement names and keeps the context filters — the narrower
	// half of [SelectBuilder.Unscoped], which the entity query uses
	// and no caller can reach directly. See unscopeDefaults.
	unscopedDefaults bool

	// ctxFrom and ctxJoins carry the resolved context filters of the
	// FROM table and of the joined tables whose filters belong in the
	// WHERE clause. They are set by resolveCtx on a per-execution copy
	// and read by WriteSQL, which is what lets a LEFT-joined table's
	// predicates land in its ON clause instead — see
	// joinKind.filterPlacement. Keeping them out of s.wheres also keeps
	// the rendered clause ordered scoping-last, so a query log shows
	// the caller's intent and the guard added to it apart.
	ctxFrom  []drops.Expression
	ctxJoins []drops.Expression

	// defaults carries the DefaultFilters of the tables this statement
	// names, resolved for one execution — see resolvedDefaults. Set by
	// resolveCtx on the per-execution copy and read by WriteSQL through
	// defaults.of, which falls back to the unresolved list, so the
	// ToSQL path and every statement whose filters hold no statement
	// render exactly what they always did.
	defaults resolvedDefaults

	// resolved marks a builder that resolveCtx has already produced.
	// Resolution is not idempotent — the FROM table still has its
	// context filters afterwards, so resolving twice binds the tenant
	// twice — and a resolved statement is reachable a second time
	// whenever it is embedded in another one.
	resolved bool
}

// Unscoped opts out of automatic predicates for this SELECT — the
// DefaultFilter and ContextFilter lists of the FROM table and of every
// joined table alike. Use it to read soft-deleted rows, and to run the
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
// query explicitly — Unscoped().Where(sqlite.Eq(TenantID, id)) — where
// the intent is on the page.
//
// That is the meaning at THIS level. The entity query makes the
// narrower trade — it drops the default filters and keeps the context
// ones — because a caller reaching for it is usually thinking about
// soft-deleted rows rather than about tenancy; see
// [EntityQuery.Unscoped].
func (s *SelectBuilder) Unscoped() *SelectBuilder { s.unscoped = true; return s }

// unscopeDefaults drops the DEFAULT filters of every table the
// statement names and keeps the context filters.
//
// It is unexported because it is not a level a caller writes at: the
// two levels a caller sees are the raw builder, where Unscoped is
// statement-wide, and the entity query, where Unscoped is
// defaults-only. This is how the second one is composed. See
// EntityQuery.Unscoped for why the entity query makes the narrower
// trade, in the same words in all four dialects.
func (s *SelectBuilder) unscopeDefaults() *SelectBuilder { s.unscopedDefaults = true; return s }

// autoDefaults returns the default filters to render for t: the list
// this execution resolved, or none when the statement opted out of the
// defaults alone.
//
// Asking here rather than at each render site is what keeps the two
// opt-outs from drifting: a clause that read s.defaults directly would
// go on rendering a soft-delete guard the caller asked to be rid of,
// and the guard would be present in one clause and absent in the next.
func (s *SelectBuilder) autoDefaults(t *Table) []drops.Expression {
	if s.unscopedDefaults {
		return nil
	}
	return s.defaults.of(t)
}

// joinKind is the SQL keyword a join renders with.
//
// The set is INNER and LEFT and stops there, deliberately. SQLite grew
// RIGHT and FULL OUTER JOIN in 3.39 (2022) and this package does not
// expose them: a builder that emitted one would produce a statement
// that parses on a 2023 build and is a syntax error on the system
// SQLite of a long-term-support distribution, which for the dialect
// docs/dialects.md calls "the right choice for tests, embedded use and
// small deployments" is the wrong trade. The consequence for scoping is
// that the two placements this file reasons about are the only two
// there are — see filterPlacement — and that the FROM table's
// predicates always belong in the WHERE clause, since nothing here can
// make it the nullable side of a join.
type joinKind string

const (
	innerJoin joinKind = "JOIN"
	leftJoin  joinKind = "LEFT JOIN"
)

type joinClause struct {
	kind  joinKind
	table *Table
	on    drops.Expression

	// ctxOn carries the joined table's resolved context filters for a
	// join kind whose filters belong in the ON clause. It is set by
	// resolveJoins on a per-execution copy and read by WriteSQL, which
	// is what keeps the ON clause composed exactly once — a resolver
	// that AND-ed its own predicates in would nest a second layer of
	// parentheses around whatever WriteSQL then added.
	ctxOn []drops.Expression
}

// joinPlacement says which clause a joined table's automatic predicates
// have to be written into.
type joinPlacement int

const (
	placeInWhere joinPlacement = iota
	placeInOn
)

// filterPlacement says where a joined table's automatic predicates —
// its DefaultFilters and its resolved ContextFilters — have to be
// written for the join to keep meaning what it says.
//
// The choice is not cosmetic and it is not the same for both kinds,
// which is why it is a function of the kind rather than one branch
// somewhere in WriteSQL.
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
// PostgreSQL's dialect has to answer this for RIGHT and FULL as well,
// where the reasoning inverts and then runs out; this one does not,
// because this builder has no such join. See joinKind.
func (k joinKind) filterPlacement() joinPlacement {
	if k == leftJoin {
		return placeInOn
	}
	return placeInWhere
}

// andWith returns on AND every predicate in extra, for an ON clause
// that has to carry a table's automatic filters. A nil on — a join
// written with no condition at all — yields the filters alone rather
// than a dangling AND.
//
// It renders the conjunction through writeAnd rather than through And
// because an ON clause is a WHERE clause in a different place: a
// caller-written join condition whose top level is an OR reassociates
// the guards that follow it exactly as it would in a WHERE clause. The
// bracketing is otherwise identical to And's, so an ON clause that was
// correct before renders unchanged.
func andWith(on drops.Expression, extra []drops.Expression) drops.Expression {
	if len(extra) == 0 {
		return on
	}
	preds := extra
	if on != nil {
		preds = append([]drops.Expression{on}, extra...)
	}
	if len(preds) == 1 {
		return preds[0]
	}
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		writeAnd(b, preds)
		b.WriteByte(')')
	})
}

// From sets the table to select from.
func (s *SelectBuilder) From(t *Table) *SelectBuilder { s.table = t; return s }

// Distinct adds the DISTINCT keyword.
func (s *SelectBuilder) Distinct() *SelectBuilder { s.distinct = true; return s }

// Join adds an INNER JOIN ... ON.
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

// LeftJoin adds a LEFT JOIN ... ON.
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

// Where AND-s the given predicates onto the statement.
//
// A predicate that does not parenthesise itself is parenthesised when
// it would otherwise reassociate the clause — a [drops.Raw] whose top
// level is an OR, most of all, since AND binds tighter and the
// automatic predicates around it would end up on the wrong side of it.
// See writeAnd.
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
//
// It renders what the builder knows without a context, which since
// [Table.ContextFilter] shipped is no longer necessarily the whole
// statement: the FROM table's DefaultFilters are written and its
// ContextFilters are not. Rendering is not how a statement gets its
// context filters — see [SelectBuilder.ToSQLCtx] and resolveCtx, which
// resolve them against a ctx and hand this method an already-scoped
// builder.
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
	// The FROM table's own automatic predicates. There is no join kind
	// in this dialect that makes the FROM table the nullable side (see
	// joinKind), so they always belong in the WHERE clause.
	var fromDefaults, fromCtx []drops.Expression
	if !s.unscoped && s.table != nil {
		fromDefaults, fromCtx = s.autoDefaults(s.table), s.ctxFrom
	}
	// autoWheres are the automatic predicates that belong in the WHERE
	// clause. The joined tables' are gathered here, as the joins render,
	// so the placement decision lives in one place with its ON-clause
	// half.
	var autoWheres []drops.Expression
	for _, j := range s.joins {
		b.WriteByte(' ')
		b.WriteString(string(j.kind))
		b.WriteByte(' ')
		j.table.writeFrom(b)
		on := j.on
		if !s.unscoped {
			switch dfs := s.autoDefaults(j.table); j.kind.filterPlacement() {
			case placeInOn:
				on = andWith(on, append(append([]drops.Expression(nil), dfs...), j.ctxOn...))
			case placeInWhere:
				autoWheres = append(autoWheres, dfs...)
			}
		}
		// A join left with no condition at all — Join(t, nil) against a
		// table that contributed no automatic predicate — renders ON 1
		// rather than a dangling "ON", which SQLite rejects outright.
		// The literal is 1 and not TRUE because the boolean keywords
		// only arrived in 3.23 and this package's other degenerate
		// predicates (see inExpr) already spell them 0 and 1.
		if on == nil {
			on = drops.Raw("1")
		}
		b.WriteString(" ON ")
		b.Append(on)
	}
	// The FROM table's go in front of them, and the whole lot in front
	// of the caller's own predicates — which the resolved context
	// filters then follow. So a statement reads scoping first, intent
	// second, and a query log shows at a glance whether a query was
	// scoped at all.
	wheres := make([]drops.Expression, 0,
		len(fromDefaults)+len(autoWheres)+len(s.wheres)+len(fromCtx)+len(s.ctxJoins))
	wheres = append(wheres, fromDefaults...)
	wheres = append(wheres, autoWheres...)
	wheres = append(wheres, s.wheres...)
	wheres = append(wheres, fromCtx...)
	wheres = append(wheres, s.ctxJoins...)
	if len(wheres) > 0 {
		b.WriteString(" WHERE ")
		writeAnd(b, wheres)
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
//
// It renders what the builder knows without a context, which since
// [Table.ContextFilter] shipped is no longer necessarily the whole
// statement: a table's context filters — the tenant axis installed by
// ScopeByTenant, an authz guard — are resolved by the executors and do
// not appear here. Use [SelectBuilder.ToSQLCtx] to see the statement a
// given ctx would send; that is the one to assert on in a test, and the
// one to log. ToSQL remains the right call where there is no request to
// speak of and never will be: rendering a CREATE VIEW body, or
// embedding the SELECT in a statement some other executor will run.
func (s *SelectBuilder) ToSQL() (sql string, args []any) { return ToSQL(s) }

// ToSQLCtx renders the complete statement for ctx, with every context
// filter of every table it names resolved into the clause that join
// shape allows. A filter that refuses — [TenantFilter] with no tenant
// on ctx — returns its error and no SQL, because the alternative to
// refusing is an unfiltered query.
func (s *SelectBuilder) ToSQLCtx(ctx context.Context) (sql string, args []any, err error) {
	r, err := s.resolveCtx(ctx)
	if err != nil {
		return "", nil, err
	}
	sql, args = r.ToSQL()
	return sql, args, nil
}

// resolveCtx returns the builder to render for one execution: this one
// when there was nothing to resolve, otherwise a shallow copy carrying
// the resolved predicates and the resolved subqueries.
//
// The copy is what keeps a builder reusable. Appending the resolved
// predicates to s.wheres would make the second execution of the same
// builder carry two tenant predicates, the third three — with the same
// value bound repeatedly, so the rows come back right and nothing fails
// until an argument limit or a query log makes it visible. Worse, a
// builder held across requests would answer the second request with the
// first one's tenant.
//
// Every table the statement names is resolved, not just the FROM one,
// because a context filter is a property of the table and the promise
// in tenant.go is about statements rather than about clauses. A joined
// table carries its own filters into the clause its join kind allows —
// see joinKind.filterPlacement. A CTE whose body is a statement drops
// built is resolved as the statement it is, so that
// WITH recent AS (SELECT … FROM posts) is scoped the way a bare SELECT
// from posts would be; so is a statement reached as a subquery
// expression — EXISTS, IN, a scalar subquery — through the same walk,
// see resolveExpr.
//
// A body drops did not build cannot be: a CTE or a subquery assembled
// out of raw fragments has nothing to resolve against and stays the
// caller's to scope.
//
// Unscoped() does not reach into any of those: a CTE body and a
// subquery are statements of their own and keep their own scoping,
// which is also how to unscope one relation of a query and no other.
func (s *SelectBuilder) resolveCtx(ctx context.Context) (*SelectBuilder, error) {
	// Resolution is not idempotent, and a resolved statement is
	// reachable twice whenever it is embedded in another one. Resolving
	// it again would bind the tenant a second time, which changes no
	// rows and shows up only in the argument list.
	if s.resolved {
		return s, nil
	}
	cp := *s
	changed := false

	if !s.unscoped {
		if s.table.hasContextFilters() {
			preds, err := s.table.resolveContextFilters(ctx)
			if err != nil {
				return nil, err
			}
			if len(preds) > 0 {
				cp.ctxFrom, changed = preds, true
			}
		}
	}
	// The default filters are skipped when the statement opted out of
	// them alone, and not merely dropped at render time: resolving one
	// walks the statements written inside it, and a default filter
	// holding a subquery over a scoped table would REFUSE on a ctx with
	// no tenant — refusing the very statement whose defaults the caller
	// just said to ignore.
	if !s.unscoped && !s.unscopedDefaults {
		// The DefaultFilters of every table the statement names, walked
		// for the statements written inside them. Same lists WriteSQL
		// reads: a predicate the renderer adds on the statement's own
		// account is still a predicate a caller wrote, and a caller can
		// write a subquery into it.
		tables := make([]*Table, 0, 1+len(s.joins))
		if s.table != nil {
			tables = append(tables, s.table)
		}
		for _, j := range s.joins {
			tables = append(tables, j.table)
		}
		defaults, err := resolveTableDefaults(ctx, tables...)
		if err != nil {
			return nil, err
		}
		if defaults != nil {
			cp.defaults, changed = defaults, true
		}
	}

	joins, joinPreds, err := s.resolveJoins(ctx)
	if err != nil {
		return nil, err
	}
	if len(joinPreds) > 0 {
		cp.ctxJoins, changed = joinPreds, true
	}
	joins, err = s.resolveJoinOns(ctx, joins)
	if err != nil {
		return nil, err
	}
	if joins != nil {
		cp.joins, changed = joins, true
	}

	// Every expression list the statement carries, because a subquery is
	// a statement wherever it is written: a predicate, a select-list
	// scalar, an ORDER BY term. Every list WriteSQL reads has to be
	// here — a list the renderer reads and the resolver does not is a
	// place a caller can write a SELECT that goes out unscoped.
	lists := []struct {
		src []drops.Expression
		dst *[]drops.Expression
	}{
		{s.columns, &cp.columns},
		{s.wheres, &cp.wheres},
		{s.orderBy, &cp.orderBy},
	}
	for _, l := range lists {
		resolved, err := resolveExprs(ctx, l.src)
		if err != nil {
			return nil, err
		}
		if resolved != nil {
			*l.dst, changed = resolved, true
		}
	}

	ctes, err := resolveCTEs(ctx, s.ctes)
	if err != nil {
		return nil, err
	}
	if ctes != nil {
		cp.ctes, changed = ctes, true
	}

	if !changed {
		return s, nil
	}
	cp.resolved = true
	return &cp, nil
}

// resolveStatement implements [ctxResolvable]: it is resolveCtx behind
// the interface resolveExpr dispatches on, so a SELECT nested in
// another statement is resolved as the statement it is.
func (s *SelectBuilder) resolveStatement(ctx context.Context) (drops.Expression, bool, error) {
	r, err := s.resolveCtx(ctx)
	if err != nil {
		return nil, false, err
	}
	return r, r != s, nil
}

// resolveJoins resolves the context filters of every joined table.
//
// It returns two things because the answer lands in two clauses: a
// rebuilt join list when some ON clause had to grow (nil when none
// did), and the predicates bound for the WHERE clause. Which of the two
// a join contributes to is joinKind.filterPlacement's decision.
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

// resolveJoinOns resolves the statements reachable from each join's ON
// condition — a join written against an EXISTS over a scoped table is
// as much a statement as one written in a WHERE clause.
//
// It appends to the list resolveJoins may already have copied rather
// than copying a second one, so the two resolutions compose into one
// join list.
func (s *SelectBuilder) resolveJoinOns(ctx context.Context, joins []joinClause) ([]joinClause, error) {
	for i, j := range s.joins {
		resolved, changed, err := resolveExpr(ctx, j.on)
		if err != nil {
			return nil, err
		}
		if !changed {
			continue
		}
		if joins == nil {
			joins = append([]joinClause(nil), s.joins...)
		}
		joins[i].on = resolved
	}
	return joins, nil
}

// All executes the query and scans every row into dest (pointer to
// slice of struct or *struct).
func (s *SelectBuilder) All(ctx context.Context, dest any) error {
	sql, args, err := s.ToSQLCtx(ctx)
	if err != nil {
		return err
	}
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	return drops.ScanAll(rows, dest)
}

// One executes the query and scans the first row into dest (pointer to
// struct), returning ErrNoRows when empty.
func (s *SelectBuilder) One(ctx context.Context, dest any) error {
	sql, args, err := s.ToSQLCtx(ctx)
	if err != nil {
		return err
	}
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
