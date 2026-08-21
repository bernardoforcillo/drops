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
// check.
type Page[T any] struct {
	Items      []T
	NextCursor string
	HasMore    bool
}

// PageBuilder composes a cursor-paginated query, keeping the cursor
// encoding/decoding internal — callers never construct or inspect
// cursors directly. Cursors are opaque, URL-safe base64 strings whose
// payload is a gob-encoded slice of the ordering columns' values,
// stable as long as the OrderBy spec doesn't change between calls.
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

// Where appends predicates joined by AND, composing with the cursor
// guard so filters narrow the page set.
func (p *PageBuilder[T]) Where(preds ...drops.Expression) *PageBuilder[T] {
	p.wheres = append(p.wheres, preds...)
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

// All runs the query and returns the page.
//
// The tenant scope and the authorization guard are applied by the
// executor, from the filters the table carries — see
// [Table.ContextFilter]. This method used to inject the tenant
// predicate and not the guard, which is the failure mode a per-call-site
// scope always has: it covers the sites somebody listed, and a page over
// a guarded entity read rows the subject was not entitled to.
func (p *PageBuilder[T]) All(ctx context.Context) (*Page[T], error) {
	if len(p.orderBys) == 0 {
		return nil, errors.New("drops/sqlite: Page requires OrderBy(...)")
	}
	sel := p.db.Select(p.e.selectCols()...).From(p.e.table)
	for _, w := range p.wheres {
		sel.Where(w)
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

// cursorGuard builds the WHERE predicate that moves past the cursor.
// Homogeneous directions use the row-comparison form (SQLite supports
// row values since 3.15); mixed directions fall back to the tie-break
// disjunction.
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
	// Mixed-direction fallback: cumulative-equality-prefix disjunction.
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

// encodeCursor extracts the ordering-column values from row and
// gob-encodes them inside a URL-safe base64 string.
func encodeCursor[T any](e *Entity[T], orderBys []OrderingColumn, row T) (string, error) {
	v := reflect.ValueOf(&row).Elem()
	vals := make([]any, len(orderBys))
	for i, o := range orderBys {
		var idx []int
		for _, cf := range e.colFields {
			if cf.col.key() == o.col.key() {
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
