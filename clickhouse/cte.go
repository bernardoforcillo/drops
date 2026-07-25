package clickhouse

import "github.com/bernardoforcillo/drops"

// Common table expression (CTE) support. ClickHouse supports the
// subquery form of WITH — "WITH <name> AS (<subquery>) SELECT …". CTEs
// attach to a SelectBuilder via With and render as the WITH prefix.
//
// ClickHouse has no RECURSIVE CTEs (as of the mainstream releases), so
// only the non-recursive form is offered here.

// CTE describes one common table expression.
type CTE struct {
	name  string
	query drops.Expression
}

// CTEDef returns a CTE definition binding name to query.
func CTEDef(name string, query drops.Expression) *CTE {
	mustIdent("cte", name)
	return &CTE{name: name, query: query}
}

// Name returns the CTE's alias.
func (c *CTE) Name() string { return c.name }

// Ref references the CTE as a relation in a subsequent FROM/JOIN.
func (c *CTE) Ref() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteIdent(c.name) })
}

// With prepends a WITH clause to the SELECT. Multiple calls accumulate.
func (s *SelectBuilder) With(ctes ...*CTE) *SelectBuilder {
	s.ctes = append(s.ctes, ctes...)
	return s
}

// writeCTEs renders the WITH prefix into b.
func writeCTEs(b *drops.Builder, ctes []*CTE) {
	if len(ctes) == 0 {
		return
	}
	b.WriteString("WITH ")
	for i, c := range ctes {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteIdent(c.name)
		b.WriteString(" AS (")
		b.Append(c.query)
		b.WriteByte(')')
	}
	b.WriteByte(' ')
}
