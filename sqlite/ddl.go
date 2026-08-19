package sqlite

import (
	"sort"

	"github.com/bernardoforcillo/drops"
)

// CreateTable returns a CREATE TABLE statement for t.
//
// Unlike PostgreSQL, SQLite cannot add constraints after the fact
// (there is no ALTER TABLE ADD CONSTRAINT), so EVERY constraint —
// composite primary key, UNIQUE, CHECK and single- or multi-column
// foreign key — is emitted inline inside the CREATE TABLE body. This is
// the deliberate dialect difference from drops/pg, whose migration
// generator emits those as separate ALTER statements.
func CreateTable(t *Table) drops.Expression { return createTable(t, false) }

// CreateTableIfNotExists is the IF NOT EXISTS variant.
func CreateTableIfNotExists(t *Table) drops.Expression { return createTable(t, true) }

func createTable(t *Table, ifNotExists bool) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("CREATE TABLE ")
		if ifNotExists {
			b.WriteString("IF NOT EXISTS ")
		}
		t.writeName(b)
		b.WriteString(" (\n  ")
		first := true
		sep := func() {
			if !first {
				b.WriteString(",\n  ")
			}
			first = false
		}
		// A composite key can arrive two ways: declared on the table
		// with PrimaryKey(cols...), or by marking more than one column
		// .PrimaryKey(). Both have to reach the table-level clause,
		// because an inline PRIMARY KEY on each column renders two of
		// them and SQLite rejects the statement with "table has more
		// than one primary key".
		keyCols := t.compositePK
		if len(keyCols) == 0 {
			var marked []*Column
			for _, c := range t.columns {
				if c.primary {
					marked = append(marked, c)
				}
			}
			if len(marked) > 1 {
				keyCols = marked
			}
		}
		singlePK := len(keyCols) == 0

		for _, c := range t.columns {
			sep()
			writeColumnDef(b, c, singlePK)
		}
		if len(keyCols) > 0 {
			sep()
			b.WriteString("PRIMARY KEY (")
			writeColList(b, keyCols)
			b.WriteByte(')')
		}
		// Named composite UNIQUE constraints (sorted for determinism).
		for _, name := range sortedKeys(t.compositeUniques) {
			sep()
			b.WriteString("CONSTRAINT ")
			b.WriteIdent(name)
			b.WriteString(" UNIQUE (")
			writeColList(b, t.compositeUniques[name])
			b.WriteByte(')')
		}
		// CHECK constraints (sorted).
		for _, name := range sortedStrMap(t.checks) {
			sep()
			b.WriteString("CONSTRAINT ")
			b.WriteIdent(name)
			b.WriteString(" CHECK (")
			b.WriteString(t.checks[name])
			b.WriteByte(')')
		}
		// Composite foreign keys (declaration order).
		for _, fk := range t.compositeFKs {
			sep()
			b.WriteString("CONSTRAINT ")
			b.WriteIdent(fk.Name)
			b.WriteString(" FOREIGN KEY (")
			writeColList(b, fk.Columns)
			b.WriteString(") REFERENCES ")
			fk.Target.writeName(b)
			b.WriteString(" (")
			writeColList(b, fk.TargetColumns)
			b.WriteByte(')')
			writeRefActions(b, fk.OnDelete, fk.OnUpdate)
		}
		b.WriteString("\n)")
	})
}

// writeColumnDef renders one column definition. allowInlinePK lets a
// single-column PRIMARY KEY be attached to the column; when the table
// has a composite PK it is false (the PK is a table-level clause).
func writeColumnDef(b *drops.Builder, c *Column, allowInlinePK bool) {
	b.WriteIdent(c.name)
	b.WriteByte(' ')
	b.WriteString(c.typ.TypeSQL())
	if allowInlinePK && c.primary {
		b.WriteString(" PRIMARY KEY")
		if c.autoInc {
			b.WriteString(" AUTOINCREMENT")
		}
	} else if c.notNull {
		b.WriteString(" NOT NULL")
	}
	if c.unique && !c.primary {
		b.WriteString(" UNIQUE")
	}
	if c.hasDefault {
		b.WriteString(" DEFAULT ")
		b.WriteString(c.defaultSQL)
	}
	if fk := c.ref; fk != nil {
		b.WriteString(" REFERENCES ")
		fk.Target.Table().writeName(b)
		b.WriteString(" (")
		b.WriteIdent(fk.Target.Name())
		b.WriteByte(')')
		writeRefActions(b, fk.OnDelete, fk.OnUpdate)
	}
}

func writeRefActions(b *drops.Builder, onDelete, onUpdate string) {
	if onDelete != "" {
		b.WriteString(" ON DELETE ")
		b.WriteString(onDelete)
	}
	if onUpdate != "" {
		b.WriteString(" ON UPDATE ")
		b.WriteString(onUpdate)
	}
}

func writeColList(b *drops.Builder, cols []*Column) {
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteIdent(c.Name())
	}
}

// DropTable returns DROP TABLE t.
func DropTable(t *Table) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("DROP TABLE ")
		t.writeName(b)
	})
}

// DropTableIfExists is the IF EXISTS variant.
func DropTableIfExists(t *Table) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("DROP TABLE IF EXISTS ")
		t.writeName(b)
	})
}

// CreateIndex returns a CREATE INDEX statement over the given columns.
func CreateIndex(name string, t *Table, cols ...ColRef) drops.Expression {
	return createIndex(name, t, false, false, cols)
}

// CreateUniqueIndex returns a CREATE UNIQUE INDEX statement.
func CreateUniqueIndex(name string, t *Table, cols ...ColRef) drops.Expression {
	return createIndex(name, t, true, false, cols)
}

// CreateIndexIfNotExists is the IF NOT EXISTS variant of CreateIndex.
func CreateIndexIfNotExists(name string, t *Table, cols ...ColRef) drops.Expression {
	return createIndex(name, t, false, true, cols)
}

func createIndex(name string, t *Table, unique, ifNotExists bool, cols []ColRef) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("CREATE ")
		if unique {
			b.WriteString("UNIQUE ")
		}
		b.WriteString("INDEX ")
		if ifNotExists {
			b.WriteString("IF NOT EXISTS ")
		}
		b.WriteIdent(name)
		b.WriteString(" ON ")
		t.writeName(b)
		b.WriteString(" (")
		for i, c := range cols {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteIdent(c.col().Name())
		}
		b.WriteByte(')')
	})
}

// DropIndex returns DROP INDEX name.
func DropIndex(name string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("DROP INDEX ")
		b.WriteIdent(name)
	})
}

// DropIndexIfExists is the IF EXISTS variant.
func DropIndexIfExists(name string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("DROP INDEX IF EXISTS ")
		b.WriteIdent(name)
	})
}

func sortedKeys(m map[string][]*Column) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedStrMap(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
