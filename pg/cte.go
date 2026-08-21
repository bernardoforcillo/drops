package pg

import (
	"context"
	"errors"
	"fmt"

	"github.com/bernardoforcillo/drops"
)

// CTE (common table expression) support. CTEs are attached to a
// SelectBuilder via With / WithRecursive and rendered as the WITH
// clause prefix when the query is built.

// ErrCTENameConflict is returned when one statement would declare two
// different CTEs under the same name.
//
// A statement gets one WITH clause, so the CTEs of a set operation's
// operands are hoisted into the leading one — see
// SelectBuilder.hoistCTEs. Two bodies with one name cannot both be
// declared there, and picking either would silently answer the other
// operand with a relation it did not ask for, so the statement is
// refused. Rename one, or select from a subquery instead of a CTE.
var ErrCTENameConflict = errors.New("drops/pg: two CTEs in one statement share a name")

// CTE describes one common table expression.
type CTE struct {
	name    string
	columns []string // optional explicit column list
	query   drops.Expression

	// origin is the CTE this one was copied from when its body was
	// resolved for a ctx, and nil for a CTE the caller built. It is
	// what makes one CTE attached to both operands of a set operation
	// one declaration after resolution rather than two that collide:
	// resolution copies, so the pointers differ by then and only the
	// origin still says they are the same definition.
	origin *CTE
}

// identity returns the value a CTE was defined as — itself, unless it
// is a resolved copy.
func (c *CTE) identity() *CTE {
	if c.origin != nil {
		return c.origin
	}
	return c
}

// CTEDef returns a CTE definition with optional column aliasing.
//
// When query is a statement drops built — a SELECT, an UPDATE, an
// INSERT or a DELETE, bare or wrapped in [Subquery] — the body is
// scoped like the statement it is: the executor resolves the context
// filters of every table it names — the tenant axis, an authz guard —
// before the WITH clause is rendered, so WITH recent AS (SELECT ...
// FROM posts) restricts the same rows a bare SELECT from posts would,
// and WITH moved AS (DELETE FROM posts RETURNING ...) removes the same
// rows a bare DELETE would. That matters more here than almost anywhere
// else, because WITH ... AS (<statement over a table>) is the shape a
// real multi-tenant analytics query and a real archive-these-rows write
// are both written in, and an unscoped body reaches every tenant's rows
// however carefully the outer SELECT is scoped.
//
// A CTE built from any other drops.Expression — a Raw fragment, a
// hand-assembled expression — cannot be: there is no ctx inside
// WriteSQL and nothing to resolve against. Its filtering is the
// caller's, exactly as with a subquery body drops did not build — see
// [Exists].
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
// Each body that is a statement drops built is resolved against the
// executing ctx before it is rendered, so a CTE selecting from — or
// writing to — a scoped table carries that table's context filters and
// its tenant stamp — see [CTEDef]. The outer SELECT's Unscoped() does
// not reach into a body: a CTE is a statement of its own and keeps its
// own scoping, which is also how to unscope one relation of a query and
// no other.
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

// resolveCTEs resolves each body against ctx, returning a rebuilt list
// — or nil when nothing needed resolving, so the caller can keep the
// builder it already had.
//
// The walk is resolveExpr, which is the point. This function used to
// type-assert *SelectBuilder itself, and a local type assertion is a
// second copy of resolveExpr's decision about what a statement is — one
// that was already staler than the original. It saw a bare SELECT body
// and nothing else: not a body wrapped in [Subquery] or
// [SelectBuilder.AsSubquery], which resolveExpr has walked into since
// round 4, and not a data-modifying body, which is the shape that made
// WITH moved AS (DELETE FROM scoped_table RETURNING ...) a cross-tenant
// write. One call closes all of them, and keeps closing whatever
// resolveExpr learns next.
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
		cp.origin = c.identity()
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

// hoistCTEs returns the CTEs the statement declares, gathered from every
// set-operation operand into the one WITH clause SQL allows a statement,
// and whether any of them asked for RECURSIVE.
//
// The operands' CTEs used to be dropped: WriteSQL rendered a right
// operand through writeCore, which writes no WITH prefix, so a union
// whose right operand selected from its own CTE was sent referring to a
// relation the statement no longer declared — 42P01, undefined table.
// Hoisting widens where such a CTE is visible and nothing else, which
// is why it is safe to do; declaring the same name twice would not be,
// so checkCTENames refuses that.
//
// The same CTE value attached to more than one operand is hoisted once.
// Deduplication is by identity rather than by name so that it stays a
// deduplication and never quietly resolves a genuine conflict.
func (s *SelectBuilder) hoistCTEs() ([]*CTE, bool) {
	if len(s.setOps) == 0 {
		return s.ctes, s.recursiveCTE
	}
	out := append([]*CTE(nil), s.ctes...)
	recursive := s.recursiveCTE
	for _, op := range s.setOps {
		operand, operandRecursive := op.right.hoistCTEs()
		recursive = recursive || operandRecursive
		for _, c := range operand {
			if containsCTE(out, c) {
				continue
			}
			out = append(out, c)
		}
	}
	return out, recursive
}

// containsCTE reports whether list already declares c.
func containsCTE(list []*CTE, c *CTE) bool {
	for _, x := range list {
		if x.identity() == c.identity() {
			return true
		}
	}
	return false
}

// checkCTENames refuses a statement whose single WITH clause would
// declare one name twice. It is checked at resolution rather than at
// render because rendering cannot return an error, and resolution is
// the step every executor goes through.
func (s *SelectBuilder) checkCTENames() error {
	ctes, _ := s.hoistCTEs()
	if len(ctes) < 2 {
		return nil
	}
	seen := make(map[string]*CTE, len(ctes))
	for _, c := range ctes {
		if prev, ok := seen[c.name]; ok && prev.identity() != c.identity() {
			return fmt.Errorf("%w: %q", ErrCTENameConflict, c.name)
		}
		seen[c.name] = c
	}
	return nil
}
