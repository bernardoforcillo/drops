package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/bernardoforcillo/drops"
)

// Entity binds a Go struct T to a Table and precomputes the column ↔
// field mapping used by the CRUD shortcuts. ClickHouse exposes only
// Insert and Select builders (mutations are async ALTERs and not
// first-class), so the Entity surface is intentionally narrow:
//
//   - Create / CreateMany insert one or many rows from the typed
//     struct. There is no RETURNING in ClickHouse, so the caller is
//     responsible for filling every column they care about.
//   - Query returns a typed builder for SELECT chains that scan into
//     T directly.
//
// Declare an Entity once at package level:
//
//	type Event struct {
//	    ID     string    `drop:"id"`
//	    UserID uint64    `drop:"user_id"`
//	    Kind   string    `drop:"kind"`
//	    Ts     time.Time `drop:"ts"`
//	}
//
//	var (
//	    Events    = clickhouse.NewTable("events").Engine(clickhouse.MergeTree())
//	    EventID   = clickhouse.Add(Events, clickhouse.UUID("id"))
//	    EventEnt  = clickhouse.NewEntity[Event](Events)
//	)
type Entity[T any] struct {
	table      *Table
	colFields  []entityColField
	validators []Validator[T]
}

// Validator is called before Create / CreateMany with a pointer to
// each candidate row. Returning a non-nil error aborts the operation
// before any SQL is issued.
type Validator[T any] func(*T) error

type entityColField struct {
	col   *Column
	field []int
}

// NewEntity validates that T is a struct and builds the column ↔
// field index map. It panics on a non-struct type because schema
// declarations are typically loaded at process startup.
//
// Field matching mirrors the row scanner: `drop:"colname"` tag wins,
// otherwise field name and snake_case form are tried.
func NewEntity[T any](t *Table) *Entity[T] {
	var zero T
	rt := reflect.TypeOf(zero)
	for rt.Kind() == reflect.Ptr {
		rt = rt.Elem()
	}
	if rt.Kind() != reflect.Struct {
		panic(fmt.Sprintf("drops/clickhouse: NewEntity requires T to be a struct, got %s", rt.Kind()))
	}
	fields := fieldMap(rt)
	colFields := make([]entityColField, 0, len(t.Columns()))
	for _, c := range t.Columns() {
		idx, ok := fields[c.Name()]
		if !ok {
			continue
		}
		colFields = append(colFields, entityColField{col: c, field: idx})
	}
	return &Entity[T]{table: t, colFields: colFields}
}

// Table returns the table the entity is bound to.
func (e *Entity[T]) Table() *Table { return e.table }

// Validate registers a validator that runs before Create /
// CreateMany. Validators are invoked in registration order; the
// first to return a non-nil error aborts the operation.
func (e *Entity[T]) Validate(v Validator[T]) *Entity[T] {
	e.validators = append(e.validators, v)
	return e
}

func (e *Entity[T]) runValidators(r *T) error {
	for _, v := range e.validators {
		if err := v(r); err != nil {
			return err
		}
	}
	return nil
}

// Create inserts a single row from r. ClickHouse has no RETURNING,
// so r is not refreshed — any DEFAULT-driven values (timestamps,
// UUIDs) stay zero on the Go side.
func (e *Entity[T]) Create(db *DB, ctx context.Context, r *T) (drops.Result, error) {
	if err := e.runValidators(r); err != nil {
		return nil, err
	}
	v := reflect.ValueOf(r).Elem()
	bindings := e.collectBindings(v)
	if len(bindings) == 0 {
		return nil, errors.New("drops/clickhouse: Create has nothing to insert")
	}
	return db.Insert(e.table).Row(bindings...).Exec(ctx)
}

// CreateMany inserts a batch of rows. This is the typical analytics
// pattern; for very large batches drop down to the native columnar
// protocol via clickhouse-go's Prepare/Exec loop. Validators run
// against every row before any SQL is issued — the first failure
// aborts the whole batch.
func (e *Entity[T]) CreateMany(db *DB, ctx context.Context, rs []T) (drops.Result, error) {
	if len(rs) == 0 {
		return nil, ErrNoRowsToInsert
	}
	for i := range rs {
		if err := e.runValidators(&rs[i]); err != nil {
			return nil, err
		}
	}
	ins := db.Insert(e.table)
	for i := range rs {
		v := reflect.ValueOf(&rs[i]).Elem()
		ins.Row(e.collectBindings(v)...)
	}
	return ins.Exec(ctx)
}

func (e *Entity[T]) collectBindings(v reflect.Value) []ColumnValue {
	out := make([]ColumnValue, 0, len(e.colFields))
	for _, cf := range e.colFields {
		fv := v.FieldByIndex(cf.field)
		if fv.IsZero() && cf.col.HasDefault() {
			continue
		}
		out = append(out, &exprBinding{
			col:  cf.col,
			expr: drops.Param{Value: fv.Interface()},
		})
	}
	return out
}

// ----------------------------------------------------------------------
// Querying
// ----------------------------------------------------------------------

// Query returns a typed query builder that scans into T.
func (e *Entity[T]) Query(db *DB) *EntityQuery[T] {
	return &EntityQuery[T]{e: e, sb: db.Select().From(e.table)}
}

// EntityQuery is the typed counterpart of SelectBuilder — same shape,
// but its executors return ([]T, error) and (T, error) directly.
type EntityQuery[T any] struct {
	e  *Entity[T]
	sb *SelectBuilder
}

// Where appends predicates joined by AND.
func (q *EntityQuery[T]) Where(preds ...drops.Expression) *EntityQuery[T] {
	q.sb.Where(preds...)
	return q
}

// Prewhere appends PREWHERE predicates.
func (q *EntityQuery[T]) Prewhere(preds ...drops.Expression) *EntityQuery[T] {
	q.sb.Prewhere(preds...)
	return q
}

// OrderBy appends ORDER BY expressions.
func (q *EntityQuery[T]) OrderBy(exprs ...drops.Expression) *EntityQuery[T] {
	q.sb.OrderBy(exprs...)
	return q
}

// Limit sets the LIMIT.
func (q *EntityQuery[T]) Limit(n int64) *EntityQuery[T] { q.sb.Limit(n); return q }

// Offset sets the OFFSET.
func (q *EntityQuery[T]) Offset(n int64) *EntityQuery[T] { q.sb.Offset(n); return q }

// Unscoped opts out of the table's DefaultFilter predicates.
func (q *EntityQuery[T]) Unscoped() *EntityQuery[T] { q.sb.Unscoped(); return q }

// All executes the query and returns the matching rows as a typed
// slice.
func (q *EntityQuery[T]) All(ctx context.Context) ([]T, error) {
	var out []T
	err := q.sb.All(ctx, &out)
	return out, err
}

// One executes the query and returns the first matching row. Returns
// ErrNoRows if the query produces no rows.
func (q *EntityQuery[T]) One(ctx context.Context) (T, error) {
	var out T
	err := q.sb.One(ctx, &out)
	return out, err
}
