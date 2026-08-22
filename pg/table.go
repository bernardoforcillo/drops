package pg

import (
	"context"
	"errors"
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

	// scope carries every automatic predicate and lifecycle hook the
	// table declares. It is a pointer, and an alias taken off this
	// table SHARES it rather than copying it — see tableScope, and see
	// Table.As for the write that went out unscoped while it was a
	// snapshot.
	scope *tableScope
}

// tableScope is the automatic-predicate state of a table: the two
// filter lists, the write-side tenant column and the lifecycle hooks.
// It lives behind a pointer so that a table and every alias taken off
// it share ONE of them.
//
// Sharing is the whole point, and it was learned from a destructive
// bug. While As copied these lists, an alias was a snapshot of the
// table's scoping at the instant As was called — and the spelling this
// package invites is exactly the one that loses:
//
//	var Users = pg.NewTable("users")   // package scope
//	var U     = Users.As("u")          // package scope, before init
//
//	func init() { Users.ContextFilter(pg.TenantFilter(UserTenantID)) }
//
// Go initialises package-level variables before it runs init, so U was
// taken while the table had no tenant axis and never gained one. A
// statement written against the alias then rendered, on a ctx carrying
// no tenant at all and without refusing, DELETE FROM "users" AS "u" —
// no predicate, every tenant's rows, and nothing walks a DELETE back.
// The same snapshot dropped a soft-delete DeleteHook applied after the
// alias was taken, turning the rewrite-to-UPDATE into a hard DELETE.
//
// It also contradicted the invariant [Table.As] is written around and
// pg/alias_test.go spends its length on: an aliased handle IS the
// column it was copied from, an aliased table IS the table. A snapshot
// makes that true of the columns and false of the scoping, which is the
// half where being wrong destroys data.
//
// One consequence to state plainly, because it follows from sharing and
// is not a bug: registering a filter or a hook ON an alias registers it
// on the table, and so on every other alias of it. That is what "the
// same table" means. Where two genuinely different scopings are wanted,
// they are two tables.
//
// The mutex guards every field, and it is the same mutex the base table
// and all its aliases lock — which is the other half of what sharing
// has to mean. An alias with a lock of its own would serialise against
// nothing while another goroutine replaced the list it was reading.
// Registration is a supported thing to do while queries are in flight
// (see ctxFilter for why), so the lists are REPLACED rather than edited
// in place: a reader takes the slice header under the read lock and
// walks it with the lock released, which is only sound while no writer
// touches an element a reader may already be holding.
type tableScope struct {
	mu sync.RWMutex

	// defaultFilters are predicates applied automatically by
	// SelectBuilder / UpdateBuilder / DeleteBuilder unless the caller
	// opts out with Unscoped(). Used to implement default scopes
	// (e.g. SoftDelete's "deleted_at IS NULL" guard). Declaration-time
	// only — see Table.DefaultFilter.
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

	// insertHooks / updateHooks / deleteHooks are the optional
	// lifecycle hooks registered on the table. They are invoked by the
	// corresponding builders during WriteSQL. Empty by default — a
	// table with no hooks behaves exactly as it did before this
	// feature shipped.
	insertHooks []InsertHook
	updateHooks []UpdateHook
	deleteHooks []DeleteHook
}

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
		name:      name,
		byName:    map[string]*Column{},
		relations: map[string]*Relation{},
		scope:     new(tableScope),
	}
}

// NewSchemaTable creates a table in an explicit schema.
func NewSchemaTable(schema, name string) *Table {
	mustIdent("schema", schema)
	mustIdent("table", name)
	return &Table{
		schema:    schema,
		name:      name,
		byName:    map[string]*Column{},
		relations: map[string]*Relation{},
		scope:     new(tableScope),
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
// An alias is a second handle on ONE table, and the rule the four
// dialects state in the same words has two halves: the alias SHARES the
// table's whole scope — both filter lists, the write-side tenant column
// and the lifecycle hooks — and it REBINDS its columns. Sharing is what
// stops the alias disagreeing with its table about which rows may be
// seen; rebinding is what stops it disagreeing with the statement about
// which relation a reference names. Neither half is optional, and each
// was got wrong on its own in some dialect before this was written
// down.
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
// The automatic predicates a table carries are SHARED with the alias
// rather than copied, and shared rather than snapshotted because the
// alias is the same table: a filter or a lifecycle hook registered on
// either handle at any time applies to both, in whichever order the two
// happen. That ordering is not hypothetical. Go initialises
// package-level variables before it runs init, so an alias declared
// beside its table is taken before any init or constructor that
// declares the scoping — and while the lists were copied, that alias
// was unscoped for ever. It rendered DELETE FROM "users" AS "u" with
// no predicate at all, on a ctx carrying no tenant, without refusing;
// and it lost a soft-delete guard registered after it was taken, so it
// read rows the application had deleted.
//
// BOTH filter lists are shared, on the same terms, because the argument
// does not distinguish them: a [Table.DefaultFilter] registered after
// As was taken went missing exactly as a [Table.ContextFilter] did, and
// the difference between the two failures is only how bad it is.
//
// The predicates cannot be rewritten, being closures over the handles
// they were given, so they are rendered inside a relation rename
// instead: see resolveFilterExprs. Without it an aliased query against
// a scoped table could not run at all — "notes"."tenantId" against
// FROM "notes" AS "n" is 42P01, not a widened result — which made the
// one table shape that must never lose its tenant axis the one shape
// that could not be queried under an alias.
//
// The consequence in the other direction is that registering on an
// alias registers on the table, and so on every other alias of it,
// which is what "the same table" has to mean. Where two genuinely
// different scopings are wanted, they are two tables — so register over
// the DECLARED column handles even when the call goes through an alias,
// since the base table renders the same predicate and an alias handle
// would qualify with a relation its statement never names.
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
// The SHAPE of the table is still a snapshot, and only the shape: a
// column, relation or constraint added to the base table after As returned does
// not reach the alias, for the same package-level-var reason described
// above. Take the alias at the query site, or after the schema is
// complete. Scoping is exempt from that caveat because scoping is the
// half where being a snapshot destroys data.
func (t *Table) As(alias string) *Table {
	mustIdent("alias", alias)
	// The whole-struct copy carries the scope POINTER across, so the
	// alias reads and writes the same filter lists, the same tenant
	// column, the same hooks and the same lock as the table it names.
	// Nothing here needs that lock: the copy reads no field the lock
	// guards.
	cp := *t
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
	// The index list is shared by value but not by array: a copy taken
	// at full capacity would let an append through the alias land in
	// the base table's spare capacity, and the next append through the
	// base overwrite it. It is the last slice here that is copied at
	// all — the filter lists and the hook lists live in the shared
	// tableScope, which cp already points at.
	cp.indexes = append([]*Index(nil), t.indexes...)
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
	t.scope.mu.Lock()
	defer t.scope.mu.Unlock()
	t.scope.insertHooks = appendShared(t.scope.insertHooks, h)
	return t
}

// OnUpdate registers a hook invoked by UpdateBuilder.WriteSQL.
func (t *Table) OnUpdate(h UpdateHook) *Table {
	t.scope.mu.Lock()
	defer t.scope.mu.Unlock()
	t.scope.updateHooks = appendShared(t.scope.updateHooks, h)
	return t
}

// OnDelete registers a hook invoked by DeleteBuilder.WriteSQL. A hook
// may return a non-nil expression to replace the rendered DELETE
// entirely — used by SoftDelete to flip DELETE into UPDATE.
func (t *Table) OnDelete(h DeleteHook) *Table {
	t.scope.mu.Lock()
	defer t.scope.mu.Unlock()
	t.scope.deleteHooks = appendShared(t.scope.deleteHooks, h)
	return t
}

// insertHookList, updateHookList and deleteHookList return the hooks
// registered on the table — through the shared scope, so an alias
// answers with the table's own rather than with whatever list existed
// when As was called. A soft-delete rewrite lost that way turns into a
// hard DELETE; see tableScope.
//
// The slice is taken under the read lock and walked with it released,
// which the appendShared discipline makes sound.
func (t *Table) insertHookList() []InsertHook {
	t.scope.mu.RLock()
	defer t.scope.mu.RUnlock()
	return t.scope.insertHooks
}

func (t *Table) updateHookList() []UpdateHook {
	t.scope.mu.RLock()
	defer t.scope.mu.RUnlock()
	return t.scope.updateHooks
}

func (t *Table) deleteHookList() []DeleteHook {
	t.scope.mu.RLock()
	defer t.scope.mu.RUnlock()
	return t.scope.deleteHooks
}

// DefaultFilter appends a predicate applied to every Select / Update /
// Delete against the table, unless the builder is marked Unscoped().
// Filters compose with AND.
//
// The predicate is fixed at declaration time, but registration is not
// assumed to be: the list lives in the shared [tableScope] under the
// same lock [Table.ContextFilter] takes, so declaring a default filter
// on a table other goroutines are querying is as supported here as it
// is there — and so an alias taken before ApplyMixins still carries the
// soft-delete guard the mixin registers.
//
// When the predicate depends on the request — the tenant, the acting
// subject — use [Table.ContextFilter], whose predicate is built per
// execution from a ctx.
func (t *Table) DefaultFilter(e drops.Expression) *Table {
	t.scope.mu.Lock()
	defer t.scope.mu.Unlock()
	t.scope.defaultFilters = appendShared(t.scope.defaultFilters, e)
	return t
}

// defaultFilterList returns the table's render-time filters through the
// shared scope. Nil-safe so a statement with no table can ask.
func (t *Table) defaultFilterList() []drops.Expression {
	if t == nil {
		return nil
	}
	t.scope.mu.RLock()
	defer t.scope.mu.RUnlock()
	return t.scope.defaultFilters
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
	panic("drops/pg: ScopeWritesByTenant column " + columnPath(c) +
		" is not a column of " + t.Name())
}

// setTenantAxis records the table's tenant column. Called by
// ScopeWritesByTenant and by Entity.ScopeByTenant, which registers the
// read-side filter in the same breath.
func (t *Table) setTenantAxis(c *Column) {
	t.scope.mu.Lock()
	defer t.scope.mu.Unlock()
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
// rather than the one it carried when As was called. See tableScope for
// the cross-tenant DELETE that answer prevents.
func (t *Table) ctxFilterList() []ctxFilter {
	if t == nil {
		return nil
	}
	t.scope.mu.RLock()
	defer t.scope.mu.RUnlock()
	return t.scope.ctxFilters
}

// hasContextFilters reports whether the table has any context filter to
// resolve. Nil-safe so a SELECT with no FROM table can ask.
func (t *Table) hasContextFilters() bool {
	return len(t.ctxFilterList()) > 0
}

// resolveContextFilters builds every registered predicate against ctx,
// walks the statements written inside those predicates, and restates
// the result against this instance of the table — see
// resolveFilterExprs for what "this instance" buys.
//
// The walk is the half that was missing. A filter answers with a
// predicate, and a predicate is a place a statement can be written:
// AuthorizeWith(CustomGuard(...)) returning
// In(col, Subquery(SELECT ... FROM posts)) is the ordinary spelling of
// "the rows this subject may see are the ones some other scoped table
// names". Handed straight to the builder, that inner statement was
// reached only by the renderer, which has no ctx — so it wrote its
// DefaultFilters, none of its ContextFilters, and did not refuse on a
// ctx carrying no tenant at all. The guard meant to decide what a
// request may see was itself the unscoped read, on SELECT, on JOIN, on
// UPDATE and on DELETE alike, because all four reach the filters
// through here. It is the same invariant [SelectBuilder.resolveCtx]
// keeps for the lists a caller writes: no expression the renderer reads
// may be invisible to the resolver, and a filter's own predicate is
// such an expression.
//
// The walk runs before resolveFilterExprs rather than after, so the
// alias rename wraps the resolved tree. The other order would resolve
// the ExprFunc that rename returns — an opaque closure the walk
// terminates at — which is the round-4 defect rebuilt one layer in.
//
// The filter function itself is called under the cycle chain, not just
// the predicate it returns. A filter is arbitrary user code and may
// consult a store — or run a query of its own, and a query of its own
// against this table is the same non-terminating shape by another
// route. Carrying the chain into it costs one allocation on a table
// that has filters at all, which is a path already building predicates.
//
// The first filter to fail aborts the whole resolution and no statement
// is sent: a filter that cannot decide what a request may see must not
// be answered with an unfiltered query. A refusal from inside the walk
// aborts it just as hard, for the same reason.
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

// ErrContextFilterCycle is returned when resolving a table's automatic
// predicates re-enters the same table: a context filter (or a default
// filter) on "notes" whose predicate embeds a statement selecting from
// "notes", directly or through another table whose filter comes back.
//
// Walking a filter's own predicate is what makes the cycle reachable at
// all, and the shape has to be answered rather than followed: each turn
// asks the filter for a fresh predicate, so there is no fixed point to
// converge on and no builder to notice it has been here before. Left
// alone it is not a wrong answer, it is a goroutine that never returns
// — a hung request holding a connection, in the resolver every read and
// every write goes through.
//
// Three answers were on the table and this is the third. A depth limit
// terminates, but it turns an unrunnable declaration into a failure at
// some arbitrary nesting depth, and picking the depth means picking
// which legitimate nesting to break. A "resolving" flag on the *Table
// terminates too, but the flag is shared state on a value every request
// reads concurrently, so it needs its own lock and it reports a cycle
// whenever two goroutines resolve the same table at the same instant —
// a false refusal that appears only under load. The chain is carried on
// the ctx instead: it is per-resolution by construction, needs no lock,
// costs one allocation per table that actually has filters, and names
// the cycle exactly — including a mutual one, A through B back to A,
// which neither of the other answers distinguishes from depth.
//
// The refusal is not a dead end. A filter that has to consult its own
// table says Unscoped() on the statement it embeds: that is the shape
// that terminates, it is the shape the author meant (the inner read
// selects the rows the filter is about to restrict, so restricting it
// with the filter being built is circular in the SQL as well), and it
// is the same escape resolveFilterExprs already documents for a filter
// that selects from its own base table under an alias.
var ErrContextFilterCycle = errors.New("drops/pg: a table's automatic filter selects from the table it filters")

// filterChainKey is the ctx key under which the chain of tables whose
// automatic filters are currently being resolved is carried. An
// unexported struct type, so nothing outside this package can plant a
// chain of its own and talk the resolver out of a refusal.
type filterChainKey struct{}

// filterChain is one link of that chain. It is a linked list rather
// than a map because it is written once per link and read by walking:
// an immutable list can be shared by every ctx derived from it without
// copying, which is what makes the chain safe to carry across the
// concurrent resolutions a single filter may fan out into.
type filterChain struct {
	ref  string
	prev *filterChain
}

// enterFilterResolution returns the ctx to resolve t's automatic
// filters under, or [ErrContextFilterCycle] when t is already being
// resolved further up the same chain.
//
// Tables are identified by relation reference rather than by pointer,
// so an alias — which Table.As gives its own *Table and its own copy of
// the filter list — counts as the same relation it aliases. That is the
// identity SQL uses, and the identity the recursion has: a filter on
// "notes" that embeds SELECT ... FROM "notes" AS "n" re-enters the same
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

// hasDefaultFilters reports whether the table carries any render-time
// default filter. Nil-safe, like hasContextFilters.
func (t *Table) hasDefaultFilters() bool { return len(t.defaultFilterList()) > 0 }

// resolveDefaultFilters returns the render-time filters, restated
// against this instance of the table.
//
// Render-time is the whole of it: there is no ctx here, so a statement
// written inside a default filter renders through WriteSQL and carries
// none of its own context filters. That is why the executors resolve
// the list first and hand the answer back through resolvedDefaults —
// see [Table.resolveDefaultFilterExprs]. This function stays the
// answer on the ToSQL path, which has no ctx to do better with.
func (t *Table) resolveDefaultFilters() []drops.Expression {
	filters := t.defaultFilterList()
	if len(filters) == 0 {
		return nil
	}
	return t.resolveFilterExprs(filters)
}

// resolveDefaultFilterExprs walks the statements written inside the
// table's default filters against ctx, returning the restated list —
// or nil when no default filter had a statement in it, so the caller
// keeps rendering exactly what it rendered before.
//
// A default filter is declaration-time and a context filter is
// request-time, but the predicate each of them answers with is the same
// kind of thing, and a statement embedded in one is as invisible to the
// renderer as a statement embedded in the other:
// DefaultFilter(NotIn(col, Subquery(SELECT ... FROM blocked))) wrote the
// inner statement with its own DefaultFilters and none of its
// ContextFilters, on any ctx at all. Whatever a caller can hand a
// filter, the resolver has to be able to walk — the invariant is about
// what the renderer reads, not about when the list was declared.
//
// Returning nil for "nothing changed" is what keeps the rendering
// promise: a filter with no statement inside it is not rebuilt, is not
// re-wrapped, and renders the bytes it always did — which is nearly
// every default filter there is, a soft-delete guard being the shape
// this feature exists for.
//
// mayHoldStatements is checked before the chain is entered so that the
// same near-universal case does not pay for a ctx it has no use for.
// A default filter list is read once per statement per table, so a
// soft-delete guard would otherwise cost one allocation on every query
// against every table that has one.
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

// resolvedDefaults holds, per table a statement names, the default
// filters resolved for one execution of that statement.
//
// It is a side table rather than a field on the *Table because the
// resolution belongs to the execution: two requests render the same
// table at once, and a resolved list written back onto the shared table
// would let one request's subquery scoping decide the other's. It is
// keyed by *Table rather than by name so that a statement naming the
// same relation twice — a self-join, one side aliased — keeps each
// side's own restatement, which is the distinction resolveFilterExprs
// exists to make.
//
// The zero value is a nil map and answers every lookup with the
// unresolved list, which is what the ToSQL path and every statement
// with nothing to resolve get.
type resolvedDefaults map[*Table][]drops.Expression

// of returns the default filters to render for t: the resolved list
// when this execution produced one, and otherwise the render-time list,
// unchanged and byte for byte.
func (d resolvedDefaults) of(t *Table) []drops.Expression {
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
func (t *Table) hasInsertHooks() bool { return len(t.insertHookList()) > 0 }
func (t *Table) hasUpdateHooks() bool { return len(t.updateHookList()) > 0 }

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

// fromTable implements fromRelation, which is how a table handed to
// [SelectBuilder.FromExpr] is recognised as a RELATION rather than as
// one more expression and gets the scoping its From-clause twin gets.
// Being a drops.Expression is exactly what let it in unscoped.
func (t *Table) fromTable() *Table { return t }
