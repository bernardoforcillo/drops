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
	// not emit them; pair the table with pg.CreateTableWithIndexes if
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
		panic(fmt.Sprintf("drops/pg: table %q has no relation %q; declared: %s",
			t.name, name, strings.Join(declared, ", ")))
	}
	return r
}

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

// PrimaryKey declares a composite PRIMARY KEY spanning cols. Call
// only when the PK has more than one column; single-column PKs
// continue to be declared on the column via *Col[T].PrimaryKey().
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
