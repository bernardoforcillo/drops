package sqlite

import "github.com/bernardoforcillo/drops"

// Global filters and how a query steps around one of them without
// stepping around the rest. The mechanism mirrors drops/pg's — see
// that package's filters.go for the reasoning — and the short version
// is that Unscoped drops every filter a table carries, which makes it
// the wrong tool for "show me the deleted rows" on a table that is
// also scoped by tenant.
//
//	t.AddFilter(sqlite.FilterSoftDelete, deletedAt.IsNull())
//	db.Select().From(t).IgnoreFilters(sqlite.FilterSoftDelete)

// Names of the filters drops installs itself, as constants so a caller
// references a symbol instead of retyping a string.
const (
	// FilterSoftDelete names the "deletedAt IS NULL" guard installed
	// by [SoftDelete] and [SoftDeleteMixin].
	FilterSoftDelete = "softDelete"

	// FilterTenant names the isolation predicate installed by
	// [Entity.ScopeByTenant]. It is built per query from the ctx
	// tenant rather than registered on the table, so Unscoped does not
	// reach it — see [EntityQuery.IgnoreFilters].
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
	// keepCtx says that unscoped means the table's DECLARATION-time
	// filters only, and that the request-time ones still apply.
	//
	// It is what separates [EntityQuery.Unscoped] from
	// [SelectBuilder.Unscoped]. "Include the soft-deleted rows" is the
	// thing callers reach for, and on the entity path it must not also
	// mean "and every other tenant's": an entity is the typed, scoped
	// surface, so the axis survives an opt-out aimed at the guards. The
	// raw builder's Unscoped stays the blunt instrument it documents
	// itself as.
	keepCtx bool
	ignored map[string]struct{}
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
func (s filterScope) applyAll(tables []*Table, wheres []drops.Expression, defaults resolvedDefaults) []drops.Expression {
	for i := len(tables) - 1; i >= 0; i-- {
		// Backwards, because each apply PREPENDS: walking the list in
		// reverse leaves the filters in the order the statement names
		// the tables.
		wheres = s.apply(tables[i], wheres, defaults)
	}
	return wheres
}

// dropsContextFilters reports whether this statement's opt-out reaches
// the request-time filters as well as the render-time ones.
func (s filterScope) dropsContextFilters() bool { return s.unscoped && !s.keepCtx }
