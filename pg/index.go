package pg

import "github.com/bernardoforcillo/drops"

// Index describes a PostgreSQL index. It is built via NewIndex (or the
// fluent helpers Unique, Concurrently, Where, Include, Using, On) and
// rendered with CreateIndex / DropIndex.
type Index struct {
	name         string
	table        *Table
	columns      []drops.Expression // column refs or arbitrary expressions
	unique       bool
	concurrently bool
	method       string // btree / hash / gist / gin / brin / spgist / hnsw / ivfflat
	include      []*Column
	where        drops.Expression
	opClasses    []string // per-column operator class, positionally paired with columns
	with         string   // raw WITH (key = value, …) clause
}

// NewIndex declares an index on t spanning cols. Cols may be column
// references (*Col[T] / *Column) or arbitrary expressions — Lower(c),
// pg.Func("upper", c), etc. — for functional indexes.
func NewIndex(name string, t *Table, cols ...drops.Expression) *Index {
	return &Index{name: name, table: t, columns: cols}
}

// Unique marks the index as UNIQUE.
func (i *Index) Unique() *Index { i.unique = true; return i }

// Concurrently emits the CONCURRENTLY keyword (PG creates the index
// without taking a long-lived ACCESS EXCLUSIVE lock; cannot run inside
// a transaction).
func (i *Index) Concurrently() *Index { i.concurrently = true; return i }

// Using sets the access method (btree, hash, gin, gist, brin, spgist).
func (i *Index) Using(method string) *Index { i.method = method; return i }

// Include adds columns to the INCLUDE clause (covering indexes).
func (i *Index) Include(cols ...*Column) *Index {
	i.include = append(i.include, cols...)
	return i
}

// Where adds a partial-index predicate.
func (i *Index) Where(pred drops.Expression) *Index { i.where = pred; return i }

// Name returns the unqualified index name.
func (i *Index) Name() string { return i.name }

// Table returns the table the index is on.
func (i *Index) Table() *Table { return i.table }

// CreateIndex returns the CREATE INDEX statement for idx.
func CreateIndex(idx *Index) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { writeIndexCreate(b, idx, false) })
}

// CreateIndexIfNotExists is the IF NOT EXISTS variant.
func CreateIndexIfNotExists(idx *Index) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { writeIndexCreate(b, idx, true) })
}

func writeIndexCreate(b *drops.Builder, idx *Index, ifNotExists bool) {
	b.WriteString("CREATE ")
	if idx.unique {
		b.WriteString("UNIQUE ")
	}
	b.WriteString("INDEX ")
	if idx.concurrently {
		b.WriteString("CONCURRENTLY ")
	}
	if ifNotExists {
		b.WriteString("IF NOT EXISTS ")
	}
	b.WriteIdent(idx.name)
	b.WriteString(" ON ")
	idx.table.writeName(b)
	if idx.method != "" {
		b.WriteString(" USING ")
		b.WriteString(idx.method)
	}
	b.WriteString(" (")
	for j, c := range idx.columns {
		if j > 0 {
			b.WriteString(", ")
		}
		writeIndexColumn(b, c)
		if j < len(idx.opClasses) && idx.opClasses[j] != "" {
			b.WriteByte(' ')
			b.WriteString(idx.opClasses[j])
		}
	}
	b.WriteByte(')')
	if len(idx.include) > 0 {
		b.WriteString(" INCLUDE (")
		for j, c := range idx.include {
			if j > 0 {
				b.WriteString(", ")
			}
			b.WriteIdent(c.Name())
		}
		b.WriteByte(')')
	}
	if idx.with != "" {
		b.WriteString(" WITH (")
		b.WriteString(idx.with)
		b.WriteByte(')')
	}
	if idx.where != nil {
		b.WriteString(" WHERE ")
		if pred, ok := indexPredicateSQL(idx.where); ok {
			b.WriteString(pred)
		} else {
			b.Append(idx.where)
		}
	}
}

// indexPredicateSQL renders a partial-index predicate as the static SQL
// a CREATE INDEX can carry, reporting false when it cannot.
//
// CREATE INDEX is a utility statement: PostgreSQL's grammar has no
// parameter slot in it. A predicate written the ordinary way —
// age.Gte(18) — binds its 18, so the statement arrives as
// "WHERE (age >= $1)" with one argument and the driver refuses it
// before the server ever parses it. The values are known where the
// schema is declared, not at query time, so they are folded into the
// text here, the same choice CommentOnTable makes for its literal.
//
// The fold is refused, rather than guessed at, for a value with no
// literal spelling; the caller then binds as before and the failure is
// the server's to report.
func indexPredicateSQL(e drops.Expression) (string, bool) {
	if e == nil {
		return "", false
	}
	sql, args := drops.StringWithDialect(Dialect, e)
	if len(args) == 0 {
		return sql, true
	}
	out, err := inlineSQLLiterals(sql, args)
	if err != nil {
		return "", false
	}
	return out, true
}

// writeIndexColumn renders one entry of a CREATE INDEX column list.
//
// Column references are written as BARE, unqualified identifiers.
// PostgreSQL rejects a table-qualified name inside an index column
// list — an index element must be a bare column name or a
// parenthesised expression, so `("t"."c")` fails with
// `syntax error at or near ")"` (SQLSTATE 42601). A column handle's
// default WriteSQL qualifies the reference (correct for SELECT/WHERE,
// invalid here), so we source the name from the handle and emit it
// unqualified instead.
//
// Anything that is not a column reference — a functional index such as
// Lower(c), or a raw expression — is rendered as-is; the caller is
// responsible for any parentheses PostgreSQL requires around it.
func writeIndexColumn(b *drops.Builder, c drops.Expression) {
	if cr, ok := c.(ColRef); ok {
		b.WriteIdent(cr.col().Name())
		return
	}
	b.Append(c)
}

// DropIndex returns DROP INDEX "name".
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

// DropIndexConcurrently emits DROP INDEX CONCURRENTLY.
func DropIndexConcurrently(name string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("DROP INDEX CONCURRENTLY ")
		b.WriteIdent(name)
	})
}
