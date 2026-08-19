package mysql

import (
	"strconv"

	"github.com/bernardoforcillo/drops"
)

// CreateTable renders CREATE TABLE for t.
//
// Column constraints go inline; the primary key, unique keys and
// foreign keys go in table-level clauses. That split is not stylistic:
// MySQL cannot express a composite primary key inline, and a
// table-level FOREIGN KEY is the only form that can be named, which is
// what makes it droppable later.
func CreateTable(t *Table) drops.Expression { return createTable(t, false) }

// CreateTableIfNotExists is CreateTable with IF NOT EXISTS.
func CreateTableIfNotExists(t *Table) drops.Expression { return createTable(t, true) }

func createTable(t *Table, ifNotExists bool) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		// Everything inside the parentheses names columns of the
		// table being defined, so nothing may qualify the reference.
		defer b.SetBareIdents(b.SetBareIdents(true))

		b.WriteString("CREATE TABLE ")
		if ifNotExists {
			b.WriteString("IF NOT EXISTS ")
		}
		t.writeName(b)
		b.WriteString(" (\n")

		for i, c := range t.columns {
			if i > 0 {
				b.WriteString(",\n")
			}
			b.WriteByte('\t')
			writeColumnDef(b, c)
		}

		if pks := t.PrimaryKeyColumns(); len(pks) > 0 {
			b.WriteString(",\n\tPRIMARY KEY (")
			for i, c := range pks {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteIdent(c.name)
			}
			b.WriteByte(')')
		}
		for _, c := range t.columns {
			if !c.unique || c.primary {
				continue
			}
			b.WriteString(",\n\tUNIQUE KEY ")
			b.WriteIdent("uq_" + t.name + "_" + c.name)
			b.WriteString(" (")
			b.WriteIdent(c.name)
			b.WriteByte(')')
		}
		for _, c := range t.columns {
			if c.ref == nil {
				continue
			}
			writeForeignKey(b, t, c)
		}

		b.WriteString("\n)")
		if t.engine != "" {
			b.WriteString(" ENGINE=")
			b.WriteString(t.engine)
		}
		if t.charset != "" {
			b.WriteString(" DEFAULT CHARSET=")
			b.WriteString(t.charset)
		}
		if t.collation != "" {
			b.WriteString(" COLLATE=")
			b.WriteString(t.collation)
		}
		if t.comment != "" {
			b.WriteString(" COMMENT='")
			b.WriteString(quoteLiteral(t.comment))
			b.WriteString("'")
		}
	})
}

// writeColumnDef renders one column definition. The clause order is
// the one MySQL's grammar requires, not a preference.
func writeColumnDef(b *drops.Builder, c *Column) {
	b.WriteIdent(c.name)
	b.WriteByte(' ')
	b.WriteString(c.typ.TypeSQL())
	if c.notNull {
		b.WriteString(" NOT NULL")
	}
	if c.hasDefault {
		b.WriteString(" DEFAULT ")
		b.WriteString(c.defaultSQL)
	}
	if c.autoInc {
		b.WriteString(" AUTO_INCREMENT")
	}
	if c.onUpdate != "" {
		b.WriteString(" ON UPDATE ")
		b.WriteString(c.onUpdate)
	}
	if c.comment != "" {
		b.WriteString(" COMMENT '")
		b.WriteString(quoteLiteral(c.comment))
		b.WriteString("'")
	}
}

func writeForeignKey(b *drops.Builder, t *Table, c *Column) {
	fk := c.ref
	name := "fk_" + t.name + "_" + c.name
	b.WriteString(",\n\tCONSTRAINT ")
	b.WriteIdent(name)
	b.WriteString(" FOREIGN KEY (")
	b.WriteIdent(c.name)
	b.WriteString(") REFERENCES ")
	if fk.Target.table != nil {
		fk.Target.table.writeName(b)
	}
	b.WriteString(" (")
	b.WriteIdent(fk.Target.name)
	b.WriteByte(')')
	if fk.OnDelete != "" {
		b.WriteString(" ON DELETE ")
		b.WriteString(fk.OnDelete)
	}
	if fk.OnUpdate != "" {
		b.WriteString(" ON UPDATE ")
		b.WriteString(fk.OnUpdate)
	}
}

// DropTable renders DROP TABLE.
func DropTable(t *Table) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("DROP TABLE ")
		t.writeName(b)
	})
}

// DropTableIfExists renders DROP TABLE IF EXISTS.
func DropTableIfExists(t *Table) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("DROP TABLE IF EXISTS ")
		t.writeName(b)
	})
}

// Index describes a secondary index.
type Index struct {
	name    string
	table   *Table
	cols    []ColRef
	unique  bool
	using   string
	prefix  map[string]int
	comment string
}

// NewIndex begins an index declaration.
func NewIndex(name string, t *Table, cols ...ColRef) *Index {
	mustIdent("index", name)
	return &Index{name: name, table: t, cols: cols, prefix: map[string]int{}}
}

// Unique makes it a UNIQUE index.
func (i *Index) Unique() *Index { i.unique = true; return i }

// Using selects the index type — BTREE (the default) or HASH on the
// engines that offer it.
func (i *Index) Using(method string) *Index { i.using = method; return i }

// Prefix indexes only the first n bytes of a column.
//
// This is not an optimisation but a requirement: MySQL cannot index a
// TEXT or BLOB column at all without one, so an index over a [Text]
// column needs a prefix length here or the server rejects the DDL.
func (i *Index) Prefix(col ColRef, n int) *Index {
	i.prefix[col.col().name] = n
	return i
}

// Comment attaches a COMMENT to the index.
func (i *Index) Comment(text string) *Index { i.comment = text; return i }

// WriteSQL renders CREATE [UNIQUE] INDEX.
func (i *Index) WriteSQL(b *drops.Builder) {
	b.WriteString("CREATE ")
	if i.unique {
		b.WriteString("UNIQUE ")
	}
	b.WriteString("INDEX ")
	b.WriteIdent(i.name)
	b.WriteString(" ON ")
	i.table.writeName(b)
	b.WriteString(" (")
	for n, c := range i.cols {
		if n > 0 {
			b.WriteString(", ")
		}
		col := c.col()
		b.WriteIdent(col.name)
		if p, ok := i.prefix[col.name]; ok && p > 0 {
			b.WriteByte('(')
			b.WriteString(strconv.Itoa(p))
			b.WriteByte(')')
		}
	}
	b.WriteByte(')')
	if i.using != "" {
		b.WriteString(" USING ")
		b.WriteString(i.using)
	}
	if i.comment != "" {
		b.WriteString(" COMMENT '")
		b.WriteString(quoteLiteral(i.comment))
		b.WriteString("'")
	}
}

// DropIndex renders DROP INDEX … ON …. MySQL, unlike PostgreSQL,
// scopes index names to their table, so the table is required.
func DropIndex(name string, t *Table) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("DROP INDEX ")
		b.WriteIdent(name)
		b.WriteString(" ON ")
		t.writeName(b)
	})
}
