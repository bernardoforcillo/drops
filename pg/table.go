package pg

import "github.com/bernardoforcillo/drops"

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
	// not emit them; pair the table with pg.CreateTableWithIndexes if
	// you want both at once.
	indexes []*Index

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
	// (e.g. SoftDelete's "deleted_at IS NULL" guard).
	defaultFilters []drops.Expression
}

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

// Name returns the table's unqualified name.
func (t *Table) Name() string { return t.name }

// Schema returns the table's schema (empty for the default schema).
func (t *Table) Schema() string { return t.schema }

// Alias returns the alias set via As, or "" if none.
func (t *Table) Alias() string { return t.alias }

// As returns a shallow copy of the table bound to alias.
func (t *Table) As(alias string) *Table {
	cp := *t
	cp.alias = alias
	return &cp
}

// Col looks up a registered column by name.
func (t *Table) Col(name string) *Column { return t.byName[name] }

// Columns returns all registered columns in declaration order.
func (t *Table) Columns() []*Column { return t.columns }

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
func (t *Table) DefaultFilter(e drops.Expression) *Table {
	t.defaultFilters = append(t.defaultFilters, e)
	return t
}

// AddIndex registers an index to be created alongside the table. The
// index is not emitted by CreateTable; use CreateTableWithIndexes or
// emit pg.CreateIndex(idx) explicitly.
func (t *Table) AddIndex(idx *Index) *Table {
	t.indexes = append(t.indexes, idx)
	return t
}

// Indexes returns the indexes registered with AddIndex.
func (t *Table) Indexes() []*Index { return t.indexes }

// hasHooks reports whether the table has any lifecycle hooks
// registered — used by builders to skip the hook pipeline when
// nothing is wired up.
func (t *Table) hasInsertHooks() bool { return len(t.insertHooks) > 0 }
func (t *Table) hasUpdateHooks() bool { return len(t.updateHooks) > 0 }
func (t *Table) hasDeleteHooks() bool { return len(t.deleteHooks) > 0 }

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
