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
func (t *Table) As(alias string) *Table {
	mustIdent("alias", alias)
	cp := *t
	cp.alias = alias
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
		panic(fmt.Sprintf("drops/mysql: table %q has no relation %q; declared: %s",
			t.name, name, strings.Join(declared, ", ")))
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
