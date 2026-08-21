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

	// unscopedDefaults drops the DEFAULT filters of every table this
	// statement names and keeps the context filters — the narrower
	// half of [SelectBuilder.Unscoped], which the entity query uses
	// and no caller can reach directly. See unscopeDefaults.
	unscopedDefaults bool
	err              error // deferred error (e.g. cursor decode failure) surfaced at Rows()

	// ctxFrom and ctxJoins carry the resolved context filters of the
	// FROM table and of the joined tables that place theirs in the
	// WHERE clause. They are set by resolveCtx on a per-execution copy
	// and read by writeCore, which is what lets the FROM table's
	// predicates land in an ON clause when the join shape requires it
	// — see fromFilterJoin. Keeping them out of s.wheres also keeps
	// the rendered clause ordered scoping-last, so a query log shows
	// the caller's intent and the guard that was added to it apart.
	ctxFrom  []drops.Expression
	ctxJoins []drops.Expression

	// defaults carries the DefaultFilters of the tables this statement
	// names, resolved for one execution — see resolvedDefaults. Set by
	// resolveCtx on the per-execution copy and read by writeCore
	// through defaults.of, which falls back to the unresolved list, so
	// the ToSQL path and every statement whose filters hold no
	// statement render exactly what they always did.
	defaults resolvedDefaults

	// resolved marks a builder that resolveCtx has already produced.
	// Resolution is not idempotent — the FROM table still has its
	// context filters, so resolving twice binds the tenant twice — and
	// a resolved statement is reachable a second time whenever it is
	// embedded in another one, which the per-parent-limit rewrite in
	// find.go does deliberately.
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
//
// This decides where the *joined* table's predicates go. Where the FROM
// table's go is the mirror question, and depends on the kinds of every
// join in the statement rather than on one of them: see fromFilterJoin.
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
// carries context filters, and when the FROM table of a statement
// containing a FULL JOIN carries them.
//
// A FULL JOIN has nowhere correct to put them — see
// joinKind.filterPlacement — and a tenant axis or an authz guard names
// a row-visibility boundary, so the statement is refused rather than
// sent with the predicate in a place that either leaks one side's
// unmatched rows or drops the other's. The two sides fail for mirrored
// reasons: the joined table's guard leaks in the ON clause, the FROM
// table's leaks there instead, and each drops the other side's
// unmatched rows in the WHERE clause. Join a pre-filtered subquery (a
// CTE, or FromExpr over a scoped SELECT), or say Unscoped() and write
// the predicates at the query where a reviewer can see them.
var ErrFullJoinScoped = errors.New("drops/pg: a FULL JOIN cannot carry a table's context filters")

// andWith returns on AND every predicate in extra, for an ON clause
// that has to carry a table's automatic filters. A nil on — a join
// written with no condition at all — yields the filters alone rather
// than a dangling AND.
//
// It renders the conjunction through writeAnd rather than through And
// because an ON clause is a WHERE clause in a different place: a
// caller-written join condition whose top level is an OR reassociates
// the guards that follow it exactly as it would in a WHERE clause, and
// And joins its operands with a bare " AND ". The bracketing is
// otherwise identical to And's, so an ON clause that was correct before
// renders unchanged.
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

// fromRelation is implemented by a FROM source that is a RELATION this
// package declared, as opposed to an expression a caller composed.
//
// It exists because the resolver walks EXPRESSIONS and does not walk
// RELATIONS, and the two meet in exactly one place: FromExpr takes a
// drops.Expression, and a *Table is one. A table handed to the
// documented way of comma-joining therefore entered the statement as an
// expression, was offered to resolveExpr, matched none of its arms —
// none of them is about relations — and was rendered as a bare relation
// name. Its tenant axis and its authz guard reached nothing:
// SELECT ... FROM "orders", "accounts" read every tenant's accounts, on
// a ctx carrying no tenant at all and without refusing.
//
// It is an interface, and unexported, for the reason resolveExpr's arms
// are: the question is what a value IS in the statement — a relation or
// an expression — and a type assertion on *Table somewhere in the
// resolver would be one more list to keep current. A caller outside the
// package cannot implement it, which is the honest statement that drops
// can scope the relations it declared and no others.
type fromRelation interface {
	drops.Expression

	// fromTable returns the table this source names, so the statement
	// can scope it the way it scopes its From table.
	fromTable() *Table
}

// fromRelations returns every relation the FROM clause names directly:
// the From table, then every FromExpr source that is one.
//
// They share one placement decision because they share one position. A
// comma join IS an inner join, so its side is neither preserved nor
// nullable, and joinKind.filterPlacement's INNER arm already says where
// such a predicate goes: the WHERE clause. What decides otherwise is
// the same thing that decides it for the From table — a RIGHT JOIN
// later in the statement makes the whole comma-joined left side
// nullable — which is fromFilterJoin's question, asked once for all of
// them. A FULL JOIN leaves nowhere correct, and resolveCtx refuses each
// of them alike.
//
// It hands back a slice rather than taking a callback, and that is not
// a style choice: a callback would carry s.from and s.fromExprs into
// the ctx inside a closure, where the taint walk in
// TestNoRenderedExpressionListIsInvisibleToTheResolver cannot follow
// them, and the resolver would look — mechanically — as though it only
// glanced at the fields the renderer walks. The check said so the first
// time this was written with a callback.
func (s *SelectBuilder) fromRelations() []*Table {
	out := make([]*Table, 0, 1+len(s.fromExprs))
	if s.from != nil {
		out = append(out, s.from)
	}
	for _, e := range s.fromExprs {
		if r, ok := e.(fromRelation); ok {
			if t := r.fromTable(); t != nil {
				out = append(out, t)
			}
		}
	}
	return out
}

// From sets the FROM clause. Required before execution.
func (s *SelectBuilder) From(t *Table) *SelectBuilder { s.from = t; return s }

// FromExpr appends an arbitrary FROM source — a subquery, CTE
// reference, set-returning function, etc. Multiple FROMs are
// comma-joined (i.e. cross-joined).
//
// A *Table handed here is a RELATION and is scoped as one: its
// DefaultFilters and its ContextFilters are carried by the statement
// exactly as [SelectBuilder.From]'s are, because a comma join is an
// inner join and the two sources are in the same position — see
// fromRelation for why that had to be said in code rather than left to
// the reader.
//
// Anything else is an expression the caller composed, and drops scopes
// what is inside it (a subquery is resolved as the statement it is) and
// nothing about the relation it names.
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
// The FROM table is the nullable side of this join, so its own
// predicates go the other way, into this join's ON clause: in the WHERE
// clause a guard on the FROM table is false for every unmatched row of
// t — the FROM table's columns are NULL there — and the RIGHT JOIN
// silently degenerates into an INNER JOIN, dropping exactly the rows of
// t it exists to keep. That is the LEFT JOIN failure with the sides
// exchanged, and it is why the FROM table's placement is a function of
// the whole statement rather than a constant. See fromFilterJoin.
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
// The FROM table of a statement containing a FULL JOIN is in the same
// position, for the mirrored reason — the WHERE clause drops t's
// unmatched rows, the ON clause lets the FROM table's through
// unfiltered — so its context filters are refused here too, and its
// DefaultFilters stay in the WHERE clause. See fromFilterJoin for why
// that trade lands the other way round on the FROM side.
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
//
// A predicate that does not parenthesise itself is parenthesised here
// when it would otherwise reassociate the clause — a [drops.Raw] whose
// top level is an OR, most of all, since AND binds tighter and the
// automatic predicates around it would end up on the wrong side of it.
// See writeAnd for the shapes that get brackets and the one that
// deliberately does not.
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

// WriteSQL renders the SELECT into a Builder bare — no parentheses of
// its own — and with no ctx to resolve against, so the FROM table's
// DefaultFilters are written and its ContextFilters are not.
//
// This sentence used to claim the opposite, that the statement was
// wrapped so the builder could be embedded as a subquery. It never
// was, and it is the sentence a reader consults when deciding whether
// to wrap: Eq(col, sel) rendered "= SELECT ..." and FromExpr(sel)
// rendered "FROM SELECT ...", both of which PostgreSQL rejects with
// 42601. Wrapping here instead would have parenthesised every
// statement this package renders, top-level ones included, so the
// wrappers stay explicit: [Subquery] for a scalar operand,
// [SelectBuilder.AsSubquery] for an aliased FROM source, [Exists] and
// [In] for the shapes that bring their own parentheses.
//
// Rendering is not how a statement gets its context filters — see
// [SelectBuilder.ToSQLCtx] and resolveCtx, which resolve them against
// a ctx and hand this method an already-scoped builder.
func (s *SelectBuilder) WriteSQL(b *drops.Builder) {
	ctes, recursive := s.hoistCTEs()
	writeCTEs(b, ctes, recursive)
	s.writeSetOps(b)
}

// writeSetOps renders the statement body and its set-operation
// continuations, without the WITH prefix hoistCTEs has already gathered.
//
// An operand is parenthesised when splicing it in bare would let the
// statement around it take something of the operand's for its own:
//
//   - its own set operations, because set operations are
//     left-associative and flattening changes what the statement means:
//     A EXCEPT (B UNION C) written out flat is (A EXCEPT B) UNION C,
//     which returns rows of C that A never contained. Before this the
//     operand's continuations were not written at all —
//     A.Union(B.Union(C)) sent A UNION B and C's rows went missing with
//     no error anywhere.
//   - its own row bounds, because SQL reads a trailing ORDER BY or
//     LIMIT as belonging to the whole set operation: A UNION SELECT ...
//     LIMIT 5 returns five rows of the union rather than all of A and
//     five of B, which is the shape the caller wrote.
func (s *SelectBuilder) writeSetOps(b *drops.Builder) {
	s.writeCore(b)
	for _, op := range s.setOps {
		b.WriteByte(' ')
		b.WriteString(op.kind)
		b.WriteByte(' ')
		if len(op.right.setOps) == 0 && !op.right.boundsOwnRows() {
			op.right.writeCore(b)
			continue
		}
		b.WriteByte('(')
		op.right.writeSetOps(b)
		b.WriteByte(')')
	}
}

// boundsOwnRows reports whether the statement carries row bounds of its
// own — an ORDER BY, a LIMIT, an OFFSET — which as a set-operation
// operand have to stay inside parentheses to keep meaning the operand
// rather than the set operation.
//
// The mirror of this is still open and fails loudly rather than
// quietly: the *left* operand is the statement itself, so its bounds
// render before the first continuation and PostgreSQL rejects the
// statement outright. Which of the two readings a chained
// A.Union(B).Limit(10) means — bound A, or bound the union — is an API
// question rather than a rendering one, and is not answered here.
func (s *SelectBuilder) boundsOwnRows() bool {
	return s.limit != nil || s.offset != nil || len(s.orderBys) > 0
}

// fromFilterJoin says where the FROM table's own automatic predicates —
// its DefaultFilters and its resolved ContextFilters — have to be
// written, and returns the index of the join whose ON clause has to
// carry them, or -1 for the WHERE clause.
//
// It is joinKind.filterPlacement's mirror. That function reasons about
// the joined table, whose placement depends on its own join's kind;
// this one reasons about the FROM table, whose placement depends on the
// kinds of every join in the statement, because it is the FROM table
// that every one of them joins against.
//
// With no joins, or with only INNER and LEFT joins, the FROM table is
// either the inner side or the preserved one, and the WHERE clause is
// where its predicates have always belonged. A RIGHT JOIN inverts that:
// the FROM table becomes the nullable side, and a predicate on a
// nullable side is false for every unmatched row, so a tenant guard in
// the WHERE clause deletes exactly the rows of the joined table the
// RIGHT JOIN exists to keep — the LEFT JOIN degeneration, mirrored, and
// the reason the LEFT JOIN half of filterPlacement exists at all.
//
// The ON clause that carries them is the first RIGHT JOIN's, and it has
// to be the first one: that is where the FROM table enters the join
// tree, so restricting it there restricts it before any preserved side
// is added, and every later join sees the restricted relation. In a
// later join's ON clause the same predicate would instead be false for
// the rows an earlier outer join had already NULL-extended, which
// silently drops them.
//
// A FULL JOIN preserves both sides, so neither placement is right for
// the FROM table either — the ON clause lets its unmatched rows through
// unfiltered and the WHERE clause drops the full-joined table's. Its
// context filters are therefore refused in resolveCtx with
// [ErrFullJoinScoped], the same answer the joined side gets. Its
// DefaultFilters stay in the WHERE clause: they are a default scope
// rather than a boundary, and dropping them would let a FULL JOIN read
// the FROM table's soft-deleted rows, which is worse than the
// over-filtering they cause. That is the same trade [SelectBuilder.FullJoin]
// documents for the joined side, decided the other way because the two
// failures are not the same failure.
func (s *SelectBuilder) fromFilterJoin() int {
	return s.firstJoinOfKind(rightJoin)
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
	// The FROM table's own automatic predicates, and the join whose ON
	// clause has to carry them — the WHERE clause when there is none.
	var fromDefaults, fromCtx []drops.Expression
	fromOn := -1
	if !s.unscoped {
		fromCtx = s.ctxFrom
		for _, rel := range s.fromRelations() {
			switch dfs := s.autoDefaults(rel); {
			case len(dfs) == 0:
			case fromDefaults == nil:
				// The single-relation statement keeps the list it
				// always rendered, byte for byte.
				fromDefaults = dfs
			default:
				fromDefaults = append(append([]drops.Expression(nil), fromDefaults...), dfs...)
			}
		}
		if len(fromDefaults) > 0 || len(fromCtx) > 0 {
			fromOn = s.fromFilterJoin()
		}
	}
	// autoWheres are the automatic predicates that belong in the WHERE
	// clause. The joined tables' are gathered here, as the joins
	// render, so that the placement decision lives in one place with
	// its ON-clause half.
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
		// A join with no condition left — Join(t, nil) against a table
		// that contributed no automatic predicate — renders ON TRUE
		// rather than a dangling "ON". Every join kind PostgreSQL has
		// requires a condition, so the bare keyword was a 42601 the
		// server rejected outright, and it was reachable in three of
		// the five shapes: any INNER or RIGHT join, and any join whose
		// table has no filters to fill the clause. TRUE is what a nil
		// condition says — join everything — so the statement now
		// means what the caller wrote.
		if on == nil {
			on = drops.Raw("TRUE")
		}
		b.WriteString(" ON ")
		b.Append(on)
	}
	// The FROM table's go in front of them, and the whole lot in front
	// of the caller's own predicates — which the resolved context
	// filters then follow. So a statement reads scoping first, intent
	// second, and a query log shows at a glance whether a query was
	// scoped at all.
	if fromOn < 0 {
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
// when there was nothing to resolve, otherwise a shallow copy carrying
// the resolved predicates and the resolved subqueries.
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
// joinKind.filterPlacement — and the FROM table's own placement depends
// on the join kinds present, see fromFilterJoin. A CTE whose body is a
// *SelectBuilder is resolved as the statement it is, so that
// WITH recent AS (SELECT ... FROM posts) is scoped the way a bare
// SELECT from posts would be; so is a statement reached as a subquery
// expression — EXISTS, ANY, a scalar subquery, an aliased FROM source —
// through the same walk, see resolveExpr.
//
// A body drops did not build cannot be: a CTE or a subquery assembled
// out of raw fragments has nothing to resolve against and stays the
// caller's to scope. [CTEDef] and [Exists] say so at the call site.
//
// Unscoped() does not reach into any of those: a CTE body and a
// subquery are statements of their own and keep their own scoping,
// which is also how to unscope one relation of a query and no other.
func (s *SelectBuilder) resolveCtx(ctx context.Context) (*SelectBuilder, error) {
	// A deferred error — a cursor that failed to decode — is reported
	// here rather than only by the executors, because resolution is the
	// one step every path to a server goes through and this statement
	// may be somebody else's CTE body. AfterCursor fails closed by
	// appending a false predicate, so rendering it anyway sends a
	// statement that matches nothing and returns no error at all: the
	// caller reads "the page was empty" where the truth is "the cursor
	// was corrupt". Before the check moved here it was a type assertion
	// in renderForCtx, which saw the statement handed to ExecExpr and no
	// statement inside it.
	if s.err != nil {
		return nil, s.err
	}
	// Resolution is not idempotent, and a resolved statement is reachable
	// twice whenever it is embedded in another one — find.go builds the
	// per-parent-limit rewrite that way. Resolving it again would bind
	// the tenant a second time, which changes no rows and shows up only
	// in the argument list.
	if s.resolved {
		return s, nil
	}
	cp := *s
	changed := false

	if !s.unscoped {
		// Every relation the FROM clause names, not only the From
		// table: a *Table handed to FromExpr is comma-joined, which is
		// an inner join, and it is scoped in the same position and by
		// the same rules. See fromRelations.
		var preds []drops.Expression
		for _, rel := range s.fromRelations() {
			if !rel.hasContextFilters() {
				continue
			}
			if i := s.firstJoinOfKind(fullJoin); i >= 0 {
				return nil, fmt.Errorf("%w: %q is FULL JOINed to the scoped FROM table %q; join a pre-filtered subquery, or say Unscoped",
					ErrFullJoinScoped, s.joins[i].table.Name(), rel.Name())
			}
			p, err := rel.resolveContextFilters(ctx)
			if err != nil {
				return nil, err
			}
			preds = append(preds, p...)
		}
		if len(preds) > 0 {
			cp.ctxFrom, changed = preds, true
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
		// for the statements written inside them. Same list writeCore
		// reads, same reason resolveJoins exists: a predicate the
		// renderer adds on the statement's own account is still a
		// predicate a caller wrote, and a caller can write a subquery
		// into it.
		tables := s.fromRelations()
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
	// scalar, a FROM source, a DISTINCT ON key, a GROUP BY or ORDER BY
	// term.
	//
	// Every list writeCore reads has to be here. distinctOn was not,
	// for as long as DISTINCT ON has existed: writeCore rendered it and
	// resolveCtx never looked at it, so
	// DistinctOn(Subquery(SELECT ... FROM posts)) sent the inner
	// statement with its DefaultFilters and none of its ContextFilters,
	// on a ctx with no tenant at all and without refusing. That is the
	// invariant this list exists to keep — no expression list the
	// renderer reads may be invisible to the resolver — and
	// TestNoRenderedExpressionListIsInvisibleToTheResolver enforces it
	// against the package source, so the next field added to the
	// builder and rendered fails on the day it is written.
	lists := []struct {
		src []drops.Expression
		dst *[]drops.Expression
	}{
		{s.columns, &cp.columns},
		{s.distinctOn, &cp.distinctOn},
		{s.fromExprs, &cp.fromExprs},
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

	opsCopied := false
	for i, op := range s.setOps {
		right, err := op.right.resolveCtx(ctx)
		if err != nil {
			return nil, err
		}
		if right == op.right {
			continue
		}
		if !opsCopied {
			cp.setOps, opsCopied, changed = append([]setOp(nil), s.setOps...), true, true
		}
		cp.setOps[i].right = right
	}

	ctes, err := resolveCTEs(ctx, s.ctes)
	if err != nil {
		return nil, err
	}
	if ctes != nil {
		cp.ctes, changed = ctes, true
	}

	out := s
	if changed {
		cp.resolved = true
		out = &cp
	}
	if err := out.checkCTENames(); err != nil {
		return nil, err
	}
	return out, nil
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
			return nil, nil, fmt.Errorf("%w: %q; join it pre-filtered, or say Unscoped",
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
//
// A conjunct is parenthesised when, and only when, leaving it bare
// would let it bind something other than itself — see escapesConjunct.
// The alternative designs are worth writing down, because the obvious
// one is wrong:
//
//   - Bare " AND " between every predicate, which is what this did, is
//     fail-open in the one feature sold as fail-closed. OR binds looser
//     than AND, so a predicate whose own top level is an OR
//     reassociates the whole clause:
//     "(deletedAt IS NULL) AND a = 1 OR b = 2 AND (tenantId = $1)"
//     parses as "(D AND a) OR (b AND T)" and returns every row matching
//     the first half for every tenant. The guard is in the statement,
//     which is exactly why a review that looks for the predicate finds
//     it and passes.
//   - Parenthesising every conjunct unconditionally is correct, but
//     nearly every predicate this package builds already renders as one
//     bracketed group — Eq gives ("users"."id" = $1) — so it would
//     double the parentheses in every statement drops has ever
//     generated, and make every query log worse forever, to defend
//     against the one predicate shape that does not self-parenthesise.
//   - Documenting that a raw predicate must parenthesise itself puts the
//     burden on the caller least able to know about it, and is the
//     default that produced this defect.
//
// So the conjunct is rendered once to see what it looks like and
// bracketed only if its shape can reach outside itself. That costs one
// extra render of each predicate of a multi-predicate clause — the
// price of a guarantee that does not depend on the caller.
func writeAnd(b *drops.Builder, preds []drops.Expression) {
	// A lone conjunct is the whole clause: there is nothing on either
	// side of it to reassociate, so it is written as the caller wrote it.
	bracket := len(preds) > 1
	for i, p := range preds {
		if i > 0 {
			b.WriteString(" AND ")
		}
		if bracket {
			writeConjunct(b, p)
			continue
		}
		b.Append(p)
	}
}

// writeConjunct writes p bracketed when leaving it bare would let it
// reach past its own position, and as the caller wrote it otherwise.
func writeConjunct(b *drops.Builder, p drops.Expression) {
	if escapesConjunct(p) {
		b.WriteByte('(')
		b.Append(p)
		b.WriteByte(')')
		return
	}
	b.Append(p)
}

// bracketConjunct returns p parenthesised when leaving it bare in a
// conjunction would let it reach past its own position, and p itself
// otherwise. It is writeConjunct's decision, taken once when the
// expression is built rather than each time it is rendered.
//
// And, Or and Not take it, because a connective is a clause in
// miniature and used to join its operands with a bare separator:
// And(drops.Raw("a OR b"), guard) reassociated to "a OR (b AND guard)"
// precisely as the WHERE clause did before writeAnd bracketed anything,
// which left the tenant guard in the statement binding nothing. Not was
// worse — NOT binds tighter than every connective a caller can put
// inside it, so Not(drops.Raw("a OR b")) rendered "(NOT a OR b)", which
// negates half of what the caller wrote.
//
// Deciding at construction rather than at render is what keeps a
// predicate tree affordable. The check reads the operand's rendered
// text, so a nested tree checked at render time would render each
// subtree once per level, per render, and a predicate built from user
// input a dozen combinators deep would cost more to bracket than to
// execute. The answer cannot change afterwards: it depends on the
// operand's shape, and the only thing resolution substitutes is a
// scoped copy of a statement — which adds bracketed conjuncts inside a
// SELECT and so can neither introduce a top-level OR nor unbalance a
// parenthesis.
func bracketConjunct(p drops.Expression) drops.Expression {
	if escapesConjunct(p) {
		return parens(p)
	}
	return p
}

// bracketConjuncts applies bracketConjunct to a whole list, into a new
// slice so the caller's variadic backing array is not written through.
func bracketConjuncts(preds []drops.Expression) []drops.Expression {
	out := make([]drops.Expression, len(preds))
	for i, p := range preds {
		out[i] = bracketConjunct(p)
	}
	return out
}

// escapesConjunct reports whether p, written bare between two " AND "s,
// could reach past its own position in the clause.
//
// Three shapes can, and all three end with a scoping predicate that is
// present in the SQL and binding nothing:
//
//   - a top-level OR, which binds looser than AND and so takes the
//     conjuncts on either side with it;
//   - a line or block comment, which swallows whatever follows it —
//     "a = 1 --" drops every later conjunct, tenant guard included;
//   - a statement terminator, which ends the statement early and leaves
//     the guard in a second one nobody executes.
//
// The check is on p's rendered text because that is the only thing
// that can be checked: a predicate arrives as an opaque Expression, and
// the ones that need bracketing are precisely the ones drops did not
// build — a [drops.Raw], a caller's own [drops.ExprFunc].
//
// A fragment whose parentheses do not balance is the one shape reported
// as safe without being read, on purpose. It is not an expression, so
// bracketing cannot repair it. There are two ways such a fragment can
// be written, and only one of them is answered by design; both are
// stated here because the second reads as safe and is not, quite.
//
// The first escapes SIDEWAYS: "a = 1) OR (1 = 1" closes the conjunct's
// own parenthesis, ORs past it, and opens another so the whole clause
// balances again. Bracketing that would close the injected parenthesis
// and turn a statement PostgreSQL refuses into a working, unscoped OR.
// Left alone it stays a syntax error, which is the fail-closed answer,
// and it is the reason this shape is not bracketed.
//
// The second escapes UPWARDS: "1) --" written into a conjunct of a
// subquery closes the subquery and comments out everything after it,
// the tenant guard included. Bracketing would not repair that one
// either — the parenthesis it opens is closed again before the comment
// begins — so the fragment is rendered as written. What stops it is not
// this check: the arguments bound for the commented-out tail have no
// placeholders left to fill, and the driver rejects the statement on
// the argument count. That is fail-closed BY ACCIDENT rather than by
// design, and an injection with no arguments after it would not be
// caught by it at all.
//
// Both live entirely inside [drops.Raw], which every dialect documents
// as the caller's to scope: drops re-emits its text and does not parse
// it. Do not build one from request data.
func escapesConjunct(p drops.Expression) bool {
	if p == nil {
		return false
	}
	sql, _ := drops.String(p)
	depth := 0
	loose := false
	for i := 0; i < len(sql); i++ {
		switch c := sql[i]; c {
		case '\'', '"':
			// A string literal or a quoted identifier: its content is
			// text, so a parenthesis or an OR inside it is neither
			// structure nor an operator. The quote character doubled
			// inside the literal escapes itself.
			for i++; i < len(sql); i++ {
				if sql[i] != c {
					continue
				}
				if i+1 < len(sql) && sql[i+1] == c {
					i++
					continue
				}
				break
			}
			if i >= len(sql) {
				// The fragment ends inside the literal, so it could
				// not be read as an expression and nothing about it
				// has been verified. Bracketing is the safe answer:
				// the statement is a syntax error either way, and the
				// alternative is to trust text nobody parsed.
				return true
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return false
			}
		case '-':
			if i+1 < len(sql) && sql[i+1] == '-' {
				return true
			}
		case '/':
			if i+1 < len(sql) && sql[i+1] == '*' {
				return true
			}
		case ';':
			return true
		case 'o', 'O':
			if depth == 0 && wordAt(sql, i, "OR") {
				loose = true
				i++
			}
		}
	}
	return loose && depth == 0
}

// wordAt reports whether the ASCII keyword word (given upper-case)
// starts at sql[i] and stands alone, rather than being part of a longer
// identifier — "OR" is an operator, the OR in "FOR" and in "ORDER" is
// not.
func wordAt(sql string, i int, word string) bool {
	if i+len(word) > len(sql) {
		return false
	}
	for j := 0; j < len(word); j++ {
		c := sql[i+j]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		if c != word[j] {
			return false
		}
	}
	return !identByte(sql, i-1) && !identByte(sql, i+len(word))
}

// identByte reports whether sql[i] can appear inside an SQL identifier.
// An index outside the string is not one, which makes a keyword at
// either end of the fragment stand alone.
func identByte(sql string, i int) bool {
	if i < 0 || i >= len(sql) {
		return false
	}
	c := sql[i]
	return c == '_' || c == '$' || (c >= '0' && c <= '9') ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

// AsSubquery returns a parenthesised, aliased form of the SELECT for use
// as a subquery in another statement.
//
// The result keeps this builder rather than closing over it, so a
// statement that embeds it resolves it against the executing ctx like
// any other subquery — see opExpr. Rendered through [SelectBuilder.ToSQL],
// where there is no ctx to resolve against, it still carries the FROM
// table's DefaultFilters and none of its context filters; that is what
// ToSQL means and why ToSQLCtx is the call to assert on.
func (s *SelectBuilder) AsSubquery(alias string) drops.Expression {
	return &opExpr{parts: []string{"(", ")"}, operands: []drops.Expression{s}, alias: alias}
}
