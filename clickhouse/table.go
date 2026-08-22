package clickhouse

import "github.com/bernardoforcillo/drops"

// Table represents a ClickHouse table. Beyond columns, it carries the
// engine spec plus the optional clauses every CREATE TABLE may
// stipulate: ORDER BY, PARTITION BY, PRIMARY KEY, SAMPLE BY, TTL, and
// the SETTINGS bag.
type Table struct {
	database string
	name     string
	alias    string
	columns  []*Column
	byName   map[string]*Column

	engine     Engine
	orderBy    []ColRef
	partition  []drops.Expression
	primaryKey []ColRef
	sampleBy   drops.Expression
	ttl        string
	settings   []string // "key = value" raw pairs

	// scope carries every automatic predicate and lifecycle hook the
	// table declares. It is a pointer, and an alias taken off this
	// table SHARES it rather than copying it — see tableScope, and see
	// Table.As.
	scope *tableScope
}

// NewTable creates a table in the default database. The name is
// validated and a bad identifier panics at startup (see
// ErrInvalidIdentifier).
func NewTable(name string) *Table {
	mustIdent("table", name)
	return &Table{name: name, byName: map[string]*Column{}, scope: &tableScope{}}
}

// NewDatabaseTable scopes the table to an explicit database.
func NewDatabaseTable(database, name string) *Table {
	mustIdent("database", database)
	mustIdent("table", name)
	return &Table{database: database, name: name, byName: map[string]*Column{}, scope: &tableScope{}}
}

// Name / Database / Alias accessors.
func (t *Table) Name() string     { return t.name }
func (t *Table) Database() string { return t.database }
func (t *Table) Alias() string    { return t.alias }

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
// reference reached through it — e := events.As("e"); e.Col("id") —
// qualifies with the alias while the original package-level handles go
// on qualifying with the table name. That is what makes both sides of a
// self-join addressable at once, and it is why the copy is not a
// shallow one: sharing the column slice would leave every reference
// pointing at the un-aliased table, which ClickHouse cannot resolve
// once the alias has shadowed the name.
//
// An aliased handle still *means* the column it was copied from. The
// INSERT column list and the hook bookkeeping identify a column through
// Column.key, which collapses the copy back onto the declared column.
// Aliasing changes how a reference renders and nothing else.
//
// The automatic predicates a table carries are SHARED with the alias
// rather than copied, and shared rather than snapshotted because the
// alias is the same table: a filter or a lifecycle hook registered on
// either handle at any time applies to both, in whichever order the two
// happen. That ordering is not hypothetical. Go initialises
// package-level variables before it runs init, so an alias declared
// beside its table is taken before any init or constructor that
// declares the scoping — and while the lists were copied, that alias
// was unscoped for ever. It rendered SELECT … FROM `events` AS `e`
// with no predicate at all, on a ctx carrying no tenant, without
// refusing; and it lost a soft-delete guard registered after it was
// taken, so it read rows the application had deleted. The example is a
// SELECT because this dialect models no DELETE; the alias loses its
// filters on whichever statements it does have.
//
// BOTH filter lists are shared, on the same terms, because the argument
// does not distinguish them: a [Table.DefaultFilter] registered after
// As was taken went missing exactly as a [Table.ContextFilter] did, and
// the difference between the two failures is only how bad it is.
//
// The predicates cannot be rewritten, being closures over the handles
// they were given, so they are rendered inside a relation rename
// instead: see resolveFilterExprs. Without it an aliased query against
// a scoped table could not run at all — `events`.`tenantId` against
// FROM `events` AS `e` is UNKNOWN_IDENTIFIER, not a widened result —
// which made the one table shape that must never lose its tenant axis
// the one shape that could not be queried under an alias.
//
// The consequence in the other direction is that registering on an
// alias registers on the table, and so on every other alias of it,
// which is what "the same table" has to mean. Where two genuinely
// different scopings are wanted, they are two tables — so register over
// the DECLARED column handles even when the call goes through an alias,
// since the base table renders the same predicate and an alias handle
// would qualify with a relation its statement never names.
//
// What is still not rewritten is anything the caller built and drops
// only re-emits: any predicate handed to Where, closed over the handles
// it was given. Build the predicates of an aliased query from the
// alias's own handles.
//
// The engine clauses — ORDER BY, PARTITION BY, PRIMARY KEY, SAMPLE BY —
// keep pointing at the original's columns. Every one of them renders
// under bare identifiers (see writeTableSuffix), so the alias cannot
// reach them and rebinding would change no output.
//
// The alias is validated like any other identifier and a bad one —
// including the empty string, which used to pass through and hand back
// an un-aliased copy — panics with ErrInvalidIdentifier.
//
// The SHAPE of the table is still a snapshot, and only the shape: a
// column, or engine clause added to the base table after As returned does
// not reach the alias, for the same package-level-var reason described
// above. Take the alias at the query site, or after the schema is
// complete. Scoping is exempt from that caveat because scoping is the
// half where being a snapshot destroys data.
func (t *Table) As(alias string) *Table {
	mustIdent("alias", alias)
	cp := *t
	// cp.scope is the same pointer, on purpose: see tableScope.
	cp.alias = alias
	cp.columns = make([]*Column, len(t.columns))
	cp.byName = make(map[string]*Column, len(t.byName))
	for i, c := range t.columns {
		aliased := *c
		aliased.table = &cp
		aliased.origin = c.key()
		cp.columns[i] = &aliased
		cp.byName[aliased.name] = &aliased
	}
	return &cp
}

// OrderByColumns returns the names of the table's sorting key, in
// declaration order. Empty when no ORDER BY was set.
func (t *Table) OrderByColumns() []string {
	out := make([]string, 0, len(t.orderBy))
	for _, c := range t.orderBy {
		out = append(out, c.col().Name())
	}
	return out
}

// Col looks up a column by name.
func (t *Table) Col(name string) *Column { return t.byName[name] }

// Columns returns the columns in declaration order.
func (t *Table) Columns() []*Column { return t.columns }

// add registers a column. Used by the generic Add helper below.
func (t *Table) add(c *Column) {
	c.table = t
	t.columns = append(t.columns, c)
	t.byName[c.name] = c
}

// Add registers c with t and returns it. Type inference keeps the
// *Col[T] handle typed.
//
//	var Events = clickhouse.NewTable("events")
//	var (
//	    EventID = clickhouse.Add(Events, clickhouse.UUID("id"))
//	    EventTS = clickhouse.Add(Events, clickhouse.DateTime("ts", "UTC"))
//	)
func Add[T any](t *Table, c *Col[T]) *Col[T] {
	t.add(c.Column)
	return c
}

// Engine binding -----------------------------------------------------

// Engine sets the table's engine. Required before CREATE TABLE.
func (t *Table) Engine(e Engine) *Table { t.engine = e; return t }

// OrderBy sets the ORDER BY columns (MergeTree family).
func (t *Table) OrderBy(cols ...ColRef) *Table {
	t.orderBy = append(t.orderBy, cols...)
	return t
}

// PartitionBy sets the PARTITION BY expression(s).
func (t *Table) PartitionBy(exprs ...drops.Expression) *Table {
	t.partition = append(t.partition, exprs...)
	return t
}

// PrimaryKey sets an explicit PRIMARY KEY (defaults to ORDER BY when
// omitted on MergeTree-family engines).
func (t *Table) PrimaryKey(cols ...ColRef) *Table {
	t.primaryKey = append(t.primaryKey, cols...)
	return t
}

// SampleBy sets the SAMPLE BY expression.
func (t *Table) SampleBy(e drops.Expression) *Table { t.sampleBy = e; return t }

// TTL sets the table-wide TTL expression (raw SQL).
func (t *Table) TTL(expr string) *Table { t.ttl = expr; return t }

// Setting appends a "key = value" pair to the SETTINGS clause.
func (t *Table) Setting(key, value string) *Table {
	t.settings = append(t.settings, key+" = "+value)
	return t
}

// Hooks / default filters --------------------------------------------

// OnInsert registers a hook invoked before an INSERT is rendered.
//
// The list lives in the shared [tableScope] under the same lock
// [Table.ContextFilter] takes, so an alias taken before the hook was
// registered still runs it.
func (t *Table) OnInsert(h InsertHook) *Table {
	t.scope.mu.Lock()
	defer t.scope.mu.Unlock()
	t.scope.insertHooks = appendShared(t.scope.insertHooks, h)
	return t
}

// DefaultFilter appends a predicate applied to every Select against
// the table, unless the builder is marked Unscoped().
//
// The predicate is fixed at declaration time, but registration is not
// assumed to be: the list lives in the shared [tableScope], so an alias
// taken before SoftDelete still carries the guard the mixin registers.
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

// DefaultFilters returns the table's default-scope predicates.
func (t *Table) DefaultFilters() []drops.Expression { return t.defaultFilterList() }

func (t *Table) insertHookList() []InsertHook {
	if t == nil {
		return nil
	}
	t.scope.mu.RLock()
	defer t.scope.mu.RUnlock()
	return t.scope.insertHooks
}

func (t *Table) hasInsertHooks() bool { return len(t.insertHookList()) > 0 }

// Rendering helpers --------------------------------------------------

// writeName writes the (database-qualified) name with no alias.
func (t *Table) writeName(b *drops.Builder) {
	if t.database != "" {
		b.WriteIdent(t.database)
		b.WriteByte('.')
	}
	b.WriteIdent(t.name)
}

// writeFrom writes the FROM/JOIN form, including alias if set.
func (t *Table) writeFrom(b *drops.Builder) {
	t.writeName(b)
	if t.alias != "" {
		b.WriteString(" AS ")
		b.WriteIdent(t.alias)
	}
}

// writeRef writes the identifier used to qualify columns belonging to
// the table — the alias when set, otherwise the qualified name.
func (t *Table) writeRef(b *drops.Builder) {
	if t.alias != "" {
		b.WriteIdent(t.alias)
		return
	}
	// A table's automatic predicates are built from the declared column
	// handles and may be rendering inside a statement whose FROM entry
	// is an alias of this table. resolveFilterExprs installs the rename
	// for the length of each such predicate; here is where it lands.
	if renamed := b.RelationAlias(t.relRef()); renamed != "" {
		b.WriteIdent(renamed)
		return
	}
	t.writeName(b)
}

// WriteSQL writes the FROM/JOIN form so a *Table satisfies
// drops.Expression and can appear anywhere a SQL fragment is expected.
func (t *Table) WriteSQL(b *drops.Builder) { t.writeFrom(b) }
