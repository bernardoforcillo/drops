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
	// rowType is T with its pointers stripped. It names the row this
	// entity maps, and is what keys the context filters the entity
	// registers on the shared table — see rowScopeFilterKey.
	rowType reflect.Type
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

// hasRowScope reports whether anything restricts which rows an
// operation on this entity may touch: a tenant axis or an
// authorisation guard declared through the entity, or any context
// filter the TABLE declares.
//
// The invariant the answer guards is the strong one: the PK cache never
// *holds* a scoped row, not merely "a scoped Get does not read it".
// That key is the primary key and nothing else — it has no room for the
// tenant or the subject — so a scoped row sitting in that namespace is
// a row waiting to be handed to the next caller who asks for that id,
// whoever they are, without a statement ever being sent and therefore
// without the tenant predicate ever running or ErrTenantMissing ever
// firing. Gating only the read leaves the namespace poisoned by every
// write and puts the leak one refactor away; so the read in Get and the
// writes in Create and Update are all gated on this one predicate. The
// writes ask through refreshPK, which also deletes the entry the gate
// refuses to overwrite — an entry a scoped write leaves in place is an
// entry nothing will ever correct.
//
// It is a question about the entity's configuration, deliberately, and
// not about what the predicates resolve to on the ctx at hand. Get used
// to ask the latter — "did the tenant and the guard produce a predicate
// this time" — and a guard that returns no predicate for the current
// subject, which is the ordinary spelling of "this subject is
// unrestricted", answered no and put a guarded entity back on the
// cached path.
//
// The third term is the one the list used to be missing, and it is the
// reason this comment named it before it existed: a table can be scoped
// with no Entity involved at all —
// Posts.ContextFilter(sqlite.TenantFilter(col)), or a second entity
// over the same table that declared the axis — and the PK cache is
// attached per entity. Without it, an entity holding no axis of its own
// went on filling a primary-key-keyed namespace with rows whose
// visibility the table restricts, and every one of them was a row
// waiting to be handed to the next caller who asked for that id.
//
// The first two terms are kept rather than folded into the third. They
// answer a question about this entity's configuration where the third
// answers one about the table's, and an entity whose guard is declared
// but whose filter registration happens to be replaced by another
// entity of the same row type must still be treated as scoped.
func (e *Entity[T]) hasRowScope() bool {
	return e.tenantCol != nil || e.guard != nil || e.table.hasContextFilters()
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
	return &Entity[T]{
		table: t, rowType: rt,
		pk: pk, pkField: pkField, pks: pks, pkFields: pkFields, colFields: colFields,
	}
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
// if absent.
//
// The tenant scope and the authorization guard are not applied here.
// They are registered on the table as context filters (see
// [Entity.ScopeByTenant] and [Entity.AuthorizeWith]) and applied by the
// executor this statement runs through, which is what puts them on
// every other path too — a relation edge, a subquery, a bare
// db.Select — instead of only on the methods somebody remembered.
//
// When a cache is attached via WithCache, Get serves hits from the
// cache and dedupes concurrent cache misses via single-flight so a
// thundering herd resolves to one DB query. An entity whose rows are
// scoped — by a tenant axis, a guard, or anything the table itself
// declares — skips that path entirely: the cache is keyed by primary
// key alone, so answering from it would hand one caller a row another
// caller cached, without a statement ever being sent. See hasRowScope.
func (e *Entity[T]) Get(db *DB, ctx context.Context, key ...any) (T, error) {
	var out T
	pred, err := e.pkPredicate(key)
	if err != nil {
		return out, err
	}
	if e.cache != nil && !e.hasRowScope() {
		return e.getCached(db, ctx, key, pred)
	}
	err = db.Select(e.selectCols()...).From(e.table).Where(pred).One(ctx, &out)
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
// in the same transaction (when audited), and populates the PK cache —
// or, for a scoped entity, clears it. See hasRowScope.
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
	if err == nil {
		// Populate the PK cache with the freshly-inserted row so the
		// next Get hits immediately. A scoped entity writes nothing and
		// clears the key instead: it carries the primary key and no
		// scope at all, so the entry would be served to every other
		// tenant and subject that asks for this id. See hasRowScope for
		// the invariant and refreshPK for why the clearing is not
		// optional.
		e.refreshPK(ctx, e.pkValuesOf(r), *r)
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
	// ToSQLCtx, not ToSQL: this is an executor, and the statement it
	// sends has to be the one the ctx names — stamped with the tenant,
	// or refused when the ctx carries none. Rendering it blind here
	// would have made Entity.Create the one INSERT path in the package
	// that wrote a row belonging to nobody.
	sqlText, args, err := ins.ToSQLCtx(ctx)
	if err != nil {
		return err
	}
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
// same transaction (when audited), and refreshes the PK cache — or, for
// a scoped entity, clears it. See hasRowScope.
func (e *Entity[T]) Update(db *DB, ctx context.Context, r *T) error {
	var err error
	rv := reflect.ValueOf(r).Elem()
	sets := e.bindings(rv, true)
	pkVals := e.pkValuesOf(r)
	pred, err := e.pkPredicate(pkVals)
	if err != nil {
		return err
	}
	do := func(tx *DB) error {
		if _, err := tx.Update(e.table).Set(sets...).Where(pred).Exec(ctx); err != nil {
			return err
		}
		return e.recordAudit(tx, ctx, "update", r, auditKey(pkVals))
	}
	if e.audit != nil {
		err = db.InTx(ctx, do)
	} else {
		err = do(db)
	}
	if err == nil {
		// Same rule as Create: a scoped row must not enter a namespace
		// keyed by the primary key alone, and an entry a scoped write
		// leaves in place is an entry nothing will ever correct. See
		// hasRowScope and refreshPK.
		e.refreshPK(ctx, pkVals, *r)
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
	var res drops.Result
	do := func(tx *DB) error {
		r, derr := tx.Delete(e.table).Where(pred).Exec(ctx)
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
// (via WithTenant); the tenant predicate is applied at execution time,
// by the executor, from the filter the axis registered on the table.
func (e *Entity[T]) Query(db *DB) *EntityQuery[T] {
	return &EntityQuery[T]{e: e, sb: db.Select(e.selectCols()...).From(e.table)}
}

// EntityQuery is a typed wrapper over SelectBuilder that returns []T / T.
//
// It used to AND the tenant predicate and the guard onto q.sb the first
// time it executed, under a scopesApplied flag. Two things were wrong
// with that and both are fixed by letting the executor do it. The
// predicates reached this path and no other, so a query built any other
// way went unscoped. And appending to the builder MUTATED it, so the
// flag existed to stop the second execution carrying the predicate
// twice — which meant a query value reused across requests answered the
// second request with the first request's tenant, permanently. The
// executor resolves onto a per-execution copy instead, so a reused
// EntityQuery is scoped afresh every time and there is no flag to keep
// in step.
type EntityQuery[T any] struct {
	e  *Entity[T]
	sb *SelectBuilder
	// unscoped records [EntityQuery.Unscoped], which means something
	// narrower here than it does on the raw builder — see there, and
	// see stmt for how the narrower meaning is composed.
	unscoped bool
}

// stmt returns the SELECT to run for ctx.
//
// With no Unscoped it is the builder itself, and the executor does
// everything. With Unscoped it is a per-execution COPY that opts the
// statement out of the table's automatic predicates and then AND-s the
// context filters back in, resolved for this ctx.
//
// The copy is not an optimisation. Appending the resolved predicates to
// q.sb would leave them there: the second execution of a reused query
// would carry the tenant twice, and a query value held across requests
// would answer the second request with the first request's tenant.
// That is the same accretion the old applyScopes flag existed to paper
// over, and copying is what removes the need for a flag.
func (q *EntityQuery[T]) stmt(ctx context.Context) (*SelectBuilder, error) {
	if !q.unscoped {
		return q.sb, nil
	}
	scope, err := q.e.table.resolveContextFilters(ctx)
	if err != nil {
		return nil, err
	}
	cp := *q.sb
	cp.unscoped = true
	cp.wheres = append(append([]drops.Expression(nil), q.sb.wheres...), scope...)
	return &cp, nil
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

// Unscoped opts out of the table's DEFAULT filters for this query —
// the declaration-time ones, a soft-delete guard above all. Without it
// a soft-deleted row is unreachable through the entity at all, which
// makes an audit or a restore flow impossible to write.
//
// It does NOT drop the table's context filters: the tenant axis and the
// authorization guard survive it. That is a deliberate difference from
// [SelectBuilder.Unscoped], which is statement-wide, and from
// drops/pg's entity query, which is statement-wide too. The two lists
// are not the same kind of thing — a default filter is a default scope,
// a context filter is a row-visibility boundary — and the failures of
// conflating them are not symmetric. Widening a default scope when the
// caller asked to widen it costs nothing. Dropping the boundary hands
// this request every tenant's rows, or every subject's, and it does so
// on the one method a caller reaches for while thinking about
// soft-deleted rows rather than about tenancy.
//
// pg makes the other trade, and its reason is real: a caller who says
// Unscoped and gets ErrTenantMissing has learned nothing about the row
// they were after. The reason it lands differently here is that this
// dialect ALREADY behaved this way — the entity injected its guard
// whether or not the query said Unscoped — and a port that silently
// widened every existing Unscoped() call to span tenants would be a
// scoping regression shipped under the banner of scoping work.
//
// A query that genuinely has to span tenants is written on the raw
// builder, db.Select().From(t).Unscoped(), where a reviewer reading the
// call sees the whole of what was given up.
func (q *EntityQuery[T]) Unscoped() *EntityQuery[T] { q.unscoped = true; return q }

func (q *EntityQuery[T]) Limit(n int64) *EntityQuery[T]  { q.sb.Limit(n); return q }
func (q *EntityQuery[T]) Offset(n int64) *EntityQuery[T] { q.sb.Offset(n); return q }

// All executes and returns every matching row. When the entity has a
// cache attached, the result is read through it under a key derived
// from the rendered SQL and its arguments, matching drops/pg.
func (q *EntityQuery[T]) All(ctx context.Context) ([]T, error) {
	if q.e.cache != nil {
		return q.allCached(ctx)
	}
	sel, err := q.stmt(ctx)
	if err != nil {
		return nil, err
	}
	var out []T
	if err := sel.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// One executes and returns the first row, or ErrNoRows. Cached the
// same way as All when the entity has a cache attached.
func (q *EntityQuery[T]) One(ctx context.Context) (T, error) {
	var out T
	q.sb.Limit(1)
	if q.e.cache != nil {
		return q.oneCached(ctx)
	}
	sel, err := q.stmt(ctx)
	if err != nil {
		return out, err
	}
	err = sel.One(ctx, &out)
	return out, err
}

// allCached and oneCached read the rendered query through the entity
// cache. Both go through the single-flight group so a cold key under
// concurrent load issues one query rather than one per caller — the
// stampede protection the PK path already had.
//
// The key is built from ToSQLCtx and not from ToSQL, and that is a
// tenant boundary rather than a detail. The query namespace is safe for
// a scoped entity only because the tenant is bound into the statement
// as an argument and hashed into the key — see queryKey, which is
// written around exactly that fact. Since the axis moved onto the table
// the tenant is added by the RESOLVER, so ToSQL renders the statement
// without it: two tenants asking one question would produce one key and
// one of them would be served the other's rows, silently, on the first
// request after the second tenant's cache warmed. Resolving here also
// means a ctx that cannot name a tenant is refused before the cache is
// consulted, rather than after it answers.
func (q *EntityQuery[T]) allCached(ctx context.Context) ([]T, error) {
	sel, err := q.stmt(ctx)
	if err != nil {
		return nil, err
	}
	sql, args, err := sel.ToSQLCtx(ctx)
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
		if qErr := sel.All(ctx, &rs); qErr != nil {
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
	var out T
	sel, err := q.stmt(ctx)
	if err != nil {
		return out, err
	}
	sql, args, err := sel.ToSQLCtx(ctx)
	if err != nil {
		return out, err
	}
	key := queryKey(q.e.table.Name(), sql, args) + ":one"
	if hit, err := q.e.cache.readPK(ctx, key, &out); err == nil && hit {
		return out, nil
	}
	v, err := q.e.cache.sf.do(key, func() (any, error) {
		var t T
		if hit, rErr := q.e.cache.readPK(ctx, key, &t); rErr == nil && hit {
			return t, nil
		}
		if qErr := sel.One(ctx, &t); qErr != nil {
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
