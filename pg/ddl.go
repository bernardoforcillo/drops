package pg

import (
	"strconv"

	"github.com/bernardoforcillo/drops"
)

// CreateTable returns a CREATE TABLE statement for t.
//
// The generated SQL covers column types, NOT NULL, DEFAULT, UNIQUE,
// PRIMARY KEY (single-column only), and inline FOREIGN KEY references.
// More elaborate DDL — composite keys, indexes, partitioning — is out of
// scope; emit it via raw SQL.
func CreateTable(t *Table) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("CREATE TABLE ")
		t.writeName(b)
		b.WriteString(" (\n  ")
		for i, c := range t.Columns() {
			if i > 0 {
				b.WriteString(",\n  ")
			}
			writeColumnDef(b, c)
		}
		b.WriteString("\n)")
	})
}

// CreateTableIfNotExists is the IF NOT EXISTS variant.
func CreateTableIfNotExists(t *Table) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("CREATE TABLE IF NOT EXISTS ")
		t.writeName(b)
		b.WriteString(" (\n  ")
		for i, c := range t.Columns() {
			if i > 0 {
				b.WriteString(",\n  ")
			}
			writeColumnDef(b, c)
		}
		b.WriteString("\n)")
	})
}

// DropTable returns a DROP TABLE statement.
func DropTable(t *Table) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("DROP TABLE ")
		t.writeName(b)
	})
}

// DropTableIfExists is the IF EXISTS variant of DropTable.
func DropTableIfExists(t *Table) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("DROP TABLE IF EXISTS ")
		t.writeName(b)
	})
}

// --- Schemas ----------------------------------------------------------

// CreateSchema returns CREATE SCHEMA "name".
func CreateSchema(name string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("CREATE SCHEMA ")
		b.WriteIdent(name)
	})
}

// CreateSchemaIfNotExists is the IF NOT EXISTS variant.
func CreateSchemaIfNotExists(name string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("CREATE SCHEMA IF NOT EXISTS ")
		b.WriteIdent(name)
	})
}

// DropSchema returns DROP SCHEMA "name".
func DropSchema(name string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("DROP SCHEMA ")
		b.WriteIdent(name)
	})
}

// DropSchemaIfExists is the IF EXISTS variant. Cascade=true appends
// CASCADE so dependent objects are dropped too.
func DropSchemaIfExists(name string, cascade bool) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("DROP SCHEMA IF EXISTS ")
		b.WriteIdent(name)
		if cascade {
			b.WriteString(" CASCADE")
		}
	})
}

// --- Extensions -------------------------------------------------------

// CreateExtension returns CREATE EXTENSION "name". Common values:
// "pgcrypto", "uuid-ossp", "postgis", "pg_trgm".
func CreateExtension(name string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("CREATE EXTENSION ")
		b.WriteIdent(name)
	})
}

// CreateExtensionIfNotExists is the IF NOT EXISTS variant.
func CreateExtensionIfNotExists(name string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("CREATE EXTENSION IF NOT EXISTS ")
		b.WriteIdent(name)
	})
}

// DropExtension returns DROP EXTENSION "name".
func DropExtension(name string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("DROP EXTENSION ")
		b.WriteIdent(name)
	})
}

// DropExtensionIfExists is the IF EXISTS variant.
func DropExtensionIfExists(name string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("DROP EXTENSION IF EXISTS ")
		b.WriteIdent(name)
	})
}

// --- Sequences --------------------------------------------------------

// SequenceOptions configures CreateSequence.
type SequenceOptions struct {
	Start     *int64
	Increment *int64
	MinValue  *int64
	MaxValue  *int64
	Cache     *int64
	Cycle     bool
	OwnedBy   *Column // optional: link sequence ownership to a column
}

// CreateSequence returns a CREATE SEQUENCE statement.
func CreateSequence(name string, opts ...SequenceOptions) drops.Expression {
	var o SequenceOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("CREATE SEQUENCE ")
		b.WriteIdent(name)
		writeSeqOptions(b, &o)
	})
}

// CreateSequenceIfNotExists is the IF NOT EXISTS variant.
func CreateSequenceIfNotExists(name string, opts ...SequenceOptions) drops.Expression {
	var o SequenceOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("CREATE SEQUENCE IF NOT EXISTS ")
		b.WriteIdent(name)
		writeSeqOptions(b, &o)
	})
}

func writeSeqOptions(b *drops.Builder, o *SequenceOptions) {
	if o.Increment != nil {
		b.WriteString(" INCREMENT BY ")
		writeInt(b, *o.Increment)
	}
	if o.MinValue != nil {
		b.WriteString(" MINVALUE ")
		writeInt(b, *o.MinValue)
	}
	if o.MaxValue != nil {
		b.WriteString(" MAXVALUE ")
		writeInt(b, *o.MaxValue)
	}
	if o.Start != nil {
		b.WriteString(" START WITH ")
		writeInt(b, *o.Start)
	}
	if o.Cache != nil {
		b.WriteString(" CACHE ")
		writeInt(b, *o.Cache)
	}
	if o.Cycle {
		b.WriteString(" CYCLE")
	}
	if o.OwnedBy != nil {
		b.WriteString(" OWNED BY ")
		o.OwnedBy.Table().writeName(b)
		b.WriteByte('.')
		b.WriteIdent(o.OwnedBy.Name())
	}
}

func writeInt(b *drops.Builder, v int64) {
	b.WriteString(strconv.FormatInt(v, 10))
}

// DropSequence returns DROP SEQUENCE "name".
func DropSequence(name string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("DROP SEQUENCE ")
		b.WriteIdent(name)
	})
}

// DropSequenceIfExists is the IF EXISTS variant.
func DropSequenceIfExists(name string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("DROP SEQUENCE IF EXISTS ")
		b.WriteIdent(name)
	})
}

// NextVal returns the SQL expression nextval('"name"'::regclass).
func NextVal(name string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString(`nextval('`)
		b.WriteString(name)
		b.WriteString(`'::regclass)`)
	})
}

// CurrVal returns currval('"name"').
func CurrVal(name string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString(`currval('`)
		b.WriteString(name)
		b.WriteString(`')`)
	})
}

// SetVal returns setval('"name"', value).
func SetVal(name string, value any) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString(`setval('`)
		b.WriteString(name)
		b.WriteString(`', `)
		writeOperand(b, value)
		b.WriteByte(')')
	})
}

// --- Views ------------------------------------------------------------

// CreateView returns CREATE VIEW "name" AS <select>.
func CreateView(name string, query drops.Expression) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("CREATE VIEW ")
		b.WriteIdent(name)
		b.WriteString(" AS ")
		b.Append(query)
	})
}

// CreateOrReplaceView returns CREATE OR REPLACE VIEW ...
func CreateOrReplaceView(name string, query drops.Expression) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("CREATE OR REPLACE VIEW ")
		b.WriteIdent(name)
		b.WriteString(" AS ")
		b.Append(query)
	})
}

// DropView returns DROP VIEW "name".
func DropView(name string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("DROP VIEW ")
		b.WriteIdent(name)
	})
}

// DropViewIfExists is the IF EXISTS variant.
func DropViewIfExists(name string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("DROP VIEW IF EXISTS ")
		b.WriteIdent(name)
	})
}

// CreateMaterializedView returns CREATE MATERIALIZED VIEW. withData=false
// emits WITH NO DATA so the view is empty until first refreshed.
func CreateMaterializedView(name string, query drops.Expression, withData bool) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("CREATE MATERIALIZED VIEW ")
		b.WriteIdent(name)
		b.WriteString(" AS ")
		b.Append(query)
		if withData {
			b.WriteString(" WITH DATA")
		} else {
			b.WriteString(" WITH NO DATA")
		}
	})
}

// DropMaterializedView returns DROP MATERIALIZED VIEW "name".
func DropMaterializedView(name string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("DROP MATERIALIZED VIEW ")
		b.WriteIdent(name)
	})
}

// RefreshMaterializedView returns REFRESH MATERIALIZED VIEW. concurrently
// emits the CONCURRENTLY keyword (PG 9.4+) which requires a UNIQUE index.
func RefreshMaterializedView(name string, concurrently bool) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("REFRESH MATERIALIZED VIEW ")
		if concurrently {
			b.WriteString("CONCURRENTLY ")
		}
		b.WriteIdent(name)
	})
}

// --- Functions / procedures ------------------------------------------

// FunctionOptions configures CreateFunction.
type FunctionOptions struct {
	Args       string // raw, e.g. "a integer, b integer"
	Returns    string // raw, e.g. "integer" or "trigger"
	Language   string // default "plpgsql"
	Body       string // function body (without surrounding $$ delimiters)
	Volatility string // "IMMUTABLE", "STABLE", "VOLATILE" (default omitted)
	OrReplace  bool
}

// CreateFunction returns a CREATE FUNCTION statement. The body is wrapped
// in $func$ delimiters so it may contain arbitrary single quotes.
func CreateFunction(name string, opts FunctionOptions) drops.Expression {
	if opts.Language == "" {
		opts.Language = "plpgsql"
	}
	return drops.ExprFunc(func(b *drops.Builder) {
		if opts.OrReplace {
			b.WriteString("CREATE OR REPLACE FUNCTION ")
		} else {
			b.WriteString("CREATE FUNCTION ")
		}
		b.WriteIdent(name)
		b.WriteByte('(')
		b.WriteString(opts.Args)
		b.WriteString(") RETURNS ")
		b.WriteString(opts.Returns)
		b.WriteString(" LANGUAGE ")
		b.WriteString(opts.Language)
		if opts.Volatility != "" {
			b.WriteByte(' ')
			b.WriteString(opts.Volatility)
		}
		b.WriteString(" AS $func$ ")
		b.WriteString(opts.Body)
		b.WriteString(" $func$")
	})
}

// DropFunction returns DROP FUNCTION "name"(args). args is the raw
// argument-types signature, e.g. "integer, integer".
func DropFunction(name, args string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("DROP FUNCTION ")
		b.WriteIdent(name)
		b.WriteByte('(')
		b.WriteString(args)
		b.WriteByte(')')
	})
}

// DropFunctionIfExists is the IF EXISTS variant.
func DropFunctionIfExists(name, args string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("DROP FUNCTION IF EXISTS ")
		b.WriteIdent(name)
		b.WriteByte('(')
		b.WriteString(args)
		b.WriteByte(')')
	})
}

// --- Triggers ---------------------------------------------------------

// TriggerOptions configures CreateTrigger.
type TriggerOptions struct {
	Timing    string // "BEFORE" | "AFTER" | "INSTEAD OF"
	Events    string // "INSERT" | "UPDATE" | "DELETE" | combinations like "INSERT OR UPDATE"
	Table     *Table
	ForEach   string // "ROW" or "STATEMENT" (default "ROW")
	When      string // raw SQL condition (optional)
	Execute   string // raw, e.g. "my_func()"
	OrReplace bool
}

// CreateTrigger returns a CREATE TRIGGER statement.
func CreateTrigger(name string, opts TriggerOptions) drops.Expression {
	if opts.ForEach == "" {
		opts.ForEach = "ROW"
	}
	return drops.ExprFunc(func(b *drops.Builder) {
		if opts.OrReplace {
			b.WriteString("CREATE OR REPLACE TRIGGER ")
		} else {
			b.WriteString("CREATE TRIGGER ")
		}
		b.WriteIdent(name)
		b.WriteByte(' ')
		b.WriteString(opts.Timing)
		b.WriteByte(' ')
		b.WriteString(opts.Events)
		b.WriteString(" ON ")
		opts.Table.writeName(b)
		b.WriteString(" FOR EACH ")
		b.WriteString(opts.ForEach)
		if opts.When != "" {
			b.WriteString(" WHEN (")
			b.WriteString(opts.When)
			b.WriteByte(')')
		}
		b.WriteString(" EXECUTE FUNCTION ")
		b.WriteString(opts.Execute)
	})
}

// DropTrigger returns DROP TRIGGER "name" ON <table>.
func DropTrigger(name string, table *Table) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("DROP TRIGGER ")
		b.WriteIdent(name)
		b.WriteString(" ON ")
		table.writeName(b)
	})
}

// DropTriggerIfExists is the IF EXISTS variant.
func DropTriggerIfExists(name string, table *Table) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("DROP TRIGGER IF EXISTS ")
		b.WriteIdent(name)
		b.WriteString(" ON ")
		table.writeName(b)
	})
}

// --- Comments ---------------------------------------------------------

// CommentOnTable returns COMMENT ON TABLE <t> IS 'text'. text must not
// contain unsanitised single quotes.
func CommentOnTable(t *Table, text string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("COMMENT ON TABLE ")
		t.writeName(b)
		b.WriteString(" IS ")
		b.AddArg(text)
	})
}

// CommentOnColumn returns COMMENT ON COLUMN <t>.<c> IS 'text'.
func CommentOnColumn(c ColRef, text string) drops.Expression {
	col := c.col()
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("COMMENT ON COLUMN ")
		if col.Table() != nil {
			col.Table().writeName(b)
			b.WriteByte('.')
		}
		b.WriteIdent(col.Name())
		b.WriteString(" IS ")
		b.AddArg(text)
	})
}

// --- Helpers for column definitions inside CREATE TABLE --------------

func writeColumnDef(b *drops.Builder, c *Column) {
	b.WriteIdent(c.Name())
	b.WriteByte(' ')
	b.WriteString(c.Type().TypeSQL())
	if c.IsPrimaryKey() {
		b.WriteString(" PRIMARY KEY")
	} else if c.IsNotNull() {
		b.WriteString(" NOT NULL")
	}
	if c.IsUnique() && !c.IsPrimaryKey() {
		b.WriteString(" UNIQUE")
	}
	if c.HasDefault() {
		b.WriteString(" DEFAULT ")
		b.WriteString(c.DefaultSQL())
	}
	if fk := c.ForeignKey(); fk != nil {
		b.WriteString(" REFERENCES ")
		fk.Target.Table().writeName(b)
		b.WriteString(" (")
		b.WriteIdent(fk.Target.Name())
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
}
