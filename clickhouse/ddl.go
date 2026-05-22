package clickhouse

import (
	"errors"
	"strings"

	"github.com/bernardoforcillo/drops"
)

// ErrEngineRequired is returned by CreateTable when the target table
// doesn't yet have an Engine set. ClickHouse refuses CREATE TABLE
// without an ENGINE clause, so we fail fast and clearly.
var ErrEngineRequired = errors.New("drops/clickhouse: table has no Engine set; call .Engine(clickhouse.MergeTree()) before CreateTable")

// CreateTable returns a CREATE TABLE statement for t. It panics-via-
// expression: rendering builds a SQL fragment that ends up emitting a
// clearly-marked error string if the engine is missing, so a caller
// who forgets gets a loud failure at exec time rather than silent bad
// DDL. Use CreateTableErr if you want the engine check at build time.
func CreateTable(t *Table) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { writeCreate(b, t, false) })
}

// CreateTableIfNotExists is the IF NOT EXISTS variant.
func CreateTableIfNotExists(t *Table) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { writeCreate(b, t, true) })
}

// CreateTableErr returns the DDL or ErrEngineRequired. Use it in
// migration tooling that wants a definite error rather than SQL that
// references a sentinel string.
func CreateTableErr(t *Table) (drops.Expression, error) {
	if t.engine == nil {
		return nil, ErrEngineRequired
	}
	return CreateTable(t), nil
}

func writeCreate(b *drops.Builder, t *Table, ifNotExists bool) {
	if ifNotExists {
		b.WriteString("CREATE TABLE IF NOT EXISTS ")
	} else {
		b.WriteString("CREATE TABLE ")
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
	b.WriteString("\n) ENGINE = ")
	if t.engine == nil {
		// Make the user-visible error explicit in the emitted SQL so a
		// stray CreateTable in an init script fails with a readable
		// message instead of a syntactically-broken DDL.
		b.WriteString("/* drops/clickhouse: engine missing — call .Engine(...) on the table */")
	} else {
		t.engine.WriteEngine(b)
	}
	writeTableSuffix(b, t)
}

// writeTableSuffix renders the ORDER BY / PARTITION BY / PRIMARY KEY
// / SAMPLE BY / TTL / SETTINGS clauses, in the order ClickHouse
// expects.
func writeTableSuffix(b *drops.Builder, t *Table) {
	if len(t.orderBy) > 0 {
		b.WriteString("\nORDER BY (")
		for i, c := range t.orderBy {
			if i > 0 {
				b.WriteString(", ")
			}
			c.WriteSQL(b)
		}
		b.WriteByte(')')
	}
	if len(t.partition) > 0 {
		b.WriteString("\nPARTITION BY (")
		for i, e := range t.partition {
			if i > 0 {
				b.WriteString(", ")
			}
			e.WriteSQL(b)
		}
		b.WriteByte(')')
	}
	if len(t.primaryKey) > 0 {
		b.WriteString("\nPRIMARY KEY (")
		for i, c := range t.primaryKey {
			if i > 0 {
				b.WriteString(", ")
			}
			c.WriteSQL(b)
		}
		b.WriteByte(')')
	}
	if t.sampleBy != nil {
		b.WriteString("\nSAMPLE BY ")
		t.sampleBy.WriteSQL(b)
	}
	if t.ttl != "" {
		b.WriteString("\nTTL ")
		b.WriteString(t.ttl)
	}
	if len(t.settings) > 0 {
		b.WriteString("\nSETTINGS ")
		b.WriteString(strings.Join(t.settings, ", "))
	}
}

// writeColumnDef writes "name Type [DEFAULT …] [CODEC(…)] [TTL …]
// [COMMENT '…']".
func writeColumnDef(b *drops.Builder, c *Column) {
	b.WriteIdent(c.name)
	b.WriteByte(' ')
	b.WriteString(c.typ.TypeSQL())
	if c.hasDef {
		b.WriteString(" DEFAULT ")
		b.WriteString(c.defSQL)
	}
	if c.codec != "" {
		b.WriteString(" CODEC(")
		b.WriteString(c.codec)
		b.WriteByte(')')
	}
	if c.ttl != "" {
		b.WriteString(" TTL ")
		b.WriteString(c.ttl)
	}
	if c.comment != "" {
		b.WriteString(" COMMENT ")
		b.WriteString(quoteLiteral(c.comment))
	}
}

// DropTable / DropTableIfExists -------------------------------------

func DropTable(t *Table) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("DROP TABLE ")
		t.writeName(b)
	})
}

func DropTableIfExists(t *Table) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("DROP TABLE IF EXISTS ")
		t.writeName(b)
	})
}

// TruncateTable / RenameTable / OptimizeTable.

func TruncateTable(t *Table) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("TRUNCATE TABLE ")
		t.writeName(b)
	})
}

func TruncateTableIfExists(t *Table) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("TRUNCATE TABLE IF EXISTS ")
		t.writeName(b)
	})
}

// OptimizeTable triggers a merge round; final=true emits FINAL so all
// data is collapsed into a single part (expensive on large tables).
func OptimizeTable(t *Table, final bool) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("OPTIMIZE TABLE ")
		t.writeName(b)
		if final {
			b.WriteString(" FINAL")
		}
	})
}

// Database DDL ------------------------------------------------------

func CreateDatabase(name string) drops.Expression {
	mustIdent("database", name)
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("CREATE DATABASE ")
		b.WriteIdent(name)
	})
}

func CreateDatabaseIfNotExists(name string) drops.Expression {
	mustIdent("database", name)
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("CREATE DATABASE IF NOT EXISTS ")
		b.WriteIdent(name)
	})
}

func DropDatabase(name string) drops.Expression {
	mustIdent("database", name)
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("DROP DATABASE ")
		b.WriteIdent(name)
	})
}

func DropDatabaseIfExists(name string) drops.Expression {
	mustIdent("database", name)
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("DROP DATABASE IF EXISTS ")
		b.WriteIdent(name)
	})
}
