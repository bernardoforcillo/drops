package sqlite

import (
	"context"
	"fmt"
	"reflect"

	"github.com/bernardoforcillo/drops"
)

// Entity is the typed CRUD layer over a Table, mirroring drops/pg's
// Entity[T]. It precomputes the column↔struct-field mapping for T and
// offers Get / Create / Update / Delete plus a fluent Query. T must be
// a struct with a field bound to each column (by `drop:"col"` tag, or
// by field name / camelCase), and the table must have a single-column
// primary key.
type Entity[T any] struct {
	table     *Table
	pk        *Column
	pkField   []int
	colFields []entityColField

	// Tenant scoping (see tenant.go). tenantCol is nil unless
	// ScopeByTenant was called.
	tenantCol   *Column
	tenantField []int
}

type entityColField struct {
	col   *Column
	field []int
}

// NewEntity builds the entity, panicking on misconfiguration (schemas
// are declared at startup, so bad config should fail loudly there).
func NewEntity[T any](t *Table) *Entity[T] {
	var zero T
	rt := reflect.TypeOf(zero)
	for rt != nil && rt.Kind() == reflect.Ptr {
		rt = rt.Elem()
	}
	if rt == nil || rt.Kind() != reflect.Struct {
		panic("drops/sqlite: NewEntity requires T to be a struct")
	}
	fields := drops.StructFields(rt)

	var pk *Column
	for _, c := range t.columns {
		if c.primary {
			if pk != nil {
				panic(fmt.Sprintf("drops/sqlite: NewEntity[%s]: table %q has more than one PRIMARY KEY column; Entity needs a single-column PK", rt.Name(), t.name))
			}
			pk = c
		}
	}
	if pk == nil {
		panic(fmt.Sprintf("drops/sqlite: NewEntity[%s]: table %q has no single-column PRIMARY KEY", rt.Name(), t.name))
	}
	pkField, ok := fields[pk.name]
	if !ok {
		panic(fmt.Sprintf("drops/sqlite: NewEntity[%s]: no struct field bound to PK column %q", rt.Name(), pk.name))
	}

	colFields := make([]entityColField, 0, len(t.columns))
	for _, c := range t.columns {
		idx, ok := fields[c.name]
		if !ok {
			continue
		}
		colFields = append(colFields, entityColField{col: c, field: idx})
	}
	return &Entity[T]{table: t, pk: pk, pkField: pkField, colFields: colFields}
}

// Table returns the entity's table.
func (e *Entity[T]) Table() *Table { return e.table }

// PK returns the primary-key column.
func (e *Entity[T]) PK() *Column { return e.pk }

// selectCols renders every mapped column as a projection expression.
func (e *Entity[T]) selectCols() []drops.Expression {
	cols := make([]drops.Expression, len(e.colFields))
	for i, cf := range e.colFields {
		cols[i] = cf.col
	}
	return cols
}

// Get fetches the row whose primary key equals id, returning ErrNoRows
// if absent. When the entity is tenant-scoped the ctx tenant is AND-ed
// into the predicate (see ScopeByTenant).
func (e *Entity[T]) Get(db *DB, ctx context.Context, id any) (T, error) {
	var out T
	tenantPred, err := e.tenantPredicate(ctx)
	if err != nil {
		return out, err
	}
	sel := db.Select(e.selectCols()...).From(e.table).Where(cmp(e.pk, "=", id))
	if tenantPred != nil {
		sel.Where(tenantPred)
	}
	err = sel.One(ctx, &out)
	return out, err
}

// Create inserts r. When the entity is tenant-scoped the ctx tenant is
// stamped onto r first (rejecting a row that carries a different one).
func (e *Entity[T]) Create(db *DB, ctx context.Context, r *T) error {
	if err := e.stampTenant(ctx, r); err != nil {
		return err
	}
	vals := e.bindings(reflect.ValueOf(r).Elem(), false)
	_, err := db.Insert(e.table).Values(vals...).Exec(ctx)
	return err
}

// Update writes every non-PK column of r, matched by primary key (and
// the ctx tenant when the entity is tenant-scoped).
func (e *Entity[T]) Update(db *DB, ctx context.Context, r *T) error {
	rv := reflect.ValueOf(r).Elem()
	tenantPred, err := e.tenantPredicate(ctx)
	if err != nil {
		return err
	}
	sets := e.bindings(rv, true)
	pkVal := rv.FieldByIndex(e.pkField).Interface()
	upd := db.Update(e.table).Set(sets...).Where(cmp(e.pk, "=", pkVal))
	if tenantPred != nil {
		upd.Where(tenantPred)
	}
	_, err = upd.Exec(ctx)
	return err
}

// Delete removes the row whose primary key equals id (scoped to the ctx
// tenant when the entity is tenant-scoped).
func (e *Entity[T]) Delete(db *DB, ctx context.Context, id any) (drops.Result, error) {
	tenantPred, err := e.tenantPredicate(ctx)
	if err != nil {
		return nil, err
	}
	del := db.Delete(e.table).Where(cmp(e.pk, "=", id))
	if tenantPred != nil {
		del.Where(tenantPred)
	}
	return del.Exec(ctx)
}

// bindings builds the column bindings from r. When skipPK is true the
// primary-key column is omitted (for UPDATE SET lists).
func (e *Entity[T]) bindings(rv reflect.Value, skipPK bool) []ColumnValue {
	out := make([]ColumnValue, 0, len(e.colFields))
	for _, cf := range e.colFields {
		if skipPK && cf.col == e.pk {
			continue
		}
		out = append(out, columnValue{col: cf.col, val: rv.FieldByIndex(cf.field).Interface()})
	}
	return out
}

// Query begins a fluent, entity-typed query. When the entity is
// tenant-scoped the caller must pass a tenant-carrying ctx to All/One
// (via WithTenant); the tenant predicate is applied at execution time.
func (e *Entity[T]) Query(db *DB) *EntityQuery[T] {
	return &EntityQuery[T]{e: e, sb: db.Select(e.selectCols()...).From(e.table)}
}

// EntityQuery is a typed wrapper over SelectBuilder that returns []T / T.
type EntityQuery[T any] struct {
	e             *Entity[T]
	sb            *SelectBuilder
	tenantApplied bool
}

// applyTenant AND-s the ctx tenant predicate onto the query the first
// time it runs. Returns ErrTenantMissing when the entity is scoped but
// ctx carries no tenant.
func (q *EntityQuery[T]) applyTenant(ctx context.Context) error {
	if q.tenantApplied || q.e.tenantCol == nil {
		return nil
	}
	pred, err := q.e.tenantPredicate(ctx)
	if err != nil {
		return err
	}
	if pred != nil {
		q.sb.Where(pred)
	}
	q.tenantApplied = true
	return nil
}

// Where AND-s predicates onto the query.
func (q *EntityQuery[T]) Where(preds ...drops.Expression) *EntityQuery[T] {
	q.sb.Where(preds...)
	return q
}

// OrderBy sets ORDER BY expressions.
func (q *EntityQuery[T]) OrderBy(exprs ...drops.Expression) *EntityQuery[T] {
	q.sb.OrderBy(exprs...)
	return q
}

// Limit / Offset bound the result window.
func (q *EntityQuery[T]) Limit(n int64) *EntityQuery[T]  { q.sb.Limit(n); return q }
func (q *EntityQuery[T]) Offset(n int64) *EntityQuery[T] { q.sb.Offset(n); return q }

// All executes and returns every matching row.
func (q *EntityQuery[T]) All(ctx context.Context) ([]T, error) {
	if err := q.applyTenant(ctx); err != nil {
		return nil, err
	}
	var out []T
	if err := q.sb.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// One executes and returns the first row, or ErrNoRows.
func (q *EntityQuery[T]) One(ctx context.Context) (T, error) {
	var out T
	if err := q.applyTenant(ctx); err != nil {
		return out, err
	}
	err := q.sb.Limit(1).One(ctx, &out)
	return out, err
}
