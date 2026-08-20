package mysql

import "github.com/bernardoforcillo/drops"

// Global filters and how a query steps around one of them without
// stepping around the rest. The mechanism mirrors drops/pg's — see
// that package's filters.go for the reasoning — and the short version
// is that Unscoped drops every filter a table carries, which makes it
// the wrong tool for "show me the deleted rows" on a table that is
// also scoped by tenant.
//
//	t.AddFilter(mysql.FilterSoftDelete, deletedAt.IsNull())
//	db.Select().From(t).IgnoreFilters(mysql.FilterSoftDelete)

// Conventional filter names, as constants so a caller references a
// symbol instead of retyping a string. drops/mysql installs no filters
// of its own — a table declares its own guards — but the names match
// drops/pg's so a schema ported between the two keeps its call sites.
const (
	// FilterSoftDelete names a "deletedAt IS NULL" guard.
	FilterSoftDelete = "softDelete"

	// FilterTenant names a tenancy-isolation predicate.
	FilterTenant = "tenant"
)

// tableFilter is one registered global filter. name is empty for the
// anonymous filters DefaultFilter installs, which nothing but Unscoped
// can drop.
type tableFilter struct {
	name string
	pred drops.Expression
}

// filterScope is what one statement has opted out of. The zero value
// opts out of nothing.
type filterScope struct {
	unscoped bool
	ignored  map[string]struct{}
}

// ignore records names this statement bypasses. An unknown name is
// kept rather than rejected: a typo that leaves a filter standing
// returns too few rows, never too many.
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

// ignores reports whether name was passed to IgnoreFilters here.
func (s filterScope) ignores(name string) bool {
	if name == "" {
		return false
	}
	_, ok := s.ignored[name]
	return ok
}

// apply prepends t's surviving filters to wheres.
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
