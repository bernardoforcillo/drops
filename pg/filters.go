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
func (s filterScope) apply(t *Table, wheres []drops.Expression) []drops.Expression {
	if t == nil || s.unscoped || len(t.filters) == 0 {
		return wheres
	}
	kept := make([]drops.Expression, 0, len(t.filters)+len(wheres))
	for _, f := range t.filters {
		if s.ignores(f.name) {
			continue
		}
		kept = append(kept, f.pred)
	}
	if len(kept) == 0 {
		return wheres
	}
	return append(kept, wheres...)
}
