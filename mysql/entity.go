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
	return &Entity[T]{table: t, pk: pk, pkField: pkField, pks: pks, pkFields: pkFields, colFields: colFields}
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
	err = db.Select(e.selectCols()...).From(e.table).Where(pred).One(ctx, &out)
	return out, err
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

// Unscoped opts out of the table's DefaultFilter predicates.
func (q *EntityQuery[T]) Unscoped() *EntityQuery[T] { q.sb.Unscoped(); return q }

// All returns every matching row.
func (q *EntityQuery[T]) All(ctx context.Context) ([]T, error) {
	var out []T
	if err := q.sb.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// One returns the first matching row.
func (q *EntityQuery[T]) One(ctx context.Context) (T, error) {
	var out T
	err := q.sb.Limit(1).One(ctx, &out)
	return out, err
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
	v := reflect.ValueOf(r).Elem()
	ins := db.Insert(e.table)
	ins.Row(e.bindings(v, false)...)
	res, err := ins.Exec(ctx)
	if err != nil {
		return e.FieldError(err)
	}
	e.applyGeneratedKey(v, res)
	return nil
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
		ins.Row(e.bindings(reflect.ValueOf(&rows[i]).Elem(), false)...)
	}
	res, err := ins.OnDuplicateKeyUpdateAll().Exec(ctx)
	return res, e.FieldError(err)
}

// Update writes every non-key column of r to the row its key
// addresses.
func (e *Entity[T]) Update(db *DB, ctx context.Context, r *T) error {
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
	_, err = db.Update(e.table).Set(sets...).Where(pred).Exec(ctx)
	return e.FieldError(err)
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
	res, err := db.Delete(e.table).Where(pred).Exec(ctx)
	return res, e.FieldError(err)
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
