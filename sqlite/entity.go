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

	// Optional cross-cutting wiring; nil unless opted into.
	audit *auditWiring // WithAudit (audit.go)
	guard Guard        // AuthorizeWith (authz.go)
	cache *EntityCache // WithCache (cache.go)
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
// if absent. Applies the tenant scope and the authorization guard when
// configured, and reads through the cache when one is attached and no
// scope/guard narrows the query.
func (e *Entity[T]) Get(db *DB, ctx context.Context, id any) (T, error) {
	var out T
	tenantPred, err := e.tenantPredicate(ctx)
	if err != nil {
		return out, err
	}
	guardPred, err := e.guardPredicate(ctx)
	if err != nil {
		return out, err
	}
	if e.cache != nil && tenantPred == nil && guardPred == nil {
		return e.getCached(db, ctx, id)
	}
	sel := db.Select(e.selectCols()...).From(e.table).Where(cmp(e.pk, "=", id))
	if tenantPred != nil {
		sel.Where(tenantPred)
	}
	if guardPred != nil {
		sel.Where(guardPred)
	}
	err = sel.One(ctx, &out)
	return out, err
}

// getCached is the cache-aware implementation of Get. Concurrent misses
// for the same key collapse to one DB read via the single-flight group.
func (e *Entity[T]) getCached(db *DB, ctx context.Context, id any) (T, error) {
	var out T
	key := e.pkKey(id)
	if hit, err := e.cache.readPK(ctx, key, &out); err == nil && hit {
		return out, nil
	}
	v, err := e.cache.sf.do(key, func() (any, error) {
		var t T
		if hit, err := e.cache.readPK(ctx, key, &t); err == nil && hit {
			return t, nil
		}
		sel := db.Select(e.selectCols()...).From(e.table).Where(cmp(e.pk, "=", id))
		if serr := sel.One(ctx, &t); serr != nil {
			return nil, serr
		}
		_ = e.cache.writeKey(ctx, key, t)
		return t, nil
	})
	if err != nil {
		return out, err
	}
	return v.(T), nil
}

// Create inserts r. Stamps the tenant (when scoped), writes an audit row
// in the same transaction (when audited), and populates the cache.
func (e *Entity[T]) Create(db *DB, ctx context.Context, r *T) error {
	if err := e.stampTenant(ctx, r); err != nil {
		return err
	}
	do := func(tx *DB) error {
		vals := e.bindings(reflect.ValueOf(r).Elem(), false)
		if _, err := tx.Insert(e.table).Values(vals...).Exec(ctx); err != nil {
			return err
		}
		return e.recordAudit(tx, ctx, "create", r, e.pkValue(r))
	}
	var err error
	if e.audit != nil {
		err = db.InTx(ctx, do)
	} else {
		err = do(db)
	}
	if err == nil && e.cache != nil {
		_ = e.cache.writeKey(ctx, e.pkKey(e.pkValue(r)), *r)
	}
	return err
}

// CreateMany inserts every row in a single multi-row INSERT. When the
// entity is tenant-scoped the ctx tenant is stamped onto each row first.
// Autogenerated PKs are not read back (batch path).
func (e *Entity[T]) CreateMany(db *DB, ctx context.Context, rows []T) (drops.Result, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	ins := db.Insert(e.table)
	for i := range rows {
		if err := e.stampTenant(ctx, &rows[i]); err != nil {
			return nil, err
		}
		vals := e.bindings(reflect.ValueOf(&rows[i]).Elem(), false)
		ins.Values(vals...)
	}
	return ins.Exec(ctx)
}

// Update writes every non-PK column of r, matched by primary key. Applies
// the tenant scope and authorization guard, records an audit row in the
// same transaction (when audited), and refreshes the cache.
func (e *Entity[T]) Update(db *DB, ctx context.Context, r *T) error {
	rv := reflect.ValueOf(r).Elem()
	tenantPred, err := e.tenantPredicate(ctx)
	if err != nil {
		return err
	}
	guardPred, err := e.guardPredicate(ctx)
	if err != nil {
		return err
	}
	sets := e.bindings(rv, true)
	pkVal := rv.FieldByIndex(e.pkField).Interface()
	do := func(tx *DB) error {
		upd := tx.Update(e.table).Set(sets...).Where(cmp(e.pk, "=", pkVal))
		if tenantPred != nil {
			upd.Where(tenantPred)
		}
		if guardPred != nil {
			upd.Where(guardPred)
		}
		if _, err := upd.Exec(ctx); err != nil {
			return err
		}
		return e.recordAudit(tx, ctx, "update", r, pkVal)
	}
	if e.audit != nil {
		err = db.InTx(ctx, do)
	} else {
		err = do(db)
	}
	if err == nil && e.cache != nil {
		_ = e.cache.writeKey(ctx, e.pkKey(pkVal), *r)
	}
	return err
}

// Delete removes the row whose primary key equals id. Applies the tenant
// scope and authorization guard, records an audit row in the same
// transaction (when audited), and invalidates the cache entry.
func (e *Entity[T]) Delete(db *DB, ctx context.Context, id any) (drops.Result, error) {
	tenantPred, err := e.tenantPredicate(ctx)
	if err != nil {
		return nil, err
	}
	guardPred, err := e.guardPredicate(ctx)
	if err != nil {
		return nil, err
	}
	var res drops.Result
	do := func(tx *DB) error {
		del := tx.Delete(e.table).Where(cmp(e.pk, "=", id))
		if tenantPred != nil {
			del.Where(tenantPred)
		}
		if guardPred != nil {
			del.Where(guardPred)
		}
		r, derr := del.Exec(ctx)
		if derr != nil {
			return derr
		}
		res = r
		return e.recordAudit(tx, ctx, "delete", nil, id)
	}
	if e.audit != nil {
		err = db.InTx(ctx, do)
	} else {
		err = do(db)
	}
	if err == nil {
		e.invalidatePK(ctx, id)
	}
	return res, err
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
	scopesApplied bool
}

// applyScopes AND-s the ctx tenant predicate and authorization guard onto
// the query the first time it runs. Returns ErrTenantMissing /
// ErrSubjectMissing when a scope/guard is configured but ctx lacks the
// needed value.
func (q *EntityQuery[T]) applyScopes(ctx context.Context) error {
	if q.scopesApplied {
		return nil
	}
	tenantPred, err := q.e.tenantPredicate(ctx)
	if err != nil {
		return err
	}
	if tenantPred != nil {
		q.sb.Where(tenantPred)
	}
	guardPred, err := q.e.guardPredicate(ctx)
	if err != nil {
		return err
	}
	if guardPred != nil {
		q.sb.Where(guardPred)
	}
	q.scopesApplied = true
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
	if err := q.applyScopes(ctx); err != nil {
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
	if err := q.applyScopes(ctx); err != nil {
		return out, err
	}
	err := q.sb.Limit(1).One(ctx, &out)
	return out, err
}
