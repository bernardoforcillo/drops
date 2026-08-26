package sqlite

import (
	"context"

	"github.com/bernardoforcillo/drops"
)

// Common table expression (CTE) support. CTEs attach to a SelectBuilder
// via With / WithRecursive and render as the WITH clause prefix when the
// statement is built. SQLite has supported WITH and WITH RECURSIVE since
// 3.8.3 (2014).

// CTE describes one common table expression.
type CTE struct {
	name    string
	columns []string // optional explicit column list
	query   drops.Expression
}

// CTEDef returns a CTE definition with an optional column alias list.
//
// When query is a statement drops built — a SELECT, an UPDATE, an
// INSERT or a DELETE, bare or wrapped in [Subquery] — the body is
// scoped like the statement it is: the executor resolves the context
// filters of every table it names — the tenant axis, an authz guard —
// before the WITH clause is rendered, so
// WITH recent AS (SELECT … FROM posts) restricts the same rows a bare
// SELECT from posts would. That matters more here than almost anywhere
// else, because WITH … AS (<statement over a table>) is the shape a
// real multi-tenant reporting query is written in, and an unscoped body
// reaches every tenant's rows however carefully the outer SELECT is
// scoped.
//
// A CTE built from any other drops.Expression — a Raw fragment, a
// hand-assembled expression — cannot be: there is no ctx inside
// WriteSQL and nothing to resolve against. Its filtering is the
// caller's, exactly as with a subquery body drops did not build.
func CTEDef(name string, query drops.Expression, columns ...string) *CTE {
	mustIdent("cte", name)
	return &CTE{name: name, columns: columns, query: query}
}

// Name returns the CTE's alias.
func (c *CTE) Name() string { return c.name }

// Ref returns an expression that references the CTE as a relation —
// useful inside a raw FROM/JOIN fragment on a subsequent SELECT.
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
//
// Each body that is a statement drops built is resolved against the
// executing ctx before it is rendered, so a CTE selecting from a scoped
// table carries that table's context filters — see [CTEDef]. The outer
// SELECT's Unscoped() does not reach into a body: a CTE is a statement
// of its own and keeps its own scoping, which is also how to unscope
// one relation of a query and no other.
func (s *SelectBuilder) With(ctes ...*CTE) *SelectBuilder {
	s.ctes = append(s.ctes, ctes...)
	return s
}

// WithRecursive marks the WITH clause as RECURSIVE. The mode is sticky —
// one WITH prefix per statement.
//
// The bodies are resolved exactly as [SelectBuilder.With]'s are, which
// for a recursive CTE means the predicate lands on both terms — the
// anchor and the recursive one, since a compound body is written as two
// statements and each is resolved as the statement it is. That is what
// makes the traversal itself stay inside the tenant rather than only
// its first row.
func (s *SelectBuilder) WithRecursive(ctes ...*CTE) *SelectBuilder {
	s.recursiveCTE = true
	s.ctes = append(s.ctes, ctes...)
	return s
}

// resolveCTEs resolves each body against ctx, returning a rebuilt list
// — or nil when nothing needed resolving, so the caller can keep the
// builder it already had.
//
// The walk is resolveExpr, which is the point: a local type assertion
// on *SelectBuilder would be a second copy of the decision about what a
// statement is, and it would already be blind to a body wrapped in
// [Subquery] and to a data-modifying body — the shape that makes
// WITH moved AS (DELETE FROM scoped RETURNING …) a cross-tenant write.
//
// Both the list and the *CTE are copied before anything is replaced.
// The CTE the caller holds is a value they can attach to a second
// query, and a resolved body written back into it would pin one
// request's tenant into every later statement that names it.
func resolveCTEs(ctx context.Context, ctes []*CTE) ([]*CTE, error) {
	var out []*CTE
	for i, c := range ctes {
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
