package pg

import "github.com/bernardoforcillo/drops"

// CTE (common table expression) support. CTEs are attached to a
// SelectBuilder via With / WithRecursive and rendered as the WITH
// clause prefix when the query is built.

// CTE describes one common table expression.
type CTE struct {
	name    string
	columns []string // optional explicit column list
	query   drops.Expression
}

// CTEDef returns a CTE definition with optional column aliasing.
func CTEDef(name string, query drops.Expression, columns ...string) *CTE {
	return &CTE{name: name, columns: columns, query: query}
}

// Name returns the CTE's alias.
func (c *CTE) Name() string { return c.name }

// Ref returns an expression that references the CTE as a relation —
// useful inside JOIN/FROM clauses on subsequent SELECTs.
func (c *CTE) Ref() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteIdent(c.name) })
}

// Col returns a column reference inside the CTE: "<cte>"."<col>".
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

// WithRecursive marks the WITH clause as RECURSIVE. Only one mode is
// possible per statement; calling this is sticky.
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
