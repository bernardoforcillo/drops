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
// The copy carries its own columns, bound to the aliased table, so a
// reference reached through it — e := events.As("e"); e.Col("id") —
// qualifies with the alias while the original package-level handles go
// on qualifying with the table name. That is what makes both sides of a
// self-join addressable at once, and it is why the copy is not a
// shallow one: sharing the column slice would leave every reference
// pointing at the un-aliased table, which ClickHouse cannot resolve
// once the alias has shadowed the name.
//
// The automatic predicates and the lifecycle hooks are SHARED with the
// table rather than copied, and they are restated under the alias when
// they render — see tableScope and Table.resolveFilterExprs. Both
// halves of that used to be wrong, and each was wrong in its own
// direction: the alias snapshotted a list that a later init would add
// to, and the predicates it did carry named the un-aliased relation,
// which ClickHouse answers with UNKNOWN_IDENTIFIER.
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
// The column list is a snapshot: a column added to the table after this
// call is absent from the alias. Alias at query time, which is where a
// self-join is written anyway, not at declaration time.
//
// Each copy remembers the column it came from, so the two handles stay
// interchangeable everywhere a column is identified rather than
// rendered — INSERT column lists and hook bookkeeping (see
// (*Column).key). Only the qualifier they render differs.
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
