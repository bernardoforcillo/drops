package mysql

import (
	"fmt"
	"sort"
	"strings"

	"github.com/bernardoforcillo/drops"
)

// Table is a MySQL table: a name, its columns, and the relations
// declared against it.
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
	relations map[string]*Relation

	// defaultFilters are AND-ed onto every SELECT from this table
	// unless the builder opts out with Unscoped.
	defaultFilters []drops.Expression

	// indexes and checks are what the migration layer needs and the
	// query layer never looks at: the secondary indexes and CHECK
	// constraints declared against this table. Registered through
	// AddIndex / AddCheck, read by BuildSnapshot.
	indexes []*Index
	checks  map[string]string
}

// NewTable creates a table in the connection's default database.
func NewTable(name string) *Table {
	mustIdent("table", name)
	return &Table{name: name, byName: map[string]*Column{}, relations: map[string]*Relation{}}
}

// NewDatabaseTable scopes the table to an explicit database, which is
// what MySQL calls a schema.
func NewDatabaseTable(database, name string) *Table {
	mustIdent("database", database)
	mustIdent("table", name)
	return &Table{
		database:  database,
		name:      name,
		byName:    map[string]*Column{},
		relations: map[string]*Relation{},
	}
}

func (t *Table) Name() string     { return t.name }
func (t *Table) Database() string { return t.database }
func (t *Table) Alias() string    { return t.alias }

// As returns a copy of the table under an alias, for self-joins.
//
// The copy carries its own columns, bound to the aliased table, so a
// reference reached through it — u := users.As("u"); u.Col("id") —
// qualifies with the alias while the original package-level handles go
// on qualifying with the table name. That is what makes both sides of a
// self-join addressable at once. Default filters are not rewritten:
// they were built against the un-aliased columns and would resolve
// against a table the aliased query no longer names, so scope an
// aliased query with Unscoped and an explicit predicate.
//
// Relations are copied too, with their near side — the column that
// belongs to this table — rebound to the alias and the far side left
// alone. On a self-referential relation that is the whole point: the
// two ends of the edge are two instances of one table, and only one of
// them is the aliased one.
//
// The copy is a snapshot. A column or relation added to the base table
// after As returned does not reach the alias, which matters because Go
// initialises package-level variables before it runs init: an alias
// declared as a var beside its table is taken before any init that
// declares relations. Take the alias at the query site, or after the
// schema is complete.
func (t *Table) As(alias string) *Table {
	mustIdent("alias", alias)
	cp := *t
	cp.alias = alias
	cp.columns = make([]*Column, len(t.columns))
	cp.byName = make(map[string]*Column, len(t.byName))
	for i, c := range t.columns {
		aliased := *c
		aliased.table = &cp
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
	cp.relations = make(map[string]*Relation, len(t.relations))
	for name, rel := range t.relations {
		r := *rel
		r.From = &cp
		if r.Kind == BelongsToKind {
			// The inverse edge holds its own key in ChildKey; ParentKey
			// names the far table.
			r.ChildKey = rebind(r.ChildKey)
		} else {
			r.ParentKey = rebind(r.ParentKey)
		}
		cp.relations[name] = &r
	}
	return &cp
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
func (t *Table) DefaultFilter(e drops.Expression) *Table {
	t.defaultFilters = append(t.defaultFilters, e)
	return t
}

// Col looks a column up by name, returning nil when absent.
func (t *Table) Col(name string) *Column { return t.byName[name] }

// Columns returns the columns in declaration order.
func (t *Table) Columns() []*Column { return t.columns }

// Relation returns the named relation, or nil.
func (t *Table) Relation(name string) *Relation { return t.relations[name] }

// Rel returns the named relation, panicking if it was never declared.
// It is how a relation becomes a compile-checked Go identifier rather
// than a string literal at every query site.
func (t *Table) Rel(name string) *Relation {
	r := t.relations[name]
	if r == nil {
		declared := make([]string, 0, len(t.relations))
		for n := range t.relations {
			declared = append(declared, n)
		}
		sort.Strings(declared)
		// An alias carries the relations its table had at the moment As
		// was called, so a relation declared afterwards reaches the base
		// handle and not this one. Naming the alias is what separates
		// that from "the relation was never declared at all" — the two
		// look identical from the empty list.
		subject := fmt.Sprintf("table %q", t.name)
		if t.alias != "" {
			subject = fmt.Sprintf("alias %q of table %q", t.alias, t.name)
		}
		panic(fmt.Sprintf("drops/mysql: %s has no relation %q; declared: %s",
			subject, name, strings.Join(declared, ", ")))
	}
	return r
}

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
func (t *Table) writeRef(b *drops.Builder) {
	if t.alias != "" {
		b.WriteIdent(t.alias)
		return
	}
	t.writeName(b)
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
