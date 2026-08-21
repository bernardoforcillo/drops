package mysql

import (
	"fmt"

	"github.com/bernardoforcillo/drops"
)

// Table is a MySQL table: a name and its columns.
type Table struct {
	database  string
	name      string
	alias     string
	engine    string
	charset   string
	collation string
	comment   string
	columns   []*Column
	byName    map[string]*Column

	// indexes and checks are what the migration layer needs and the
	// query layer never looks at: the secondary indexes and CHECK
	// constraints declared against this table. Registered through
	// AddIndex / AddCheck, read by BuildSnapshot.
	indexes []*Index
	checks  map[string]string

	// origin is the table this one was copied from by As, and nil on
	// a table as declared — see key.
	origin *Table

	// scope carries every automatic predicate the table declares —
	// both filter lists and the write-side tenant column. It is a
	// pointer, and an alias taken off this table SHARES it rather than
	// copying it — see tableScope, and see Table.As for the write that
	// went out unscoped while it was a snapshot.
	scope *tableScope
}

// NewTable creates a table in the connection's default database.
func NewTable(name string) *Table {
	mustIdent("table", name)
	return &Table{name: name, byName: map[string]*Column{}, scope: &tableScope{}}
}

// NewDatabaseTable scopes the table to an explicit database, which is
// what MySQL calls a schema.
func NewDatabaseTable(database, name string) *Table {
	mustIdent("database", database)
	mustIdent("table", name)
	return &Table{
		database: database,
		name:     name,
		byName:   map[string]*Column{},
		scope:    &tableScope{},
	}
}

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
// reference reached through it — u := users.As("u"); u.Col("id") —
// qualifies with the alias while the original package-level handles go
// on qualifying with the table name. That is what makes both sides of a
// self-join addressable at once.
//
// An aliased handle still *means* the column it was copied from. The
// INSERT column list a short row is aligned against, an Entity's key
// columns and a page's ordering column all identify a column through
// Column.key, which collapses the copy back onto the declared column.
// Where such a handle is also rendered — a page's ORDER BY and cursor
// guard, an UPDATE's assignments, an upsert's — it is restated as the
// handle that qualifies with the relation the statement names: the
// alias for an UPDATE or SELECT that carries one, and the table for an
// INSERT, whose INTO clause has no AS to carry. Aliasing changes how a
// reference renders and nothing else.
//
// The automatic predicates a table carries are SHARED with the alias
// rather than copied, and shared rather than snapshotted because the
// alias is the same table: a filter or a lifecycle hook registered on
// either handle at any time applies to both, in whichever order the two
// happen. That ordering is not hypothetical. Go initialises
// package-level variables before it runs init, so an alias declared
// beside its table is taken before any init or constructor that
// declares the scoping — and while the lists were copied, that alias
// was unscoped for ever. It rendered DELETE `u` FROM `users` AS `u` with no predicate at all,
// on a ctx carrying no tenant, without refusing; and it lost a
// soft-delete guard registered after it was taken, so it read rows the
// application had deleted.
//
// BOTH filter lists are shared, on the same terms, because the argument
// does not distinguish them: a [Table.DefaultFilter] registered after
// As was taken went missing exactly as a [Table.ContextFilter] did, and
// the difference between the two failures is only how bad it is.
//
// The predicates cannot be rewritten, being closures over the handles
// they were given, so they are rendered inside a relation rename
// instead: see resolveFilterExprs. Without it an aliased query against
// a scoped table could not run at all — `users`.`deletedAt` against FROM `users` AS `u` is MySQL 1054, not a widened result — which made the one
// table shape that must never lose its tenant axis the one shape that
// could not be queried under an alias.
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
// only re-emits: a predicate, and the expression inside a [SetExpr].
// Both are closed over the handles they were given, so build them from
// the handles of the relation the statement names.
//
// The SHAPE of the table is still a snapshot, and only the shape: a
// column, index or check added to the base table after As returned does
// not reach the alias, for the same package-level-var reason described
// above. Take the alias at the query site, or after the schema is
// complete. Scoping is exempt from that caveat because scoping is the
// half where being a snapshot destroys data.
func (t *Table) As(alias string) *Table {
	mustIdent("alias", alias)
	cp := *t
	cp.alias = alias
	// The origin chains to the table this one was declared as rather
	// than to t, so aliasing an alias does not make a stranger of the
	// root.
	cp.origin = t.key()
	cp.columns = make([]*Column, len(t.columns))
	cp.byName = make(map[string]*Column, len(t.byName))
	for i, c := range t.columns {
		aliased := *c
		aliased.table = &cp
		aliased.origin = c.key()
		cp.columns[i] = &aliased
		cp.byName[aliased.name] = &aliased
	}
	if t.checks != nil {
		cp.checks = make(map[string]string, len(t.checks))
		for name, expr := range t.checks {
			cp.checks[name] = expr
		}
	}
	// The index list is shared by value but not by array: a copy taken
	// at full capacity would let an append through the alias land in
	// the base table's spare capacity, and the next append through
	// another handle overwrite it. It is the last slice here that is
	// copied at all — both filter lists live in the shared tableScope,
	// which cp already points at.
	cp.indexes = append([]*Index(nil), t.indexes...)
	return &cp
}

// key returns the identity a table is recognised by, collapsing every
// alias copy onto the table it was declared as. It is Column.key for
// the *Table handles, and for the same reason: an alias is a second
// handle on one table.
func (t *Table) key() *Table {
	if t.origin != nil {
		return t.origin
	}
	return t
}

// subject names a table handle for a panic message. Both an alias and
// its table answer the base name from Name, so a message that prints
// only that reads as "cannot be added to itself" when the two handles
// are what differ.
func (t *Table) subject() string {
	if t.alias != "" {
		return fmt.Sprintf("alias %q of table %q", t.alias, t.name)
	}
	return fmt.Sprintf("table %q", t.name)
}

// Engine sets the storage engine (InnoDB unless you say otherwise —
// and there is rarely a reason to).
func (t *Table) Engine(name string) *Table { t.engine = name; return t }

// Charset / Collate set the table's character set and collation.
func (t *Table) Charset(name string) *Table { t.charset = name; return t }
func (t *Table) Collate(name string) *Table { t.collation = name; return t }

// Comment attaches a COMMENT to the table.
func (t *Table) Comment(text string) *Table { t.comment = text; return t }

// DefaultFilter registers a predicate AND-ed onto every SELECT from
// this table — a soft-delete or tenant guard. Bypass it with
// (*SelectBuilder).Unscoped.
//
// It is registered on the shared scope, so it reaches every alias of
// the table however early the alias was taken, and registering it
// through an alias registers it on the table. See [Table.As].
func (t *Table) DefaultFilter(e drops.Expression) *Table {
	t.scope.mu.Lock()
	defer t.scope.mu.Unlock()
	t.scope.defaultFilters = appendShared(t.scope.defaultFilters, e)
	return t
}

// Col looks a column up by name, returning nil when absent.
func (t *Table) Col(name string) *Column { return t.byName[name] }

// Columns returns the columns in declaration order.
func (t *Table) Columns() []*Column { return t.columns }

// Add registers a column with the table and returns it, so a
// declaration reads as one expression:
//
//	var (
//	    Users    = mysql.NewTable("users")
//	    UserID   = mysql.Add(Users, mysql.BigSerial("id").PrimaryKey())
//	    UserName = mysql.Add(Users, mysql.Varchar("name", 255).NotNull())
//	)
func Add[T any](t *Table, c *Col[T]) *Col[T] {
	if _, dup := t.byName[c.Column.name]; dup {
		panic(fmt.Sprintf("drops/mysql: table %q already has a column named %q", t.name, c.Column.name))
	}
	c.Column.table = t
	t.columns = append(t.columns, c.Column)
	t.byName[c.Column.name] = c.Column
	return c
}

// PrimaryKeyColumns returns the columns marked PRIMARY KEY, in
// declaration order.
func (t *Table) PrimaryKeyColumns() []*Column {
	var out []*Column
	for _, c := range t.columns {
		if c.primary {
			out = append(out, c)
		}
	}
	return out
}

// writeRef writes the reference used in FROM / column qualification:
// the alias when there is one, otherwise the (database-qualified)
// name.
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
	t.writeName(b)
}

// relRef names the relation a column belonging to this table qualifies
// with when the table is not aliased, in the spelling
// [drops.Builder.RelationAlias] keys renames by.
//
// A database-qualified table is keyed by "database.table", because
// that is what it renders as and because two databases may each have a
// "users" — MySQL's database being what PostgreSQL calls a schema.
func (t *Table) relRef() string {
	if t.database == "" {
		return t.name
	}
	return t.database + "." + t.name
}

// writeName writes the database-qualified table name, no alias.
func (t *Table) writeName(b *drops.Builder) {
	if t.database != "" {
		b.WriteIdent(t.database)
		b.WriteByte('.')
	}
	b.WriteIdent(t.name)
}

// writeFrom writes the FROM entry, including an AS clause when the
// table is aliased.
func (t *Table) writeFrom(b *drops.Builder) {
	t.writeName(b)
	if t.alias != "" {
		b.WriteString(" AS ")
		b.WriteIdent(t.alias)
	}
}

// WriteSQL implements drops.Expression.
func (t *Table) WriteSQL(b *drops.Builder) { t.writeFrom(b) }
