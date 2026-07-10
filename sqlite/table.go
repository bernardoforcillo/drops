package sqlite

import "github.com/bernardoforcillo/drops"

// Table represents a SQLite table. SQLite has no schemas (only attached
// databases), so a table is just a name plus its columns and
// table-level constraints.
//
// Unlike PostgreSQL, SQLite cannot add most constraints with ALTER
// TABLE, so composite primary keys, UNIQUE constraints and foreign keys
// are rendered inside CREATE TABLE (see ddl.go).
type Table struct {
	name    string
	alias   string
	columns []*Column
	byName  map[string]*Column

	compositePK      []*Column
	compositeUniques map[string][]*Column
	checks           map[string]string
	compositeFKs     []*CompositeFK

	// defaultFilters are predicates applied automatically to every
	// Select / Update / Delete against this table unless the builder opts
	// out with Unscoped. SoftDelete registers "deletedAt IS NULL" here;
	// they are equally useful for tenant scoping.
	defaultFilters []drops.Expression

	relations map[string]*Relation
}

// DefaultFilter appends a predicate applied to every Select / Update /
// Delete against t unless the builder calls Unscoped. Mirrors
// drops/pg's Table.DefaultFilter.
func (t *Table) DefaultFilter(e drops.Expression) *Table {
	t.defaultFilters = append(t.defaultFilters, e)
	return t
}

// DefaultFilters returns the registered default-filter predicates.
func (t *Table) DefaultFilters() []drops.Expression { return t.defaultFilters }

// Relation returns the named relation declared on t, or nil.
func (t *Table) Relation(name string) *Relation { return t.relations[name] }

// setRelation registers r under name (used by NewRelations).
func (t *Table) setRelation(name string, r *Relation) {
	if t.relations == nil {
		t.relations = map[string]*Relation{}
	}
	t.relations[name] = r
}

// NewTable creates a table. The name is validated; a bad identifier
// panics at declaration time.
func NewTable(name string) *Table {
	mustIdent("table", name)
	return &Table{name: name, byName: map[string]*Column{}}
}

// Add registers c on t and returns the same typed handle, so callers
// keep the *Col[T] for use in queries:
//
//	id := sqlite.Add(users, sqlite.Integer("id").PrimaryKey())
func Add[T any](t *Table, c *Col[T]) *Col[T] {
	t.add(c.Column)
	return c
}

func (t *Table) add(c *Column) {
	if _, dup := t.byName[c.name]; dup {
		panic("drops/sqlite: duplicate column " + c.name + " on table " + t.name)
	}
	c.table = t
	t.columns = append(t.columns, c)
	t.byName[c.name] = c
}

func (t *Table) Name() string            { return t.name }
func (t *Table) Alias() string           { return t.alias }
func (t *Table) Columns() []*Column      { return t.columns }
func (t *Table) Col(name string) *Column { return t.byName[name] }

// As returns an aliased view of the table for use in joins.
func (t *Table) As(alias string) *Table {
	cp := *t
	cp.alias = alias
	return &cp
}

// PrimaryKey declares a composite (multi-column) primary key. For a
// single-column key use (*Col[T]).PrimaryKey() instead.
func (t *Table) PrimaryKey(cols ...ColRef) *Table {
	t.compositePK = make([]*Column, len(cols))
	for i, c := range cols {
		t.compositePK[i] = c.col()
	}
	return t
}

// CompositePrimaryKey returns the composite PK columns, or nil.
func (t *Table) CompositePrimaryKey() []*Column { return t.compositePK }

// AddUnique declares a named multi-column UNIQUE constraint.
func (t *Table) AddUnique(name string, cols ...ColRef) *Table {
	if t.compositeUniques == nil {
		t.compositeUniques = map[string][]*Column{}
	}
	cs := make([]*Column, len(cols))
	for i, c := range cols {
		cs[i] = c.col()
	}
	t.compositeUniques[name] = cs
	return t
}

// CompositeUniques returns the table's multi-column unique constraints.
func (t *Table) CompositeUniques() map[string][]*Column { return t.compositeUniques }

// AddCheck declares a named CHECK constraint whose value is the raw SQL
// expression (e.g. "age >= 0").
func (t *Table) AddCheck(name, expr string) *Table {
	if t.checks == nil {
		t.checks = map[string]string{}
	}
	t.checks[name] = expr
	return t
}

// Checks returns the table's CHECK constraints keyed by name.
func (t *Table) Checks() map[string]string { return t.checks }

// ForeignKey declares a single-column foreign key from col to target.
func (t *Table) ForeignKey(col, target *Column, opts ...func(*FK)) *Table {
	if col == nil || target == nil {
		panic("drops/sqlite: ForeignKey requires non-nil columns")
	}
	fk := &FK{Target: target}
	for _, o := range opts {
		o(fk)
	}
	col.ref = fk
	return t
}

// CompositeFK is a multi-column foreign key: FOREIGN KEY (Columns...)
// REFERENCES Target (TargetColumns...).
type CompositeFK struct {
	Name          string
	Columns       []*Column
	Target        *Table
	TargetColumns []*Column
	OnDelete      string
	OnUpdate      string
}

// ForeignKeyN declares a composite (N-column) foreign key from cols to
// targetCols on target, paired positionally. len(cols) must equal
// len(targetCols) and both be non-empty.
func (t *Table) ForeignKeyN(cols []ColRef, target *Table, targetCols []ColRef, opts ...func(*FK)) *Table {
	if target == nil {
		panic("drops/sqlite: ForeignKeyN requires a non-nil target table")
	}
	if len(cols) == 0 || len(targetCols) == 0 {
		panic("drops/sqlite: ForeignKeyN requires at least one column on each side")
	}
	if len(cols) != len(targetCols) {
		panic("drops/sqlite: ForeignKeyN local and target column counts must match")
	}
	from := make([]*Column, len(cols))
	names := make([]string, len(cols))
	for i, c := range cols {
		if c == nil {
			panic("drops/sqlite: ForeignKeyN got a nil local column")
		}
		from[i] = c.col()
		names[i] = c.col().Name()
	}
	to := make([]*Column, len(targetCols))
	targetNames := make([]string, len(targetCols))
	for i, c := range targetCols {
		if c == nil {
			panic("drops/sqlite: ForeignKeyN got a nil target column")
		}
		to[i] = c.col()
		targetNames[i] = c.col().Name()
	}
	var f FK
	for _, o := range opts {
		o(&f)
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

// CompositeForeignKeys returns the table's multi-column foreign keys.
func (t *Table) CompositeForeignKeys() []*CompositeFK { return t.compositeFKs }

// writeName writes the bare (quoted) table name — used in DDL and FROM.
func (t *Table) writeName(b *drops.Builder) { b.WriteIdent(t.name) }

// writeFrom writes the table with its alias, for FROM/JOIN.
func (t *Table) writeFrom(b *drops.Builder) {
	t.writeName(b)
	if t.alias != "" {
		b.WriteString(" AS ")
		b.WriteIdent(t.alias)
	}
}

// writeRef writes the identifier used to qualify columns (the alias if
// set, else the table name).
func (t *Table) writeRef(b *drops.Builder) {
	if t.alias != "" {
		b.WriteIdent(t.alias)
		return
	}
	b.WriteIdent(t.name)
}

// WriteSQL implements drops.Expression (renders the FROM form).
func (t *Table) WriteSQL(b *drops.Builder) { t.writeFrom(b) }

// fkName builds a camelCase FK constraint name:
// <tableFrom><ColFrom...><TableTo><ColTo...>Fk.
func fkName(tableFrom string, cols []string, tableTo string, targetCols []string) string {
	out := tableFrom
	for _, c := range cols {
		out += titleFirst(c)
	}
	out += titleFirst(tableTo)
	for _, c := range targetCols {
		out += titleFirst(c)
	}
	return out + "Fk"
}

func titleFirst(s string) string {
	if s == "" {
		return s
	}
	if s[0] >= 'a' && s[0] <= 'z' {
		return string(s[0]-'a'+'A') + s[1:]
	}
	return s
}
