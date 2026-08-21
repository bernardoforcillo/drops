package sqlite

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/bernardoforcillo/drops"
)

// Page is the typed result of a cursor-based pagination. NextCursor is
// empty when no further rows exist; HasMore short-circuits the presence
// check.
type Page[T any] struct {
	Items      []T
	NextCursor string
	HasMore    bool
}

// PageBuilder composes a cursor-paginated query, keeping the cursor
// encoding/decoding internal — callers never construct or inspect
// cursors directly. Cursors are opaque, URL-safe base64 strings — the
// same encoding [EncodeCursor] produces, and the same keyset guard
// [SelectBuilder.AfterCursor] builds from them, because a page walked
// through this builder and a page walked through the SELECT builder are
// the same walk. Stable as long as the OrderBy spec doesn't change
// between calls.
type PageBuilder[T any] struct {
	e        *Entity[T]
	db       *DB
	orderBys []OrderingColumn
	wheres   []drops.Expression
	after    string
	limit    int
}

// OrderingColumn pairs a *Column with its sort direction. Build one with
// Asc / Desc.
type OrderingColumn struct {
	col *Column
	asc bool
}

// Asc returns an OrderingColumn for c sorted ascending.
func Asc(c ColRef) OrderingColumn { return OrderingColumn{col: c.col(), asc: true} }

// Desc returns an OrderingColumn for c sorted descending.
func Desc(c ColRef) OrderingColumn { return OrderingColumn{col: c.col(), asc: false} }

// Page returns a cursor-paginated builder for this entity. Default limit
// is 50; override with Limit.
func (e *Entity[T]) Page(db *DB) *PageBuilder[T] {
	return &PageBuilder[T]{e: e, db: db, limit: 50}
}

// OrderBy fixes the cursor's stability axis. At least one column is
// required; the last should be unique (typically the PK) so every row
// has a distinct cursor.
func (p *PageBuilder[T]) OrderBy(cols ...OrderingColumn) *PageBuilder[T] {
	p.orderBys = append(p.orderBys, cols...)
	return p
}

// Where appends predicates joined by AND, ignoring the nil ones, and
// composing with the cursor guard so filters narrow the page set.
func (p *PageBuilder[T]) Where(preds ...drops.Expression) *PageBuilder[T] {
	p.wheres = append(p.wheres, dropNilPreds(preds)...)
	return p
}

// After resumes iteration after the supplied cursor. Pass "" for the
// first page.
func (p *PageBuilder[T]) After(cursor string) *PageBuilder[T] {
	p.after = cursor
	return p
}

// Limit caps the page size (default 50).
func (p *PageBuilder[T]) Limit(n int) *PageBuilder[T] {
	if n > 0 {
		p.limit = n
	}
	return p
}

// All runs the query and returns the page. Honours the entity's tenant
// scope when one is configured.
func (p *PageBuilder[T]) All(ctx context.Context) (*Page[T], error) {
	if len(p.orderBys) == 0 {
		return nil, errors.New("drops/sqlite: Page requires OrderBy(...)")
	}
	tenantPred, err := p.e.tenantPredicate(ctx)
	if err != nil {
		return nil, err
	}

	sel := p.db.Select(p.e.selectCols()...).From(p.e.table)
	for _, w := range p.wheres {
		sel.Where(w)
	}
	if tenantPred != nil {
		sel.Where(tenantPred)
	}
	if p.after != "" {
		guard, err := cursorGuard(p.orderBys, p.after)
		if err != nil {
			return nil, err
		}
		sel.Where(guard)
	}
	for _, o := range p.orderBys {
		sel.OrderBy(orderingExpr(o))
	}
	// Fetch one extra row to detect HasMore without a follow-up COUNT.
	sel.Limit(int64(p.limit + 1))

	var rows []T
	if err := sel.All(ctx, &rows); err != nil {
		return nil, err
	}

	hasMore := len(rows) > p.limit
	if hasMore {
		rows = rows[:p.limit]
	}
	out := &Page[T]{Items: rows, HasMore: hasMore}
	if hasMore && len(rows) > 0 {
		cur, err := encodeCursor(p.e, p.orderBys, rows[len(rows)-1])
		if err != nil {
			return nil, err
		}
		out.NextCursor = cur
	}
	return out, nil
}

// orderingExpr renders "<col> ASC" / "<col> DESC".
func orderingExpr(o OrderingColumn) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		o.col.WriteSQL(b)
		if o.asc {
			b.WriteString(" ASC")
		} else {
			b.WriteString(" DESC")
		}
	})
}

// cursorSpec restates the ordering columns as the cursor shape the
// keyset guard is written against.
//
// The ORDER BY these columns render carries no NULLS clause, so SQLite
// applies its per-direction default, and NullsDefault is what
// [nullsFirst] resolves against the same default. The two therefore
// cannot drift.
func cursorSpec(orderBys []OrderingColumn) CursorSpec {
	keys := make([]OrderKey, len(orderBys))
	for i, o := range orderBys {
		keys[i] = OrderKey{Col: o.col, Desc: !o.asc}
	}
	return CursorSpec{Keys: keys}
}

// cursorGuard builds the WHERE predicate that moves past the cursor.
//
// It is [keysetWhere], the guard the SELECT builder's AfterCursor uses,
// rather than a second row comparison written here. The row comparison
// this used to render — (c1, c2) > (?, ?) — is not NULL-aware, and a
// nullable ordering column is the ordinary case rather than an exotic
// one: a page whose last row held a NULL rendered a comparison against
// NULL, which matches nothing, so the walk reported no further rows and
// stopped short of every row behind that NULL with nothing anywhere
// saying why. See [keysetStrict] for what replaces it.
func cursorGuard(orderBys []OrderingColumn, cursor string) (drops.Expression, error) {
	vals, err := Cursor(cursor).Decode()
	if err != nil {
		return nil, fmt.Errorf("drops/sqlite: invalid cursor: %w", err)
	}
	if len(vals) != len(orderBys) {
		return nil, fmt.Errorf("drops/sqlite: cursor has %d value(s), OrderBy has %d column(s)", len(vals), len(orderBys))
	}
	return keysetWhere(cursorSpec(orderBys), vals, true), nil
}

// encodeCursor extracts the ordering-column values from row and hands
// them to [EncodeCursor], which is the encoding [Cursor.Decode] and the
// keyset guard both read.
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
		var idx []int
		for _, cf := range e.colFields {
			if cf.col == o.col {
				idx = cf.field
				break
			}
		}
		if idx == nil {
			return "", fmt.Errorf("drops/sqlite: Page.OrderBy column %q has no matching struct field", o.col.Name())
		}
		vals[i] = v.FieldByIndex(idx).Interface()
	}
	cur, err := EncodeCursor(cursorSpec(orderBys), vals...)
	if err != nil {
		return "", err
	}
	return string(cur), nil
}
