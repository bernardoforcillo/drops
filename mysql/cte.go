package mysql

import (
	"context"
	"fmt"

	"github.com/bernardoforcillo/drops"
)

// Common table expressions, ported from drops/pg's cte.go. A CTE is
// attached to a [SelectBuilder] with With / WithRecursive and rendered
// as the WITH prefix when the statement is built.
//
// # Server support
//
// WITH arrived in MySQL 8.0 and MariaDB 10.2. Older servers do not
// parse it — the statement fails with a syntax error at "WITH", before
// anything is executed — so drops does not try to emulate one. Ask
// [ServerInfo.SupportsCTE] when the target version is not known.
//
// # Recursion, and the one place the two servers disagree silently
//
// Both servers cap how many times a recursive CTE may iterate, which
// PostgreSQL does not do at all. The cap is the same number and a
// different variable, and — the part that matters — a different
// outcome when it is hit:
//
//   - MySQL: cte_max_recursion_depth, default 1000. Exceeding it
//     raises error 3636 and the query fails.
//   - MariaDB: max_recursive_iterations, default 1000. Exceeding it
//     stops the recursion and returns the rows produced so far, with
//     no error and no warning.
//
// So the same recursive query that errors on MySQL returns a short,
// wrong answer on MariaDB. A recursion whose depth is not provably
// under the cap must either bound itself in its own WHERE clause or
// raise the limit first — see [SetRecursionLimit], which writes
// whichever variable the connected server has.
//
// One more MySQL-side trap with no PostgreSQL counterpart: the
// non-recursive (anchor) member fixes the column types for the whole
// CTE. An anchor selecting the literal 'a' makes that column CHAR(1),
// and the recursive member's longer strings are then truncated rather
// than widening it. CAST the anchor's columns to the width the
// recursion needs.

// CTE describes one common table expression.
type CTE struct {
	name    string
	columns []string // optional explicit column list
	query   drops.Expression
}

// CTEDef returns a CTE definition with optional column aliasing.
func CTEDef(name string, query drops.Expression, columns ...string) *CTE {
	mustIdent("CTE", name)
	for _, c := range columns {
		mustIdent("CTE column", c)
	}
	return &CTE{name: name, columns: columns, query: query}
}

// Name returns the CTE's alias.
func (c *CTE) Name() string { return c.name }

// Ref returns an expression that references the CTE as a relation —
// pass it to (*SelectBuilder).FromExpr, or use it as a join target.
func (c *CTE) Ref() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteIdent(c.name) })
}

// Col returns a column reference inside the CTE: `<cte>`.`<col>`.
func (c *CTE) Col(col string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteIdent(c.name)
		b.WriteByte('.')
		b.WriteIdent(col)
	})
}

// With prepends a WITH clause to the SELECT. Multiple calls accumulate.
func (s *SelectBuilder) With(ctes ...*CTE) *SelectBuilder {
	s.ctes = append(s.ctes, ctes...)
	return s
}

// WithRecursive marks the WITH clause RECURSIVE. One statement has one
// mode, so calling this is sticky: every CTE on the statement is
// rendered under the same RECURSIVE keyword, which is what both
// servers require and what PostgreSQL does too.
func (s *SelectBuilder) WithRecursive(ctes ...*CTE) *SelectBuilder {
	s.recursiveCTE = true
	s.ctes = append(s.ctes, ctes...)
	return s
}

// writeCTEs renders the WITH prefix into b.
func writeCTEs(b *drops.Builder, ctes []*CTE, recursive bool) {
	if len(ctes) == 0 {
		return
	}
	b.WriteString("WITH ")
	if recursive {
		b.WriteString("RECURSIVE ")
	}
	for i, c := range ctes {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteIdent(c.name)
		if len(c.columns) > 0 {
			b.WriteString(" (")
			for j, col := range c.columns {
				if j > 0 {
					b.WriteString(", ")
				}
				b.WriteIdent(col)
			}
			b.WriteByte(')')
		}
		b.WriteString(" AS (")
		b.Append(c.query)
		b.WriteByte(')')
	}
	b.WriteByte(' ')
}

// SupportsCTE reports whether the server parses a WITH clause: MySQL
// 8.0 and MariaDB 10.2 onwards.
func (s ServerInfo) SupportsCTE() bool {
	if s.MariaDB {
		return s.AtLeast(10, 2, 0)
	}
	return s.AtLeast(8, 0, 0)
}

// SetRecursionLimit raises (or lowers) the connection's cap on
// recursive-CTE iterations, writing whichever session variable the
// connected server has: cte_max_recursion_depth on MySQL,
// max_recursive_iterations on MariaDB. Both default to 1000.
//
// It is a session setting, so it applies to the connection the
// statement lands on. Against a pool that means "some connection" —
// call it inside the transaction (or on a pinned *DB) that runs the
// recursive query, not once at start-up.
//
// A limit of zero is rejected rather than passed through: MySQL reads
// zero as "no limit", MariaDB does not, and a call whose meaning
// depends on which server answered is not one drops will make.
func SetRecursionLimit(ctx context.Context, db *DB, n int) error {
	if n <= 0 {
		return fmt.Errorf("drops/mysql: SetRecursionLimit needs a positive limit, got %d", n)
	}
	info, err := ServerVersion(ctx, db)
	if err != nil {
		return err
	}
	name := "cte_max_recursion_depth"
	if info.MariaDB {
		name = "max_recursive_iterations"
	}
	// The variable name is chosen here, never supplied by a caller,
	// so the interpolation cannot carry anything but one of these two
	// literals; the value is bound.
	_, err = db.Exec(ctx, "SET SESSION "+name+" = ?", n)
	return err
}
