package pg

import (
	"context"

	"github.com/bernardoforcillo/drops"
)

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
//
// When query is a *SelectBuilder the body is scoped like any other
// statement: the executor resolves its FROM table's context filters —
// the tenant axis, an authz guard — before the WITH clause is rendered,
// so WITH recent AS (SELECT ... FROM posts) restricts the same rows a
// bare SELECT from posts would. That matters more here than almost
// anywhere else, because WITH ... AS (SELECT ... FROM <table>) is the
// shape a real multi-tenant analytics query is written in, and an
// unscoped CTE body reaches every tenant's rows however carefully the
// outer SELECT is scoped.
//
// A CTE built from any other drops.Expression — a Raw fragment, a
// hand-assembled expression, an INSERT ... RETURNING — cannot be: there
// is no ctx inside WriteSQL and nothing to resolve against. Its
// filtering is the caller's, exactly as with
// [SelectBuilder.AsSubquery].
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
//
// Each body whose query is a *SelectBuilder is resolved against the
// executing ctx before it is rendered, so a CTE selecting from a scoped
// table carries that table's context filters — see [CTEDef]. The outer
// SELECT's Unscoped() does not reach into a body: a CTE is a statement
// of its own and keeps its own scoping, which is also how to unscope
// one relation of a query and no other.
func (s *SelectBuilder) With(ctes ...*CTE) *SelectBuilder {
	s.ctes = append(s.ctes, ctes...)
	return s
}

// WithRecursive marks the WITH clause as RECURSIVE. Only one mode is
// possible per statement; calling this is sticky.
//
// The bodies are resolved exactly as [SelectBuilder.With]'s are, which
// for a recursive CTE means the predicate lands on both terms — the
// anchor and the recursive one. That is what makes the traversal itself
// stay inside the tenant rather than only its first row.
func (s *SelectBuilder) WithRecursive(ctes ...*CTE) *SelectBuilder {
	s.recursiveCTE = true
	s.ctes = append(s.ctes, ctes...)
	return s
}

// resolveCTEs resolves the bodies that are *SelectBuilders against ctx,
// returning a rebuilt list — or nil when nothing needed resolving, so
// the caller can keep the builder it already had.
//
// Both the list and the *CTE are copied before anything is replaced.
// The CTE the caller holds is a value they can attach to a second
// query, and a resolved body written back into it would pin one
// request's tenant into every later statement that names it.
func resolveCTEs(ctx context.Context, ctes []*CTE) ([]*CTE, error) {
	var out []*CTE
	for i, c := range ctes {
		body, ok := c.query.(*SelectBuilder)
		if !ok {
			continue
		}
		resolved, err := body.resolveCtx(ctx)
		if err != nil {
			return nil, err
		}
		if resolved == body {
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
