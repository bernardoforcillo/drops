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
// by field name / camelCase), and the table must have a primary key —
// declared either on its columns or on the table itself.
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

	pks := primaryKeyColumns(t)
	if len(pks) == 0 {
		panic(fmt.Sprintf("drops/sqlite: NewEntity[%s]: table %q has no PRIMARY KEY; "+
			"mark a column .PrimaryKey() or declare the key on the table with PrimaryKey(cols...)",
			rt.Name(), t.name))
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

// primaryKeyColumns returns t's PRIMARY KEY columns in key order,
// whichever of the two declarations the schema used.
//
// A key arrives either as Table.PrimaryKey(cols...) or by marking
// columns with (*Col[T]).PrimaryKey(), and drops/pg's own
// primaryKeyColumns says why every reader has to accept both: a table
// declared one way silently loses its key in a reader that knows only
// the other. CreateTable here already reads both spellings, so a
// reader that did not disagreed with the DDL it was rendered beside.
func primaryKeyColumns(t *Table) []*Column {
	if pk := t.compositePK; len(pk) > 0 {
		return pk
	}
	var out []*Column
	for _, c := range t.columns {
		if c.primary {
			out = append(out, c)
		}
	}
	return out
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
	if err := drift.Report("drops/sqlite", rt.Name(), t.name, missing,
		drift.SpareFields(rt, bound), "sqlite.AllowUnmappedColumns"); err != nil {
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
// admits NULL, not on whether it said so: a bare sqlite.Text("bio") is exactly
// the shape that has been accepting NULLs nobody declared.
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
	return drift.ReportNullable("drops/sqlite", rt.Name(), t.name, bad, "sqlite.AllowNullableColumns")
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
	bound := make([][]ColumnValue, len(rows))
	for i := range rows {
		if err := e.stampTenant(ctx, &rows[i]); err != nil {
			return nil, err
		}
		bound[i] = e.bindings(reflect.ValueOf(&rows[i]).Elem(), false)
	}
	ins := db.Insert(e.table)
	for _, vals := range e.alignBindings(bound) {
		ins.Values(vals...)
	}
	return ins.Exec(ctx)
}

// alignBindings widens every row to the same column list.
//
// bindings omits an autoincrement / defaulted / key column the caller
// left zero, so the server supplies it. That is right for one row and
// wrong for a batch: INSERT names its columns once, from the first
// row, so a row that bound a different set has its values read under
// the wrong names. Where the sets differ in size SQLite catches it
// ("all VALUES must have the same number of terms"); where they merely
// differ — one row sets its key and leaves a defaulted column zero,
// the next does the reverse — nothing catches it and the values land
// shifted. So the comparison is by column, not by width. A row missing
// a column the batch binds is filled with that column's declared
// DEFAULT, or NULL when it has none — which is what the server would
// have stored had the row been inserted on its own.
func (e *Entity[T]) alignBindings(rows [][]ColumnValue) [][]ColumnValue {
	bound := map[*Column]bool{}
	for _, r := range rows {
		for _, cv := range r {
			bound[cv.column()] = true
		}
	}
	var cols []*Column
	for _, cf := range e.colFields {
		if bound[cf.col] {
			cols = append(cols, cf.col)
		}
	}
	if rowsMatchColumns(rows, cols) {
		return rows
	}
	out := make([][]ColumnValue, len(rows))
	for i, r := range rows {
		byCol := make(map[*Column]ColumnValue, len(r))
		for _, cv := range r {
			byCol[cv.column()] = cv
		}
		wide := make([]ColumnValue, 0, len(cols))
		for _, c := range cols {
			if cv, ok := byCol[c]; ok {
				wide = append(wide, cv)
				continue
			}
			fill := "NULL"
			if c.hasDefault {
				fill = c.defaultSQL
			}
			wide = append(wide, &exprBinding{col: c, expr: drops.Raw(fill)})
		}
		out[i] = wide
	}
	return out
}

// rowsMatchColumns reports whether every row already binds exactly cols,
// in order.
func rowsMatchColumns(rows [][]ColumnValue, cols []*Column) bool {
	for _, r := range rows {
		if len(r) != len(cols) {
			return false
		}
		for i, cv := range r {
			if cv.column() != cols[i] {
				return false
			}
		}
	}
	return true
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
	// A query that named FilterTenant asked for the cross-tenant read
	// explicitly, so it must skip tenantPredicate entirely — that call
	// errors when the ctx carries no tenant.
	if q.sb.scope.ignores(FilterTenant) {
		return q.applyGuardOnly(ctx)
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
// Unscoped opts out of every global filter registered on the table —
// the blunt instrument; see [SelectBuilder.Unscoped]. The tenant guard
// [Entity.ScopeByTenant] installs is deliberately out of its reach: it
// comes from the ctx, not the table, and losing customer isolation as a
// side effect of asking for soft-deleted rows is the accident this API
// exists to prevent. Drop it by naming it — IgnoreFilters(FilterTenant).
func (q *EntityQuery[T]) Unscoped() *EntityQuery[T] { q.sb.Unscoped(); return q }

// IgnoreFilters bypasses the named global filters and leaves every
// other one standing:
//
//	// this tenant's rows, deleted ones included
//	posts.Query(db).IgnoreFilters(sqlite.FilterSoftDelete).All(ctx)
//
// Beyond the table's own filters it also accepts [FilterTenant], which
// drops the isolation predicate [Entity.ScopeByTenant] injects — a
// cross-tenant read is a real need, and one that should read as one at
// the call site.
func (q *EntityQuery[T]) IgnoreFilters(names ...string) *EntityQuery[T] {
	q.sb.IgnoreFilters(names...)
	return q
}

// applyGuardOnly applies the authorisation guard without the tenant
// predicate. Dropping tenancy by name never drops authorisation with
// it — they are separate scopes and only one was named.
func (q *EntityQuery[T]) applyGuardOnly(ctx context.Context) error {
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
