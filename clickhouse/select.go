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
	// keepCtx says that unscoped means the table's DECLARATION-time
	// filters only, and that the request-time ones still apply.
	//
	// It is what separates [EntityQuery.Unscoped] from
	// [SelectBuilder.Unscoped]. "Include the rows the default scope
	// hides" is the thing callers reach for, and on the entity path it
	// must not also mean "and every other tenant's": an entity is the
	// typed, scoped surface, so the axis survives an opt-out aimed at
	// the guards. The raw builder's Unscoped stays the blunt
	// instrument it documents itself as.
	keepCtx bool
	err     error // deferred error (e.g. cursor decode failure) surfaced at Rows()
	ctes    []*CTE

	// ctxFrom and ctxJoins are the context filters of the FROM table
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

// dropsContextFilters reports whether this statement's opt-out reaches
// the request-time filters as well as the render-time ones.
func (s *SelectBuilder) dropsContextFilters() bool { return s.unscoped && !s.keepCtx }

// unscopeDefaults opts out of the table's DECLARATION-time filters and
// keeps its request-time ones. It is what [EntityQuery.Unscoped]
// reaches for; [SelectBuilder.Unscoped] stays the blunt instrument.
func (s *SelectBuilder) unscopeDefaults() *SelectBuilder {
	s.unscoped, s.keepCtx = true, true
	return s
}

// defaultsOf returns the render-time filters of t that apply to this
// statement, resolved for this execution where they had to be.
func (s *SelectBuilder) defaultsOf(t *Table) []drops.Expression {
	if t == nil || s.unscoped || !t.hasDefaultFilters() {
		return nil
	}
	return s.defaults.of(t)
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
	// Held rather than closed over, like every other operand in this
	// package: a Param IS the bound value, and a closure around
	// AddArg is the shape the census exists to stop spreading.
	s.sampleBy = drops.Param{Value: e}
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
	s.prewheres = append(s.prewheres, dropNilPreds(preds)...)
	return s
}

// Where appends predicates joined by AND. Nil predicates are ignored,
// so a caller can pass one that is only sometimes present without
// first collecting the non-nil ones into a slice.
func (s *SelectBuilder) Where(preds ...drops.Expression) *SelectBuilder {
	s.wheres = append(s.wheres, dropNilPreds(preds)...)
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
	s.havings = append(s.havings, dropNilPreds(preds)...)
	return s
}
func (s *SelectBuilder) OrderBy(exprs ...drops.Expression) *SelectBuilder {
	s.orderBys = append(s.orderBys, exprs...)
	return s
}
func (s *SelectBuilder) Limit(n int64) *SelectBuilder  { s.limit = &n; return s }
func (s *SelectBuilder) Offset(n int64) *SelectBuilder { s.offset = &n; return s }

// Setting appends a "key = value" pair to the SETTINGS clause.
//
// Both halves are raw SQL and are checked to be one setting, on the
// terms [Table.Setting] states: a comma outside a literal would make
// the value this setting and whatever follows it. A pair that fails
// the check panics, so a caller passing something that did not come
// from them — a key out of a request, a value out of configuration —
// should not hand it here unfiltered.
func (s *SelectBuilder) Setting(key, value string) *SelectBuilder {
	mustSettingKey(key)
	mustSettingValue(key, value)
	s.settings = append(s.settings, key+" = "+value)
	return s
}

// WriteSQL renders the SELECT.
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
	// Where each table's automatic predicates go — see filterPlacement.
	onExtra, defaults, ctxPreds := s.placeFilters()
	for i, j := range s.joins {
		b.WriteByte(' ')
		b.WriteString(string(j.kind))
		b.WriteByte(' ')
		j.table.writeFrom(b)
		b.WriteString(" ON ")
		// A nil condition is the empty conjunction, rendered where the
		// grammar insists on a predicate. When the join carries
		// predicates of its own, they ARE the condition — a leading
		// true would be noise in the one place a reviewer reads
		// closely.
		switch {
		case j.on == nil && len(onExtra[i]) == 0:
			b.Append(orTrue(nil))
		case j.on == nil:
			writeAnd(b, onExtra[i])
		case len(onExtra[i]) == 0:
			b.Append(j.on)
		default:
			// Through writeAnd rather than And, so the ON clause reads
			// as a WHERE clause does — the terms joined by AND with no
			// bracket around the lot, and each term bracketed only if
			// its own shape could reach past it.
			writeAnd(b, append([]drops.Expression{j.on}, onExtra[i]...))
		}
	}
	if len(s.prewheres) > 0 {
		b.WriteString(" PREWHERE ")
		// Bracketed on shape rather than on count, unlike every other
		// clause: a PREWHERE stands in FRONT of the WHERE the guards
		// are written into, so a LONE conjunct that opens a comment
		// swallows them. writeAnd leaves a lone conjunct as the caller
		// wrote it, which is right where the clause is the last thing
		// on the line and wrong here.
		writeAnd(b, bracketOperands(s.prewheres))
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
var ErrFullJoinScoped = errors.New("drops/clickhouse: a FULL JOIN has nowhere to put a table's context filters")

// ErrAsofJoinScoped is the same refusal for ASOF, and it is refused for
// two reasons rather than one.
//
// ASOF keeps the CLOSEST match per left row rather than every match, so
// a predicate in the WHERE clause is evaluated after that choice has
// been made: the guard does not narrow which row is chosen, it deletes
// the left row whose chosen match the guard hides — a row that has a
// permitted match further away, and that a correctly scoped query
// returns.
//
// And the ON clause cannot take it either. ASOF's ON is a grammar and
// not a predicate: a run of equalities followed by exactly one
// inequality, which has to be the last term. A guard appended there is
// a syntax error at best and changes which term ClickHouse reads as the
// inequality at worst.
//
// So both clauses are wrong, and the statement is refused with the same
// two ways through the FULL JOIN has.
var ErrAsofJoinScoped = errors.New("drops/clickhouse: an ASOF JOIN has nowhere to put a table's context filters")

// checkJoinPlacement refuses the shapes the two errors above describe.
//
// Only context filters count: a default filter is rendered by WriteSQL
// and has been landing in the WHERE clause since long before this, so
// changing where it goes belongs to a round that can verify it.
func (s *SelectBuilder) checkJoinPlacement() error {
	for _, j := range s.joins {
		var err error
		switch j.kind {
		case fullJoin:
			err = ErrFullJoinScoped
		case asofJoin:
			err = ErrAsofJoinScoped
		default:
			continue
		}
		// Which sides are a problem differs between the two.
		//
		// A FULL JOIN preserves both, so a guard on either has nowhere
		// to go. An ASOF join drops unmatched left rows like an inner
		// one, so the FROM table's guard in the WHERE clause selects
		// exactly the pairs it should; it is the JOINED table's that
		// has neither clause available.
		sides := []*Table{j.table}
		if j.kind == fullJoin {
			sides = append(sides, s.from)
		}
		for _, t := range sides {
			if !t.hasContextFilters() {
				continue
			}
			return fmt.Errorf("%w: %q carries them; join a pre-filtered subquery, or say Unscoped and write the predicate at the query",
				err, t.relRef())
		}
	}
	return nil
}

// bracketOperands wraps each expression that could re-associate with
// what follows it. See bracketOperand.
func bracketOperands(exprs []drops.Expression) []drops.Expression {
	out := make([]drops.Expression, len(exprs))
	for i, e := range exprs {
		out[i] = bracketOperand(e)
	}
	return out
}

// filterPlacement returns the join whose ON clause a table's automatic
// predicates belong in, and -1 for the WHERE clause.
//
// after is the position the table enters the statement at: -1 for the
// FROM table, and the join's own index for a joined table.
//
// It is a correctness question rather than a stylistic one. An outer
// join preserves rows that did not match, and a predicate on the
// preserved side written in the WHERE clause is evaluated AFTER the
// join has NULL-extended those rows — so it is false for exactly the
// rows the outer join exists to keep, and the join silently becomes an
// inner one. The predicate has to move into an ON clause, where it
// filters its relation before the preservation happens.
//
// A LEFT- or FULL-joined table's predicates go in its own ON: those
// joins preserve the other side, and this table's rows are the ones a
// WHERE would delete.
//
// An ANY-joined table's go there too, and that half is ClickHouse's
// own. ANY keeps ONE arbitrary match per left row and picks it while
// the join runs, so a guard applied afterwards does not narrow which
// row is picked — it deletes the pair whose pick happened to belong to
// somebody else, dropping rows the caller is entitled to, and dropping
// them non-deterministically.
//
// Everything else asks the mirror-image question — is there a RIGHT or
// FULL join AFTER this table enters? — because from that point on it is
// everything to the left that is preserved.
func (s *SelectBuilder) filterPlacement(after int, kind joinKind) int {
	if kind == leftJoin || kind == fullJoin || kind == anyJoin {
		return after
	}
	for i := after + 1; i < len(s.joins); i++ {
		if k := s.joins[i].kind; k == rightJoin || k == fullJoin {
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

	place(s.filterPlacement(-1, innerJoin), s.defaultsOf(s.from), s.ctxFrom)
	for i, j := range s.joins {
		var ctx []drops.Expression
		if i < len(s.ctxJoins) {
			ctx = s.ctxJoins[i]
		}
		place(s.filterPlacement(i, j.kind), s.defaultsOf(j.table), ctx)
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
// failed to decode must not reach the server as the false predicate it
// fails closed with, which matches nothing and reports nothing.
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
	// be written in any of them.
	for _, l := range []struct {
		src []drops.Expression
		dst *[]drops.Expression
	}{
		{s.columns, &cp.columns},
		{s.prewheres, &cp.prewheres},
		{s.wheres, &cp.wheres},
		{s.groupBys, &cp.groupBys},
		{s.havings, &cp.havings},
		{s.orderBys, &cp.orderBys},
		// SAMPLE takes an expression, and a caller may hand it a
		// statement like any other operand position.
		{[]drops.Expression{s.sampleBy}, nil},
	} {
		if l.dst == nil {
			if s.sampleBy == nil {
				continue
			}
			r, ch, err := resolveExpr(ctx, s.sampleBy)
			if err != nil {
				return nil, err
			}
			if ch {
				cp.sampleBy, changed = r, true
			}
			continue
		}
		r, err := resolveExprs(ctx, l.src)
		if err != nil {
			return nil, err
		}
		if r != nil {
			*l.dst, changed = r, true
		}
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
	if !s.dropsContextFilters() {
		// Inside the branch Unscoped turns off, because Unscoped is
		// the documented way past the refusal.
		if err := s.checkJoinPlacement(); err != nil {
			return nil, err
		}
		tables := []*Table{s.from}
		preds, err := s.from.resolveContextFilters(ctx)
		if err != nil {
			return nil, err
		}
		if len(preds) > 0 {
			cp.ctxFrom, changed = preds, true
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

// ToSQL renders the statement using the ClickHouse placeholder style.
func (s *SelectBuilder) ToSQL() (sql string, args []any) {
	// The whole Dialect, not just Placeholder: the placeholder style
	// is the same either way, but identifier quoting is not. A Builder
	// with no Dialect falls back to drops.StdQuoteIdent, which doubles
	// the quote and leaves backslashes alone — see quoteIdent for the
	// name that then arrives at the server, and ToSQL for the same
	// note.
	b := drops.NewBuilder(drops.WithDialect(Dialect))
	s.WriteSQL(b)
	return b.SQL()
}

// Rows runs the SELECT and returns the raw cursor, with the tables'
// context filters resolved against ctx first.
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
