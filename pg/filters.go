package pg

import "github.com/bernardoforcillo/drops"

// A global filter is a predicate drops AND-s into every SELECT, UPDATE
// and DELETE against a table without the call site asking for it — a
// soft-delete guard, a tenancy axis, a "only rows this account may
// see" rule. The value of such a filter is that it cannot be
// forgotten; the danger is that when a query needs to step around one
// of them, it steps around all of them at once.
//
// That is what Unscoped does, and why it is not enough on its own. A
// report that legitimately wants soft-deleted rows reaches for
// Unscoped, and silently loses the tenancy predicate with it — the
// caller asked to see deleted rows and got another customer's. So
// filters carry names, and a query bypasses them one at a time:
//
//	t.AddFilter(pg.FilterSoftDelete, pg.IsNull(deletedAt))
//	t.AddFilter(pg.FilterTenant, tenantGuard)
//
//	// deleted rows, still only this tenant's:
//	db.Select().From(t).IgnoreFilters(pg.FilterSoftDelete)
//
// Unscoped stays, as the blunt instrument: it drops every filter the
// table registers, named or not. Reach for it when you mean "no
// scoping at all" — a migration, a backfill, an integrity check — and
// for nothing narrower.
//
// Filters registered through the older [Table.DefaultFilter] are
// anonymous: nothing can bypass them but Unscoped. Prefer AddFilter,
// so the predicate you install today can be stepped around by name
// tomorrow.

// Names of the filters drops installs itself. They are constants so a
// caller references a symbol instead of retyping a string the compiler
// never sees.
const (
	// FilterSoftDelete names the "deletedAt IS NULL" guard installed
	// by [SoftDeleteMixin].
	FilterSoftDelete = "softDelete"

	// FilterTenant names the isolation predicate installed by
	// [Entity.ScopeByTenant]. Unlike the table-level filters it is
	// built per query from the ctx tenant, so [SelectBuilder.Unscoped]
	// does not reach it — see [EntityQuery.IgnoreFilters].
	FilterTenant = "tenant"
)

// tableFilter is one registered global filter. name is empty for the
// anonymous filters DefaultFilter installs, which no IgnoreFilters
// call can name and therefore only Unscoped drops.
type tableFilter struct {
	name string
	pred drops.Expression
}

// filterScope is the query-side half of the mechanism: what this one
// statement has opted out of. The zero value opts out of nothing, so a
// builder that never mentions filters behaves exactly as it did before
// names existed.
type filterScope struct {
	unscoped bool
	ignored  map[string]struct{}
}

// ignore records names this statement bypasses. An unknown name is
// kept rather than rejected — the builder may not know its table yet,
// and a typo that leaves a filter in place fails closed (the query
// returns too few rows) instead of open.
func (s *filterScope) ignore(names ...string) {
	if s.ignored == nil {
		s.ignored = make(map[string]struct{}, len(names))
	}
	for _, n := range names {
		if n == "" {
			continue
		}
		s.ignored[n] = struct{}{}
	}
}

// ignores reports whether name was passed to IgnoreFilters on this
// statement. Unscoped is deliberately not folded in: it is a statement
// about the table's own filters, and the tenant guard that consults
// this is not one of them.
func (s filterScope) ignores(name string) bool {
	if name == "" {
		return false
	}
	_, ok := s.ignored[name]
	return ok
}

// apply prepends t's surviving filters to wheres. Filters lead so the
// rendered WHERE reads scope-first, which is also where they have
// always been.
//
// defaults is what this execution resolved for t, and is nil on the
// ToSQL path and whenever no default filter had a statement inside it
// — in which case the render-time list is used, unchanged and byte for
// byte. See resolvedDefaults.
func (s filterScope) apply(t *Table, wheres []drops.Expression, defaults resolvedDefaults) []drops.Expression {
	kept := s.filtersOf(t, defaults)
	if len(kept) == 0 {
		return wheres
	}
	return append(kept, wheres...)
}

// filtersOf returns the default filters of t that survive this
// statement's opt-outs, restated for the instance of the table the
// statement names.
//
// It is the one place that answers "which of this table's render-time
// predicates apply here", so the renderers that place them differently
// — a joined table's go in its ON clause, the FROM table's in the
// WHERE — ask the same question and get the same answer.
func (s filterScope) filtersOf(t *Table, defaults resolvedDefaults) []drops.Expression {
	if t == nil || s.unscoped || !t.hasDefaultFilters() {
		return nil
	}
	// Through the table's scope rather than off the table, so an alias
	// applies the guards its table carries now, and restated so that
	// the handles they were declared with resolve to this alias — see
	// tableScope and resolveFilterExprs.
	filters := defaults.of(t)
	kept := make([]drops.Expression, 0, len(filters))
	for _, f := range filters {
		if s.ignores(f.name) {
			continue
		}
		kept = append(kept, f.pred)
	}
	return kept
}

// applyAll is apply over every table the statement names, in the order
// it names them.
//
// A DELETE ... USING and an UPDATE ... FROM join a second table in, and
// its rows choose which of the target's rows the statement touches — so
// a soft-delete guard on the joined table decides what gets deleted as
// surely as one on the target does. Applying only the target's was how
// a DELETE joined against a table full of soft-deleted rows removed the
// rows those referred to.
func (s filterScope) applyAll(tables []*Table, wheres []drops.Expression, defaults resolvedDefaults) []drops.Expression {
	for i := len(tables) - 1; i >= 0; i-- {
		// Backwards, because each apply PREPENDS: walking the list in
		// reverse leaves the filters in the order the statement names
		// the tables.
		wheres = s.apply(tables[i], wheres, defaults)
	}
	return wheres
}
