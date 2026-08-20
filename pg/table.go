package pg

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/bernardoforcillo/drops"
)

// Table represents a schema-qualified PostgreSQL table.
type Table struct {
	schema    string
	name      string
	alias     string
	columns   []*Column
	byName    map[string]*Column
	relations map[string]*Relation

	// indexes is the list of CREATE INDEX statements declared
	// alongside the table — typically by a Mixin. CreateTable does
	// not emit them; pair the table with CreateTableWithIndexes if
	// you want both at once.
	indexes []*Index

	// compositePK, when non-empty, is the table's PRIMARY KEY when
	// it spans multiple columns. Single-column PKs continue to
	// live on the column (via *Col[T].PrimaryKey()).
	compositePK []*Column

	// compositeUniques are multi-column UNIQUE constraints, keyed
	// by name so diffs are stable. Single-column uniques continue
	// to live on the column (via *Col[T].Unique()).
	compositeUniques map[string][]*Column

	// checks are CHECK constraints, keyed by name. The value is
	// the raw SQL expression (e.g. "age >= 0").
	checks map[string]string

	// compositeFKs are multi-column (N-column) foreign keys declared
	// at the table level via ForeignKeyN. Single-column FKs continue
	// to live on the column (via *Col[T].References / Table.ForeignKey).
	// Emitted as separate ALTER TABLE ADD CONSTRAINT statements.
	compositeFKs []*CompositeFK

	// policies are RLS policies declared on the table.
	policies map[string]*Policy

	// rlsEnabled mirrors PG's ALTER TABLE ... ENABLE ROW LEVEL
	// SECURITY. Policies are only enforced when this is true.
	rlsEnabled bool

	// insertHooks / updateHooks / deleteHooks are the optional
	// lifecycle hooks registered on this table. They are invoked by
	// the corresponding builders during WriteSQL. Empty by default —
	// a table with no hooks behaves exactly as it did before this
	// feature shipped.
	insertHooks []InsertHook
	updateHooks []UpdateHook
	deleteHooks []DeleteHook

	// defaultFilters are predicates applied automatically by
	// SelectBuilder / UpdateBuilder / DeleteBuilder unless the caller
	// opts out with Unscoped(). Used to implement default scopes
	// (e.g. SoftDelete's "deleted_at IS NULL" guard). Declaration-time
	// only — see DefaultFilter.
	defaultFilters []drops.Expression

	// ctxFilters are the request-scoped twins of defaultFilters:
	// predicates that cannot be built until a ctx is in hand. They are
	// resolved by the executors rather than by WriteSQL — see
	// Table.ContextFilter.
	//
	// Guarded by ctxFiltersMu, and replaced rather than edited in
	// place: a reader takes the slice header under the read lock and
	// then walks it with the lock released, which is only sound while
	// no writer touches an element a reader may already be holding.
	ctxFilters   []ctxFilter
	ctxFiltersMu *sync.RWMutex

	// tenantCol is the column an INSERT into this table stamps from the
	// ctx tenant — the write-side half of the tenant axis, declared by
	// Entity.ScopeByTenant or by Table.ScopeWritesByTenant.
	//
	// It is a column rather than a predicate because an INSERT has no
	// WHERE clause for a predicate to reach: see
	// InsertBuilder.ToSQLCtx. Guarded by ctxFiltersMu, which already
	// exists to make declaring the axis on a table other goroutines are
	// querying a supported thing to do — the two halves are registered
	// by the same call and are read by statements in flight.
	tenantCol *Column
}

// ctxFilter pairs a context filter with the key it was registered
// under. A key makes registration idempotent: ScopeByTenant and
// AuthorizeWith install their filter every time they are called, and an
// entity built inside a request handler rather than at package scope
// would otherwise stack one more predicate onto the shared table per
// request — a WHERE clause that grows without bound and a slice that is
// never collected. A filter registered through the exported
// ContextFilter carries no key and always appends, because two calls
// there are two deliberate filters.
//
// That affordance is the reason the list is locked. Building an entity
// per request is the pattern the key exists to make idempotent, so it
// is a pattern this package invites, and it registers filters onto a
// table other goroutines are querying at that moment — a write to the
// slice against reads from every executor in flight, which is a data
// race in the plain sense and one a race detector reports on a fixture
// no larger than "handler builds entity, handler runs query". Making
// registration idempotent removed the unbounded growth and left the
// race; the lock is the other half of the same promise, and it is the
// honest one to keep because the alternative — declaring registration
// legal only before the first query — would make the key pointless and
// would refuse a spelling both ScopeByTenant and AuthorizeWith
// document as supported.
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

// NewTable creates a table in the default ("public") schema. The name
// is validated and the constructor panics on invalid identifiers — see
// ErrInvalidIdentifier — because schemas are typically declared in
// package init / var blocks where a bad name should fail loudly at
// startup rather than at the first query.
func NewTable(name string) *Table {
	mustIdent("table", name)
	return &Table{
		name:         name,
		byName:       map[string]*Column{},
		relations:    map[string]*Relation{},
		ctxFiltersMu: new(sync.RWMutex),
	}
}

// NewSchemaTable creates a table in an explicit schema.
func NewSchemaTable(schema, name string) *Table {
	mustIdent("schema", schema)
	mustIdent("table", name)
	return &Table{
		schema:       schema,
		name:         name,
		byName:       map[string]*Column{},
		relations:    map[string]*Relation{},
		ctxFiltersMu: new(sync.RWMutex),
	}
}

// Relation looks up a registered relation by name. Returns nil if no
// such relation exists.
func (t *Table) Relation(name string) *Relation { return t.relations[name] }

// Rel returns the named relation, panicking if it was never declared.
//
// It is how a relation becomes a Go identifier rather than a string
// literal at the query site:
//
//	var UserPosts = Users.Rel("posts")
//	...
//	db.Find(Users).Load(UserPosts)
//
// The name is still spelled once, at declaration, where a typo panics
// at process start instead of failing the first query that happens to
// eager-load it. Everywhere the relation is used it is a symbol the
// compiler checks and a rename refactors.
func (t *Table) Rel(name string) *Relation {
	r := t.relations[name]
	if r == nil {
		declared := make([]string, 0, len(t.relations))
		for n := range t.relations {
			declared = append(declared, n)
		}
		sort.Strings(declared)
		// An alias carries the relations its table had at the moment
		// As was called, so one declared afterwards reaches the base
		// handle and not this one. Naming the alias is what separates
		// that from "the relation was never declared at all" — the two
		// look identical from the list.
		subject := fmt.Sprintf("table %q", t.name)
		if t.alias != "" {
			subject = fmt.Sprintf("alias %q of table %q", t.alias, t.name)
		}
		panic(fmt.Sprintf("drops/pg: %s has no relation %q; declared: %s",
			subject, name, strings.Join(declared, ", ")))
	}
	return r
}

// Name returns the table's unqualified name.
func (t *Table) Name() string { return t.name }

// Schema returns the table's schema (empty for the default schema).
func (t *Table) Schema() string { return t.schema }

// Alias returns the alias set via As, or "" if none.
func (t *Table) Alias() string { return t.alias }

// As returns a copy of the table under an alias, for self-joins.
//
// The copy carries its own columns, bound to the aliased table, so a
// reference reached through it — u := Users.As("u"); u.Col("id") —
// qualifies with the alias while the original package-level handles go
// on qualifying with the table name. That is what makes both sides of
// a self-join addressable at once.
//
// An aliased handle still *means* the column it was copied from. The
// INSERT column list, a hook's Has, an Entity's key columns, the
// tenant axis and a cursor's ordering column all identify a column
// through Column.key, which collapses the copy back onto the declared
// column; where such a handle is also rendered — the tenant guard, a
// page's ORDER BY and cursor guard — it is restated as the handle the
// entity's own table hands out, so the reference qualifies with the
// relation the statement names. Aliasing changes how a reference
// renders and nothing else.
//
// The automatic predicates a table carries — a default filter
// registered by SoftDeleteMixin, a context filter registered by
// ContextFilter or ScopeByTenant, an authz guard built from the
// package-level columns — move with it, and the alias renders them
// qualified with the alias. They cannot be rewritten, being closures
// over the handles they were given, so they are rendered inside a
// relation rename instead: see resolveFilterExprs. Without it an
// aliased query against a scoped table could not run at all —
// "notes"."tenantId" against FROM "notes" AS "n" is 42P01, not a
// widened result — which made the one table shape that must never lose
// its tenant axis the one shape that could not be queried under an
// alias.
//
// What is still not rewritten is anything else the caller built and
// drops only re-emits: a Patch operation, and any predicate handed to
// Where. Both are closed over the handles they were given, so build a
// Patch for an aliased entity, and the predicates of an aliased query,
// from the alias's own handles.
//
// Indexes are shared with the base table for the same reason — an
// index's column list may hold arbitrary expressions — so Index.Table
// on an alias's index answers with the un-aliased table. Nothing
// renders an index through an alias, since CREATE INDEX takes no AS
// clause.
//
// The copy is a snapshot. A column, relation or constraint added to
// the base table after As returned does not reach the alias, which
// matters because Go initialises package-level variables before it
// runs init: an alias declared as a var beside its table is taken
// before any init that declares relations. Take the alias at the query
// site, or after the schema is complete.
func (t *Table) As(alias string) *Table {
	mustIdent("alias", alias)
	// The whole-struct copy reads ctxFilters, which a request-scoped
	// ScopeByTenant may be writing right now, so it is taken under the
	// read lock. The alias then gets a lock of its own: its filter list
	// is its own from here, and two independent handles have no reason
	// to serialise against each other.
	t.ctxFiltersMu.RLock()
	cp := *t
	cp.ctxFilters = append([]ctxFilter(nil), t.ctxFilters...)
	t.ctxFiltersMu.RUnlock()
	cp.ctxFiltersMu = new(sync.RWMutex)
	cp.alias = alias
	cp.columns = make([]*Column, len(t.columns))
	cp.byName = make(map[string]*Column, len(t.byName))
	for i, c := range t.columns {
		aliased := *c
		aliased.table = &cp
		// The origin chains to the declared column rather than to c,
		// so aliasing an alias does not make a stranger of the root.
		aliased.origin = c.key()
		cp.columns[i] = &aliased
		cp.byName[aliased.name] = &aliased
	}
	// rebind maps a column declared on t to the aliased copy's handle
	// for it, and leaves any column belonging to another table alone.
	rebind := func(c *Column) *Column {
		if c != nil && c.table == t {
			if aliased := cp.byName[c.name]; aliased != nil {
				return aliased
			}
		}
		return c
	}
	rebindAll := func(cols []*Column) []*Column {
		if cols == nil {
			return nil
		}
		out := make([]*Column, len(cols))
		for i, c := range cols {
			out[i] = rebind(c)
		}
		return out
	}
	// Every map on the copy has to be its own, or a relation, check,
	// unique or policy declared against the alias writes through into
	// the table it was aliased from.
	cp.relations = make(map[string]*Relation, len(t.relations))
	for name, rel := range t.relations {
		r := *rel
		r.From = &cp
		// Only the near side — the end of the edge that belongs to
		// this table — moves to the alias. On a self-referential
		// relation both ends name this table and rebinding both would
		// erase the distinction the alias exists to draw.
		switch r.Kind {
		case BelongsToKind, MorphToKind:
			// The inverse edges hold their own key in ChildKey;
			// ParentKey (and, for MorphTo, nothing else) names the far
			// table.
			r.ChildKey = rebind(r.ChildKey)
			if r.Kind == MorphToKind {
				r.MorphTypeCol = rebind(r.MorphTypeCol)
			}
		default:
			r.ParentKey = rebind(r.ParentKey)
		}
		cp.relations[name] = &r
	}
	cp.compositePK = rebindAll(t.compositePK)
	if t.compositeUniques != nil {
		cp.compositeUniques = make(map[string][]*Column, len(t.compositeUniques))
		for name, cols := range t.compositeUniques {
			cp.compositeUniques[name] = rebindAll(cols)
		}
	}
	if t.checks != nil {
		cp.checks = make(map[string]string, len(t.checks))
		for name, expr := range t.checks {
			cp.checks[name] = expr
		}
	}
	if t.policies != nil {
		cp.policies = make(map[string]*Policy, len(t.policies))
		for name, p := range t.policies {
			cp.policies[name] = p
		}
	}
	cp.compositeFKs = make([]*CompositeFK, len(t.compositeFKs))
	for i, fk := range t.compositeFKs {
		f := *fk
		f.Columns = rebindAll(fk.Columns)
		cp.compositeFKs[i] = &f
	}
	// The remaining slices are shared by value but not by array: a
	// copy taken at full capacity would let an append through the
	// alias land in the base table's spare capacity, and the next
	// append through the base overwrite it.
	cp.indexes = append([]*Index(nil), t.indexes...)
	cp.insertHooks = append([]InsertHook(nil), t.insertHooks...)
	cp.updateHooks = append([]UpdateHook(nil), t.updateHooks...)
	cp.deleteHooks = append([]DeleteHook(nil), t.deleteHooks...)
	cp.defaultFilters = append([]drops.Expression(nil), t.defaultFilters...)
	// ctxFilters was copied at the top instead, because reading it
	// needs the read lock this far down no longer holds.
	return &cp
}

// Col looks up a registered column by name.
func (t *Table) Col(name string) *Column { return t.byName[name] }

// Columns returns all registered columns in declaration order.
func (t *Table) Columns() []*Column { return t.columns }

// ForeignKey attaches a foreign-key constraint from col to target on this
// table. Both must be non-nil registered columns. It is the untyped companion
// to (*Col[T]).References — use it to add FKs to AutoTable-derived columns. The
// FK is recorded on the column, so CreateTable and the schema diff/Push see it.
func (t *Table) ForeignKey(col, target *Column, opts ...func(*FK)) *Table {
	if col == nil || target == nil {
		panic("drops/pg: ForeignKey requires non-nil columns")
	}
	fk := &FK{Target: target}
	for _, o := range opts {
		o(fk)
	}
	col.ref = fk
	return t
}

// CompositeFK is a multi-column foreign key declared at the table
// level: FOREIGN KEY (Columns...) REFERENCES Target (TargetColumns...).
// Columns and TargetColumns are positionally paired and must be the
// same length. OnDelete / OnUpdate carry the referential actions.
type CompositeFK struct {
	Name          string
	Columns       []*Column
	Target        *Table
	TargetColumns []*Column
	OnDelete      string
	OnUpdate      string
}

// ForeignKeyN declares a composite (N-column) foreign key from cols on
// this table to targetCols on target, paired positionally. It is the
// multi-column counterpart to ForeignKey; use it for keys that
// reference a composite primary key or a multi-column unique
// constraint.
//
//	orders.ForeignKeyN(
//	    []pg.ColRef{oTenantID, oUserID}, users,
//	    []pg.ColRef{uTenantID, uID}, pg.OnDelete("CASCADE"))
//
// len(cols) must equal len(targetCols) and both must be non-empty;
// violations panic at declaration time. Referential actions are set
// with the same OnDelete / OnUpdate options as ForeignKey. The
// constraint is emitted as a separate ALTER TABLE ADD CONSTRAINT (never
// inlined into CREATE TABLE), matching how every other table-level
// constraint is rendered.
func (t *Table) ForeignKeyN(cols []ColRef, target *Table, targetCols []ColRef, opts ...func(*FK)) *Table {
	if target == nil {
		panic("drops/pg: ForeignKeyN requires a non-nil target table")
	}
	if len(cols) == 0 || len(targetCols) == 0 {
		panic("drops/pg: ForeignKeyN requires at least one column on each side")
	}
	if len(cols) != len(targetCols) {
		panic("drops/pg: ForeignKeyN local and target column counts must match")
	}
	from := make([]*Column, len(cols))
	for i, c := range cols {
		if c == nil {
			panic("drops/pg: ForeignKeyN got a nil local column")
		}
		from[i] = c.col()
	}
	to := make([]*Column, len(targetCols))
	for i, c := range targetCols {
		if c == nil {
			panic("drops/pg: ForeignKeyN got a nil target column")
		}
		to[i] = c.col()
	}
	var f FK
	for _, o := range opts {
		o(&f)
	}
	names := make([]string, len(from))
	for i, c := range from {
		names[i] = c.Name()
	}
	targetNames := make([]string, len(to))
	for i, c := range to {
		targetNames[i] = c.Name()
	}
	t.compositeFKs = append(t.compositeFKs, &CompositeFK{
		Name:          fkName(t.name, names, target.Name(), targetNames),
		Columns:       from,
		Target:        target,
		TargetColumns: to,
		OnDelete:      f.OnDelete,
		OnUpdate:      f.OnUpdate,
	})
	return t
}

// CompositeForeignKeys returns the table's multi-column foreign keys in
// declaration order.
func (t *Table) CompositeForeignKeys() []*CompositeFK { return t.compositeFKs }

// add is the internal registration step used by Add. It does not return
// anything because callers (the Add helper) need to preserve the typed
// *Col[T] handle they were passed.
func (t *Table) add(c *Column) {
	c.table = t
	t.columns = append(t.columns, c)
	t.byName[c.name] = c
}

// Add registers c with t and returns it. It is the primary way to attach
// columns to a table — type inference keeps the *Col[T] handle typed:
//
//	var Users    = pg.NewTable("users")
//	var (
//	    UserID   = pg.Add(Users, pg.BigSerial("id").PrimaryKey())   // *Col[int64]
//	    UserName = pg.Add(Users, pg.Text("name").NotNull())          // *Col[string]
//	    UserAge  = pg.Add(Users, pg.Integer("age"))                  // *Col[int32]
//	)
//
// Go does not allow generic methods, so Add lives as a free function.
func Add[T any](t *Table, c *Col[T]) *Col[T] {
	t.add(c.Column)
	return c
}

// OnInsert registers a hook invoked by InsertBuilder.WriteSQL. The
// hook can fill column values the caller didn't explicitly bind; user
// values always win.
func (t *Table) OnInsert(h InsertHook) *Table {
	t.insertHooks = append(t.insertHooks, h)
	return t
}

// OnUpdate registers a hook invoked by UpdateBuilder.WriteSQL.
func (t *Table) OnUpdate(h UpdateHook) *Table {
	t.updateHooks = append(t.updateHooks, h)
	return t
}

// OnDelete registers a hook invoked by DeleteBuilder.WriteSQL. A hook
// may return a non-nil expression to replace the rendered DELETE
// entirely — used by SoftDelete to flip DELETE into UPDATE.
func (t *Table) OnDelete(h DeleteHook) *Table {
	t.deleteHooks = append(t.deleteHooks, h)
	return t
}

// DefaultFilter appends a predicate applied to every Select / Update /
// Delete against the table, unless the builder is marked Unscoped().
// Filters compose with AND.
//
// The predicate is fixed at declaration time, and so is registration:
// unlike [Table.ContextFilter] this list is not locked, because nothing
// in the package registers a default filter per request — mixins apply
// theirs where the schema is declared. Register from the goroutine that
// builds the schema, before the first query.
//
// When the predicate depends on the request — the tenant, the acting
// subject — use [Table.ContextFilter], whose predicate is built per
// execution from a ctx.
func (t *Table) DefaultFilter(e drops.Expression) *Table {
	t.defaultFilters = append(t.defaultFilters, e)
	return t
}

// ContextFilter registers a predicate resolved at execution time and
// AND-ed into every SELECT, UPDATE and DELETE against the table, unless
// the builder is marked Unscoped().
//
//	Posts.ContextFilter(pg.TenantFilter(PostTenantID))
//
// A DefaultFilter is rendered by WriteSQL, which has no ctx; this is
// resolved by the executors — Rows / All / One / Count on a SELECT and
// Exec / All / One on an UPDATE or DELETE. That distinction is the
// whole point rather than an implementation detail: an eager-loaded
// relation builds its child query as db.Select().From(rel.To) and runs
// it through those same executors, so one hook covers the root query,
// every relation edge (including the per-parent-limit rewrite), UPDATE
// and DELETE at once. A predicate resolved at render time reaches only
// the statements somebody remembered to build with it, which is how a
// tenant guard ends up filtering the parents and loading every tenant's
// children.
//
// The cost is that a rendered statement is no longer complete: see
// [SelectBuilder.ToSQL], and prefer ToSQLCtx when you need the SQL a
// given ctx would actually send.
//
// Safe to call while queries against the table are in flight — see
// ctxFilter for why that is a supported thing to do rather than an
// accident.
func (t *Table) ContextFilter(fn ContextFilterFunc) *Table {
	if fn == nil {
		return t
	}
	t.ctxFiltersMu.Lock()
	defer t.ctxFiltersMu.Unlock()
	t.ctxFilters = append(t.copyCtxFilters(), ctxFilter{fn: fn})
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
//	Posts.ContextFilter(pg.TenantFilter(PostTenantID)).
//	    ScopeWritesByTenant(PostTenantID)
//
// [Entity.ScopeByTenant] calls this for you, so an entity-declared axis
// covers both halves at once. Declaring it twice with the same column
// is idempotent.
//
// The consequence worth stating plainly, because it is the same one
// ScopeByTenant carries: from here on every INSERT into this table
// needs a tenant on its ctx, including one built straight from
// db.Insert(). A statement that legitimately writes outside the ctx
// tenant says so with [InsertBuilder.Unscoped].
func (t *Table) ScopeWritesByTenant(col ColRef) *Table {
	if col == nil {
		return t
	}
	t.setTenantAxis(col.col())
	return t
}

// setTenantAxis records the table's tenant column. Called by
// ScopeWritesByTenant and by Entity.ScopeByTenant, which registers the
// read-side filter in the same breath.
func (t *Table) setTenantAxis(c *Column) {
	t.ctxFiltersMu.Lock()
	defer t.ctxFiltersMu.Unlock()
	t.tenantCol = c
}

// tenantAxis returns the column an INSERT into the table stamps, or nil
// when the table declared none. Nil-safe so a builder can ask without
// knowing whether it has a table at all.
func (t *Table) tenantAxis() *Column {
	if t == nil {
		return nil
	}
	t.ctxFiltersMu.RLock()
	defer t.ctxFiltersMu.RUnlock()
	return t.tenantCol
}

// setContextFilter registers fn under key, replacing any filter
// previously registered under the same key. See ctxFilter for why the
// entity-owned filters are keyed and the exported ones are not.
func (t *Table) setContextFilter(key string, fn ContextFilterFunc) {
	t.ctxFiltersMu.Lock()
	defer t.ctxFiltersMu.Unlock()
	for i := range t.ctxFilters {
		if t.ctxFilters[i].key == key {
			// Replaced by swapping in a new slice rather than by
			// assigning through the old one: a reader is walking that
			// backing array right now with the lock released, and an
			// entity rebuilt per request replaces its own filter on
			// every single request.
			next := t.copyCtxFilters()
			next[i].fn = fn
			t.ctxFilters = next
			return
		}
	}
	t.ctxFilters = append(t.copyCtxFilters(), ctxFilter{key: key, fn: fn})
}

// copyCtxFilters returns a fresh copy of the filter list at exactly its
// own length. The caller holds the write lock. Copying at length rather
// than appending in place is what keeps an append out of the spare
// capacity a reader's slice header still spans.
func (t *Table) copyCtxFilters() []ctxFilter {
	out := make([]ctxFilter, len(t.ctxFilters))
	copy(out, t.ctxFilters)
	return out
}

// hasContextFilters reports whether the table has any context filter to
// resolve. Nil-safe so a SELECT with no FROM table can ask.
func (t *Table) hasContextFilters() bool {
	if t == nil {
		return false
	}
	t.ctxFiltersMu.RLock()
	defer t.ctxFiltersMu.RUnlock()
	return len(t.ctxFilters) > 0
}

// resolveContextFilters builds every registered predicate against ctx,
// each restated against this instance of the table — see
// resolveFilterExprs for what "this instance" buys.
//
// The first filter to fail aborts the whole resolution and no statement
// is sent: a filter that cannot decide what a request may see must not
// be answered with an unfiltered query.
//
// The filter list is snapshotted under the read lock and walked with it
// released. Holding it across the calls would put arbitrary user code —
// a guard that consults a store, a filter that builds a subquery —
// inside the lock, and a filter that registered another filter would
// deadlock against itself.
func (t *Table) resolveContextFilters(ctx context.Context) ([]drops.Expression, error) {
	if t == nil {
		return nil, nil
	}
	t.ctxFiltersMu.RLock()
	filters := t.ctxFilters
	t.ctxFiltersMu.RUnlock()
	if len(filters) == 0 {
		return nil, nil
	}
	out := make([]drops.Expression, 0, len(filters))
	for _, f := range filters {
		e, err := f.fn(ctx)
		if err != nil {
			return nil, err
		}
		if e != nil {
			out = append(out, e)
		}
	}
	return t.resolveFilterExprs(out), nil
}

// hasDefaultFilters reports whether the table carries any render-time
// default filter. Nil-safe, like hasContextFilters.
func (t *Table) hasDefaultFilters() bool { return t != nil && len(t.defaultFilters) > 0 }

// resolveDefaultFilters returns the render-time filters, restated
// against this instance of the table.
func (t *Table) resolveDefaultFilters() []drops.Expression {
	if !t.hasDefaultFilters() {
		return nil
	}
	return t.resolveFilterExprs(t.defaultFilters)
}

// relRef names the relation a column belonging to this table qualifies
// with when the table is not aliased, in the spelling
// [drops.Builder.RelationAlias] keys renames by.
func (t *Table) relRef() string {
	if t.schema == "" {
		return t.name
	}
	return t.schema + "." + t.name
}

// resolveFilterExprs restates a table's automatic predicates against
// the instance of the table the statement actually names.
//
// It is the same problem PageBuilder.resolveOrderBys solves for an
// ordering column, one degree harder. An ordering column arrives as a
// handle, so it can be swapped for the entity's own by Column.key. A
// filter arrives as a predicate — an opaque tree of closures over
// whichever handles built it, with no way in — and every filter this
// package registers for you closes over the *declared* handle:
// TenantFilter over the column it was given, ScopeByTenant over the
// entity's own, a Guard over whatever the caller wrote. Against
// FROM "notes" AS "n" all of them render "notes"."tenantId", which
// names a relation the statement has no FROM entry for, and PostgreSQL
// raises 42P01. Not a widened result — a query that cannot run. So a
// tenant-scoped table, the one kind that must never lose its axis,
// became the one kind that could not be queried under an alias at all,
// and every self-join of one went with it.
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

// AddIndex registers an index to be created alongside the table. The
// index is not emitted by CreateTable — PostgreSQL takes it as its own
// statement; use CreateTableWithIndexes, or emit pg.CreateIndex(idx)
// yourself.
func (t *Table) AddIndex(idx *Index) *Table {
	t.indexes = append(t.indexes, idx)
	return t
}

// Indexes returns the indexes registered with AddIndex.
func (t *Table) Indexes() []*Index { return t.indexes }

// PrimaryKey declares a composite PRIMARY KEY spanning cols. Call
// only when the PK has more than one column; single-column PKs
// continue to be declared on the column via *Col[T].PrimaryKey().
func (t *Table) PrimaryKey(cols ...ColRef) *Table {
	t.compositePK = make([]*Column, len(cols))
	for i, c := range cols {
		t.compositePK[i] = c.col()
	}
	return t
}

// CompositePrimaryKey returns the composite PK columns, or nil
// when the table uses a single-column PK (or none).
func (t *Table) CompositePrimaryKey() []*Column { return t.compositePK }

// primaryKeyColumns returns the table's PRIMARY KEY columns in key
// order, whichever of the two declarations the schema used.
//
// A key can arrive as Table.PrimaryKey(cols...) or by marking columns
// with (*Col[T]).PrimaryKey(). Every reader that needs "the key" —
// the CREATE TABLE body, NewEntity — has to accept both, or a table
// declared one way silently loses its key in the other.
func (t *Table) primaryKeyColumns() []*Column {
	if len(t.compositePK) > 0 {
		return t.compositePK
	}
	var out []*Column
	for _, c := range t.columns {
		if c.primary {
			out = append(out, c)
		}
	}
	return out
}

// AddUnique declares a multi-column UNIQUE constraint named name
// spanning cols. Single-column uniques continue to live on the
// column via *Col[T].Unique().
func (t *Table) AddUnique(name string, cols ...ColRef) *Table {
	if t.compositeUniques == nil {
		t.compositeUniques = map[string][]*Column{}
	}
	out := make([]*Column, len(cols))
	for i, c := range cols {
		out[i] = c.col()
	}
	t.compositeUniques[name] = out
	return t
}

// CompositeUniques returns the table's multi-column unique
// constraints.
func (t *Table) CompositeUniques() map[string][]*Column { return t.compositeUniques }

// AddCheck declares a CHECK constraint with the given name. expr
// is the raw SQL expression after CHECK (...), e.g.
// "age >= 0 AND age < 200".
func (t *Table) AddCheck(name, expr string) *Table {
	if t.checks == nil {
		t.checks = map[string]string{}
	}
	t.checks[name] = expr
	return t
}

// Checks returns the registered CHECK constraints.
func (t *Table) Checks() map[string]string { return t.checks }

// EnableRLS marks the table as having Row-Level Security
// enabled. The snapshot/diff generator emits the matching
// ALTER TABLE ... ENABLE ROW LEVEL SECURITY when the flag flips
// from false to true.
func (t *Table) EnableRLS() *Table { t.rlsEnabled = true; return t }

// RLSEnabled reports whether the table has RLS enabled.
func (t *Table) RLSEnabled() bool { return t.rlsEnabled }

// AddPolicy attaches a row-level security policy to the table.
// Policies are inert until EnableRLS is also called.
func (t *Table) AddPolicy(p *Policy) *Table {
	if t.policies == nil {
		t.policies = map[string]*Policy{}
	}
	t.policies[p.Name()] = p
	return t
}

// Policies returns the registered policies keyed by name.
func (t *Table) Policies() map[string]*Policy { return t.policies }

// hasHooks reports whether the table has any lifecycle hooks
// registered — used by builders to skip the hook pipeline when
// nothing is wired up.
func (t *Table) hasInsertHooks() bool { return len(t.insertHooks) > 0 }
func (t *Table) hasUpdateHooks() bool { return len(t.updateHooks) > 0 }

// writeName writes only the (schema-qualified) table name, with no alias.
// Used by DDL where AS clauses are not permitted.
func (t *Table) writeName(b *drops.Builder) {
	b.WriteQualified(t.schema, t.name)
}

// writeFrom writes the form used inside FROM/JOIN clauses
// ("schema"."table" AS "alias").
func (t *Table) writeFrom(b *drops.Builder) {
	t.writeName(b)
	if t.alias != "" {
		b.WriteString(" AS ")
		b.WriteIdent(t.alias)
	}
}

// writeRef writes the identifier used to qualify columns belonging to the
// table — the alias if set, otherwise the (schema-qualified) name.
//
// A handle on the declared table also renders as an alias while the
// builder is inside a fragment that renamed the relation: that is how
// an automatic predicate built from the package-level columns follows
// the table into an aliased query. See resolveFilterExprs.
func (t *Table) writeRef(b *drops.Builder) {
	if t.alias != "" {
		b.WriteIdent(t.alias)
		return
	}
	if renamed := b.RelationAlias(t.relRef()); renamed != "" {
		b.WriteIdent(renamed)
		return
	}
	b.WriteQualified(t.schema, t.name)
}

// WriteSQL writes the FROM/JOIN form. Implements drops.Expression.
func (t *Table) WriteSQL(b *drops.Builder) { t.writeFrom(b) }
