package mysql

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/internal/drift"
)

// Entity binds a Go struct T to a Table and precomputes the
// column-to-field mapping the CRUD shortcuts need. Declare one once,
// alongside its table.
type Entity[T any] struct {
	table *Table

	// pk / pkField describe a single-column primary key and are nil
	// for a composite one; pks / pkFields are always populated, in
	// declaration order.
	pk        *Column
	pkField   []int
	pks       []*Column
	pkFields  [][]int
	colFields []entityColField

	// constraintFields maps an index or constraint name to the
	// struct field a violation of it is reported against. Filled by
	// MapConstraint; the names drops and MySQL generate are derived
	// rather than stored — see (*Entity[T]).FieldError.
	constraintFields map[string]string

	// Tenant scoping (see tenant.go). tenantCol is nil unless
	// ScopeByTenant was called; tenantField is the struct field it
	// binds, so a write can stamp the ctx tenant onto the row.
	tenantCol   *Column
	tenantField []int

	// Optional cross-cutting wiring; nil unless opted into.
	audit *auditWiring // WithAudit (audit.go)
	cache *EntityCache // WithCache (cache.go)

	// rowType is T with its pointers stripped — the type NewEntity
	// mapped the columns against. It is held rather than recomputed
	// because it is the key an entity's row-scope filters are
	// registered under: see rowScopeFilterKey, which needs the type's
	// import path and name to keep two entities over one table from
	// replacing each other's.
	rowType reflect.Type
}

type entityColField struct {
	col   *Column
	field []int
}

// EntityOption configures [NewEntity].
type EntityOption func(*entityConfig)

type entityConfig struct {
	allowUnmapped map[string]bool
	allowAny      bool
	allowNullable map[string]bool
	allowAnyNull  bool
}

// AllowUnmappedColumns exempts the named columns from the check that
// every column has a struct field — for columns the database owns and
// the application never writes.
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

// AllowAnyUnmappedColumn disables the check entirely.
func AllowAnyUnmappedColumn() EntityOption {
	return func(c *entityConfig) { c.allowAny = true }
}

// AllowNullableColumns exempts the named columns from the check that
// a column admitting NULL is bound to a field that can receive one.
//
// Use it where the database will never actually produce a NULL and
// the constraint cannot say so — a column another writer keeps
// populated, a view whose outer join can never miss. Naming the
// columns leaves the check working everywhere else.
func AllowNullableColumns(names ...string) EntityOption {
	return func(c *entityConfig) {
		if c.allowNullable == nil {
			c.allowNullable = map[string]bool{}
		}
		for _, n := range names {
			c.allowNullable[n] = true
		}
	}
}

// AllowAnyNullableColumn disables the nullability check entirely, for
// migrating a codebase with too many mismatches to fix at once;
// prefer [AllowNullableColumns], which keeps the check working for
// the columns you have not exempted.
func AllowAnyNullableColumn() EntityOption {
	return func(c *entityConfig) { c.allowAnyNull = true }
}

// ErrKeyArity is returned when a key carries the wrong number of
// values for the entity's primary key.
var ErrKeyArity = errors.New("drops/mysql: wrong number of primary-key values")

// ErrPKNotSet is returned by Update when the key fields are all zero.
var ErrPKNotSet = errors.New("drops/mysql: primary key field is the zero value")

// NewEntity builds the entity, panicking on misconfiguration —
// schemas are declared in package init blocks, so startup is where bad
// configuration should surface.
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
		panic("drops/mysql: NewEntity requires T to be a struct")
	}
	fields := drops.StructFields(rt)

	pks := t.PrimaryKeyColumns()
	if len(pks) == 0 {
		panic(fmt.Sprintf("drops/mysql: NewEntity[%s]: table %q has no PRIMARY KEY", rt.Name(), t.name))
	}
	pkFields := make([][]int, len(pks))
	for i, c := range pks {
		idx, ok := fields[c.name]
		if !ok {
			panic(fmt.Sprintf("drops/mysql: NewEntity[%s]: no struct field bound to PK column %q", rt.Name(), c.name))
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
	return &Entity[T]{
		table: t, pk: pk, pkField: pkField,
		pks: pks, pkFields: pkFields, colFields: colFields,
		rowType: rt,
	}
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
	if err := drift.Report("drops/mysql", rt.Name(), t.name, missing,
		drift.SpareFields(rt, bound), "mysql.AllowUnmappedColumns"); err != nil {
		return err
	}
	return checkNullability(rt, t, colFields, cfg)
}

// checkNullability reports columns that admit NULL bound to a field
// that cannot receive one.
//
// The mismatch is invisible to the compiler — a column's T is the
// type its comparisons take, and the scan destination is a field
// drops reaches only by reflection — and invisible at run time too,
// until the first row that happens to be NULL. NewEntity is the one
// place both types are in scope. It fires on whether the column
// admits NULL, not on whether it said so: a bare mysql.Text("bio") is
// exactly the shape that has been accepting NULLs nobody declared.
func checkNullability(rt reflect.Type, t *Table, colFields []entityColField, cfg entityConfig) error {
	if cfg.allowAnyNull {
		return nil
	}
	var bad []drift.NullMismatch
	for _, cf := range colFields {
		c := cf.col
		if !c.IsNullable() || cfg.allowNullable[c.Name()] {
			continue
		}
		ft := drift.FieldTypeAt(rt, cf.field)
		if ft == nil || drift.AcceptsNull(ft) {
			continue
		}
		bad = append(bad, drift.NullMismatch{
			Column:    c.Name(),
			Field:     drift.FieldPath(rt, cf.field),
			FieldType: ft.String(),
			Stated:    c.nullStated,
		})
	}
	return drift.ReportNullable("drops/mysql", rt.Name(), t.name, bad, "mysql.AllowNullableColumns")
}

// hasRowScope reports whether reading through this entity is narrowed
// by anything a cache key would have to account for: a tenant axis or a
// context filter registered on the table.
//
// drops/pg and drops/sqlite also ask about an authorisation guard here.
// This package has none, so there is nothing to ask — and when authz
// arrives, this is the line that has to grow with it, or a guarded read
// starts being served from a cache entry written for somebody else.
func (e *Entity[T]) hasRowScope() bool {
	return e.tenantCol != nil || e.table.hasContextFilters()
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

// Table returns the entity's table.
func (e *Entity[T]) Table() *Table { return e.table }

// PK returns the single primary-key column, or nil for a composite
// key.
func (e *Entity[T]) PK() *Column { return e.pk }

// PKs returns the primary-key columns in declaration order.
func (e *Entity[T]) PKs() []*Column {
	out := make([]*Column, len(e.pks))
	copy(out, e.pks)
	return out
}

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
	preds := make([]drops.Expression, len(key))
	for i, c := range e.pks {
		preds[i] = cmp(c, "=", key[i])
	}
	if len(preds) == 1 {
		return preds[0], nil
	}
	return And(preds...), nil
}

func (e *Entity[T]) pkValuesOf(r *T) []any {
	v := reflect.ValueOf(r).Elem()
	out := make([]any, len(e.pkFields))
	for i, idx := range e.pkFields {
		out[i] = v.FieldByIndex(idx).Interface()
	}
	return out
}

func (e *Entity[T]) pkIsZero(r *T) bool {
	v := reflect.ValueOf(r).Elem()
	for _, idx := range e.pkFields {
		if !v.FieldByIndex(idx).IsZero() {
			return false
		}
	}
	return true
}

// isKeyColumn reports whether c is one of the entity's key columns,
// through Column.key so a handle reached off an alias of the table
// answers the same as the declared one. Entity builds e.pks and
// e.colFields off the one *Table, so the two sides cannot disagree
// today — but Entity.PK and Entity.PKs hand those pointers out, and
// the first caller to route one back here would otherwise have an
// UPDATE reassign the primary key.
func (e *Entity[T]) isKeyColumn(c *Column) bool {
	for _, k := range e.pks {
		if k.key() == c.key() {
			return true
		}
	}
	return false
}

// selectCols returns the projection: the mapped columns, in order.
func (e *Entity[T]) selectCols() []drops.Expression {
	out := make([]drops.Expression, len(e.colFields))
	for i, cf := range e.colFields {
		out[i] = cf.col
	}
	return out
}

// Get fetches the row addressed by key, returning drops.ErrNoRows when
// none matches and [ErrKeyArity] when the value count is wrong.
//
//	u, err := UserEntity.Get(db, ctx, 42)
//	m, err := MembershipEntity.Get(db, ctx, orgID, userID)
func (e *Entity[T]) Get(db *DB, ctx context.Context, key ...any) (T, error) {
	var out T
	pred, err := e.pkPredicate(key)
	if err != nil {
		return out, err
	}
	// The tenant axis reaches the statement as a context filter on the
	// table and the executors resolve it — nothing injects it here. It
	// is also why a scoped entity does not read through the cache: the
	// PK namespace has no room for the scope, so an entry written for
	// one tenant would answer another's Get.
	if e.cache != nil && !e.hasRowScope() {
		return e.getCached(db, ctx, key, pred)
	}
	err = db.Select(e.selectCols()...).From(e.table).Where(pred).One(ctx, &out)
	return out, err
}

// getCached is the cache-aware implementation of Get. Concurrent misses
// for the same key collapse to one database read via the single-flight
// group.
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

// Query begins a typed SELECT over the entity's table.
func (e *Entity[T]) Query(db *DB) *EntityQuery[T] {
	return &EntityQuery[T]{e: e, sb: db.Select(e.selectCols()...).From(e.table)}
}

// EntityQuery is a typed wrapper over SelectBuilder returning []T / T.
type EntityQuery[T any] struct {
	e  *Entity[T]
	sb *SelectBuilder
}

func (q *EntityQuery[T]) Where(preds ...drops.Expression) *EntityQuery[T] {
	q.sb.Where(preds...)
	return q
}

func (q *EntityQuery[T]) OrderBy(exprs ...drops.Expression) *EntityQuery[T] {
	q.sb.OrderBy(exprs...)
	return q
}

func (q *EntityQuery[T]) Limit(n int64) *EntityQuery[T]  { q.sb.Limit(n); return q }
func (q *EntityQuery[T]) Offset(n int64) *EntityQuery[T] { q.sb.Offset(n); return q }

// Unscoped opts out of every global filter on the table — named and
// anonymous alike; the blunt instrument. See [SelectBuilder.Unscoped].
func (q *EntityQuery[T]) Unscoped() *EntityQuery[T] { q.sb.UnscopedDefaults(); return q }

// IgnoreFilters bypasses the named global filters and leaves every
// other one standing — see [SelectBuilder.IgnoreFilters]. It is the
// method to reach for on a table wearing more than one guard, where
// Unscoped would drop the ones this query still wants.
//
//	postEntity.Query(db).IgnoreFilters(mysql.FilterSoftDelete).All(ctx)
func (q *EntityQuery[T]) IgnoreFilters(names ...string) *EntityQuery[T] {
	q.sb.IgnoreFilters(names...)
	return q
}

// ToSQL renders the query without running it — the same statement All
// and One would send.
func (q *EntityQuery[T]) ToSQL() (string, []any) { return q.sb.ToSQL() }

// All returns every matching row.
func (q *EntityQuery[T]) All(ctx context.Context) ([]T, error) {
	if q.e.cache != nil {
		return q.allCached(ctx)
	}
	var out []T
	if err := q.sb.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// One returns the first matching row. Cached the same way as All when
// the entity has a cache attached.
func (q *EntityQuery[T]) One(ctx context.Context) (T, error) {
	q.sb.Limit(1)
	if q.e.cache != nil {
		return q.oneCached(ctx)
	}
	var out T
	err := q.sb.One(ctx, &out)
	return out, err
}

// allCached and oneCached read the rendered query through the entity
// cache. Both go through the single-flight group so a cold key under
// concurrent load issues one query rather than one per caller — the
// stampede protection the PK path already had.
//
// Unlike the PK path, these do NOT skip a scoped entity, and the reason
// is the key: it is taken from the RESOLVED statement, so the tenant
// that arrived as a context filter is already in it. Two tenants
// running the same query get two keys. The PK namespace has no such
// room, which is why hasRowScope keeps a scoped entity out of it.
func (q *EntityQuery[T]) allCached(ctx context.Context) ([]T, error) {
	sql, args, err := q.sb.ToSQLCtx(ctx)
	if err != nil {
		return nil, err
	}
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
	sql, args, err := q.sb.ToSQLCtx(ctx)
	if err != nil {
		var zero T
		return zero, err
	}
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

// Create inserts r.
//
// Where PostgreSQL reads a generated key back through RETURNING,
// MySQL has no such clause — so when the table has a single
// AUTO_INCREMENT key and r's key field is zero, Create writes the
// driver's LastInsertId back into r. A driver that does not expose one
// leaves the field alone rather than failing: the row is inserted
// either way, and silently reporting an id of 0 would be worse.
func (e *Entity[T]) Create(db *DB, ctx context.Context, r *T) error {
	// Stamped before the row is read into bindings, so the INSERT
	// carries the ctx tenant rather than whatever the caller left in
	// the struct — and refuses outright when the struct already
	// carries somebody else's. Without it the zero the caller never
	// set reaches the axis check as another tenant's value.
	if err := e.stampTenant(ctx, r); err != nil {
		return err
	}
	v := reflect.ValueOf(r).Elem()
	do := func(tx *DB) error {
		ins := tx.Insert(e.table)
		ins.Row(e.bindings(v, false)...)
		res, err := ins.Exec(ctx)
		if err != nil {
			return e.FieldError(err)
		}
		// Before the audit row, because the audit names the key and on
		// MySQL the key is whatever the server just assigned. Reading
		// it from the same transaction is also why LastInsertId is
		// trustworthy here: it answers per connection, and the
		// transaction pins one.
		e.applyGeneratedKey(v, res)
		return e.recordAudit(tx, ctx, "create", r, auditKey(e.pkValuesOf(r)))
	}
	var err error
	if e.audit != nil {
		err = db.InTx(ctx, do)
	} else {
		err = do(db)
	}
	if err == nil {
		// Through refreshPK rather than straight into the cache: the
		// PK namespace has no room for the scope, so a scoped entity
		// must not put a row there — and deletes whatever is under the
		// key instead. See refreshPK.
		e.refreshPK(ctx, e.pkValuesOf(r), *r)
	}
	return err
}

// CreateMany inserts every row in one multi-row INSERT. Generated keys
// are not read back: MySQL's LastInsertId reports only the first row's
// id, and inferring the rest assumes a contiguous block that
// innodb_autoinc_lock_mode=2 does not guarantee.
func (e *Entity[T]) CreateMany(db *DB, ctx context.Context, rows []T) (drops.Result, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	ins := db.Insert(e.table)
	for i := range rows {
		// Per row, because the stamp fills a field of each: a batch
		// stamped only on the first would bind values under the wrong
		// names for the rest — the INSERT column list is derived from
		// row zero.
		if err := e.stampTenant(ctx, &rows[i]); err != nil {
			return nil, err
		}
		ins.Row(e.bindings(reflect.ValueOf(&rows[i]).Elem(), false)...)
	}
	res, err := ins.Exec(ctx)
	return res, e.FieldError(err)
}

// UpsertMany inserts rows, updating the non-key columns of any that
// collide.
//
// The collision is on any unique index, not on the primary key alone —
// see [InsertBuilder.OnDuplicateKeyUpdate]. On a table whose only
// unique index is the primary key the two readings coincide; on one
// with a unique email they do not, and this will update the row that
// already owns the address.
func (e *Entity[T]) UpsertMany(db *DB, ctx context.Context, rows []T) (drops.Result, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	ins := db.Insert(e.table)
	for i := range rows {
		// Per row, because the stamp fills a field of each: a batch
		// stamped only on the first would bind values under the wrong
		// names for the rest — the INSERT column list is derived from
		// row zero.
		if err := e.stampTenant(ctx, &rows[i]); err != nil {
			return nil, err
		}
		ins.Row(e.bindings(reflect.ValueOf(&rows[i]).Elem(), false)...)
	}
	res, err := ins.OnDuplicateKeyUpdateAll().Exec(ctx)
	return res, e.FieldError(err)
}

// Update writes every non-key column of r to the row its key
// addresses.
func (e *Entity[T]) Update(db *DB, ctx context.Context, r *T) error {
	// Stamped first, for the reason Create is: an UPDATE writes every
	// non-key column, and on a scoped entity the tenant column is one
	// of them — an unstamped struct would write its zero over a row it
	// is otherwise allowed to touch and hand it to no tenant at all.
	if err := e.stampTenant(ctx, r); err != nil {
		return err
	}
	if e.pkIsZero(r) {
		return ErrPKNotSet
	}
	pred, err := e.pkPredicate(e.pkValuesOf(r))
	if err != nil {
		return err
	}
	v := reflect.ValueOf(r).Elem()
	sets := e.bindings(v, true)
	if len(sets) == 0 {
		return ErrNoAssignments
	}
	pkVals := e.pkValuesOf(r)
	do := func(tx *DB) error {
		if _, uerr := tx.Update(e.table).Set(sets...).Where(pred).Exec(ctx); uerr != nil {
			return e.FieldError(uerr)
		}
		return e.recordAudit(tx, ctx, "update", r, auditKey(pkVals))
	}
	if e.audit != nil {
		err = db.InTx(ctx, do)
	} else {
		err = do(db)
	}
	if err == nil {
		e.refreshPK(ctx, pkVals, *r)
	}
	return err
}

// Save inserts r when every key field is zero, and updates it
// otherwise.
func (e *Entity[T]) Save(db *DB, ctx context.Context, r *T) error {
	if e.pkIsZero(r) {
		return e.Create(db, ctx, r)
	}
	return e.Update(db, ctx, r)
}

// Delete removes the row addressed by key.
func (e *Entity[T]) Delete(db *DB, ctx context.Context, key ...any) (drops.Result, error) {
	pred, err := e.pkPredicate(key)
	if err != nil {
		return nil, err
	}
	var res drops.Result
	do := func(tx *DB) error {
		r, derr := tx.Delete(e.table).Where(pred).Exec(ctx)
		if derr != nil {
			return e.FieldError(derr)
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

// bindings extracts column values from a row. skipKey omits the
// primary-key columns, which an UPDATE must not reassign. A zero value
// in an AUTO_INCREMENT or defaulted column is omitted so the server
// fills it.
func (e *Entity[T]) bindings(v reflect.Value, skipKey bool) []ColumnValue {
	out := make([]ColumnValue, 0, len(e.colFields))
	for _, cf := range e.colFields {
		if skipKey && e.isKeyColumn(cf.col) {
			continue
		}
		fv := v.FieldByIndex(cf.field)
		if !skipKey && fv.IsZero() && (cf.col.autoInc || cf.col.hasDefault) {
			continue
		}
		out = append(out, columnValue{col: cf.col, val: fv.Interface()})
	}
	return out
}

// applyGeneratedKey writes a generated AUTO_INCREMENT id back into the
// row. Only a single-column integer key can receive one — a composite
// or string key has nothing MySQL could have generated.
func (e *Entity[T]) applyGeneratedKey(v reflect.Value, res drops.Result) {
	if e.pk == nil || !e.pk.autoInc || res == nil {
		return
	}
	fv := v.FieldByIndex(e.pkField)
	if !fv.IsZero() || !fv.CanSet() {
		return
	}
	id, ok := LastInsertID(res)
	if !ok {
		return
	}
	switch fv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		fv.SetInt(id)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if id >= 0 {
			fv.SetUint(uint64(id))
		}
	}
}
