package clickhouse

import (
	"errors"
	"strings"

	"github.com/bernardoforcillo/drops"
)

// ErrEngineRequired reports a table with no Engine set. ClickHouse
// refuses CREATE TABLE without an ENGINE clause, so the declaration is
// incomplete and no statement built from it can run.
//
// [CreateTableErr] returns it instead of DDL, and [Push] returns it
// before it reads the server. [CreateTable] cannot: it returns an
// expression, and renders the marker below in place of the engine.
var ErrEngineRequired = errors.New("drops/clickhouse: table has no Engine set; call .Engine(clickhouse.MergeTree()) before creating it")

// CreateTable returns a CREATE TABLE statement for t.
//
// It does not fail on a table with no engine, because it returns an
// expression and an expression has nowhere to put an error: it renders
// a clearly-marked comment where the ENGINE clause belongs, so a caller
// who forgot gets a readable rejection at exec time rather than a bare
// syntax error. Use [CreateTableErr] to have the check at build time,
// and note that [Push] makes it for you.
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
	// Every clause below names columns of the table being created, so
	// none of them may qualify the reference. The mode covers columns
	// nested inside expressions too — toYYYYMM(ts) in a PARTITION BY.
	defer b.SetBareIdents(b.SetBareIdents(true))

	if len(t.orderBy) > 0 {
		b.WriteString("\nORDER BY (")
		writeCols(b, t.orderBy)
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
		writeCols(b, t.primaryKey)
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

// writeColumnDef writes "name Type [DEFAULT …] [COMMENT '…']
// [CODEC(…)] [TTL …]".
//
// The clause order is the one ClickHouse's column-declaration parser
// walks, and it walks it strictly: COMMENT is read before CODEC and
// CODEC before TTL, so a comment emitted last turns the whole CREATE
// TABLE into a syntax error.
func writeColumnDef(b *drops.Builder, c *Column) {
	b.WriteIdent(c.name)
	b.WriteByte(' ')
	b.WriteString(c.typ.TypeSQL())
	if c.hasDef {
		b.WriteString(" DEFAULT ")
		b.WriteString(c.defSQL)
	}
	if c.comment != "" {
		b.WriteString(" COMMENT ")
		b.WriteString(quoteLiteral(c.comment))
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

// writeCols renders a comma-separated column list. It relies on the
// caller having put the Builder in bare-identifier mode.
func writeCols(b *drops.Builder, cols []ColRef) {
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		c.WriteSQL(b)
	}
}
