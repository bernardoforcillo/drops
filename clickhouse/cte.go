package clickhouse

import (
	"context"

	"github.com/bernardoforcillo/drops"
)

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
func (c *CTE) Ref() drops.Expression { return identExpr(c.name) }

// identExpr renders a bare identifier through the Builder's dialect. It
// is a named type rather than a closure so that nothing in this package
// builds an expression the resolver cannot walk — see opExpr. There is
// nothing inside an identifier to walk, which is exactly why it can be
// a leaf rather than an opExpr.
type identExpr string

func (i identExpr) WriteSQL(b *drops.Builder) { b.WriteIdent(string(i)) }

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

// resolveCTEs resolves each CTE body against ctx, returning a rebuilt
// list — or nil when no body needed resolving, so a statement whose
// CTEs hold nothing to scope renders exactly what it always did.
//
// A CTE body is a statement, and a statement over a scoped table is
// scoped wherever it is written. Rendered through WriteSQL — which is
// all a WITH prefix used to do with it — the body wrote its
// DefaultFilters, none of its ContextFilters, and refused nothing on a
// ctx with no tenant at all: WITH recent AS (SELECT … FROM events)
// read every tenant's events while the outer SELECT carried its own
// tenant predicate and looked scoped.
//
// The walk is resolveExpr's, so it reaches a body that is a builder,
// a body that is an expression with a builder inside it, and a nested
// CTE's own bodies through that body's resolveCtx. A body drops did
// not build — a [drops.Raw], a caller's own [drops.ExprFunc] — has
// nothing to resolve and stays the caller's to scope.
//
// The copy is per execution. Writing the resolved body back into the
// *CTE would pin one request's tenant into a definition a caller may
// hold at package scope and use from every request.
func resolveCTEs(ctx context.Context, ctes []*CTE) ([]*CTE, error) {
	var out []*CTE
	for i, c := range ctes {
		if c == nil {
			continue
		}
		resolved, changed, err := resolveExpr(ctx, c.query)
		if err != nil {
			return nil, err
		}
		if !changed {
			continue
		}
		if out == nil {
			out = append([]*CTE(nil), ctes...)
		}
		cp := *c
		cp.query = resolved
		out[i] = &cp
	}
	return out, nil
}
