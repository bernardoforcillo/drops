package pg

import (
	"fmt"

	"github.com/bernardoforcillo/drops"
)

// Filtering a parent by a property of its relation.
//
// With and Load answer "give me these authors, and their books". They
// cannot answer "give me the authors who have a published book" — the
// question is about the parent, and the relation is only the evidence.
// Written by hand it is a correlated subquery, and the hand-written
// version is where the related table's global filters get lost: a
// soft-deleted book goes on making its author match, because nobody
// remembered to restate the soft-delete guard inside the subquery.
//
//	db.Find(Authors).WhereHas("books", func(q *pg.RelQuery) {
//	    q.Where(BookPublished.Eq(true))
//	})
//
// compiles to
//
//	SELECT * FROM authors WHERE EXISTS (
//	  SELECT 1 FROM books
//	  WHERE books.author_id = authors.id
//	    AND books.deleted_at IS NULL      -- the books table's own filters
//	    AND books.published = $1          -- the caller's predicate
//	)
//
// and WhereDoesntHave is the same subquery under NOT EXISTS.

// RelQuery narrows the relation a WhereHas tests for. It is the
// WhereHas counterpart of [RelConfig]: the callback shape and the Where
// method are the same, so a reader who knows WithRel knows this. What
// it does not carry is anything about the shape of a result — no
// OrderBy, no Limit, no nested eager loads — because nothing here is
// loaded. The relation is only ever asked whether it has a row.
type RelQuery struct {
	node *hasNode
	err  *error // shared with the owning builder for error propagation
}

// hasNode is one level of a WhereHas. Nested WhereHas calls hang off
// children, each compiling to a further EXISTS inside this one's WHERE.
type hasNode struct {
	rel      *Relation
	negate   bool
	wheres   []drops.Expression
	children []*hasNode
	// scope is what this test opts out of, and it covers the related
	// table's filters — the whole point of threading them in is that
	// stepping around one has to be asked for.
	scope filterScope
	// bound, when set, replaces the EXISTS with a comparison against
	// the number of related rows.
	bound *countBound
}

// Where AND-s predicates into the subquery. They reference the related
// table's columns, exactly as the ones handed to [RelConfig.Where] do.
// Nil predicates are ignored, as in [SelectBuilder.Where].
func (q *RelQuery) Where(preds ...drops.Expression) *RelQuery {
	q.node.wheres = append(q.node.wheres, dropNilPreds(preds)...)
	return q
}

// Unscoped drops every global filter the related table registers from
// inside this test — the blunt instrument, and here it is blunter than
// usual: it is the difference between "has a published book" and "has a
// published book, deleted ones counting". See [SelectBuilder.Unscoped].
func (q *RelQuery) Unscoped() *RelQuery {
	q.node.scope.unscoped = true
	return q
}

// IgnoreFilters bypasses the named global filters on the related table
// from inside this test and leaves the rest standing — see
// [SelectBuilder.IgnoreFilters]. For a many-to-many it covers the
// junction table's filters too.
//
// [FilterTenant] is not among them. The tenant axis is not a filter a
// table carries — [Entity.ScopeByTenant] builds it per query from the
// ctx tenant — so it is never inside the subquery, and naming it here
// does nothing. See [FindBuilder.WhereHas].
func (q *RelQuery) IgnoreFilters(names ...string) *RelQuery {
	q.node.scope.ignore(names...)
	return q
}

// WhereHas requires the related rows to themselves have a relation:
// "every author with a book that has a review". It nests, so the
// predicate this builds is an EXISTS inside the enclosing EXISTS.
func (q *RelQuery) WhereHas(name string, fn func(*RelQuery)) *RelQuery {
	return q.nest(name, fn, false)
}

// WhereHasRel is [RelQuery.WhereHas] taking a relation handle, the way
// [RelConfig.LoadRel] does. A nil relation is ignored.
func (q *RelQuery) WhereHasRel(rel *Relation, fn func(*RelQuery)) *RelQuery {
	if rel == nil {
		return q
	}
	return q.nestRel(rel, rel.Name, fn, false)
}

// WhereDoesntHave is the negation of [RelQuery.WhereHas].
func (q *RelQuery) WhereDoesntHave(name string, fn func(*RelQuery)) *RelQuery {
	return q.nest(name, fn, true)
}

// WhereDoesntHaveRel is [RelQuery.WhereDoesntHave] taking a relation
// handle. A nil relation is ignored.
func (q *RelQuery) WhereDoesntHaveRel(rel *Relation, fn func(*RelQuery)) *RelQuery {
	if rel == nil {
		return q
	}
	return q.nestRel(rel, rel.Name, fn, true)
}

func (q *RelQuery) nest(name string, fn func(*RelQuery), negate bool) *RelQuery {
	on := q.node.rel.To
	return q.nestRel(on.Relation(name), name, fn, negate)
}

func (q *RelQuery) nestRel(rel *Relation, name string, fn func(*RelQuery), negate bool) *RelQuery {
	child, err := buildHasNode(rel, q.node.rel.To, name, fn, negate)
	if err != nil {
		if *q.err == nil {
			*q.err = err
		}
		return q
	}
	q.node.children = append(q.node.children, child)
	return q
}

// Count bounds. Whether to have these at all is a real question: EXISTS
// already answers "at least one", and it answers it by stopping at the
// first matching row, which no count can do. So the plain form keeps
// EXISTS and a bound switches rendering entirely — to a correlated
// count(…) compared against the threshold.
//
// They earn their place because "at least three books" is the same
// question as "at least one book" with a different number in it, and a
// caller who cannot ask it goes back to writing the subquery by hand —
// which is where the related table's filters get dropped, the accident
// this whole API exists to prevent. The number is spelled into the
// method name rather than passed as an operator constant so a
// mis-typed comparison is a compile error.
//
// A bound counts the related rows, not the rows a join would produce:
// for a many-to-many that is count(DISTINCT <target key>), so a
// junction that links the same pair twice does not inflate the answer.

// CountEq matches parents with exactly n related rows. CountEq(0) is
// [FindBuilder.WhereDoesntHave] spelled differently.
func (q *RelQuery) CountEq(n int) *RelQuery { return q.bind("=", n) }

// CountGt matches parents with more than n related rows.
func (q *RelQuery) CountGt(n int) *RelQuery { return q.bind(">", n) }

// CountGte matches parents with n or more related rows.
func (q *RelQuery) CountGte(n int) *RelQuery { return q.bind(">=", n) }

// CountLt matches parents with fewer than n related rows, parents with
// none included.
func (q *RelQuery) CountLt(n int) *RelQuery { return q.bind("<", n) }

// CountLte matches parents with at most n related rows, parents with
// none included.
func (q *RelQuery) CountLte(n int) *RelQuery { return q.bind("<=", n) }

func (q *RelQuery) bind(op string, n int) *RelQuery {
	q.node.bound = &countBound{op: op, n: n}
	return q
}

// countBound is a comparison against the number of related rows.
type countBound struct {
	op string
	n  int
}

// complement returns the bound that matches exactly the parents this
// one does not. Negation folds into the operator so a WhereDoesntHave
// carrying a bound renders as one comparison rather than a comparison
// wrapped in NOT.
func (c countBound) complement() countBound {
	switch c.op {
	case "=":
		return countBound{"<>", c.n}
	case "<>":
		return countBound{"=", c.n}
	case ">":
		return countBound{"<=", c.n}
	case ">=":
		return countBound{"<", c.n}
	case "<":
		return countBound{">=", c.n}
	case "<=":
		return countBound{">", c.n}
	}
	return c
}

// admitsZero reports whether a parent with no related rows at all
// satisfies the bound. It decides whether the self-referencing form can
// be expressed as a grouped semi-join, which by construction only ever
// yields parents with at least one related row.
func (c countBound) admitsZero() bool {
	switch c.op {
	case "=":
		return c.n == 0
	case "<>":
		return c.n != 0
	case ">":
		return c.n < 0
	case ">=":
		return c.n <= 0
	case "<":
		return c.n > 0
	case "<=":
		return c.n >= 0
	}
	return false
}

// ----------------------------------------------------------------------
// builder entry points
// ----------------------------------------------------------------------

// WhereHas narrows the result to the rows whose named relation has at
// least one row, optionally further narrowed by the callback:
//
//	db.Find(Authors).WhereHas("books", func(q *pg.RelQuery) {
//	    q.Where(BookPublished.Eq(true))
//	})
//
// It compiles to an EXISTS over the relation's own join condition — one
// statement, no join fan-out on the parent, nothing loaded. The related
// table's global filters are inside the subquery, so a soft-deleted
// book does not make its author match; say so with
// [RelQuery.IgnoreFilters] when it should.
//
// The callback may be nil, which asks only whether the relation has a
// row at all.
//
// Every relation kind but MorphTo is supported; a many-to-many joins
// its junction to its target inside the subquery. MorphTo is refused,
// for the reason nested With refuses it: the table to look in varies
// row by row, so there is no single subquery to write.
//
// Like a global filter, the predicate is built from the columns the
// relation was declared with, so it qualifies with the declared table
// name and does not follow a [Table.As] alias.
//
// What "global filters" covers is the ones the related *Table
// registers. It does not cover the tenant axis: [Entity.ScopeByTenant]
// builds that predicate per query out of the ctx tenant, which a
// *Table cannot see, so it narrows the outer SELECT and not this
// subquery — a parent of yours whose only related row belongs to
// another tenant still matches. The eager loaders have the same reach,
// for the same reason. Where that matters, exclude the other tenant in
// the callback: q.Where(BookOrg.Eq(org)).
func (f *FindBuilder) WhereHas(name string, fn func(*RelQuery)) *FindBuilder {
	return f.whereHas(f.table.Relation(name), name, fn, false)
}

// WhereHasRel is [FindBuilder.WhereHas] taking a relation handle rather
// than a name, the way [FindBuilder.LoadRel] does — and for the same
// reason: a relation a query knows statically should be a symbol the
// compiler checks. A nil relation is ignored, so an optional handle
// needs no guard.
//
//	var AuthorBooks = Authors.Rel("books")
//	db.Find(Authors).WhereHasRel(AuthorBooks, func(q *pg.RelQuery) {
//	    q.Where(BookPublished.Eq(true))
//	})
func (f *FindBuilder) WhereHasRel(rel *Relation, fn func(*RelQuery)) *FindBuilder {
	if rel == nil {
		return f
	}
	return f.whereHas(rel, rel.Name, fn, false)
}

// WhereDoesntHave is the negation of [FindBuilder.WhereHas]: the rows
// whose named relation has no row matching the callback — "every order
// with no shipment". It compiles to NOT EXISTS over the same subquery,
// filters included, so an order whose only shipment is soft-deleted
// counts as having none.
func (f *FindBuilder) WhereDoesntHave(name string, fn func(*RelQuery)) *FindBuilder {
	return f.whereHas(f.table.Relation(name), name, fn, true)
}

// WhereDoesntHaveRel is [FindBuilder.WhereDoesntHave] taking a relation
// handle. A nil relation is ignored.
func (f *FindBuilder) WhereDoesntHaveRel(rel *Relation, fn func(*RelQuery)) *FindBuilder {
	if rel == nil {
		return f
	}
	return f.whereHas(rel, rel.Name, fn, true)
}

// whereHas resolves the relation, runs the callback and attaches the
// compiled predicate to the root SELECT.
//
// The predicate lands on the SelectBuilder immediately rather than
// being held until All, because Entity's fast-scan, cache and Stream
// paths render that SelectBuilder themselves and never reach
// FindBuilder.All. A filter those paths silently dropped would be the
// worst kind of bug this file could have — and so is the error for a
// name that resolved to nothing, which each of them surfaces itself.
func (f *FindBuilder) whereHas(rel *Relation, name string, fn func(*RelQuery), negate bool) *FindBuilder {
	node, err := buildHasNode(rel, f.table, name, fn, negate)
	if err != nil {
		if f.relErr == nil {
			f.relErr = err
		}
		return f
	}
	f.sel.Where(hasExpr(node))
	return f
}

// buildHasNode validates the relation, runs the caller's callback and
// returns the populated node. Validation happens before the callback so
// an unsupported relation is reported as itself rather than as whatever
// the callback did to it.
func buildHasNode(rel *Relation, on *Table, name string, fn func(*RelQuery), negate bool) (*hasNode, error) {
	if rel == nil {
		return nil, fmt.Errorf("drops/pg: unknown relation %q on table %q", name, on.Name())
	}
	if _, _, err := hasCorrelation(rel); err != nil {
		return nil, err
	}
	node := &hasNode{rel: rel, negate: negate}
	var nerr error
	if fn != nil {
		fn(&RelQuery{node: node, err: &nerr})
	}
	if nerr != nil {
		return nil, nerr
	}
	return node, nil
}

// ----------------------------------------------------------------------
// rendering
// ----------------------------------------------------------------------

// hasCorrelation returns the two columns the subquery is tied to the
// enclosing query by: outer belongs to the table being filtered, inner
// to the first table the subquery names.
func hasCorrelation(rel *Relation) (outer, inner *Column, err error) {
	switch rel.Kind {
	case HasManyKind, HasOneKind, MorphManyKind:
		return rel.ParentKey, rel.ChildKey, nil
	case BelongsToKind:
		return rel.ChildKey, rel.ParentKey, nil
	case ManyToManyKind:
		// The subquery starts at the junction, so the correlation is
		// against the junction's FK and the target is reached by a join.
		return rel.ParentKey, rel.ThroughFK1, nil
	case MorphToKind:
		return nil, nil, fmt.Errorf("drops/pg: relation %q is MorphTo — WhereHas is not supported because the table to look in varies row by row",
			rel.Name)
	}
	return nil, nil, fmt.Errorf("drops/pg: relation %q has unknown kind", rel.Name)
}

// hasExpr compiles a node into the predicate the enclosing WHERE gets.
func hasExpr(n *hasNode) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { writeHas(b, n) })
}

// selfReferencing reports whether the subquery's own FROM would name
// the table the correlation points at. When it does, an EXISTS cannot
// be written: the inner FROM shadows the name, and the correlation
// binds to the inner row instead of the outer one.
func selfReferencing(rel *Relation) bool {
	from := tableKey(rel.From)
	if rel.To != nil && tableKey(rel.To) == from {
		return true
	}
	if rel.Through != nil && tableKey(rel.Through) == from {
		return true
	}
	return false
}

func writeHas(b *drops.Builder, n *hasNode) {
	rel := n.rel
	outer, inner, err := hasCorrelation(rel)
	if err != nil {
		// buildHasNode refuses these, so reaching here means a node was
		// constructed some other way. Fail loudly rather than emit SQL
		// that means something else.
		panic(err.Error())
	}

	negate := n.negate
	bound := n.bound
	if bound != nil && negate {
		c := bound.complement()
		bound, negate = &c, false
	}

	if !selfReferencing(rel) {
		corr := Eq(inner, outer)
		if bound == nil {
			if negate {
				b.WriteString("NOT ")
			}
			b.WriteString("EXISTS (SELECT 1")
			writeHasBody(b, n, corr)
			b.WriteByte(')')
			return
		}
		b.WriteString("((SELECT ")
		writeHasCount(b, rel)
		writeHasBody(b, n, corr)
		b.WriteString(") ")
		b.WriteString(bound.op)
		b.WriteByte(' ')
		b.AddArg(bound.n)
		b.WriteByte(')')
		return
	}

	// Self-referencing relation — an employee's reports, a category's
	// children, a user's followers. The correlated EXISTS is unwritable
	// here without aliasing the inner table, and an alias is exactly
	// what the caller's predicates and the table's own filters cannot
	// follow: both are closed over the handles they were built from,
	// which qualify with the declared table name (see [Table.As]).
	//
	// So the test becomes an uncorrelated semi-join on the key instead.
	// Every reference inside the subquery binds to the subquery's own
	// FROM, which is what those predicates mean; the only reference to
	// the outer row is the key on the left of IN, which sits outside
	// the subquery and binds to the outer table. Nothing needs
	// rewriting, and nothing is silently wrong.
	if bound != nil && bound.admitsZero() {
		// A semi-join can only ever return parents that have at least
		// one related row, so a bound that a parent with none satisfies
		// has to be asked the other way round.
		c := bound.complement()
		bound, negate = &c, !negate
	}
	b.WriteByte('(')
	if negate {
		// NOT IN is UNKNOWN when the outer key is NULL, which would
		// drop exactly the rows NOT EXISTS keeps: an employee with no
		// manager does not have an active one.
		b.Append(IsNull(outer))
		b.WriteString(" OR ")
	}
	b.Append(outer)
	if negate {
		b.WriteString(" NOT IN (SELECT ")
	} else {
		b.WriteString(" IN (SELECT ")
	}
	b.Append(inner)
	// A NULL key joins to nothing, so excluding it leaves IN unchanged
	// — and rescues NOT IN, which a single NULL in the list would
	// otherwise turn into "no rows at all".
	writeHasBody(b, n, IsNotNull(inner))
	if bound != nil {
		b.WriteString(" GROUP BY ")
		b.Append(inner)
		b.WriteString(" HAVING ")
		writeHasCount(b, rel)
		b.WriteByte(' ')
		b.WriteString(bound.op)
		b.WriteByte(' ')
		b.AddArg(bound.n)
	}
	b.WriteString("))")
}

// writeHasBody writes the subquery's FROM (with the junction join, for
// a many-to-many) and its WHERE.
func writeHasBody(b *drops.Builder, n *hasNode, lead drops.Expression) {
	rel := n.rel
	b.WriteString(" FROM ")
	if rel.Kind == ManyToManyKind {
		rel.Through.writeFrom(b)
		b.WriteString(" INNER JOIN ")
		rel.To.writeFrom(b)
		b.WriteString(" ON ")
		b.Append(Eq(rel.ChildKey, rel.ThroughFK2))
	} else {
		rel.To.writeFrom(b)
	}
	b.WriteString(" WHERE ")
	writeAnd(b, hasPredicates(n, lead))
}

// hasPredicates assembles everything the subquery's WHERE carries: the
// tie to the enclosing row, the relation's own discriminator, the
// tables' global filters, the caller's predicates and any nested test.
//
// The filters are the reason this is threaded rather than left to
// Select().From(), which would have supplied them for free: this
// renders its subquery by hand, so a filter it does not spell out is a
// filter the query does not have — the same hole the per-parent limit
// path had, where adding a cap to an eager load quietly handed back
// soft-deleted children.
func hasPredicates(n *hasNode, lead drops.Expression) []drops.Expression {
	rel := n.rel
	preds := []drops.Expression{lead}
	if rel.Kind == MorphManyKind {
		preds = append(preds, Eq(rel.MorphTypeCol, rel.MorphType))
	}
	if rel.Kind == ManyToManyKind {
		preds = append(preds, n.scope.apply(rel.Through, nil)...)
	}
	preds = append(preds, n.scope.apply(rel.To, nil)...)
	preds = append(preds, n.wheres...)
	for _, child := range n.children {
		preds = append(preds, hasExpr(child))
	}
	return preds
}

// writeHasCount writes the aggregate a count bound compares against.
func writeHasCount(b *drops.Builder, rel *Relation) {
	if rel.Kind == ManyToManyKind {
		// Counting junction rows would answer a different question:
		// two links to the same book are still one book.
		b.WriteString("count(DISTINCT ")
		b.Append(rel.ChildKey)
		b.WriteByte(')')
		return
	}
	b.WriteString("count(*)")
}

// ----------------------------------------------------------------------
// typed counterpart
// ----------------------------------------------------------------------

// WhereHas narrows the query to rows whose named relation has a
// matching row — see [FindBuilder.WhereHas].
func (q *EntityQuery[T]) WhereHas(name string, fn func(*RelQuery)) *EntityQuery[T] {
	q.fb.WhereHas(name, fn)
	return q
}

// WhereHasRel is [EntityQuery.WhereHas] taking a relation handle.
func (q *EntityQuery[T]) WhereHasRel(rel *Relation, fn func(*RelQuery)) *EntityQuery[T] {
	q.fb.WhereHasRel(rel, fn)
	return q
}

// WhereDoesntHave narrows the query to rows whose named relation has no
// matching row — see [FindBuilder.WhereDoesntHave].
func (q *EntityQuery[T]) WhereDoesntHave(name string, fn func(*RelQuery)) *EntityQuery[T] {
	q.fb.WhereDoesntHave(name, fn)
	return q
}

// WhereDoesntHaveRel is [EntityQuery.WhereDoesntHave] taking a relation
// handle.
func (q *EntityQuery[T]) WhereDoesntHaveRel(rel *Relation, fn func(*RelQuery)) *EntityQuery[T] {
	q.fb.WhereDoesntHaveRel(rel, fn)
	return q
}
