package sqlite

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/internal/drift"
)

// Entity is the typed CRUD layer over a Table, mirroring drops/pg's
// Entity[T]. It precomputes the column↔struct-field mapping for T and
// offers Get / Create / Update / Delete plus a fluent Query. T must be
// a struct with a field bound to each column (by `drop:"col"` tag, or
// by field name / camelCase), and the table must have a single-column
// primary key.
type Entity[T any] struct {
	table *Table
	// pk / pkField describe the primary key when it is a single
	// column and are nil for a composite one; pks / pkFields are
	// always populated, in declaration order.
	pk        *Column
	pkField   []int
	pks       []*Column
	pkFields  [][]int
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
func NewEntity[T any](t *Table, opts ...EntityOption) *Entity[T] {
	var cfg entityConfig
	for _, o := range opts {
		o(&cfg)
	}

	var zero T
	rt := reflect.TypeOf(zero)
	for rt != nil && rt.Kind() == reflect.Ptr {
		rt = rt.Elem()
	}
	if rt == nil || rt.Kind() != reflect.Struct {
		panic("drops/sqlite: NewEntity requires T to be a struct")
	}
	fields := drops.StructFields(rt)

	var pks []*Column
	for _, c := range t.columns {
		if c.primary {
			pks = append(pks, c)
		}
	}
	if len(pks) == 0 {
		panic(fmt.Sprintf("drops/sqlite: NewEntity[%s]: table %q has no PRIMARY KEY", rt.Name(), t.name))
	}
	pkFields := make([][]int, len(pks))
	for i, c := range pks {
		idx, ok := fields[c.name]
		if !ok {
			panic(fmt.Sprintf("drops/sqlite: NewEntity[%s]: no struct field bound to PK column %q", rt.Name(), c.name))
		}
		pkFields[i] = idx
	}
	var pk *Column
	var pkField []int
	if len(pks) == 1 {
		pk, pkField = pks[0], pkFields[0]
	}

	colFields := make([]entityColField, 0, len(t.columns))
	for _, c := range t.columns {
		idx, ok := fields[c.name]
		if !ok {
			continue
		}
		colFields = append(colFields, entityColField{col: c, field: idx})
	}
	if err := checkDrift(rt, t, colFields, cfg); err != nil {
		panic(err.Error())
	}
	return &Entity[T]{table: t, pk: pk, pkField: pkField, pks: pks, pkFields: pkFields, colFields: colFields}
}

// EntityOption configures [NewEntity].
type EntityOption func(*entityConfig)

type entityConfig struct {
	allowUnmapped map[string]bool
	allowAny      bool
}

// AllowUnmappedColumns exempts the named columns from the check that
// every column has a struct field. Use it for columns the database
// owns and the application never writes; naming them keeps the check
// working for the rest.
func AllowUnmappedColumns(names ...string) EntityOption {
	return func(c *entityConfig) {
		if c.allowUnmapped == nil {
			c.allowUnmapped = map[string]bool{}
		}
		for _, n := range names {
			c.allowUnmapped[n] = true
		}
	}
}

// AllowAnyUnmappedColumn disables the check entirely, for migrating a
// codebase with too many gaps to name at once.
func AllowAnyUnmappedColumn() EntityOption {
	return func(c *entityConfig) { c.allowAny = true }
}

// checkDrift reports columns bound to no struct field — see
// [github.com/bernardoforcillo/drops/internal/drift].
func checkDrift(rt reflect.Type, t *Table, colFields []entityColField, cfg entityConfig) error {
	if cfg.allowAny {
		return nil
	}
	mapped := make(map[string]bool, len(colFields))
	bound := make(map[string]bool, len(colFields))
	for _, cf := range colFields {
		mapped[cf.col.name] = true
		bound[drift.FieldKey(cf.field)] = true
	}
	var missing []string
	for _, c := range t.columns {
		if mapped[c.name] || c.IsManaged() || cfg.allowUnmapped[c.name] {
			continue
		}
		missing = append(missing, c.name)
	}
	return drift.Report("drops/sqlite", rt.Name(), t.name, missing,
		drift.SpareFields(rt, bound), "sqlite.AllowUnmappedColumns")
}

// Table returns the entity's table.
func (e *Entity[T]) Table() *Table { return e.table }

// PK returns the primary-key column.
func (e *Entity[T]) PK() *Column { return e.pk }

// PKs returns the primary-key columns in declaration order.
func (e *Entity[T]) PKs() []*Column {
	out := make([]*Column, len(e.pks))
	copy(out, e.pks)
	return out
}

// ErrKeyArity is returned when a key is given the wrong number of
// values for the entity's primary key.
var ErrKeyArity = errors.New("drops/sqlite: wrong number of primary-key values")

// pkPredicate addresses one row by key. A count mismatch is an error
// rather than a partial match: half a composite key would silently
// address every row sharing that column.
func (e *Entity[T]) pkPredicate(key []any) (drops.Expression, error) {
	if len(key) != len(e.pks) {
		names := make([]string, len(e.pks))
		for i, c := range e.pks {
			names[i] = c.name
		}
		return nil, fmt.Errorf("%w: table %q has %d key column(s) (%s), got %d value(s)",
			ErrKeyArity, e.table.name, len(e.pks), strings.Join(names, ", "), len(key))
	}
	if len(key) == 1 {
		return cmp(e.pks[0], "=", key[0]), nil
	}
	preds := make([]drops.Expression, len(key))
	for i, c := range e.pks {
		preds[i] = cmp(c, "=", key[i])
	}
	return And(preds...), nil
}

// pkValuesOf reads the key out of a row, in PKs order.
func (e *Entity[T]) pkValuesOf(r *T) []any {
	v := reflect.ValueOf(r).Elem()
	out := make([]any, len(e.pkFields))
	for i, idx := range e.pkFields {
		out[i] = v.FieldByIndex(idx).Interface()
	}
	return out
}

// isKeyColumn reports whether c is part of the primary key.
func (e *Entity[T]) isKeyColumn(c *Column) bool {
	for _, k := range e.pks {
		if k == c {
			return true
		}
	}
	return false
}

// auditKey renders a key for the audit trail's single rowID column,
// joining a composite key rather than losing all but its first column.
func auditKey(values []any) any {
	if len(values) == 1 {
		return values[0]
	}
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = fmt.Sprintf("%v", v)
	}
	return strings.Join(parts, "|")
}

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
func (e *Entity[T]) Get(db *DB, ctx context.Context, key ...any) (T, error) {
	var out T
	pred, err := e.pkPredicate(key)
	if err != nil {
		return out, err
	}
	tenantPred, err := e.tenantPredicate(ctx)
	if err != nil {
		return out, err
	}
	guardPred, err := e.guardPredicate(ctx)
	if err != nil {
		return out, err
	}
	if e.cache != nil && tenantPred == nil && guardPred == nil {
		return e.getCached(db, ctx, key, pred)
	}
	sel := db.Select(e.selectCols()...).From(e.table).Where(pred)
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
func (e *Entity[T]) getCached(db *DB, ctx context.Context, pkValues []any, pred drops.Expression) (T, error) {
	var out T
	key := e.pkKey(pkValues)
	if hit, err := e.cache.readPK(ctx, key, &out); err == nil && hit {
		return out, nil
	}
	v, err := e.cache.sf.do(key, func() (any, error) {
		var t T
		if hit, err := e.cache.readPK(ctx, key, &t); err == nil && hit {
			return t, nil
		}
		sel := db.Select(e.selectCols()...).From(e.table).Where(pred)
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
		rv := reflect.ValueOf(r).Elem()
		vals := e.bindings(rv, false)
		ins := tx.Insert(e.table).Values(vals...)
		// SQLite has had RETURNING since 3.35, so a server-assigned
		// key comes back in the same statement rather than through a
		// second round trip. Without this the caller holds a row whose
		// key is still zero and cannot address what it just wrote.
		if err := e.insertReturningKey(tx, ctx, ins, rv); err != nil {
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
		_ = e.cache.writeKey(ctx, e.pkKey(e.pkValuesOf(r)), *r)
	}
	return err
}

// insertReturningKey runs the INSERT, reading server-assigned key
// columns back into the row.
//
// Only columns the caller left zero are read back: one the caller set
// is theirs, and overwriting it with what the server echoed would be
// indistinguishable from working until the two disagree.
func (e *Entity[T]) insertReturningKey(db *DB, ctx context.Context, ins *InsertBuilder, rv reflect.Value) error {
	var want []int
	for i, idx := range e.pkFields {
		if rv.FieldByIndex(idx).IsZero() {
			want = append(want, i)
		}
	}
	if len(want) == 0 {
		_, err := ins.Exec(ctx)
		return err
	}
	dests := make([]any, 0, len(want))
	for _, i := range want {
		ins.Returning(e.pks[i])
		dests = append(dests, rv.FieldByIndex(e.pkFields[i]).Addr().Interface())
	}
	sqlText, args := ins.ToSQL()
	rows, err := db.Query(ctx, sqlText, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return fmt.Errorf("drops/sqlite: INSERT ... RETURNING produced no row for table %q", e.table.name)
	}
	if err := rows.Scan(dests...); err != nil {
		return err
	}
	return rows.Err()
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
	pkVals := e.pkValuesOf(r)
	pred, err := e.pkPredicate(pkVals)
	if err != nil {
		return err
	}
	do := func(tx *DB) error {
		upd := tx.Update(e.table).Set(sets...).Where(pred)
		if tenantPred != nil {
			upd.Where(tenantPred)
		}
		if guardPred != nil {
			upd.Where(guardPred)
		}
		if _, err := upd.Exec(ctx); err != nil {
			return err
		}
		return e.recordAudit(tx, ctx, "update", r, auditKey(pkVals))
	}
	if e.audit != nil {
		err = db.InTx(ctx, do)
	} else {
		err = do(db)
	}
	if err == nil && e.cache != nil {
		_ = e.cache.writeKey(ctx, e.pkKey(pkVals), *r)
	}
	return err
}

// Delete removes the row whose primary key equals id. Applies the tenant
// scope and authorization guard, records an audit row in the same
// transaction (when audited), and invalidates the cache entry.
func (e *Entity[T]) Delete(db *DB, ctx context.Context, key ...any) (drops.Result, error) {
	pred, err := e.pkPredicate(key)
	if err != nil {
		return nil, err
	}
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
		del := tx.Delete(e.table).Where(pred)
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
		return e.recordAudit(tx, ctx, "delete", nil, auditKey(key))
	}
	if e.audit != nil {
		err = db.InTx(ctx, do)
	} else {
		err = do(db)
	}
	if err == nil {
		e.invalidatePK(ctx, key)
	}
	return res, err
}

// bindings builds the column bindings from r. When skipPK is true the
// primary-key column is omitted (for UPDATE SET lists).
// bindings extracts a row's column values.
//
// skipPK omits the key columns, which an UPDATE must not reassign. On
// the insert path a key column whose value is still zero is omitted
// too, so the server assigns it: binding the zero explicitly makes
// every row claim id 0, and the second insert fails on the primary
// key. A column with a DEFAULT is omitted for the same reason — the
// zero value the caller never set is not what they meant to store.
func (e *Entity[T]) bindings(rv reflect.Value, skipPK bool) []ColumnValue {
	out := make([]ColumnValue, 0, len(e.colFields))
	for _, cf := range e.colFields {
		if skipPK && e.isKeyColumn(cf.col) {
			continue
		}
		fv := rv.FieldByIndex(cf.field)
		if !skipPK && fv.IsZero() && (cf.col.autoInc || cf.col.hasDefault || e.isKeyColumn(cf.col)) {
			continue
		}
		out = append(out, columnValue{col: cf.col, val: fv.Interface()})
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
// Unscoped opts out of the table's DefaultFilter predicates for this
// query. Without it a soft-deleted row is unreachable through the
// entity at all, which makes an audit or a restore flow impossible to
// write.
func (q *EntityQuery[T]) Unscoped() *EntityQuery[T] { q.sb.Unscoped(); return q }

func (q *EntityQuery[T]) Limit(n int64) *EntityQuery[T]  { q.sb.Limit(n); return q }
func (q *EntityQuery[T]) Offset(n int64) *EntityQuery[T] { q.sb.Offset(n); return q }

// All executes and returns every matching row. When the entity has a
// cache attached, the result is read through it under a key derived
// from the rendered SQL and its arguments, matching drops/pg.
func (q *EntityQuery[T]) All(ctx context.Context) ([]T, error) {
	if err := q.applyScopes(ctx); err != nil {
		return nil, err
	}
	if q.e.cache != nil {
		return q.allCached(ctx)
	}
	var out []T
	if err := q.sb.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// One executes and returns the first row, or ErrNoRows. Cached the
// same way as All when the entity has a cache attached.
func (q *EntityQuery[T]) One(ctx context.Context) (T, error) {
	var out T
	if err := q.applyScopes(ctx); err != nil {
		return out, err
	}
	q.sb.Limit(1)
	if q.e.cache != nil {
		return q.oneCached(ctx)
	}
	err := q.sb.One(ctx, &out)
	return out, err
}

// allCached and oneCached read the rendered query through the entity
// cache. Both go through the single-flight group so a cold key under
// concurrent load issues one query rather than one per caller — the
// stampede protection the PK path already had.
func (q *EntityQuery[T]) allCached(ctx context.Context) ([]T, error) {
	sql, args := q.sb.ToSQL()
	key := queryKey(q.e.table.Name(), sql, args)
	var out []T
	if hit, err := q.e.cache.readPK(ctx, key, &out); err == nil && hit {
		return out, nil
	}
	v, err := q.e.cache.sf.do(key, func() (any, error) {
		var hits []T
		if hit, rErr := q.e.cache.readPK(ctx, key, &hits); rErr == nil && hit {
			return hits, nil
		}
		var rs []T
		if qErr := q.sb.All(ctx, &rs); qErr != nil {
			return rs, qErr
		}
		_ = q.e.cache.writeKey(ctx, key, rs)
		return rs, nil
	})
	if err != nil {
		return out, err
	}
	return v.([]T), nil
}

func (q *EntityQuery[T]) oneCached(ctx context.Context) (T, error) {
	sql, args := q.sb.ToSQL()
	key := queryKey(q.e.table.Name(), sql, args) + ":one"
	var out T
	if hit, err := q.e.cache.readPK(ctx, key, &out); err == nil && hit {
		return out, nil
	}
	v, err := q.e.cache.sf.do(key, func() (any, error) {
		var t T
		if hit, rErr := q.e.cache.readPK(ctx, key, &t); rErr == nil && hit {
			return t, nil
		}
		if qErr := q.sb.One(ctx, &t); qErr != nil {
			return t, qErr
		}
		_ = q.e.cache.writeKey(ctx, key, t)
		return t, nil
	})
	if err != nil {
		return out, err
	}
	return v.(T), nil
}
