package sqlite

import (
	"context"
	"errors"

	"github.com/bernardoforcillo/drops"
)

// Patch issues a single UPDATE that applies SQL-side operations to the
// row whose PK equals id, without round-tripping the value through the
// application. Ideal for high-contention counters:
//
//	PostEntity.Patch(db, ctx, postID,
//	    sqlite.Inc(PostLikes, 1),
//	    sqlite.SetIfGreater(PostMaxScore, currentScore),
//	)
//	// UPDATE "posts" SET
//	//   "likes"    = "likes" + ?,
//	//   "maxScore" = max("maxScore", ?)
//	// WHERE ("id" = ?) AND ("tenantId" = ?)
//
// The op set honours the entity's tenant scope (see ScopeByTenant).
// Returns drops.Result so callers can read rows-affected to distinguish
// "row missing" from "row updated" without a follow-up SELECT.

// PatchOp describes one SET assignment in a Patch. It is exactly the
// sqlite ColumnValue shape, so patch ops slot directly into
// UpdateBuilder.Set. Construct one with Inc / Dec / Set /
// SetIfGreater / SetIfLess / SetIfChanged.
type PatchOp = ColumnValue

// Patch applies ops to the row identified by id.
func (e *Entity[T]) Patch(db *DB, ctx context.Context, id any, ops ...PatchOp) (drops.Result, error) {
	return e.PatchKey(db, ctx, []any{id}, ops...)
}

// PatchKey is [Entity.Patch] for a composite primary key. Patch
// cannot take a variadic key because its operations already are, so
// the multi-column form spells the key as a slice.
func (e *Entity[T]) PatchKey(db *DB, ctx context.Context, key []any, ops ...PatchOp) (drops.Result, error) {
	if len(ops) == 0 {
		return nil, errors.New("drops/sqlite: Patch requires at least one operation")
	}
	pred, err := e.pkPredicate(key)
	if err != nil {
		return nil, err
	}
	tenantPred, err := e.tenantPredicate(ctx)
	if err != nil {
		return nil, err
	}
	upd := db.Update(e.table).Set(ops...).Where(pred)
	if tenantPred != nil {
		upd.Where(tenantPred)
	}
	return upd.Exec(ctx)
}

// number is the constraint for numeric counter ops.
type number interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 |
		~float32 | ~float64
}

// Inc emits "col = col + delta".
func Inc[T number](col *Col[T], delta T) PatchOp {
	return &incOp[T]{col: col.Column, delta: delta}
}

// Dec is shorthand for Inc(col, -delta).
func Dec[T number](col *Col[T], delta T) PatchOp {
	return &incOp[T]{col: col.Column, delta: -delta}
}

type incOp[T number] struct {
	col   *Column
	delta T
}

func (o *incOp[T]) column() *Column { return o.col }
func (o *incOp[T]) writeValue(b *drops.Builder) {
	o.col.WriteSQL(b)
	b.WriteString(" + ")
	b.AddArg(o.delta)
}

// Set is a typed plain assignment, usable in the Patch op list.
func Set[T any](col *Col[T], v T) PatchOp {
	return &setValOp[T]{col: col.Column, val: v}
}

type setValOp[T any] struct {
	col *Column
	val T
}

func (o *setValOp[T]) column() *Column             { return o.col }
func (o *setValOp[T]) writeValue(b *drops.Builder) { b.AddArg(o.val) }

// SetIfGreater emits "col = max(col, ?)" — raises but never lowers the
// value (high-watermark). SQLite's scalar max() is the GREATEST analog.
func SetIfGreater[T any](col *Col[T], v T) PatchOp {
	return &monotonicOp[T]{col: col.Column, val: v, fn: "max"}
}

// SetIfLess emits "col = min(col, ?)" — lowers but never raises the
// value (low-watermark).
func SetIfLess[T any](col *Col[T], v T) PatchOp {
	return &monotonicOp[T]{col: col.Column, val: v, fn: "min"}
}

type monotonicOp[T any] struct {
	col *Column
	val T
	fn  string
}

func (o *monotonicOp[T]) column() *Column { return o.col }
func (o *monotonicOp[T]) writeValue(b *drops.Builder) {
	b.WriteString(o.fn)
	b.WriteByte('(')
	o.col.WriteSQL(b)
	b.WriteString(", ")
	b.AddArg(o.val)
	b.WriteByte(')')
}

// SetIfChanged emits a CASE that assigns ? only when it differs from
// the current value, using SQLite's NULL-safe IS NOT comparison so the
// branch is well-defined even when the column is NULL.
func SetIfChanged[T any](col *Col[T], v T) PatchOp {
	return &ifChangedOp[T]{col: col.Column, val: v}
}

type ifChangedOp[T any] struct {
	col *Column
	val T
}

func (o *ifChangedOp[T]) column() *Column { return o.col }
func (o *ifChangedOp[T]) writeValue(b *drops.Builder) {
	b.WriteString("CASE WHEN ")
	o.col.WriteSQL(b)
	b.WriteString(" IS NOT ")
	b.AddArg(o.val)
	b.WriteString(" THEN ")
	b.AddArg(o.val)
	b.WriteString(" ELSE ")
	o.col.WriteSQL(b)
	b.WriteString(" END")
}
