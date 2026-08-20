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
//	//   "likes"      = "posts"."likes" + $1,
//	//   "engagement" = "posts"."engagement" + $2,
//	//   "maxScore"   = GREATEST("posts"."maxScore", $3)
//	// WHERE ("posts"."id" = $4)
//	//   AND <tenant predicate>
//	//   AND <guard predicate>
//
// An op names its column on the right-hand side as well as the left,
// and qualified with the relation the handle was declared on — which
// is not restated against the relation the UPDATE names. So an entity
// built on an alias must be patched with the alias's own handles; the
// package-level column names a relation the statement does not have,
// and PostgreSQL answers 42P01.
//
// Patch is atomic at the row level — concurrent patches against
// the same row serialise on row locks, no lost updates. Returns
// drops.Result so callers can read rows-affected (e.g. to
// distinguish "row missing" from "row updated" without an
// extra SELECT).

// PatchOp describes one SET assignment in a Patch. Construct one
// with [Inc], [Dec], [Set], [SetIfGreater], [SetIfLess] or
// [SetIfChanged]. (Not SetVal, which is the sequence function.)
type PatchOp interface {
	column() *Column
	writeValue(b *drops.Builder)
}

// Patch issues an UPDATE that applies ops to the row whose PK
// equals id. Honours the entity's tenant scope and authorisation
// guard — both reach the statement as context filters on the table,
// resolved by Exec — and the audit log (the audit row's payload is
// empty since the post-update state isn't fetched; callers needing
// post-row snapshots should use Update with a refreshed struct).
//
// Returns the result so callers can detect "no row matched"
// without an additional SELECT.
func (e *Entity[T]) Patch(db *DB, ctx context.Context, id any, ops ...PatchOp) (drops.Result, error) {
	return e.PatchKey(db, ctx, []any{id}, ops...)
}

// PatchKey is [Entity.Patch] for a composite primary key. Patch
// cannot take a variadic key because its operations already are
// variadic, so the multi-column form spells the key as a slice:
//
//	MembershipEntity.PatchKey(db, ctx, []any{orgID, userID},
//	    pg.Inc(SeatCount, 1))
func (e *Entity[T]) PatchKey(db *DB, ctx context.Context, key []any, ops ...PatchOp) (drops.Result, error) {
	if len(ops) == 0 {
		return nil, errors.New("drops/pg: Patch requires at least one operation")
	}
	pred, err := e.pkPredicate(key)
	if err != nil {
		return nil, err
	}
	var res drops.Result
	doPatch := func(tx *DB) error {
		upd := tx.Update(e.table)
		for _, op := range ops {
			upd.Set(op)
		}
		upd.Where(pred)
		r, err := upd.Exec(ctx)
		if err != nil {
			return err
		}
		res = r
		return e.recordAudit(tx, ctx, "patch", nil, auditKey(key))
	}
	if e.audit != nil {
		err = db.InTx(ctx, doPatch)
	} else {
		err = doPatch(db)
	}
	if err == nil {
		// Invalidate the cached entry — the patched value is
		// computed server-side and we don't have it locally.
		e.invalidatePK(ctx, key)
	}
	return res, err
}

// ----------------------------------------------------------------------
// PatchOp constructors
// ----------------------------------------------------------------------

// Inc emits "col = col + delta".
//
// A NULL column is not a zero here: NULL + 1 is NULL, so an Inc
// against a nullable counter erases it instead of starting it. Declare
// a counter NOT NULL DEFAULT 0, which is what one wants anyway.
func Inc[T number](col *Col[T], delta T) PatchOp {
	return &incOp[T]{col: col.Column, delta: delta, op: '+'}
}

// Dec emits "col = col - delta". Pass the amount to subtract as a
// positive number.
//
// It renders a subtraction rather than negating the delta and adding
// it, which is the shorter spelling and is wrong for half of [number]:
// -delta on an unsigned type wraps instead of going negative, so
// Dec(col, uint32(5)) would bind 4294967291 and the counter would
// climb by four billion. PostgreSQL has no unsigned column type, but
// the type here is the Go handle's, and Custom[uint32] gives one over
// a bigint column — so the case is reachable, and silent, because the
// rendered statement is perfect and the server has nothing to object
// to.
//
// The column being signed, a subtraction is free to take the counter
// below zero and the server stores the negative (a Col[uint32] handle
// then fails to scan it back). A CHECK constraint is what makes it
// stop, raising 23514.
func Dec[T number](col *Col[T], delta T) PatchOp {
	return &incOp[T]{col: col.Column, delta: delta, op: '-'}
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
	op    byte // '+' or '-'
}

func (o *incOp[T]) column() *Column { return o.col }
func (o *incOp[T]) writeValue(b *drops.Builder) {
	o.col.WriteSQL(b)
	b.WriteByte(' ')
	b.WriteByte(o.op)
	b.WriteByte(' ')
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

func (o *setValOp[T]) column() *Column             { return o.col }
func (o *setValOp[T]) writeValue(b *drops.Builder) { b.AddArg(o.val) }

// SetIfGreater emits "col = GREATEST(col, $1)" — only raises the
// value, never lowers it. Useful for high-watermark counters
// (max score, last-seen timestamp).
//
// PostgreSQL's GREATEST ignores NULL arguments, so against a NULL
// column this stores the new value rather than NULL. MySQL's returns
// NULL, so a schema served by both dialects still wants the column
// NOT NULL.
func SetIfGreater[T any](col *Col[T], v T) PatchOp {
	return &monotonicOp[T]{col: col.Column, val: v, fn: "GREATEST"}
}

// SetIfLess emits "col = LEAST(col, $1)" — only lowers the
// value, never raises it. Useful for low-watermark counters. Same
// NULL behaviour as [SetIfGreater].
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
// current value, as a CASE over IS DISTINCT FROM. The operator is
// null-safe: a NULL column counts as different from a non-NULL value,
// where a plain <> would evaluate to NULL, fall to the ELSE, and
// silently never assign.
//
// It does not decide whether the row is written. PostgreSQL writes a
// new row version for every UPDATE whose WHERE matches, so the row is
// touched and AFTER UPDATE triggers fire whether or not the CASE
// picked the new value — a plain [Set] of the identical value does
// exactly the same. What the CASE decides is only which value lands.
//
// Under a deterministic collation — the default — IS DISTINCT FROM is
// byte-exact, so on such a column SetIfChanged stores what [Set] would
// have stored, always. The two part company on a column declared with
// a nondeterministic collation, where 'ada' IS DISTINCT FROM 'ADA' is
// false and SetIfChanged declines to write a change of case or accent
// that Set would have written. drops/mysql's version has that
// behaviour on every text column, its <=> comparing under the column's
// collation; here it takes an explicit nondeterministic collation to
// reach.
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
