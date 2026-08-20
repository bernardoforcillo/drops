package mysql

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/bernardoforcillo/drops"
)

// Page is the typed result of a cursor-based pagination. NextCursor is
// empty when no further rows exist; HasMore short-circuits the
// presence check.
type Page[T any] struct {
	Items      []T
	NextCursor string
	HasMore    bool
}

// PageBuilder composes a cursor-paginated query. It exists to keep the
// cursor encoding and decoding internal — callers never construct or
// inspect cursors directly.
//
// The token is the same opaque, backend-stamped string [EncodeCursor]
// produces, over the same ordering fingerprint. PostgreSQL's Page
// builder has a private gob format of its own; here one format serves
// both entry points, so a cursor handed out by Page can be spent on a
// hand-built SELECT over the same keys, and one taken from another
// dialect's store is rejected either way.
type PageBuilder[T any] struct {
	e        *Entity[T]
	db       *DB
	orderBys []OrderingColumn
	wheres   []drops.Expression
	after    string
	limit    int

	allowNonUnique bool
}

// OrderingColumn pairs a *Column with its sort direction. Build one
// with Asc / Desc.
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
//	page, err := UserEntity.Page(db).
//	    OrderBy(mysql.Asc(UserID)).
//	    Limit(20).
//	    After(prevCursor).
//	    All(ctx)
//	if page.HasMore {
//	    // request the next page with page.NextCursor
//	}
func (e *Entity[T]) Page(db *DB) *PageBuilder[T] {
	return &PageBuilder[T]{e: e, db: db, limit: 50}
}

// OrderBy fixes the cursor's stability axis. At least one column is
// required, and the last one must be unique and NOT NULL — see
// [NewCursorSpec] for what happens when it is not, and why MySQL's
// collations make that more than a formality.
func (p *PageBuilder[T]) OrderBy(cols ...OrderingColumn) *PageBuilder[T] {
	p.orderBys = append(p.orderBys, cols...)
	return p
}

// AllowNonUniqueKey waives the tiebreaker check for this walk, with
// the consequences [CursorSpec.AllowNonUniqueKey] describes.
func (p *PageBuilder[T]) AllowNonUniqueKey() *PageBuilder[T] {
	p.allowNonUnique = true
	return p
}

// Where appends predicates joined by AND. Composes with the cursor
// guard so additional filters narrow the page set.
func (p *PageBuilder[T]) Where(preds ...drops.Expression) *PageBuilder[T] {
	p.wheres = append(p.wheres, preds...)
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
//
// Unlike the chained SelectBuilder form, this one has an error to
// return, so a spec or a cursor it cannot use comes back as that error
// rather than as an empty page.
func (p *PageBuilder[T]) All(ctx context.Context) (*Page[T], error) {
	if len(p.orderBys) == 0 {
		return nil, errors.New("drops/mysql: Page requires OrderBy(...)")
	}
	spec := p.rebindSpec(p.spec())
	if err := spec.validate(); err != nil {
		return nil, err
	}
	// Resolve the ordering columns to struct fields before the query
	// rather than at the first page boundary. A cursor is built out of
	// the last row's field values, so a column with no field could
	// never produce one — and without this the walk would look healthy
	// until it grew past one page, which is the point at which nobody
	// is watching.
	fields, err := p.fieldIndexes(spec)
	if err != nil {
		return nil, err
	}

	sel := p.db.Select(p.e.selectCols()...).From(p.e.table)
	for _, w := range p.wheres {
		sel.Where(w)
	}
	if p.after != "" {
		values, err := Cursor(p.after).decodeFor(spec)
		if err != nil {
			return nil, err
		}
		sel.Where(keysetWhere(spec, values, true))
	}
	for _, k := range spec.Keys {
		sel.OrderBy(orderKeyExpr(k))
	}
	// Fetch one extra row to detect HasMore without a follow-up COUNT
	// or a re-query.
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
		cur, err := encodePageCursor(spec, fields, rows[len(rows)-1])
		if err != nil {
			return nil, err
		}
		out.NextCursor = string(cur)
	}
	return out, nil
}

// spec turns the builder's ordering into the CursorSpec that drives
// the guard, the ORDER BY and the cursor's fingerprint.
func (p *PageBuilder[T]) spec() CursorSpec {
	keys := make([]OrderKey, len(p.orderBys))
	for i, o := range p.orderBys {
		keys[i] = OrderKey{Col: o.col, Desc: !o.asc}
	}
	spec := CursorSpec{Keys: keys}
	if p.allowNonUnique {
		spec = spec.AllowNonUniqueKey()
	}
	return spec
}

// rebindSpec restates every ordering key with the handle this
// entity's own table hands out.
//
// [Asc] and [Desc] are free functions, so an ordering column arrives
// as whichever handle the caller happened to hold, and it is rendered
// twice — into the ORDER BY and into the cursor guard's comparison. A
// handle from another instance of the table qualifies with a relation
// the SELECT does not name, and MySQL answers 1054: an entity on an
// alias paged by the package-level column, or an entity on the base
// table paged by an alias handle, both fail. The column meant is the
// same one either way — Column.key says so — so the fix is to page by
// the handle the entity queries.
//
// A key the entity has no column for is left as it came; fieldIndexes
// is where that turns into an error, so there is one place that
// reports it.
//
// The restatement cannot change which cursors a walk accepts:
// CursorSpec.fingerprint identifies a column by name, so a token
// stamped under one handle stays spendable under the other, and every
// token already in the wild stays valid.
func (p *PageBuilder[T]) rebindSpec(spec CursorSpec) CursorSpec {
	keys := make([]OrderKey, len(spec.Keys))
	for i, k := range spec.Keys {
		keys[i] = k
		if k.Col == nil || k.Col.col() == nil {
			continue
		}
		for _, cf := range p.e.colFields {
			if cf.col.key() == k.Col.col().key() {
				keys[i].Col = cf.col
				break
			}
		}
	}
	spec.Keys = keys
	return spec
}

// fieldIndexes pairs each ordering key with the entity field bound to
// it. Columns are matched by name rather than by pointer, so a handle
// reached through an aliased table resolves to the same field.
func (p *PageBuilder[T]) fieldIndexes(spec CursorSpec) ([][]int, error) {
	out := make([][]int, len(spec.Keys))
	for i, k := range spec.Keys {
		col := k.Col.col()
		for _, cf := range p.e.colFields {
			if cursorColKey(cf.col) == cursorColKey(col) {
				out[i] = cf.field
				break
			}
		}
		if out[i] == nil {
			return nil, fmt.Errorf("drops/mysql: Page.OrderBy column %q has no matching struct field", col.Name())
		}
	}
	return out, nil
}

// encodePageCursor extracts the ordering columns' values from the last
// row of the page and stamps them into a cursor.
func encodePageCursor[T any](spec CursorSpec, fields [][]int, row T) (Cursor, error) {
	v := reflect.ValueOf(&row).Elem()
	vals := make([]any, len(fields))
	for i, idx := range fields {
		vals[i] = v.FieldByIndex(idx).Interface()
	}
	return EncodeCursor(spec, vals...)
}
