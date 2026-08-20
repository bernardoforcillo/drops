package mysql

import (
	"context"
	"errors"

	"github.com/bernardoforcillo/drops"
)

// High-traffic counters — like counts, view counts, engagement scores
// — are hit from thousands of goroutines at once. Reading the value
// out, adding one, and writing it back round-trips through the
// application and races on it; "UPDATE count = count + 1" is the right
// shape but tedious to write through an Entity, which wants a struct.
//
// Patch sidesteps both: one UPDATE carrying arbitrary server-side
// assignments, bound to the entity's primary key.
//
//	PostEntity.Patch(db, ctx, postID,
//	    mysql.Inc(PostLikes, 1),
//	    mysql.Inc(PostEngagement, 1),
//	    mysql.SetIfGreater(PostMaxScore, currentScore),
//	)
//	// UPDATE `posts` SET
//	//   `likes`      = `posts`.`likes` + ?,
//	//   `engagement` = `posts`.`engagement` + ?,
//	//   `maxScore`   = GREATEST(`posts`.`maxScore`, ?)
//	// WHERE (`posts`.`id` = ?)
//
// Ported from drops/pg's patch.go. Two things are genuinely different
// under MySQL:
//
// InnoDB's default isolation is REPEATABLE READ rather than
// PostgreSQL's READ COMMITTED, but that changes nothing here: an
// UPDATE takes its rows at READ COMMITTED regardless of the level, so
// concurrent patches serialise on the row lock and none is lost. What
// it does change is what the *same transaction* sees afterwards —
// a SELECT following the patch inside one transaction reads the
// snapshot, so re-read through the same transaction, not a new one.
//
// [SetIfChanged] cannot use PostgreSQL's IS DISTINCT FROM, which
// neither server has. MySQL's <=> is what it renders, and <=> is
// null-safe but not collation-blind — so on a text column it decides
// "changed" by the column's collation rather than byte for byte. Read
// its doc before using it on one.

// PatchOp describes one SET assignment in a Patch. Construct one with
// [Inc], [Dec], [Set], [SetIfGreater], [SetIfLess] or [SetIfChanged].
//
// It is an alias of [ColumnValue] rather than a separate interface, so
// the same op also goes straight into (*UpdateBuilder).Set and an
// INSERT's value list. pg keeps the two apart; there is no reason to
// here, since the shape is identical and the alias is what lets one
// helper serve both.
type PatchOp = ColumnValue

// Patch issues an UPDATE applying ops to the row whose primary key
// equals id. It returns the result, so a caller can tell "no such row"
// from "row updated" without a second SELECT.
func (e *Entity[T]) Patch(db *DB, ctx context.Context, id any, ops ...PatchOp) (drops.Result, error) {
	return e.PatchKey(db, ctx, []any{id}, ops...)
}

// PatchKey is [Entity.Patch] for a composite primary key. Patch cannot
// take a variadic key because its operations are already variadic, so
// the multi-column form spells the key as a slice:
//
//	MembershipEntity.PatchKey(db, ctx, []any{orgID, userID},
//	    mysql.Inc(SeatCount, 1))
func (e *Entity[T]) PatchKey(db *DB, ctx context.Context, key []any, ops ...PatchOp) (drops.Result, error) {
	if len(ops) == 0 {
		return nil, errors.New("drops/mysql: Patch requires at least one operation")
	}
	pred, err := e.pkPredicate(key)
	if err != nil {
		return nil, err
	}
	upd := db.Update(e.table).Set(ops...).Where(pred)
	return upd.Exec(ctx)
}

// ----------------------------------------------------------------------
// PatchOp constructors
// ----------------------------------------------------------------------

// number is the constraint for numeric counter ops.
type number interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 |
		~float32 | ~float64
}

// Inc emits "col = col + delta".
//
// On an UNSIGNED column a delta that would take the value below zero
// is error 1690, "BIGINT UNSIGNED value is out of range", on both
// servers and under every sql_mode. Strict mode has nothing to do with
// it: what fails is the unsigned arithmetic, before any value reaches
// the column, so the statement raises rather than storing anything.
// (Strict mode governs a different case — assigning a negative
// *literal*, which raises 1264 under it and clamps to zero without.)
//
// So [Dec] on an unsigned counter needs a signed column, or a guard in
// the WHERE clause that keeps the subtraction from going negative:
//
//	ent.PatchKey(db, ctx, key, mysql.Dec(Seats, 1))   // 1690 at zero
//
// Wrapping the arithmetic does not help, because the arithmetic is
// what raises: GREATEST(col - 1, 0) fails at the same point col - 1
// does.
func Inc[T number](col *Col[T], delta T) PatchOp {
	return &incOp[T]{col: col.Column, delta: delta, op: '+'}
}

// Dec emits "col = col - delta". Pass the amount to subtract as a
// positive number. See [Inc] about unsigned columns.
//
// It renders a subtraction rather than negating the delta and adding
// it, which would be the shorter spelling and is wrong for half of
// [number]: -delta on an unsigned type wraps instead of going
// negative, so Dec(col, uint64(5)) would bind 18446744073709551611 and
// the counter would climb. That renders perfectly and only a server
// can see it, which is why there is an integration test for it.
func Dec[T number](col *Col[T], delta T) PatchOp {
	return &incOp[T]{col: col.Column, delta: delta, op: '-'}
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

// Set is a typed plain assignment — the same thing (*Col[T]).Val
// produces, named so it reads alongside the other patch operations.
func Set[T any](col *Col[T], v T) PatchOp {
	return columnValue{col: col.Column, val: v}
}

// SetIfGreater emits "col = GREATEST(col, ?)" — raises the value,
// never lowers it. For high-watermark counters: a max score, a
// last-seen timestamp.
//
// GREATEST returns NULL if any argument is NULL, so against a nullable
// column this sets NULL rather than the new value the first time. Wrap
// the column in a default, or declare it NOT NULL.
func SetIfGreater[T any](col *Col[T], v T) PatchOp {
	return &monotonicOp[T]{col: col.Column, val: v, fn: "GREATEST"}
}

// SetIfLess emits "col = LEAST(col, ?)" — lowers the value, never
// raises it. Same NULL caveat as [SetIfGreater].
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

// SetIfChanged emits "col = ?" only when the value differs from the
// current one, as a CASE so the row is still touched — an ON UPDATE
// CURRENT_TIMESTAMP column, though, is not: MySQL only fires that when
// a value actually changes, which is the point of using this rather
// than a plain assignment.
//
// The comparison is MySQL's null-safe <=>, standing in for
// PostgreSQL's IS DISTINCT FROM, which neither MySQL nor MariaDB has.
// Without it a NULL column compared to a non-NULL value yields NULL,
// the CASE falls to its ELSE, and the assignment quietly never
// happens.
//
// # It is not IS DISTINCT FROM on a text column
//
// <=> fixes the null half and inherits the collation half, and there
// the two dialects part company. PostgreSQL compares text under a
// deterministic collation, so 'ada' IS DISTINCT FROM 'ADA' is true and
// the row is written. MySQL compares under the column's collation,
// which by default is case-insensitive, accent-insensitive, and — on
// MariaDB's utf8mb4_general_ci — space-padding. Under it 'ada' <=>
// 'ADA' is true, and so are 'e' <=> 'é' and 'a' <=> 'a  '. So
// SetIfChanged silently declines to write a change of case, of accent,
// or of trailing space that PostgreSQL would have written, and a plain
// assignment on the same column would have written too.
//
// Where that matters — normalising a display name, correcting an
// accent — either assign with [Set] and let the server decide whether
// the row changed, or compare under a binary collation:
//
//	mysql.SetExpr(UserName, drops.Raw(
//	    "CASE WHEN NOT (`users`.`name` COLLATE utf8mb4_bin <=> ?) THEN ? ELSE `users`.`name` END"))
//
// drops does not pick the binary collation for you, because on a
// numeric, date or binary column there is no collation to pick and the
// operator is already exact.
func SetIfChanged[T any](col *Col[T], v T) PatchOp {
	return &ifChangedOp[T]{col: col.Column, val: v}
}

type ifChangedOp[T any] struct {
	col *Column
	val T
}

func (o *ifChangedOp[T]) column() *Column { return o.col }
func (o *ifChangedOp[T]) writeValue(b *drops.Builder) {
	// CASE WHEN NOT (col <=> ?) THEN ? ELSE col END. The value binds
	// twice; MySQL's placeholders are positional, so there is no way
	// to name one parameter in two places.
	b.WriteString("CASE WHEN NOT (")
	o.col.WriteSQL(b)
	b.WriteString(" <=> ")
	b.AddArg(o.val)
	b.WriteString(") THEN ")
	b.AddArg(o.val)
	b.WriteString(" ELSE ")
	o.col.WriteSQL(b)
	b.WriteString(" END")
}

// SetExpr assigns an arbitrary expression to a column inside a Patch —
// the escape hatch for an operation the named ones do not cover, such
// as a JSON_SET or a string append.
//
//	UserEntity.Patch(db, ctx, id,
//	    mysql.SetExpr(UserPrefs, mysql.JSONSet(UserPrefs, "$.theme", "dark")))
func SetExpr(col ColRef, e drops.Expression) PatchOp {
	return exprValue{col: col.col(), expr: e}
}
