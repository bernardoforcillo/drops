package clickhouse

import (
	"context"
	"errors"
	"fmt"

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

	// unscopedDefaults drops the DEFAULT filters of every table this
	// statement names and keeps the context filters — the narrower
	// half of [SelectBuilder.Unscoped], which the entity query uses
	// and no caller can reach directly. See unscopeDefaults.
	unscopedDefaults bool
	err              error // deferred error (e.g. cursor decode failure) surfaced at Rows()
	ctes             []*CTE

	// ctxFrom and ctxJoins carry the resolved context filters of the
	// FROM table and of the joined tables whose filters belong in the
	// WHERE clause. They are set by resolveCtx on a per-execution copy
	// and read by WriteSQL, which is what lets a LEFT- or ANY-joined
	// table's predicates land in its ON clause instead — see
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

	// ctxOn holds this join's table's resolved context filters when
	// they belong in the ON clause rather than the WHERE clause. Set by
	// resolveJoins on the per-execution copy of the builder.
	ctxOn []drops.Expression
}

type joinPlacement int

const (
	placeInWhere joinPlacement = iota
	placeInOn
	placeNowhere
)

// filterPlacement says where a joined table's automatic predicates —
// its DefaultFilters and its resolved ContextFilters — have to be
// written for the join to keep meaning what it says.
//
// The choice is not cosmetic and it is not the same for every kind,
// which is why it is a function of the kind rather than one branch
// somewhere in WriteSQL. ClickHouse has the widest join vocabulary of
// the dialects drops targets, and two of its kinds reach an answer no
// other dialect has to think about.
//
// An INNER JOIN emits only matched pairs, so a predicate on the joined
// table selects the same rows in the ON clause and in the WHERE clause.
// The WHERE clause is where it goes, next to the FROM table's own, so
// one reader sees every restriction the statement carries in one place.
// ALL INNER JOIN is the same join said explicitly — ALL is what
// join_default_strictness already means — and gets the same answer.
//
// A LEFT JOIN preserves the FROM table. Its joined side is the nullable
// one, and a WHERE predicate on a nullable side is false for every
// unmatched row — so a tenant guard written there deletes exactly the
// parent rows the LEFT JOIN exists to keep, turning it into an INNER
// JOIN and dropping parents that have no child. In the ON clause the
// same predicate restricts which children match and leaves the parents
// alone, which is what was meant.
//
// An ANY INNER JOIN arrives at the ON clause by a completely different
// route, and it is the one a port from another dialect gets wrong. ANY
// keeps at most one matching right row per left row — "the first one
// found" — and it picks it while the join runs, from the rows the ON
// clause admits. A guard in the WHERE clause is applied afterwards, to
// whichever row ANY happened to pick: a left row whose right table
// holds a matching row of this tenant AND a matching row of another
// disappears from the result whenever ANY picked the other one. That is
// not a leak, it is the mirror of one — the guard silently deletes rows
// the caller is entitled to, non-deterministically. In the ON clause it
// restricts the candidates ANY chooses from, which is what was meant.
//
// A RIGHT JOIN inverts the LEFT case: the joined table is the preserved
// side, so its predicate belongs in the WHERE clause again — and
// belongs there emphatically, because in the ON clause an unmatched row
// of the joined table survives the join with a NULL left side,
// predicate and all, and a tenant guard in the ON clause of a RIGHT
// JOIN is therefore a leak rather than an over-filter.
//
// A FULL JOIN preserves both sides, so both readings apply at once and
// neither placement is right: in the ON clause the unmatched rows of
// the joined table survive unfiltered (the RIGHT JOIN leak), and in the
// WHERE clause the unmatched rows of the FROM table are dropped (the
// LEFT JOIN degeneration). There is no third place, so it is refused —
// see [ErrFullJoinScoped].
//
// An ASOF JOIN is refused for a reason no other dialect supplies, and
// it is refused rather than placed because BOTH clauses are wrong. It
// has ANY's problem — it keeps one right row per left row, the closest
// one by the inequality — so the WHERE clause deletes rows
// non-deterministically exactly as it does for ANY. And it cannot take
// the ON-clause answer, because an ASOF JOIN's ON clause is not a
// general predicate slot: ClickHouse requires it to be a run of
// equalities followed by exactly one inequality, in that order, and
// appending a tenant guard after the caller's inequality violates the
// grammar. Nothing here can reorder a caller's ON condition, so there
// is nowhere to put the predicate. See [ErrAsofJoinScoped].
//
// This decides where the *joined* table's predicates go. Where the FROM
// table's go is the mirror question, and depends on the kinds of every
// join in the statement rather than on one of them: see fromFilterJoin.
func (k joinKind) filterPlacement() joinPlacement {
	switch k {
	case leftJoin, anyJoin:
		return placeInOn
	case fullJoin, asofJoin:
		return placeNowhere
	default:
		return placeInWhere
	}
}

// ErrFullJoinScoped is returned when a SELECT full-joins a table that
// carries context filters, and when the FROM table of a statement
// containing a FULL JOIN carries them.
//
// A FULL JOIN has nowhere correct to put them — see
// joinKind.filterPlacement — and a tenant axis names a row-visibility
// boundary, so the statement is refused rather than sent with the
// predicate in a place that either leaks one side's unmatched rows or
// drops the other's. Say Unscoped() and write the predicates at the
// query, where a reviewer can see them.
var ErrFullJoinScoped = errors.New("drops/clickhouse: a FULL JOIN cannot carry a table's context filters")

// ErrAsofJoinScoped is returned when a SELECT ASOF-joins a table that
// carries context filters.
//
// An ASOF JOIN keeps the closest match per left row rather than every
// match, so a guard in the WHERE clause deletes the pairs whose closest
// row belongs to somebody else instead of choosing the closest row that
// does not — a wrong answer that varies with the data. The ON clause,
// which is where that would be fixed, is constrained by ClickHouse to a
// run of equalities followed by one trailing inequality, so a guard
// cannot be appended to it. Join a table with no automatic filters,
// or say Unscoped() and write the predicates at the query.
var ErrAsofJoinScoped = errors.New("drops/clickhouse: an ASOF JOIN cannot carry a table's context filters")

// fromFilterJoin says where the FROM table's own automatic predicates
// have to be written, and returns the index of the join whose ON clause
// has to carry them, or -1 for the WHERE clause.
//
// It is joinKind.filterPlacement's mirror. That function reasons about
// the joined table, whose placement depends on its own join's kind;
// this one reasons about the FROM table, whose placement depends on the
// kinds of every join in the statement, because it is the FROM table
// that every one of them joins against.
//
// With no joins, or with only inner, LEFT, ANY and ASOF joins, the FROM
// table is either the inner side or the preserved one, and the WHERE
// clause is where its predicates have always belonged. A RIGHT JOIN
// inverts that: the FROM table becomes the nullable side, and a
// predicate on a nullable side is false for every unmatched row, so a
// tenant guard in the WHERE clause deletes exactly the rows of the
// joined table the RIGHT JOIN exists to keep — the LEFT JOIN
// degeneration, mirrored, and the reason the LEFT JOIN half of
// filterPlacement exists at all.
//
// The ON clause that carries them is the first RIGHT JOIN's, and it has
// to be the first one: that is where the FROM table enters the join
// tree, so restricting it there restricts it before any preserved side
// is added, and every later join sees the restricted relation. In a
// later join's ON clause the same predicate would instead be false for
// the rows an earlier outer join had already NULL-extended, which
// silently drops them.
func (s *SelectBuilder) fromFilterJoin() int {
	for i, j := range s.joins {
		if j.kind == rightJoin {
			return i
		}
	}
	return -1
}

// firstJoinOfKind returns the index of the first join of kind k, or -1.
func (s *SelectBuilder) firstJoinOfKind(k joinKind) int {
	for i, j := range s.joins {
		if j.kind == k {
			return i
		}
	}
	return -1
}

// From sets the FROM table. Required before execution.
func (s *SelectBuilder) From(t *Table) *SelectBuilder { s.from = t; return s }

// Final appends FINAL after the table, forcing CH to merge parts
// at read time (handy with ReplacingMergeTree / CollapsingMergeTree
// when you accept the cost).
//
// FINAL and the automatic predicates compose the right way round, and
// that is a consequence of drops writing them into WHERE rather than
// PREWHERE: the merge happens first and the guard is then tested
// against the surviving version of the row. See [SelectBuilder.Prewhere]
// for what the other order would do.
func (s *SelectBuilder) Final() *SelectBuilder { s.final = true; return s }

// SampleBy adds a SAMPLE clause (e.g. SampleBy(0.1) for 10%).
//
// Sampling reads a deterministic subset of the parts and never a row
// the predicates exclude, so it under-reads and cannot widen what a
// scoped query returns. What it does change is every aggregate computed
// over it, which is the caller's business and not drops'.
func (s *SelectBuilder) SampleBy(e any) *SelectBuilder {
	if expr, ok := e.(drops.Expression); ok {
		s.sampleBy = expr
		return s
	}
	s.sampleBy = drops.Param{Value: e}
	return s
}

// Distinct toggles SELECT DISTINCT.
func (s *SelectBuilder) Distinct() *SelectBuilder { s.distinct = true; return s }

// Join adds an INNER JOIN. The joined table's own automatic predicates
// travel with it — see joinKind.filterPlacement.
func (s *SelectBuilder) Join(t *Table, on drops.Expression) *SelectBuilder {
	s.joins = append(s.joins, joinClause{kind: innerJoin, table: t, on: on})
	return s
}

// LeftJoin adds a LEFT JOIN. A scoped joined table's predicates go into
// this join's ON clause, not the WHERE clause, or the join degenerates
// to an inner one — see joinKind.filterPlacement.
func (s *SelectBuilder) LeftJoin(t *Table, on drops.Expression) *SelectBuilder {
	s.joins = append(s.joins, joinClause{kind: leftJoin, table: t, on: on})
	return s
}

// RightJoin adds a RIGHT JOIN. It inverts the placement of both sides'
// predicates — see joinKind.filterPlacement and fromFilterJoin.
func (s *SelectBuilder) RightJoin(t *Table, on drops.Expression) *SelectBuilder {
	s.joins = append(s.joins, joinClause{kind: rightJoin, table: t, on: on})
	return s
}

// FullJoin adds a FULL JOIN. A scoped table on either side of one is
// refused with [ErrFullJoinScoped]: there is nowhere correct to write
// the predicate.
func (s *SelectBuilder) FullJoin(t *Table, on drops.Expression) *SelectBuilder {
	s.joins = append(s.joins, joinClause{kind: fullJoin, table: t, on: on})
	return s
}

// AnyJoin adds an ANY INNER JOIN, which keeps at most one matching row
// per left row. A scoped joined table's predicates go into this join's
// ON clause — see joinKind.filterPlacement for why the WHERE clause
// would drop rows the caller is entitled to.
func (s *SelectBuilder) AnyJoin(t *Table, on drops.Expression) *SelectBuilder {
	s.joins = append(s.joins, joinClause{kind: anyJoin, table: t, on: on})
	return s
}

// AllJoin adds an ALL INNER JOIN.
func (s *SelectBuilder) AllJoin(t *Table, on drops.Expression) *SelectBuilder {
	s.joins = append(s.joins, joinClause{kind: allJoin, table: t, on: on})
	return s
}

// AsofJoin adds an ASOF JOIN. A scoped joined table is refused with
// [ErrAsofJoinScoped]: its ON clause has a grammar rather than a free
// predicate slot, and its WHERE clause answers a different question.
func (s *SelectBuilder) AsofJoin(t *Table, on drops.Expression) *SelectBuilder {
	s.joins = append(s.joins, joinClause{kind: asofJoin, table: t, on: on})
	return s
}

// Prewhere adds a PREWHERE predicate — evaluated before the main
// WHERE, with the right primary-key columns it can dramatically cut
// scanned data on MergeTree tables.
//
// Nothing drops adds automatically is written here, and the reason is
// worth stating because the opposite looks like free performance: a
// tenant column is usually a leading sorting-key column, which is
// exactly the shape PREWHERE rewards.
//
// It is not free. PREWHERE runs before FINAL, so on a
// ReplacingMergeTree a guard written there is tested against every
// stored version of a row rather than against the one FINAL keeps: a
// row whose superseded version carries this tenant survives the
// PREWHERE, FINAL then replaces it with the current version carrying
// another tenant, and the statement returns another tenant's row
// through a predicate that reads correctly. ClickHouse itself declines
// this rewrite — optimize_move_to_prewhere is on by default and
// optimize_move_to_prewhere_if_final is off — so writing the guard into
// WHERE both keeps the semantics and lets the server move it into
// PREWHERE on its own account wherever moving it is sound. drops has
// nothing to add to that judgement and does not try.
//
// A caller's own PREWHERE predicates are bracketed slightly more
// eagerly than a WHERE clause's, because this clause stands in front of
// the one the guards are written into — see writeLeadingAnd.
func (s *SelectBuilder) Prewhere(preds ...drops.Expression) *SelectBuilder {
	s.prewheres = append(s.prewheres, preds...)
	return s
}

// Where appends predicates joined by AND.
func (s *SelectBuilder) Where(preds ...drops.Expression) *SelectBuilder {
	s.wheres = append(s.wheres, preds...)
	return s
}

// Unscoped opts out of automatic predicates for this SELECT — the
// DefaultFilter and ContextFilter lists of the FROM table and of every
// joined table alike. Use it to read soft-deleted rows, and to run the
// administrative query that has to span every tenant's rows.
//
// It is statement-wide rather than per table on purpose: a caller who
// says Unscoped is describing this query's authority, and a flag that
// unscoped the FROM table while a joined one kept its tenant axis would
// answer with a silently narrowed slice of the rows that were asked
// for.
//
// It clears the context filters too because a half-scoped statement is
// the worse of the two answers: a caller who reaches for Unscoped to
// read soft-deleted rows and instead gets ErrTenantMissing has learned
// nothing about the row they were after, and one who gets the tenant
// predicate they did not ask for silently reads a subset. Scope such a
// query explicitly — Unscoped().Where(clickhouse.Eq(TenantID, id)) —
// where the intent is on the page.
//
// It does not reach into a CTE body or a subquery: those are statements
// of their own and keep their own scoping, which is also how to unscope
// one relation of a query and no other.
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
//
// It renders what the builder knows without a context, which since
// [Table.ContextFilter] shipped is no longer necessarily the whole
// statement: the FROM table's DefaultFilters are written and its
// ContextFilters are not. Rendering is not how a statement gets its
// context filters — see [SelectBuilder.ToSQLCtx] and resolveCtx, which
// resolve them against a ctx and hand this method an already-scoped
// builder.
func (s *SelectBuilder) WriteSQL(b *drops.Builder) {
	writeCTEs(b, s.ctes)
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
	// The FROM table's own automatic predicates, and the join whose ON
	// clause has to carry them when a RIGHT JOIN has made the FROM
	// table the nullable side.
	var fromDefaults, fromCtx []drops.Expression
	fromOn := -1
	if !s.unscoped && s.from != nil {
		fromDefaults, fromCtx = s.autoDefaults(s.from), s.ctxFrom
		if len(fromDefaults) > 0 || len(fromCtx) > 0 {
			fromOn = s.fromFilterJoin()
		}
	}
	// autoWheres are the automatic predicates that belong in the WHERE
	// clause. The joined tables' are gathered here, as the joins render,
	// so the placement decision lives in one place with its ON-clause
	// half.
	var autoWheres []drops.Expression
	for i, j := range s.joins {
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
		if i == fromOn {
			on = andWith(on, append(append([]drops.Expression(nil), fromDefaults...), fromCtx...))
		}
		b.WriteString(" ON ")
		b.Append(on)
	}
	if len(s.prewheres) > 0 {
		b.WriteString(" PREWHERE ")
		writeLeadingAnd(b, s.prewheres)
	}
	// The FROM table's go in front of the joined tables', and the whole
	// lot in front of the caller's own predicates — which the resolved
	// context filters then follow. So a statement reads scoping first,
	// intent second, and a query log shows at a glance whether a query
	// was scoped at all.
	if fromOn < 0 && len(fromDefaults) > 0 {
		autoWheres = append(append([]drops.Expression(nil), fromDefaults...), autoWheres...)
	}
	wheres := make([]drops.Expression, 0, len(autoWheres)+len(s.wheres)+len(fromCtx)+len(s.ctxJoins))
	wheres = append(wheres, autoWheres...)
	wheres = append(wheres, s.wheres...)
	if fromOn < 0 {
		wheres = append(wheres, fromCtx...)
	}
	wheres = append(wheres, s.ctxJoins...)
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
//
// It renders what the builder knows without a context, which since
// [Table.ContextFilter] shipped is no longer necessarily the whole
// statement: a table's context filters — the tenant axis installed by
// ScopeByTenant — are resolved by the executors and do not appear here.
// Use [SelectBuilder.ToSQLCtx] to see the statement a given ctx would
// send; that is the one to assert on in a test, and the one to log.
// ToSQL remains the right call where there is no request to speak of
// and never will be: rendering a materialised view's body, or embedding
// the SELECT in a statement some other executor will run.
func (s *SelectBuilder) ToSQL() (sql string, args []any) {
	b := drops.NewBuilder(Placeholder)
	s.WriteSQL(b)
	return b.SQL()
}

// ToSQLCtx renders the complete statement for ctx, with every context
// filter of every table it names resolved into the clause that join
// shape allows. A filter that refuses — [TenantFilter] with no tenant
// on ctx — returns its error and no SQL, because the alternative to
// refusing is an unfiltered query.
func (s *SelectBuilder) ToSQLCtx(ctx context.Context) (sql string, args []any, err error) {
	if s.err != nil {
		return "", nil, s.err
	}
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
// WITH recent AS (SELECT … FROM events) is scoped the way a bare SELECT
// from events would be; so is a statement reached as a subquery
// expression — EXISTS, IN, a scalar subquery — through the same walk,
// see resolveExpr.
//
// A body drops did not build cannot be: a CTE or a subquery assembled
// out of raw fragments has nothing to resolve against and stays the
// caller's to scope.
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
		if s.from.hasContextFilters() {
			if i := s.firstJoinOfKind(fullJoin); i >= 0 {
				return nil, fmt.Errorf("%w: %q is FULL JOINed to the scoped FROM table %q; join an unscoped table, or say Unscoped",
					ErrFullJoinScoped, s.joins[i].table.Name(), s.from.Name())
			}
			preds, err := s.from.resolveContextFilters(ctx)
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
		if s.from != nil {
			tables = append(tables, s.from)
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
	// scalar, a GROUP BY term, a SAMPLE expression. Every list WriteSQL
	// reads has to be here — a list the renderer reads and the resolver
	// does not is a place a caller can write a SELECT that goes out
	// unscoped.
	lists := []struct {
		src []drops.Expression
		dst *[]drops.Expression
	}{
		{s.columns, &cp.columns},
		{s.prewheres, &cp.prewheres},
		{s.wheres, &cp.wheres},
		{s.groupBys, &cp.groupBys},
		{s.havings, &cp.havings},
		{s.orderBys, &cp.orderBys},
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
	if s.sampleBy != nil {
		resolved, sChanged, err := resolveExpr(ctx, s.sampleBy)
		if err != nil {
			return nil, err
		}
		if sChanged {
			cp.sampleBy, changed = resolved, true
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
	if s.err != nil {
		return nil, false, s.err
	}
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
// a join contributes to is joinKind.filterPlacement's decision, and the
// two kinds where neither clause is correct — FULL and ASOF — are
// refused here rather than answered wrongly.
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
			err := ErrFullJoinScoped
			if j.kind == asofJoin {
				err = ErrAsofJoinScoped
			}
			return nil, nil, fmt.Errorf("%w: %q; join an unscoped table, or say Unscoped",
				err, j.table.Name())
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
// condition — a join written against an IN over a scoped table is as
// much a statement as one written in a WHERE clause.
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

// Rows runs the SELECT and returns the raw cursor.
func (s *SelectBuilder) Rows(ctx context.Context) (drops.Rows, error) {
	sql, args, err := s.ToSQLCtx(ctx)
	if err != nil {
		return nil, err
	}
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
//
// The inner statement is rendered for ctx, not through ToSQL: a count
// over a scoped table is a read of that table, and rendering the body
// blind would answer "how many rows are there" for every tenant while
// the query that lists them answers for one.
func (s *SelectBuilder) Count(ctx context.Context) (int64, error) {
	inner, args, err := s.ToSQLCtx(ctx)
	if err != nil {
		return 0, err
	}
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
