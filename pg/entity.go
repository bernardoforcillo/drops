package pg

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/bernardoforcillo/drops"
)

// Entity binds a Go struct T to a Table and precomputes the metadata
// the CRUD shortcuts need: the column-to-field index path, the
// primary-key column, and the PK's field path inside T. It is the
// entry point for type-safe CRUD operations that scan into T directly,
// without the caller having to declare scan targets.
//
// Declare an Entity once at package level alongside its table:
//
//	type User struct {
//	    ID    int64  `db:"id"`
//	    Name  string `db:"name"`
//	    Email string `db:"email"`
//	}
//
//	var (
//	    Users      = pg.NewTable("users")
//	    UserID     = pg.Add(Users, pg.BigSerial("id").PrimaryKey())
//	    UserName   = pg.Add(Users, pg.Text("name").NotNull())
//	    UserEmail  = pg.Add(Users, pg.Text("email").NotNull().Unique())
//	    UserEntity = pg.NewEntity[User](Users)
//	)
//
// All Entity methods take *DB as their first argument so the same
// entity can be reused across transactions and connection pools:
//
//	u, err := UserEntity.Get(db, ctx, 42)
//	err  = UserEntity.Create(db, ctx, &u)
//	err  = UserEntity.Update(db, ctx, &u)
//	err  = UserEntity.Save(db, ctx, &u) // INSERT or UPDATE depending on PK
//	res, err := UserEntity.Delete(db, ctx, 42)
//
// Entity composes with Phase-1 features: lifecycle hooks (Timestamps,
// SoftDelete, …) registered on the table fire normally because every
// operation routes through the underlying Insert / Update / Delete
// builders.
type Entity[T any] struct {
	table     *Table
	pk        *Column
	pkField   []int
	colFields []entityColField // columns that map to a struct field
}

// entityColField bundles a column with its field-index path inside T.
type entityColField struct {
	col   *Column
	field []int
}

// NewEntity validates that T has a field bound to every primary-key
// column on t and precomputes the column ↔ field mapping. It panics
// on misalignment because schemas are typically declared in package
// init blocks — bad config should fail at startup, not at the first
// query.
//
// Field matching rules mirror the row scanner: `db:"colname"` tag
// wins; otherwise the field name and its snake_case form are tried.
// Fields tagged `db:"-"` are skipped.
func NewEntity[T any](t *Table) *Entity[T] {
	var zero T
	rt := reflect.TypeOf(zero)
	for rt.Kind() == reflect.Ptr {
		rt = rt.Elem()
	}
	if rt.Kind() != reflect.Struct {
		panic(fmt.Sprintf("drops/pg: NewEntity requires T to be a struct, got %s", rt.Kind()))
	}
	fields := fieldMap(rt)

	var pk *Column
	for _, c := range t.Columns() {
		if c.IsPrimaryKey() {
			if pk != nil {
				panic(fmt.Sprintf("drops/pg: NewEntity[%s]: table %q has more than one PRIMARY KEY column; composite keys are not supported by Entity", rt.Name(), t.Name()))
			}
			pk = c
		}
	}
	if pk == nil {
		panic(fmt.Sprintf("drops/pg: NewEntity[%s]: table %q has no PRIMARY KEY column; CRUD shortcuts require one", rt.Name(), t.Name()))
	}

	pkField, ok := lookupField(fields, pk.Name())
	if !ok {
		panic(fmt.Sprintf("drops/pg: NewEntity[%s]: no struct field bound to PK column %q on table %q", rt.Name(), pk.Name(), t.Name()))
	}

	colFields := make([]entityColField, 0, len(t.Columns()))
	for _, c := range t.Columns() {
		idx, ok := lookupField(fields, c.Name())
		if !ok {
			continue
		}
		colFields = append(colFields, entityColField{col: c, field: idx})
	}

	return &Entity[T]{
		table:     t,
		pk:        pk,
		pkField:   pkField,
		colFields: colFields,
	}
}

// lookupField resolves a column name to a field index path using the
// scanner's fieldMap. It tries the exact name first, then the
// snake_case form, then the PascalCase form — the same triple-rule
// the scanner uses.
func lookupField(fields map[string][]int, name string) ([]int, bool) {
	if idx, ok := fields[name]; ok {
		return idx, true
	}
	return nil, false
}

// Table returns the table the entity is bound to.
func (e *Entity[T]) Table() *Table { return e.table }

// PK returns the entity's primary-key column.
func (e *Entity[T]) PK() *Column { return e.pk }

// ----------------------------------------------------------------------
// CRUD operations
// ----------------------------------------------------------------------

// ErrPKNotSet is returned by Update / Save when r's primary-key
// field is the zero value but the operation requires it to be set.
var ErrPKNotSet = errors.New("drops/pg: primary key field is the zero value")

// Get fetches the row whose primary key equals id. Returns ErrNoRows
// if no row matches.
func (e *Entity[T]) Get(db *DB, ctx context.Context, id any) (T, error) {
	var out T
	err := db.Find(e.table).Where(Eq(e.pk, id)).One(ctx, &out)
	return out, err
}

// Create INSERTs r and refreshes it from the RETURNING row — useful
// for picking up generated PKs, server-side defaults, and hook-added
// values (e.g. created_at = now()).
//
// Columns whose Go field is the zero value are omitted from the
// INSERT when the column either has a declared DEFAULT or is the
// primary key — letting the DB generate the value. To override that
// behaviour for a specific field, set it to a non-zero value before
// calling Create.
func (e *Entity[T]) Create(db *DB, ctx context.Context, r *T) error {
	v := reflect.ValueOf(r).Elem()
	bindings := e.collectInsertBindings(v)
	if len(bindings) == 0 {
		return errors.New("drops/pg: Create has nothing to insert")
	}
	ins := db.Insert(e.table).Row(bindings...)
	for _, c := range e.table.Columns() {
		ins.Returning(c)
	}
	return ins.One(ctx, r)
}

// Update writes r's current field values to the row whose PK equals
// r's PK and refreshes r from the RETURNING row. ErrPKNotSet is
// returned if r's PK is the zero value.
//
// All non-PK columns mapped to fields on T are included in the SET
// list — the typical "blind UPDATE" semantics. Change-tracking is
// out of scope for now; callers needing finer control use db.Update
// directly.
func (e *Entity[T]) Update(db *DB, ctx context.Context, r *T) error {
	v := reflect.ValueOf(r).Elem()
	pkv := v.FieldByIndex(e.pkField)
	if pkv.IsZero() {
		return ErrPKNotSet
	}
	upd := db.Update(e.table)
	wroteSet := false
	for _, cf := range e.colFields {
		if cf.col == e.pk {
			continue
		}
		upd.Set(&exprBinding{
			col:  cf.col,
			expr: drops.Param{Value: v.FieldByIndex(cf.field).Interface()},
		})
		wroteSet = true
	}
	if !wroteSet && !e.table.hasUpdateHooks() {
		return errors.New("drops/pg: Update has no fields to set")
	}
	upd.Where(Eq(e.pk, pkv.Interface()))
	for _, c := range e.table.Columns() {
		upd.Returning(c)
	}
	return upd.One(ctx, r)
}

// Save inserts r if its primary-key field is the zero value, or
// updates it otherwise. Compared to a single ON CONFLICT statement,
// this incurs an extra branch in Go but keeps the generated SQL
// straightforward; switch to a hand-written upsert when the
// race-window between the read and the write matters.
func (e *Entity[T]) Save(db *DB, ctx context.Context, r *T) error {
	v := reflect.ValueOf(r).Elem()
	if v.FieldByIndex(e.pkField).IsZero() {
		return e.Create(db, ctx, r)
	}
	return e.Update(db, ctx, r)
}

// Delete removes the row whose primary key equals id. The table's
// DeleteHooks (e.g. SoftDelete) fire normally — so on a soft-deleted
// table this rewrites to UPDATE deleted_at = now() instead.
func (e *Entity[T]) Delete(db *DB, ctx context.Context, id any) (drops.Result, error) {
	return db.Delete(e.table).Where(Eq(e.pk, id)).Exec(ctx)
}

// collectInsertBindings extracts column values from r. Columns whose
// Go field is the zero value are omitted when they have a DEFAULT or
// are the primary key — letting the DB fill them in.
func (e *Entity[T]) collectInsertBindings(v reflect.Value) []ColumnValue {
	out := make([]ColumnValue, 0, len(e.colFields))
	for _, cf := range e.colFields {
		fv := v.FieldByIndex(cf.field)
		if fv.IsZero() && (cf.col.HasDefault() || cf.col == e.pk || isImplicitDefault(cf.col)) {
			continue
		}
		out = append(out, &exprBinding{
			col:  cf.col,
			expr: drops.Param{Value: fv.Interface()},
		})
	}
	return out
}

// isImplicitDefault reports whether a column's SQL type implies a
// server-side default value (serial families). These columns aren't
// flagged by HasDefault() because their default lives in the type
// declaration, not in a DEFAULT clause.
func isImplicitDefault(c *Column) bool {
	switch c.Type().TypeSQL() {
	case "serial", "bigserial", "smallserial":
		return true
	}
	return false
}

// ----------------------------------------------------------------------
// Querying
// ----------------------------------------------------------------------

// Query returns a typed query builder for ad-hoc Where / OrderBy /
// Limit / Offset / With chains that scan into T or []T.
func (e *Entity[T]) Query(db *DB) *EntityQuery[T] {
	return &EntityQuery[T]{e: e, fb: db.Find(e.table)}
}

// EntityQuery is the typed counterpart of FindBuilder — same shape,
// but its executors return ([]T, error) and (T, error) directly.
type EntityQuery[T any] struct {
	e  *Entity[T]
	fb *FindBuilder
}

// Where appends predicates joined by AND.
func (q *EntityQuery[T]) Where(preds ...drops.Expression) *EntityQuery[T] {
	q.fb.Where(preds...)
	return q
}

// OrderBy appends ORDER BY expressions.
func (q *EntityQuery[T]) OrderBy(exprs ...drops.Expression) *EntityQuery[T] {
	q.fb.OrderBy(exprs...)
	return q
}

// Limit sets the LIMIT.
func (q *EntityQuery[T]) Limit(n int64) *EntityQuery[T] { q.fb.Limit(n); return q }

// Offset sets the OFFSET.
func (q *EntityQuery[T]) Offset(n int64) *EntityQuery[T] { q.fb.Offset(n); return q }

// With eager-loads the named relations (see FindBuilder.With).
func (q *EntityQuery[T]) With(names ...string) *EntityQuery[T] {
	q.fb.With(names...)
	return q
}

// WithRel eager-loads a relation with per-edge configuration.
func (q *EntityQuery[T]) WithRel(name string, fn func(*RelConfig)) *EntityQuery[T] {
	q.fb.WithRel(name, fn)
	return q
}

// Unscoped opts out of the table's DefaultFilter predicates.
func (q *EntityQuery[T]) Unscoped() *EntityQuery[T] {
	q.fb.Unscoped()
	return q
}

// All executes the query and returns the matching rows as a typed
// slice.
func (q *EntityQuery[T]) All(ctx context.Context) ([]T, error) {
	var out []T
	err := q.fb.All(ctx, &out)
	return out, err
}

// One executes the query and returns the first matching row. Returns
// ErrNoRows if the query produces no rows.
func (q *EntityQuery[T]) One(ctx context.Context) (T, error) {
	var out T
	err := q.fb.One(ctx, &out)
	return out, err
}
