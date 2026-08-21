package pg

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/bernardoforcillo/drops"
)

// Page is the typed result of a cursor-based pagination. NextCursor
// is empty when no further rows exist; HasMore short-circuits the
// presence check.
type Page[T any] struct {
	Items      []T
	NextCursor string
	HasMore    bool
}

// PageBuilder composes a cursor-paginated query. It exists to keep
// the cursor encoding/decoding internal — callers never construct or
// inspect cursors directly.
//
// Cursors are opaque, URL-safe base64 strings — the same encoding
// [EncodeCursor] produces, and the same keyset guard [AfterCursor]
// builds from them, because a page walked through this builder and a
// page walked through the SELECT builder are the same walk. Stable as
// long as the OrderBy spec doesn't change between calls.
//
// A page is a read like any other, so it carries the entity's scoping:
// the [Entity.ScopeByTenant] axis and the [Entity.AuthorizeWith] guard
// both narrow which rows may appear on it, and a missing ctx tenant or
// subject fails the page rather than widening it.
type PageBuilder[T any] struct {
	e        *Entity[T]
	db       *DB
	orderBys []OrderingColumn
	wheres   []drops.Expression
	after    string
	limit    int
}

// OrderingColumn pairs a *Column with its sort direction. Build one
// with Asc / Desc.
type OrderingColumn struct {
	col *Column
	asc bool
}

// Asc returns an OrderingColumn for c sorted ascending. Accepts
// either *Column or *Col[T] via ColRef.
func Asc(c ColRef) OrderingColumn { return OrderingColumn{col: c.col(), asc: true} }

// Desc returns an OrderingColumn for c sorted descending.
func Desc(c ColRef) OrderingColumn { return OrderingColumn{col: c.col(), asc: false} }

// Page returns a cursor-paginated builder for this entity. The
// default limit is 50; override with Limit.
//
//	pg, err := UserEntity.Page(db).
//	    OrderBy(pg.Asc(UserID)).
//	    Limit(20).
//	    After(prevCursor).
//	    All(ctx)
//	if pg.HasMore {
//	    // request next page with pg.NextCursor
//	}
func (e *Entity[T]) Page(db *DB) *PageBuilder[T] {
	return &PageBuilder[T]{e: e, db: db, limit: 50}
}

// OrderBy fixes the cursor's stability axis. At least one column is
// required; the last column should be unique (typically the PK) so
// every row has a distinct cursor.
func (p *PageBuilder[T]) OrderBy(cols ...OrderingColumn) *PageBuilder[T] {
	p.orderBys = append(p.orderBys, cols...)
	return p
}

// Where appends predicates joined by AND, ignoring the nil ones.
// Composes with the cursor guard so additional filters narrow the page
// set.
func (p *PageBuilder[T]) Where(preds ...drops.Expression) *PageBuilder[T] {
	p.wheres = append(p.wheres, dropNilPreds(preds)...)
	return p
}

// After resumes iteration after the supplied cursor. Pass the empty
// string for the first page.
func (p *PageBuilder[T]) After(cursor string) *PageBuilder[T] {
	p.after = cursor
	return p
}

// Limit caps the page size. Defaults to 50.
func (p *PageBuilder[T]) Limit(n int) *PageBuilder[T] {
	if n > 0 {
		p.limit = n
	}
	return p
}

// All runs the query and returns the page.
func (p *PageBuilder[T]) All(ctx context.Context) (*Page[T], error) {
	if len(p.orderBys) == 0 {
		return nil, errors.New("drops/pg: Page requires OrderBy(...)")
	}

	orderBys, err := p.resolveOrderBys()
	if err != nil {
		return nil, err
	}

	sel := p.db.Select().From(p.e.table)
	// A page is a read like any other, so it owes the entity's scoping
	// — the tenant axis and the authorisation guard both narrow which
	// rows may appear on it, and a page that skipped them handed the
	// caller another tenant's rows with a cursor to walk more of them.
	tenantPred, err := p.e.tenantPredicate(ctx)
	if err != nil {
		return nil, err
	}
	if tenantPred != nil {
		sel.Where(tenantPred)
	}
	guardPred, err := p.e.guardPredicate(ctx)
	if err != nil {
		return nil, err
	}
	if guardPred != nil {
		sel.Where(guardPred)
	}
	for _, w := range p.wheres {
		sel.Where(w)
	}
	// Apply the cursor guard, if any.
	if p.after != "" {
		guard, err := cursorGuard(orderBys, p.after)
		if err != nil {
			return nil, err
		}
		sel.Where(guard)
	}
	// Stable ordering — every OrderingColumn renders into the
	// SELECT's ORDER BY.
	for _, o := range orderBys {
		sel.OrderBy(orderingExpr(o))
	}
	// Fetch one extra row to detect HasMore without a follow-up
	// COUNT or a re-query.
	sel.Limit(int64(p.limit + 1))

	var rows []T
	if p.e.fastScan != nil {
		if err := p.e.scanAllFast(p.db, ctx, sel, &rows); err != nil {
			return nil, err
		}
	} else {
		if err := sel.All(ctx, &rows); err != nil {
			return nil, err
		}
	}

	hasMore := len(rows) > p.limit
	if hasMore {
		rows = rows[:p.limit]
	}

	out := &Page[T]{Items: rows, HasMore: hasMore}
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		cur, err := encodeCursor(p.e, orderBys, last)
		if err != nil {
			return nil, err
		}
		out.NextCursor = cur
	}
	return out, nil
}

// resolveOrderBys restates every ordering column with the handle this
// entity's own table hands out.
//
// Asc and Desc are free functions, so an ordering column arrives as
// whichever handle the caller happened to hold, and it is rendered
// three times — into the ORDER BY, into the cursor guard's row
// comparison, and (by name) into the error when no struct field
// matches. A handle from another instance of the table qualifies with
// a relation the SELECT does not name, and PostgreSQL answers 42P01:
// an entity on an alias paged by the package-level column, or an
// entity on the base table paged by an alias handle, both fail. The
// column meant is the same one either way — Column.key says so — so
// the fix is to page by the handle the entity queries, which is what
// ScopeByTenant does with the tenant axis and for the same reason.
//
// Resolving here also moves the "no struct field" error ahead of the
// query. A cursor is built from the row's field values, so an
// ordering column with no field could never produce one; failing
// before the round trip beats failing at the first page boundary.
func (p *PageBuilder[T]) resolveOrderBys() ([]OrderingColumn, error) {
	out := make([]OrderingColumn, len(p.orderBys))
	for i, o := range p.orderBys {
		var own *Column
		for _, cf := range p.e.colFields {
			if cf.col.key() == o.col.key() {
				own = cf.col
				break
			}
		}
		if own == nil {
			return nil, fmt.Errorf("drops/pg: Page.OrderBy column %q has no matching struct field", o.col.Name())
		}
		out[i] = OrderingColumn{col: own, asc: o.asc}
	}
	return out, nil
}

// orderingExpr renders an OrderingColumn into the ORDER BY form
// drops.Expression-able by the SELECT builder.
func orderingExpr(o OrderingColumn) drops.Expression {
	if o.asc {
		return o.col.Asc()
	}
	return o.col.Desc()
}

// cursorSpec restates the ordering columns as the cursor shape the
// keyset guard is written against.
//
// The ORDER BY these columns render — [Column.Asc] and [Column.Desc] —
// carries no NULLS clause, so PostgreSQL applies its per-direction
// default, and NullsDefault is what [nullsFirst] resolves against the
// same default. The two therefore cannot drift.
func cursorSpec(orderBys []OrderingColumn) CursorSpec {
	keys := make([]OrderKey, len(orderBys))
	for i, o := range orderBys {
		keys[i] = OrderKey{Col: o.col, Desc: !o.asc}
	}
	return CursorSpec{Keys: keys}
}

// cursorGuard builds the WHERE predicate that moves past the supplied
// cursor.
//
// It is [keysetWhere], the guard the SELECT builder's AfterCursor uses,
// rather than a second row-comparison written here. The row comparison
// this used to render — (c1, c2) > ($1, $2) — is not NULL-aware, and a
// nullable ordering column is the ordinary case rather than an exotic
// one: a page whose last row held a NULL rendered a comparison against
// NULL, which matches nothing, so the walk reported no further rows and
// stopped short of every row behind that NULL with nothing anywhere
// saying why. See [keysetStrict] for what replaces it.
func cursorGuard(orderBys []OrderingColumn, cursor string) (drops.Expression, error) {
	vals, err := Cursor(cursor).Decode()
	if err != nil {
		return nil, fmt.Errorf("drops/pg: invalid cursor: %w", err)
	}
	if len(vals) != len(orderBys) {
		return nil, fmt.Errorf("drops/pg: cursor has %d value(s), OrderBy has %d column(s)", len(vals), len(orderBys))
	}
	return keysetWhere(cursorSpec(orderBys), vals, true), nil
}

// encodeCursor extracts the ordering-column values from the last row
// and hands them to [EncodeCursor], which is the encoding
// [Cursor.Decode] and the keyset guard both read.
//
// It used to gob-encode the values, which could not carry two of the
// shapes an ordering column most often has: a nil pointer, which is how
// a NULL arrives on the row struct, gob refuses outright, and a
// time.Time inside an interface needs a gob.Register nothing performed
// — so paging by a timestamp, the column keyset pagination exists for,
// failed on the first page boundary.
func encodeCursor[T any](e *Entity[T], orderBys []OrderingColumn, row T) (string, error) {
	v := reflect.ValueOf(&row).Elem()
	vals := make([]any, len(orderBys))
	for i, o := range orderBys {
		// Find the entity field bound to o.col.
		var idx []int
		for _, cf := range e.colFields {
			if cf.col.key() == o.col.key() {
				idx = cf.field
				break
			}
		}
		if idx == nil {
			return "", fmt.Errorf("drops/pg: Page.OrderBy column %q has no matching struct field", o.col.Name())
		}
		vals[i] = v.FieldByIndex(idx).Interface()
	}
	cur, err := EncodeCursor(cursorSpec(orderBys), vals...)
	if err != nil {
		return "", err
	}
	return string(cur), nil
}
