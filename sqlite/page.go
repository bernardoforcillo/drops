package sqlite

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/gob"
	"errors"
	"fmt"
	"reflect"

	"github.com/bernardoforcillo/drops"
)

// Page is the typed result of a cursor-based pagination. NextCursor is
// empty when no further rows exist; HasMore short-circuits the presence
// check. It mirrors drops/pg's Page[T] so pagination code ports across
// dialects with a package swap.
type Page[T any] struct {
	Items      []T
	NextCursor string
	HasMore    bool
}

// PageBuilder composes a cursor-paginated query. It keeps cursor
// encoding/decoding internal — callers never construct or inspect cursors
// directly.
//
// Cursors are opaque, URL-safe base64 strings whose payload is a
// gob-encoded slice of the ordering columns' values. A cursor stays valid
// as long as the OrderBy spec doesn't change between calls.
//
// Keyset (a.k.a. seek) pagination is O(limit) regardless of how deep the
// caller has paged — unlike OFFSET, which SQLite must still walk past. Use
// EntityQuery.Offset for small offsets; use Page for deep, stable
// iteration.
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

// Asc returns an OrderingColumn for c sorted ascending. Accepts either
// *Column or *Col[T] via ColRef.
func Asc(c ColRef) OrderingColumn { return OrderingColumn{col: c.col(), asc: true} }

// Desc returns an OrderingColumn for c sorted descending.
func Desc(c ColRef) OrderingColumn { return OrderingColumn{col: c.col(), asc: false} }

// Page returns a cursor-paginated builder for this entity. The default
// limit is 50; override with Limit.
//
//	p, err := UserEntity.Page(db).
//	    OrderBy(sqlite.Asc(UserID)).
//	    Limit(20).
//	    After(prevCursor).
//	    All(ctx)
//	if p.HasMore {
//	    // request next page with p.NextCursor
//	}
func (e *Entity[T]) Page(db *DB) *PageBuilder[T] {
	return &PageBuilder[T]{e: e, db: db, limit: 50}
}

// OrderBy fixes the cursor's stability axis. At least one column is
// required; the last column should be unique (typically the PK) so every
// row has a distinct cursor.
func (p *PageBuilder[T]) OrderBy(cols ...OrderingColumn) *PageBuilder[T] {
	p.orderBys = append(p.orderBys, cols...)
	return p
}

// Where appends predicates joined by AND. Composes with the cursor guard
// so additional filters narrow the page set.
func (p *PageBuilder[T]) Where(preds ...drops.Expression) *PageBuilder[T] {
	p.wheres = append(p.wheres, preds...)
	return p
}

// After resumes iteration after the supplied cursor. Pass the empty string
// for the first page.
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
		return nil, errors.New("drops/sqlite: Page requires OrderBy(...)")
	}

	sel := p.db.Select(p.e.selectCols()...).From(p.e.table)
	for _, w := range p.wheres {
		sel.Where(w)
	}
	// Apply the cursor guard, if any.
	if p.after != "" {
		guard, err := cursorGuard(p.orderBys, p.after)
		if err != nil {
			return nil, err
		}
		sel.Where(guard)
	}
	// Stable ordering — every OrderingColumn renders into the ORDER BY.
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
		last := rows[len(rows)-1]
		cur, err := encodeCursor(p.e, p.orderBys, last)
		if err != nil {
			return nil, err
		}
		out.NextCursor = cur
	}
	return out, nil
}

// orderingExpr renders an OrderingColumn into the ORDER BY form.
func orderingExpr(o OrderingColumn) drops.Expression {
	if o.asc {
		return o.col.Asc()
	}
	return o.col.Desc()
}

// cursorGuard builds the WHERE predicate that moves past the supplied
// cursor.
//
// Single/homogeneous form: WHERE (c1, c2) > (?, ?) — SQLite supports
// row-value comparison (>= 3.15, 2016) and evaluates it lexicographically.
// Mixed asc/desc directions fall back to the explicit tie-break
// disjunction so the comparison stays well-defined.
func cursorGuard(orderBys []OrderingColumn, cursor string) (drops.Expression, error) {
	vals, err := decodeCursor(cursor)
	if err != nil {
		return nil, fmt.Errorf("drops/sqlite: invalid cursor: %w", err)
	}
	if len(vals) != len(orderBys) {
		return nil, fmt.Errorf("drops/sqlite: cursor has %d value(s), OrderBy has %d column(s)", len(vals), len(orderBys))
	}
	allAsc, allDesc := true, true
	for _, o := range orderBys {
		if o.asc {
			allDesc = false
		} else {
			allAsc = false
		}
	}
	if allAsc || allDesc {
		// Row-comparison form.
		op := ">"
		if allDesc {
			op = "<"
		}
		return drops.ExprFunc(func(b *drops.Builder) {
			b.WriteByte('(')
			for i, o := range orderBys {
				if i > 0 {
					b.WriteString(", ")
				}
				o.col.WriteSQL(b)
			}
			b.WriteString(") ")
			b.WriteString(op)
			b.WriteString(" (")
			for i, v := range vals {
				if i > 0 {
					b.WriteString(", ")
				}
				b.AddArg(v)
			}
			b.WriteByte(')')
		}), nil
	}
	// Mixed-direction fallback: tie-break disjunction. For c1 ASC, c2 DESC:
	//
	//   c1 > v1 OR (c1 = v1 AND c2 < v2)
	//
	// Generalises N-wise via cumulative-equality prefixes.
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		for i := range orderBys {
			if i > 0 {
				b.WriteString(" OR ")
			}
			b.WriteByte('(')
			for j := 0; j < i; j++ {
				if j > 0 {
					b.WriteString(" AND ")
				}
				orderBys[j].col.WriteSQL(b)
				b.WriteString(" = ")
				b.AddArg(vals[j])
			}
			if i > 0 {
				b.WriteString(" AND ")
			}
			orderBys[i].col.WriteSQL(b)
			if orderBys[i].asc {
				b.WriteString(" > ")
			} else {
				b.WriteString(" < ")
			}
			b.AddArg(vals[i])
			b.WriteByte(')')
		}
		b.WriteByte(')')
	}), nil
}

// encodeCursor extracts the ordering-column values from the last row and
// gob-encodes them inside a URL-safe base64 string.
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
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(vals); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf.Bytes()), nil
}

// decodeCursor is the inverse of encodeCursor.
func decodeCursor(s string) ([]any, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	var vals []any
	if err := gob.NewDecoder(bytes.NewReader(raw)).Decode(&vals); err != nil {
		return nil, err
	}
	return vals, nil
}
