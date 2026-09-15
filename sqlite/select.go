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
	scope        filterScope

	// ctxFrom and ctxJoins are the context filters of the FROM table
	// and of each joined table, built for one execution by resolveCtx.
	// They are held rather than resolved while rendering because
	// WriteSQL has no ctx to build them from — and kept one slice per
	// join rather than flattened, because where a joined table's
	// predicates may be written depends on the kind of ITS join. See
	// writeJoins.
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

// Unscoped opts out of every global filter on the FROM table — named
// and anonymous alike. The blunt instrument: it cannot tell a
// soft-delete guard from a tenancy one, so a query that only wants
// deleted rows loses its isolation with them. Name what you are
// stepping around with IgnoreFilters instead.
func (s *SelectBuilder) Unscoped() *SelectBuilder { s.scope.unscoped = true; return s }

// IgnoreFilters bypasses the named global filters on the FROM table and
// leaves every other one standing:
//
//	db.Select().From(posts).IgnoreFilters(sqlite.FilterSoftDelete)
//
// Names come from [Table.AddFilter]; drops' own are FilterSoftDelete
// and FilterTenant. A name no filter carries is ignored.
func (s *SelectBuilder) IgnoreFilters(names ...string) *SelectBuilder {
	s.scope.ignore(names...)
	return s
}

type joinClause struct {
	kind  string // "JOIN", "LEFT JOIN", ...
	table *Table
	on    drops.Expression
}

// From sets the table to select from.
func (s *SelectBuilder) From(t *Table) *SelectBuilder { s.table = t; return s }

// Distinct adds the DISTINCT keyword.
func (s *SelectBuilder) Distinct() *SelectBuilder { s.distinct = true; return s }

// Join adds an INNER JOIN ... ON.
func (s *SelectBuilder) Join(t *Table, on drops.Expression) *SelectBuilder {
	s.joins = append(s.joins, joinClause{"JOIN", t, on})
	return s
}

// LeftJoin adds a LEFT JOIN ... ON.
func (s *SelectBuilder) LeftJoin(t *Table, on drops.Expression) *SelectBuilder {
	s.joins = append(s.joins, joinClause{"LEFT JOIN", t, on})
	return s
}

// Where AND-s the given predicates onto the statement. Nil predicates
// are ignored, so a caller can pass one that is only sometimes present
// without first collecting the non-nil ones into a slice.
func (s *SelectBuilder) Where(preds ...drops.Expression) *SelectBuilder {
	s.wheres = append(s.wheres, dropNilPreds(preds)...)
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
	defaults, ctxPreds := s.writeJoins(b)
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

// writeJoins renders the join list and returns the automatic
// predicates that belong in the WHERE clause instead — the render-time
// ones first, then the request-time ones.
//
// Where each table's predicates go is a correctness question rather
// than a stylistic one. A LEFT JOIN preserves rows of the FROM side
// that did not match, and a predicate on the JOINED table written in
// the WHERE clause is evaluated after the join has NULL-extended those
// rows — so it is false for exactly the rows the LEFT JOIN exists to
// keep, and the join silently becomes an inner one. Those predicates
// move into that join's ON clause, where they filter the joined
// relation before the preservation happens. An INNER JOIN preserves
// nothing, so its table's predicates stay in the WHERE clause, which is
// where all of them have always been.
func (s *SelectBuilder) writeJoins(b *drops.Builder) (defaults, ctxPreds []drops.Expression) {
	defaults = append(defaults, s.scope.filtersOf(s.table, s.defaults)...)
	ctxPreds = append(ctxPreds, s.ctxFrom...)
	for i, j := range s.joins {
		b.WriteByte(' ')
		b.WriteString(j.kind)
		b.WriteByte(' ')
		j.table.writeFrom(b)
		b.WriteString(" ON ")
		joinPreds := s.scope.filtersOf(j.table, s.defaults)
		if i < len(s.ctxJoins) {
			joinPreds = append(joinPreds, s.ctxJoins[i]...)
		}
		var onExtra []drops.Expression
		if j.kind == "LEFT JOIN" {
			onExtra = joinPreds
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
	return defaults, ctxPreds
}

// ToSQL renders the statement with SQLite placeholders.
//
// It carries the table's DefaultFilters and none of its
// ContextFilters, because a render has no ctx to build one from — so
// on a tenant-scoped table it is not the statement that would be sent.
// Prefer [SelectBuilder.ToSQLCtx].
func (s *SelectBuilder) ToSQL() (sql string, args []any) { return ToSQL(s) }

// ToSQLCtx renders the statement ctx would send: the context filters of
// the FROM table and of every joined table built, walked and AND-ed in,
// and every statement written inside the query — a subquery operand, a
// CTE body — resolved on the same terms.
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
	// be written in any of them — a projection, a predicate, an
	// ordering key.
	if r, err := resolveExprs(ctx, s.columns); err != nil {
		return nil, err
	} else if r != nil {
		cp.columns, changed = r, true
	}
	if r, err := resolveExprs(ctx, s.wheres); err != nil {
		return nil, err
	} else if r != nil {
		cp.wheres, changed = r, true
	}
	if r, err := resolveExprs(ctx, s.orderBy); err != nil {
		return nil, err
	} else if r != nil {
		cp.orderBy, changed = r, true
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
		tables := []*Table{s.table}
		preds, err := s.table.resolveContextFilters(ctx)
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
// one slice per join rather than flattened — see writeJoins for why the
// shape is the point.
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
