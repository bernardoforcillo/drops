package pg

import (
	"fmt"
	"sort"
	"strings"

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

	// rlsForced mirrors PG's ALTER TABLE ... FORCE ROW LEVEL
	// SECURITY: policies apply to the table's owner too.
	rlsForced bool

	// insertHooks / updateHooks / deleteHooks are the optional
	// lifecycle hooks registered on this table. They are invoked by
	// the corresponding builders during WriteSQL. Empty by default —
	// a table with no hooks behaves exactly as it did before this
	// feature shipped.
	insertHooks []InsertHook
	updateHooks []UpdateHook
	deleteHooks []DeleteHook

	// filters are the predicates applied automatically by
	// SelectBuilder / UpdateBuilder / DeleteBuilder. A named filter
	// (AddFilter) can be bypassed one at a time with IgnoreFilters;
	// an anonymous one (DefaultFilter) only by Unscoped. Used to
	// implement default scopes — SoftDelete's "deletedAt IS NULL"
	// guard, a tenancy axis. See filters.go.
	filters []tableFilter

	// renamedFrom is the name this table used to have, set by
	// RenamedFrom. See (*Col[T]).RenamedFrom for what it is for.
	renamedFrom string
}

// RenamedFrom states that this table is the table that used to be
// called previous. It is the table-level counterpart of
// (*Col[T]).RenamedFrom, and carries the same fact for the same
// reason: a diff sees one table gone and another arrived, and nothing
// but the schema can say they are the same table.
//
//	var Users = pg.NewTable("people").RenamedFrom("users")
func (t *Table) RenamedFrom(previous string) *Table {
	t.renamedFrom = previous
	return t
}

// PreviousName returns the name the table was declared to have been
// renamed from, or empty when it was not.
func (t *Table) PreviousName() string { return t.renamedFrom }

// NewTable creates a table in the default ("public") schema. The name
// is validated and the constructor panics on invalid identifiers — see
// ErrInvalidIdentifier — because schemas are typically declared in
// package init / var blocks where a bad name should fail loudly at
// startup rather than at the first query.
func NewTable(name string) *Table {
	mustIdent("table", name)
	return &Table{name: name, byName: map[string]*Column{}, relations: map[string]*Relation{}}
}

// NewSchemaTable creates a table in an explicit schema.
func NewSchemaTable(schema, name string) *Table {
	mustIdent("schema", schema)
	mustIdent("table", name)
	return &Table{schema: schema, name: name, byName: map[string]*Column{}, relations: map[string]*Relation{}}
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

// RelationNames returns the names of every relation declared on t,
// sorted. It is the counterpart of [Table.FilterNames]: a table can
// already be asked whether it has one relation, and this asks it
// which ones it has — the question a tool that walks a schema has to
// ask, because nothing outside the package can range over the map.
func (t *Table) RelationNames() []string {
	out := make([]string, 0, len(t.relations))
	for name := range t.relations {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
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
// What is not rewritten is anything the caller built and drops only
// re-emits: a predicate, and a Patch operation. Both are closed over
// the handles they were given. A global filter registered by
// SoftDeleteMixin, or an authz guard built from the package-level
// columns, still qualifies with the table name — so against a query
// whose only FROM entry is the alias PostgreSQL raises 42P01, and in a
// self-join the guard binds to whichever side is un-aliased rather
// than to the one you meant. Scope an aliased query with Unscoped (or
// IgnoreFilters, when only one filter is in the way) plus an explicit
// predicate built from the alias's own handles, and build a Patch for
// an aliased entity from the alias's handles too.
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
	// The remaining slices are shared by value but not by array: a
	// copy taken at full capacity would let an append through the
	// alias land in the base table's spare capacity, and the next
	// append through the base overwrite it.
	cp.indexes = append([]*Index(nil), t.indexes...)
	cp.insertHooks = append([]InsertHook(nil), t.insertHooks...)
	cp.updateHooks = append([]UpdateHook(nil), t.updateHooks...)
	cp.deleteHooks = append([]DeleteHook(nil), t.deleteHooks...)
	cp.filters = append([]tableFilter(nil), t.filters...)
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

// DefaultFilter appends an anonymous predicate applied to every
// Select / Update / Delete against the table. Filters compose with AND.
//
// Anonymous means nothing can bypass it but Unscoped(), which bypasses
// every other filter on the table at the same time. Prefer AddFilter,
// which gives the predicate a name a single query can step around
// without giving up the rest of the table's scoping.
func (t *Table) DefaultFilter(e drops.Expression) *Table {
	t.filters = append(t.filters, tableFilter{pred: e})
	return t
}

// AddFilter appends a predicate under name, applied to every Select /
// Update / Delete against the table exactly as DefaultFilter's is —
// except that a query can bypass this one alone, by name:
//
//	Posts.AddFilter(pg.FilterSoftDelete, pg.IsNull(deletedAt))
//	db.Select().From(Posts).IgnoreFilters(pg.FilterSoftDelete)
//
// Names are per table and re-registering one appends a second filter
// rather than replacing the first, so both apply and IgnoreFilters
// drops both. An empty name panics: it would register a filter that
// reads as named at the call site and behaves as anonymous at the
// query, which is the failure this whole mechanism exists to prevent.
func (t *Table) AddFilter(name string, e drops.Expression) *Table {
	if name == "" {
		panic("drops/pg: AddFilter needs a non-empty name — use DefaultFilter for an anonymous filter")
	}
	t.filters = append(t.filters, tableFilter{name: name, pred: e})
	return t
}

// Filters returns the table's global-filter predicates in registration
// order, named and anonymous alike.
func (t *Table) Filters() []drops.Expression {
	out := make([]drops.Expression, len(t.filters))
	for i, f := range t.filters {
		out[i] = f.pred
	}
	return out
}

// FilterNames returns the names of the table's named filters in
// registration order. Anonymous filters contribute nothing.
func (t *Table) FilterNames() []string {
	var out []string
	for _, f := range t.filters {
		if f.name != "" {
			out = append(out, f.name)
		}
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

// PrimaryKey declares the table's PRIMARY KEY spanning cols.
//
// It is the spelling for a key of more than one column, which cannot
// ride on a column definition, and it accepts a key of one — the
// snapshot, the CREATE TABLE and the diff all treat that identically
// to the same key declared with (*Col[T]).PrimaryKey(), so a schema
// that narrows a two-column key to one by editing this call is a
// migration and not a silent divergence.
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

// ForceRLS marks the table as having Row-Level Security forced —
// ALTER TABLE ... FORCE ROW LEVEL SECURITY, which subjects the
// table's owner to its own policies instead of exempting them.
//
// Like AddPolicy it is inert until EnableRLS is also called: PostgreSQL
// keeps the two flags apart, and forcing a table nothing filters
// filters nothing.
func (t *Table) ForceRLS() *Table { t.rlsForced = true; return t }

// RLSForced reports whether the table has RLS forced.
func (t *Table) RLSForced() bool { return t.rlsForced }

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
func (t *Table) writeRef(b *drops.Builder) {
	if t.alias != "" {
		b.WriteIdent(t.alias)
		return
	}
	b.WriteQualified(t.schema, t.name)
}

// WriteSQL writes the FROM/JOIN form. Implements drops.Expression.
func (t *Table) WriteSQL(b *drops.Builder) { t.writeFrom(b) }
