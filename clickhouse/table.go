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

	// Lifecycle hooks and default filters — ClickHouse has no
	// builder-side UPDATE/DELETE, so only InsertHook and
	// SelectBuilder default filters are honoured. Empty by default.
	insertHooks    []InsertHook
	defaultFilters []drops.Expression
}

// NewTable creates a table in the default database. The name is
// validated and a bad identifier panics at startup (see
// ErrInvalidIdentifier).
func NewTable(name string) *Table {
	mustIdent("table", name)
	return &Table{name: name, byName: map[string]*Column{}}
}

// NewDatabaseTable scopes the table to an explicit database.
func NewDatabaseTable(database, name string) *Table {
	mustIdent("database", database)
	mustIdent("table", name)
	return &Table{database: database, name: name, byName: map[string]*Column{}}
}

// Name / Database / Alias accessors.
func (t *Table) Name() string     { return t.name }
func (t *Table) Database() string { return t.database }
func (t *Table) Alias() string    { return t.alias }

// As returns a shallow copy bound to alias.
func (t *Table) As(alias string) *Table {
	cp := *t
	cp.alias = alias
	return &cp
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

// OnInsert registers a hook invoked by InsertBuilder.WriteSQL.
func (t *Table) OnInsert(h InsertHook) *Table {
	t.insertHooks = append(t.insertHooks, h)
	return t
}

// DefaultFilter appends a predicate applied to every Select against
// the table, unless the builder is marked Unscoped().
func (t *Table) DefaultFilter(e drops.Expression) *Table {
	t.defaultFilters = append(t.defaultFilters, e)
	return t
}

func (t *Table) hasInsertHooks() bool { return len(t.insertHooks) > 0 }

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
	t.writeName(b)
}

// WriteSQL writes the FROM/JOIN form so a *Table satisfies
// drops.Expression and can appear anywhere a SQL fragment is expected.
func (t *Table) WriteSQL(b *drops.Builder) { t.writeFrom(b) }
