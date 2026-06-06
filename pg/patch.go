package pg

import (
	"context"
	"errors"

	"github.com/bernardoforcillo/drops"
)

// High-traffic counters (like counts, view counts, engagement
// scores) hit the same row from thousands of goroutines. Doing
// "SELECT count, UPDATE count = count + 1" round-trips the value
// through the application and races on it; doing
// "UPDATE count = count + 1" inline is the right shape but
// tedious to write through Entity (which wants a struct).
//
// Patch sidesteps both: it issues one UPDATE with arbitrary
// SQL-side patch operations, all bound to the entity's PK so
// the path is type-safe and audit/guard/tenant friendly.
//
//	PostEntity.Patch(db, ctx, postID,
//	    pg.Inc(PostLikes, 1),
//	    pg.Inc(PostEngagement, 1),
//	    pg.SetIfGreater(PostMaxScore, currentScore),
//	)
//	// UPDATE "posts" SET
//	//   "likes"      = "likes" + 1,
//	//   "engagement" = "engagement" + 1,
//	//   "maxScore"   = GREATEST("maxScore", $1)
//	// WHERE "id" = $2
//	//   AND <tenant predicate>
//	//   AND <guard predicate>
//
// Patch is atomic at the row level — concurrent patches against
// the same row serialise on row locks, no lost updates. Returns
// drops.Result so callers can read rows-affected (e.g. to
// distinguish "row missing" from "row updated" without an
// extra SELECT).

// PatchOp describes one SET assignment in a Patch. Construct one
// with Inc / Dec / SetVal / SetIfGreater / SetIfLess /
// SetIfChanged.
type PatchOp interface {
	column() *Column
	writeValue(b *drops.Builder)
}

// Patch issues an UPDATE that applies ops to the row whose PK
// equals id. Honours the entity's tenant scope, authorisation
// guard, and audit log (the audit row's payload is empty since
// the post-update state isn't fetched; callers needing post-row
// snapshots should use Update with a refreshed struct).
//
// Returns the result so callers can detect "no row matched"
// without an additional SELECT.
func (e *Entity[T]) Patch(db *DB, ctx context.Context, id any, ops ...PatchOp) (drops.Result, error) {
	if len(ops) == 0 {
		return nil, errors.New("drops/pg: Patch requires at least one operation")
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
	doPatch := func(tx *DB) error {
		upd := tx.Update(e.table)
		for _, op := range ops {
			upd.Set(op)
		}
		upd.Where(Eq(e.pk, id))
		if tenantPred != nil {
			upd.Where(tenantPred)
		}
		if guardPred != nil {
			upd.Where(guardPred)
		}
		r, err := upd.Exec(ctx)
		if err != nil {
			return err
		}
		res = r
		return e.recordAudit(tx, ctx, "patch", nil, id)
	}
	if e.audit != nil {
		err = db.InTx(ctx, doPatch)
	} else {
		err = doPatch(db)
	}
	if err == nil {
		// Invalidate the cached entry — the patched value is
		// computed server-side and we don't have it locally.
		e.invalidatePK(ctx, id)
	}
	return res, err
}

// ----------------------------------------------------------------------
// PatchOp constructors
// ----------------------------------------------------------------------

// Inc emits "col = col + delta". For unsigned counters use a
// non-negative delta; for decrements pass a negative one or use
// Dec.
func Inc[T number](col *Col[T], delta T) PatchOp {
	return &incOp[T]{col: col.Column, delta: delta}
}

// Dec is shorthand for Inc(col, -delta).
func Dec[T number](col *Col[T], delta T) PatchOp {
	return &incOp[T]{col: col.Column, delta: -delta}
}

// number is the constraint for numeric counter ops.
type number interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 |
		~float32 | ~float64
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

// Set is a typed plain assignment — equivalent to
// (*Col[T]).Val but usable in the Patch op list alongside the
// SQL-side ops.
func Set[T any](col *Col[T], v T) PatchOp {
	return &setValOp[T]{col: col.Column, val: v}
}

type setValOp[T any] struct {
	col *Column
	val T
}

func (o *setValOp[T]) column() *Column            { return o.col }
func (o *setValOp[T]) writeValue(b *drops.Builder) { b.AddArg(o.val) }

// SetIfGreater emits "col = GREATEST(col, $1)" — only raises the
// value, never lowers it. Useful for high-watermark counters
// (max score, last-seen timestamp).
func SetIfGreater[T any](col *Col[T], v T) PatchOp {
	return &monotonicOp[T]{col: col.Column, val: v, fn: "GREATEST"}
}

// SetIfLess emits "col = LEAST(col, $1)" — only lowers the
// value, never raises it. Useful for low-watermark counters.
func SetIfLess[T any](col *Col[T], v T) PatchOp {
	return &monotonicOp[T]{col: col.Column, val: v, fn: "LEAST"}
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

// SetIfChanged emits "col = $1" only when $1 differs from the
// current value — implemented as a CASE WHEN so the row is
// touched (and triggers fire) even if no change happens.
// Useful when the surrounding entity has hook-driven side
// effects you don't want to elide silently.
func SetIfChanged[T any](col *Col[T], v T) PatchOp {
	return &ifChangedOp[T]{col: col.Column, val: v}
}

type ifChangedOp[T any] struct {
	col *Column
	val T
}

func (o *ifChangedOp[T]) column() *Column { return o.col }
func (o *ifChangedOp[T]) writeValue(b *drops.Builder) {
	// CASE WHEN col IS DISTINCT FROM $N THEN $N+1 ELSE col END
	// — the value is bound twice (drops.Builder allocates
	// distinct placeholders) which is fine for PostgreSQL.
	b.WriteString("CASE WHEN ")
	o.col.WriteSQL(b)
	b.WriteString(" IS DISTINCT FROM ")
	b.AddArg(o.val)
	b.WriteString(" THEN ")
	b.AddArg(o.val)
	b.WriteString(" ELSE ")
	o.col.WriteSQL(b)
	b.WriteString(" END")
}
