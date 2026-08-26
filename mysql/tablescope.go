package mysql

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/bernardoforcillo/drops"
)

// A table's automatic predicates, and the state a table shares with
// every alias taken off it.

// tableScope is the automatic-predicate state of a table: the two
// filter lists and the write-side tenant column. It lives behind a
// pointer so that a table and every alias taken off it share ONE of
// them.
//
// Sharing is the whole point, and it is the rule the four dialects
// state in the same words: an alias shares the whole scope — both
// filter lists — and rebinds its columns. While As copied a list, an
// alias was a snapshot of the table's scoping at the instant As was
// called, and the spelling this package invites is exactly the one that
// loses:
//
//	var Users = mysql.NewTable("users")   // package scope
//	var U     = Users.As("u")             // package scope, before init
//
//	func init() { Users.ContextFilter(mysql.TenantFilter(UserTenantID)) }
//
// Go initialises package-level variables before it runs init, so U was
// taken while the table had no tenant axis and never gained one. A
// statement written against the alias then rendered, on a ctx carrying
// no tenant at all and without refusing, DELETE `u` FROM `users` AS `u`
// — no predicate, every tenant's rows, and nothing walks a DELETE back.
// The same snapshot dropped a soft-delete guard registered after the
// alias was taken, so the alias read rows the application had deleted.
//
// The two lists are shared on the same terms because the argument does
// not distinguish them: a default filter registered after As was taken
// went missing exactly as a context filter did, and the difference
// between the two failures is only how bad it is. Splitting them was
// this dialect's local answer to a question nobody had written down.
//
// One consequence to state plainly, because it follows from sharing and
// is not a bug: registering a filter ON an alias registers it on the
// table, and so on every other alias of it. That is what "the same
// table" means. Where two genuinely different scopings are wanted, they
// are two tables.
//
// The mutex guards every field, and it is the same mutex the base table
// and all its aliases lock — which is the other half of what sharing
// has to mean. An alias with a lock of its own would serialise against
// nothing while another goroutine replaced the list it was reading.
// Registration is a supported thing to do while queries are in flight
// (see ctxFilter), so the lists are REPLACED rather than edited in
// place: a reader takes the slice header under the read lock and walks
// it with the lock released, which is only sound while no writer
// touches an element a reader may already be holding.
type tableScope struct {
	mu sync.RWMutex

	// defaultFilters are predicates applied automatically by
	// SelectBuilder / UpdateBuilder / DeleteBuilder unless the caller
	// opts out with Unscoped(). Used to implement default scopes (e.g.
	// a soft-delete "deletedAt IS NULL" guard). Declaration-time only —
	// see Table.DefaultFilter.
	defaultFilters []drops.Expression

	// ctxFilters are the request-scoped twins of defaultFilters:
	// predicates that cannot be built until a ctx is in hand. They are
	// resolved by the executors rather than by WriteSQL — see
	// Table.ContextFilter.
	ctxFilters []ctxFilter

	// tenantCol is the column an INSERT into this table stamps from the
	// ctx tenant — the write-side half of the tenant axis, declared by
	// Entity.ScopeByTenant or by Table.ScopeWritesByTenant.
	//
	// It is a column rather than a predicate because an INSERT has no
	// WHERE clause for a predicate to reach: see
	// InsertBuilder.ToSQLCtx.
	tenantCol *Column
}

// ctxFilter pairs a context filter with the key it was registered
// under. A key makes registration idempotent: ScopeByTenant installs
// its filter every time it is called, and an entity built inside a
// request handler rather than at package scope would otherwise stack
// one more predicate onto the shared table per request — a WHERE clause
// that grows without bound and a slice that is never collected. A
// filter registered through the exported ContextFilter carries no key
// and always appends, because two calls there are two deliberate
// filters.
//
// That affordance is the reason the list is locked. Building an entity
// per request is the pattern the key exists to make idempotent, so it
// is a pattern this package invites, and it registers filters onto a
// table other goroutines are querying at that moment — a write to the
// slice against reads from every executor in flight, which is a data
// race in the plain sense.
type ctxFilter struct {
	key string
	fn  ContextFilterFunc
}

// ContextFilterFunc builds a predicate from the request context. It
// returns a nil Expression to contribute nothing (an entity whose
// tenant axis was never declared), and an error to refuse the query
// outright — which is how [TenantFilter] fails closed instead of
// running unfiltered.
type ContextFilterFunc func(context.Context) (drops.Expression, error)

// appendShared appends v to a list other goroutines may be walking
// right now, copying at exactly len rather than appending in place.
//
// The caller holds the write lock, but a reader does not: it took the
// slice header under the read lock and released it before walking. An
// in-place append into spare capacity would write an element that
// reader's header already spans. Copying at length is what keeps the
// list a value nobody else can see change under them.
func appendShared[T any](list []T, v T) []T {
	out := make([]T, len(list), len(list)+1)
	copy(out, list)
	return append(out, v)
}

// ContextFilter registers a predicate resolved at execution time and
// AND-ed into every SELECT, UPDATE and DELETE against the table, unless
// the builder is marked Unscoped().
//
//	Posts.ContextFilter(mysql.TenantFilter(PostTenantID))
//
// A DefaultFilter is rendered by WriteSQL, which has no ctx; this is
// resolved by the executors — All / One / Rows on a SELECT, Exec on an
// UPDATE or DELETE, and every entity method that reaches them. That
// distinction is the whole point rather than an implementation detail:
// one hook then covers the root query, a joined table, a CTE body, a
// subquery operand, an UPDATE and a DELETE, because all of them go
// through an executor. A predicate resolved at render time reaches only
// the statements somebody remembered to build with it.
//
// The cost is that a rendered statement is no longer complete: see
// [SelectBuilder.ToSQL], and prefer ToSQLCtx when you need the SQL a
// given ctx would actually send.
//
// Registering on an alias registers on the table — see [Table.As] — and
// is safe to call while queries against the table are in flight, which
// ctxFilter explains is a supported thing to do rather than an
// accident.
func (t *Table) ContextFilter(fn ContextFilterFunc) *Table {
	if fn == nil {
		return t
	}
	t.scope.mu.Lock()
	defer t.scope.mu.Unlock()
	t.scope.ctxFilters = appendShared(t.scope.ctxFilters, ctxFilter{fn: fn})
	return t
}

// ScopeWritesByTenant declares col as the column every INSERT into this
// table stamps from the ctx tenant, refusing when the ctx carries none.
//
// It is the write-side half of what [Table.ContextFilter] does for
// reads, and it is declared separately because it cannot be derived
// from the filter. A [ContextFilterFunc] is a closure that answers with
// a predicate; nothing can ask it which column it names, and guessing
// one from the predicate it rendered would be drops deciding which
// column owns a row — so a table scoped with
// ContextFilter(TenantFilter(col)) and nothing else keeps its guarantee
// on every read, every UPDATE and every DELETE while its INSERTs stay
// the caller's to bind. Naming the column here hands them to drops:
//
//	Posts.ContextFilter(mysql.TenantFilter(PostTenantID)).
//	    ScopeWritesByTenant(PostTenantID)
//
// [Entity.ScopeByTenant] calls this for you, so an entity-declared axis
// covers both halves at once. Declaring it twice with the same column
// is idempotent.
//
// The consequence worth stating plainly: from here on every INSERT into
// this table needs a tenant on its ctx, including one built straight
// from db.Insert(), and an ON DUPLICATE KEY UPDATE against it is gated
// so that a collision with another tenant's row rewrites nothing — see
// [InsertBuilder.ToSQLCtx]. A statement that legitimately writes
// outside the ctx tenant says so with [InsertBuilder.Unscoped].
// The handle has to be one of this table's own columns, and a handle
// this table does not own is a panic at declaration time rather than a
// statement at request time. It takes whatever it is given, and what
// it is given ends up in the INSERT column list and in every axis
// check the package makes — so a codegen'd OtherTable.TenantID passed
// here named a column the table does not have: the INSERT stamped a
// column the server then refused, and the refusal reached the caller
// as a query error about a name they never typed. [ErrTenantNotInSortingKey]
// could report the same declaration as "its sorting key is (tenantId,
// id), which does not include other.tenantId" — a sentence that
// contradicts itself.
//
// Ownership is asked by [Column.key], so a handle taken off a table
// alias names the same axis as the declared one; what is STORED is
// this table's own handle rather than the one passed in, for the
// reason [Entity.ScopeByTenant] stores its own: an alias handle
// qualifies with an alias an INSERT into this table has no name for.
func (t *Table) ScopeWritesByTenant(col ColRef) *Table {
	if col == nil {
		return t
	}
	c := col.col()
	for _, own := range t.columns {
		if own.key() == c.key() {
			t.setTenantAxis(own)
			return t
		}
	}
	panic("drops/mysql: ScopeWritesByTenant column " + columnPath(c) +
		" is not a column of " + t.Name())
}

// setTenantAxis records the table's tenant column. Called by
// ScopeWritesByTenant and by Entity.ScopeByTenant, which registers the
// read-side filter in the same breath.
//
// The column is stored as the DECLARED handle — Column.key — so that a
// table and its aliases, whose columns are separate copies in this
// dialect, name one axis between them. An INSERT built from an alias's
// handles would otherwise stamp a second column that happens to have
// the same name.
func (t *Table) setTenantAxis(c *Column) {
	t.scope.mu.Lock()
	defer t.scope.mu.Unlock()
	if c != nil {
		c = c.key()
	}
	t.scope.tenantCol = c
}

// tenantAxis returns the column an INSERT into the table stamps, or nil
// when the table declared none. Nil-safe so a builder can ask without
// knowing whether it has a table at all.
func (t *Table) tenantAxis() *Column {
	if t == nil {
		return nil
	}
	t.scope.mu.RLock()
	defer t.scope.mu.RUnlock()
	return t.scope.tenantCol
}

// setContextFilter registers fn under key, replacing any filter
// previously registered under the same key. See ctxFilter for why the
// entity-owned filters are keyed and the exported ones are not.
func (t *Table) setContextFilter(key string, fn ContextFilterFunc) {
	t.scope.mu.Lock()
	defer t.scope.mu.Unlock()
	for i := range t.scope.ctxFilters {
		if t.scope.ctxFilters[i].key == key {
			// Replaced by swapping in a new slice rather than by
			// assigning through the old one: a reader is walking that
			// backing array right now with the lock released, and an
			// entity rebuilt per request replaces its own filter on
			// every single request.
			next := make([]ctxFilter, len(t.scope.ctxFilters))
			copy(next, t.scope.ctxFilters)
			next[i].fn = fn
			t.scope.ctxFilters = next
			return
		}
	}
	t.scope.ctxFilters = appendShared(t.scope.ctxFilters, ctxFilter{key: key, fn: fn})
}

// ctxFilterList returns the table's request-scoped filters through the
// shared scope, so an alias resolves the axis its table carries now
// rather than the one it carried when As was called. See [Table.As] for
// the cross-tenant DELETE that answer prevents.
func (t *Table) ctxFilterList() []ctxFilter {
	if t == nil || t.scope == nil {
		return nil
	}
	t.scope.mu.RLock()
	defer t.scope.mu.RUnlock()
	return t.scope.ctxFilters
}

// hasContextFilters reports whether the table has any context filter to
// resolve. Nil-safe so a SELECT with no FROM table can ask.
func (t *Table) hasContextFilters() bool { return len(t.ctxFilterList()) > 0 }

// defaultFilterList returns the table's render-time filters through the
// shared scope, so an alias renders the guards its table carries now
// rather than the ones it carried when As was called. Nil-safe so a
// statement with no table can ask.
func (t *Table) defaultFilterList() []drops.Expression {
	if t == nil || t.scope == nil {
		return nil
	}
	t.scope.mu.RLock()
	defer t.scope.mu.RUnlock()
	return t.scope.defaultFilters
}

// hasDefaultFilters reports whether the table carries any render-time
// default filter. Nil-safe, like hasContextFilters.
func (t *Table) hasDefaultFilters() bool { return len(t.defaultFilterList()) > 0 }

// resolveContextFilters builds every registered predicate against ctx,
// walks the statements written inside those predicates, and restates
// the result against this instance of the table — see
// resolveFilterExprs for what "this instance" buys.
//
// The walk matters as much as the call. A filter answers with a
// predicate, and a predicate is a place a statement can be written: a
// membership guard answers with In(col, <SELECT over the junction
// table>), which is the ordinary spelling of "the rows this subject may
// see are the ones some other scoped table names". Handed straight to
// the builder, that inner statement is reached only by the renderer,
// which has no ctx — so it would write its DefaultFilters, none of its
// ContextFilters, and refuse nothing on a ctx carrying no tenant at
// all. The guard meant to decide what a request may see would itself be
// the unscoped read.
//
// The walk runs before resolveFilterExprs rather than after, so the
// alias rename wraps the resolved tree rather than the resolver
// terminating at the closure the rename returns.
//
// The filter function itself is called under the cycle chain, not just
// the predicate it returns. A filter is arbitrary user code and may run
// a query of its own, and a query of its own against this table is the
// same non-terminating shape by another route.
//
// The first filter to fail aborts the whole resolution and no statement
// is sent: a filter that cannot decide what a request may see must not
// be answered with an unfiltered query.
//
// The filter list is snapshotted under the read lock and walked with it
// released. Holding it across the calls would put arbitrary user code
// inside the lock, and a filter that registered another filter would
// deadlock against itself.
func (t *Table) resolveContextFilters(ctx context.Context) ([]drops.Expression, error) {
	if t == nil {
		return nil, nil
	}
	filters := t.ctxFilterList()
	if len(filters) == 0 {
		return nil, nil
	}
	inner, err := enterFilterResolution(ctx, t)
	if err != nil {
		return nil, err
	}
	out := make([]drops.Expression, 0, len(filters))
	for _, f := range filters {
		e, err := f.fn(inner)
		if err != nil {
			return nil, err
		}
		if e != nil {
			out = append(out, e)
		}
	}
	resolved, err := resolveExprs(inner, out)
	if err != nil {
		return nil, err
	}
	if resolved != nil {
		out = resolved
	}
	return t.resolveFilterExprs(out), nil
}

// resolveDefaultFilters returns the render-time filters, restated
// against this instance of the table.
//
// Render-time is the whole of it: there is no ctx here, so a statement
// written inside a default filter renders through WriteSQL and carries
// none of its own context filters. That is why the executors resolve
// the list first and hand the answer back through resolvedDefaults —
// see resolveDefaultFilterExprs. This function stays the answer on the
// ToSQL path, which has no ctx to do better with.
//
// The restatement is a no-op on the common alias, whose default filters
// were copied along with its columns and so already name it. It matters
// for the filter registered on the base table before the alias was
// taken: that one closes over the declared handles, and under
// FROM `users` AS `u` those name a relation the statement has no FROM
// entry for.
func (t *Table) resolveDefaultFilters() []drops.Expression {
	filters := t.defaultFilterList()
	if len(filters) == 0 {
		return nil
	}
	return t.resolveFilterExprs(filters)
}

// resolveDefaultFilterExprs walks the statements written inside the
// table's default filters against ctx, returning the restated list — or
// nil when no default filter had a statement in it, so the caller keeps
// rendering exactly what it rendered before.
//
// A default filter is declaration-time and a context filter is
// request-time, but the predicate each answers with is the same kind of
// thing, and a statement embedded in one is as invisible to the
// renderer as a statement embedded in the other:
// DefaultFilter(NotIn(col, blockedSelect)) wrote the inner statement
// with its own DefaultFilters and none of its ContextFilters, on any
// ctx at all. The invariant is about what the renderer reads, not about
// when the list was declared.
//
// Returning nil for "nothing changed" is what keeps the rendering
// promise: a filter with no statement inside it is not rebuilt, is not
// re-wrapped, and renders the bytes it always did — which is every
// soft-delete guard there is, the shape this feature exists beside.
func (t *Table) resolveDefaultFilterExprs(ctx context.Context) ([]drops.Expression, error) {
	filters := t.defaultFilterList()
	if len(filters) == 0 || !mayHoldStatements(filters) {
		return nil, nil
	}
	inner, err := enterFilterResolution(ctx, t)
	if err != nil {
		return nil, err
	}
	resolved, err := resolveExprs(inner, filters)
	if err != nil {
		return nil, err
	}
	if resolved == nil {
		return nil, nil
	}
	return t.resolveFilterExprs(resolved), nil
}

// ErrContextFilterCycle is returned when resolving a table's automatic
// predicates re-enters the same table: a context filter (or a default
// filter) on "notes" whose predicate embeds a statement selecting from
// "notes", directly or through another table whose filter comes back.
//
// Walking a filter's own predicate is what makes the cycle reachable at
// all, and the shape has to be answered rather than followed: each turn
// asks the filter for a fresh predicate, so there is no fixed point to
// converge on and no builder to notice it has been here before. Left
// alone it is not a wrong answer, it is a goroutine that never returns.
//
// The chain is carried on the ctx rather than as a flag on the *Table:
// it is per-resolution by construction, needs no lock, costs one
// allocation per table that actually has filters, and names the cycle
// exactly — including a mutual one, A through B back to A. A flag on
// the shared table would instead report a cycle whenever two goroutines
// resolved the same table at the same instant, which is a false refusal
// that appears only under load.
//
// The refusal is not a dead end. A filter that has to consult its own
// table says Unscoped() on the statement it embeds: that is the shape
// that terminates, and it is the shape the author meant, since the
// inner read selects the rows the filter is about to restrict.
var ErrContextFilterCycle = errors.New("drops/mysql: a table's automatic filter selects from the table it filters")

// filterChainKey is the ctx key under which the chain of tables whose
// automatic filters are currently being resolved is carried. An
// unexported struct type, so nothing outside this package can plant a
// chain of its own and talk the resolver out of a refusal.
type filterChainKey struct{}

// filterChain is one link of that chain. It is a linked list rather
// than a map because it is written once per link and read by walking:
// an immutable list can be shared by every ctx derived from it without
// copying.
type filterChain struct {
	ref  string
	prev *filterChain
}

// enterFilterResolution returns the ctx to resolve t's automatic
// filters under, or [ErrContextFilterCycle] when t is already being
// resolved further up the same chain.
//
// Tables are identified by relation reference rather than by pointer,
// so an alias counts as the same relation it aliases. That is the
// identity SQL uses, and the identity the recursion has: a filter on
// `notes` that embeds SELECT … FROM `notes` AS `n` re-enters the same
// filter list however the inner statement spells the table.
func enterFilterResolution(ctx context.Context, t *Table) (context.Context, error) {
	ref := t.relRef()
	chain, _ := ctx.Value(filterChainKey{}).(*filterChain)
	for c := chain; c != nil; c = c.prev {
		if c.ref == ref {
			return nil, fmt.Errorf("%w: %s; say Unscoped on the statement the filter embeds",
				ErrContextFilterCycle, renderFilterCycle(chain, ref))
		}
	}
	return context.WithValue(ctx, filterChainKey{}, &filterChain{ref: ref, prev: chain}), nil
}

// renderFilterCycle spells the cycle out from the table it closes on,
// as "notes -> authors -> notes". The chain is held innermost-first, so
// it is reversed here: a caller reading the error wants the order the
// resolver walked, not the order it unwinds.
func renderFilterCycle(chain *filterChain, ref string) string {
	var refs []string
	for c := chain; c != nil; c = c.prev {
		refs = append(refs, c.ref)
		if c.ref == ref {
			break
		}
	}
	var b strings.Builder
	for i := len(refs) - 1; i >= 0; i-- {
		b.WriteString(refs[i])
		b.WriteString(" -> ")
	}
	b.WriteString(ref)
	return b.String()
}

// resolveFilterExprs restates a table's automatic predicates against
// the instance of the table the statement actually names.
//
// A filter arrives as a predicate — an opaque tree of nodes over
// whichever handles built it, with no way in — and every filter this
// package registers for you closes over the *declared* handle:
// TenantFilter over the column it was given, ScopeByTenant over the
// entity's own. Against FROM `notes` AS `n` all of them render
// `notes`.`tenantId`, which names a relation the statement has no FROM
// entry for. MySQL answers that with error 1054, "Unknown column
// 'notes.tenantId' in 'where clause'" — not a widened result, a query
// that cannot run. So a tenant-scoped table, the one kind that must
// never lose its axis, would be the one kind that could not be queried
// under an alias at all, and every self-join of one with it.
//
// It bites hardest on a DELETE, and for a reason peculiar to this
// dialect: an aliased DELETE has to be written in the multi-table form
// (see [DeleteBuilder.WriteSQL]), which names the alias twice, so there
// is no un-aliased spelling of the statement to fall back on.
//
// Since the tree cannot be rewritten it is rendered under a rename
// instead: for the length of each predicate, references to the declared
// relation resolve to this instance's alias. Aliasing was always a
// query-scope rename of exactly that kind — Column.key says an aliased
// handle is the same column — and this is the rename applied to the one
// place the handles were out of reach.
//
// Two shapes stay the caller's: a filter that embeds a subquery
// selecting from the base table in its own right (renamed along with
// everything else, since the rename cannot see the difference), and a
// self-join whose predicate deliberately names the un-aliased side. Say
// Unscoped and write the predicate at the query for either.
func (t *Table) resolveFilterExprs(exprs []drops.Expression) []drops.Expression {
	if t.alias == "" || len(exprs) == 0 {
		return exprs
	}
	ref, alias := t.relRef(), t.alias
	out := make([]drops.Expression, len(exprs))
	for i, e := range exprs {
		out[i] = drops.ExprFunc(func(b *drops.Builder) {
			defer b.SetRelationAlias(b.SetRelationAlias(ref, alias))
			b.Append(e)
		})
	}
	return out
}

// resolvedDefaults holds, per table a statement names, the default
// filters resolved for one execution of that statement.
//
// It is a side table rather than a field on the *Table because the
// resolution belongs to the execution: two requests render the same
// table at once, and a resolved list written back onto the shared table
// would let one request's subquery scoping decide the other's. It is
// keyed by *Table rather than by name so that a statement naming the
// same relation twice — a self-join, one side aliased — keeps each
// side's own restatement.
//
// The zero value is a nil map and answers every lookup with the
// unresolved list, which is what the ToSQL path and every statement
// with nothing to resolve get.
type resolvedDefaults map[*Table][]drops.Expression

// of returns the default filters to render for t: the resolved list
// when this execution produced one, and otherwise the render-time list,
// unchanged and byte for byte.
func (d resolvedDefaults) of(t *Table) []drops.Expression {
	if t == nil {
		return nil
	}
	if e, ok := d[t]; ok {
		return e
	}
	return t.resolveDefaultFilters()
}

// resolveTableDefaults resolves the default filters of every table a
// statement names, returning nil when not one of them held a statement
// — so a builder that had nothing to resolve keeps the nil map and
// renders through the unresolved path.
//
// A refusal aborts the whole statement, exactly as a refusing context
// filter does: a default filter that embeds a read it cannot scope
// cannot be answered with the unscoped read.
func resolveTableDefaults(ctx context.Context, tables ...*Table) (resolvedDefaults, error) {
	var out resolvedDefaults
	for _, t := range tables {
		if t == nil || !t.hasDefaultFilters() {
			continue
		}
		if _, done := out[t]; done {
			continue
		}
		resolved, err := t.resolveDefaultFilterExprs(ctx)
		if err != nil {
			return nil, err
		}
		if resolved == nil {
			continue
		}
		if out == nil {
			out = resolvedDefaults{}
		}
		out[t] = resolved
	}
	return out, nil
}
