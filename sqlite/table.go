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

	relations map[string]*Relation

	// scope is the automatic-predicate state: the two filter lists, the
	// write-side tenant column and the lifecycle hooks. It is a POINTER
	// so that a table and every alias taken off it share one — see
	// tableScope for what a copy cost.
	scope *tableScope

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
//	var Users = sqlite.NewTable("people").RenamedFrom("users")
func (t *Table) RenamedFrom(previous string) *Table {
	t.renamedFrom = previous
	return t
}

// PreviousName returns the name the table was declared to have been
// renamed from, or empty when it was not.
func (t *Table) PreviousName() string { return t.renamedFrom }

// OnInsert registers an INSERT hook, run before every INSERT renders.
func (t *Table) OnInsert(h InsertHook) *Table {
	t.scope.mu.Lock()
	defer t.scope.mu.Unlock()
	t.scope.insertHooks = appendShared(t.scope.insertHooks, h)
	return t
}

// OnUpdate registers an UPDATE hook, run before every UPDATE renders.
func (t *Table) OnUpdate(h UpdateHook) *Table {
	t.scope.mu.Lock()
	defer t.scope.mu.Unlock()
	t.scope.updateHooks = appendShared(t.scope.updateHooks, h)
	return t
}

// OnDelete registers a DELETE hook, run before every DELETE renders. A
// hook may replace the DELETE with another statement (soft delete).
func (t *Table) OnDelete(h DeleteHook) *Table {
	t.scope.mu.Lock()
	defer t.scope.mu.Unlock()
	t.scope.deleteHooks = appendShared(t.scope.deleteHooks, h)
	return t
}

// DefaultFilter appends an anonymous predicate applied automatically to
// every Select / Update / Delete against the table.
//
// Anonymous means only Unscoped() can bypass it — and Unscoped bypasses
// every other filter on the table at the same time. Prefer AddFilter,
// which names the predicate so one query can step around it while the
// table's remaining scoping stays in force.
func (t *Table) DefaultFilter(e drops.Expression) *Table {
	t.addDefaultFilter(tableFilter{pred: e})
	return t
}

// ScopeWritesByTenant names the column an INSERT into this table
// stamps from the ctx tenant.
//
// It is the write-side half of a tenant axis, and it is separate from
// the read-side half because a ContextFilterFunc is a closure: it
// answers with a predicate, and nothing can ask it which column owns a
// row — so a table scoped with ContextFilter(TenantFilter(col)) and
// nothing else keeps its guarantee on every read, every UPDATE and
// every DELETE while its INSERTs stay the caller's to bind. Naming the
// column here hands them to drops:
//
//	Posts.ContextFilter(sqlite.TenantFilter(PostTenantID)).
//	    ScopeWritesByTenant(PostTenantID)
//
// [Entity.ScopeByTenant] calls this for you, so an entity-declared axis
// covers both halves at once. Declaring it twice with the same column
// is idempotent.
//
// The consequence worth stating plainly: from here on every INSERT into
// this table needs a tenant on its ctx, including one built straight
// from db.Insert() — see [InsertBuilder.ToSQLCtx]. A statement that
// legitimately writes outside the ctx tenant says so with
// [InsertBuilder.Unscoped].
//
// The handle has to be one of this table's own columns, and a handle
// this table does not own is a panic at declaration time rather than a
// statement at request time: what it is given ends up in the INSERT
// column list and in every axis check the package makes.
//
// Ownership is asked by [Column.key], so a handle taken off a table
// alias names the same axis as the declared one; what is STORED is this
// table's own handle rather than the one passed in, because an alias
// handle qualifies with an alias an INSERT into this table has no name
// for.
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
	panic("drops/sqlite: ScopeWritesByTenant column " + columnPath(c) +
		" is not a column of " + t.Name())
}

// AddFilter appends a predicate under name, applied exactly as
// DefaultFilter's is except that a query can bypass this one alone:
//
//	posts.AddFilter(sqlite.FilterSoftDelete, deletedAt.IsNull())
//	db.Select().From(posts).IgnoreFilters(sqlite.FilterSoftDelete)
//
// An empty name panics: it would read as named at the call site and
// behave as anonymous at the query.
func (t *Table) AddFilter(name string, e drops.Expression) *Table {
	if name == "" {
		panic("drops/sqlite: AddFilter needs a non-empty name — use DefaultFilter for an anonymous filter")
	}
	t.addDefaultFilter(tableFilter{name: name, pred: e})
	return t
}

// DefaultFilters returns the table's global-filter predicates in
// registration order, named and anonymous alike.
func (t *Table) DefaultFilters() []drops.Expression {
	return filterPreds(t.defaultFilterList())
}

// FilterNames returns the names of the table's named filters in
// registration order. Anonymous filters contribute nothing.
func (t *Table) FilterNames() []string {
	var out []string
	for _, f := range t.defaultFilterList() {
		if f.name != "" {
			out = append(out, f.name)
		}
	}
	return out
}

func (t *Table) hasInsertHooks() bool { return len(t.insertHookList()) > 0 }
func (t *Table) hasUpdateHooks() bool { return len(t.updateHookList()) > 0 }

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
	return &Table{name: name, byName: map[string]*Column{}, scope: &tableScope{}}
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
//
// The copy rebinds the table's columns onto itself, so a handle taken
// off the alias renders "u"."id" rather than "users"."id" — and
// [Column.key] collapses it back onto the declared column everywhere
// the question is "which column is this", so an alias is a rename and
// never a stranger.
//
// What the copy does NOT copy is the scope: cp.scope is the same
// pointer, so the alias carries the filters, the tenant axis and the
// hooks the table has NOW rather than the ones it had when As was
// called. See tableScope for the cross-tenant DELETE a snapshot cost.
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
	// Every map on the copy has to be its own, or a relation, check or
	// unique declared against the alias writes through into the table
	// it was aliased from.
	if t.relations != nil {
		cp.relations = make(map[string]*Relation, len(t.relations))
		for name, rel := range t.relations {
			r := *rel
			// Only the near side — the end of the edge that belongs to
			// this table — moves to the alias. On a self-referential
			// relation both ends name this table and rebinding both
			// would erase the distinction the alias exists to draw.
			r.Local = rebind(r.Local)
			cp.relations[name] = &r
		}
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
	cp.compositeFKs = make([]*CompositeFK, len(t.compositeFKs))
	for i, fk := range t.compositeFKs {
		f := *fk
		f.Columns = rebindAll(fk.Columns)
		cp.compositeFKs[i] = &f
	}
	return &cp
}

// PrimaryKey declares a composite (multi-column) primary key. For a
// single-column key use (*Col[T]).PrimaryKey() instead.
//
// Like the column-level spelling it states NOT NULL on each member,
// last-writer-wins over an earlier Nullable. That is not decoration:
// SQLite's own PRIMARY KEY does not enforce it — a legacy bug it
// keeps for compatibility — so a key column that does not say NOT NULL
// really will accept a NULL, and the two spellings of the same key
// would otherwise describe two different tables.
func (t *Table) PrimaryKey(cols ...ColRef) *Table {
	t.compositePK = make([]*Column, len(cols))
	for i, c := range cols {
		col := c.col()
		col.notNull, col.nullStated = true, true
		t.compositePK[i] = col
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
	// A table's automatic predicates are built from the declared
	// column handles and may be rendering inside a statement whose
	// FROM entry is an alias of this table. resolveFilterExprs
	// installs the rename for the length of each such predicate; here
	// is where it lands.
	if renamed := b.RelationAlias(t.relRef()); renamed != "" {
		b.WriteIdent(renamed)
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
