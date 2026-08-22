package sqlite

import (
	"context"
	"errors"
	"fmt"

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
// The op set honours the table's automatic predicates — the tenant axis
// (see ScopeByTenant), an authorization guard, a soft-delete filter —
// because it runs through UpdateBuilder.Exec like any other write.
// Returns drops.Result so callers can read rows-affected to distinguish
// "row missing" from "row updated" without a follow-up SELECT.

// PatchOp describes one SET assignment in a Patch. It is exactly the
// sqlite ColumnValue shape, so patch ops slot directly into
// UpdateBuilder.Set. Construct one with Inc / Dec / Set /
// SetIfGreater / SetIfLess / SetIfChanged.
type PatchOp = ColumnValue

// ErrForeignColumn is returned by [Entity.PatchKey] when an op names a
// column that is not a column of the entity's table.
//
// It is deliberately not [ErrTenantMismatch] and does not wrap it. A
// handle taken off another table object is a bug whatever the column
// holds — the statement it renders assigns a relation the query does
// not name — and reporting it as a tenant problem would send the
// caller to their tenancy rather than to the import that handed them
// the wrong handle. That the tenant axis is one of the things this
// refusal protects is a consequence of the rule, not its definition.
//
// Match it with errors.Is. The message names the handle's own table as
// well as the entity's, because "tenantId" alone reads as the right
// column and which table it came from is the whole mistake.
var ErrForeignColumn = errors.New("drops/sqlite: patch op names a column of another table")

// Patch applies ops to the row identified by id.
//
// An op naming the tenant column is [ErrTenantMismatch]: the axis is
// what addresses the row, never something a patch assigns. An op
// naming a column of some OTHER table is [ErrForeignColumn], whatever
// the column is — including the one that renders as this table's
// tenant axis.
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
	// Every op has to name a column of the entity's own table, and this
	// runs before the axis check below because it is what makes the
	// axis check mean anything.
	//
	// The SET list renders the bare column name, so a handle for the
	// same-named column of a DIFFERENT table object renders exactly
	// like this table's own and the server writes the row this UPDATE
	// addresses — while [Column.key], which collapses alias copies onto
	// the column they were declared as, calls the two handles
	// strangers. The axis check compares by key, so a foreign
	// OtherTable.TenantID walked past it and rendered
	// SET "tenantId" = ? beside a WHERE clause still addressing the
	// ctx tenant: the transfer that check exists to refuse, one
	// character away in any schema whose codegen gives every table its
	// own <Table>Cols.TenantID.
	//
	// The rule is wider than the axis on purpose, because the mistake
	// is. An op naming another table's column is a bug whatever the
	// column holds — nothing in the statement names that relation — so
	// it is refused as one, and the axis case stops being reachable as
	// a side effect. See [ErrForeignColumn].
	for _, op := range ops {
		if !e.ownsColumn(op.column()) {
			return nil, fmt.Errorf("%w: %s is not a column of %q",
				ErrForeignColumn, columnPath(op.column()), e.table.Name())
		}
	}
	// The tenant column is an axis, never an assignment. An op naming
	// it renders SET "tenantId" = ? beside a WHERE clause that still
	// addresses the ctx tenant — one statement handing the row to
	// somebody else, reported as one row affected, and this is the one
	// write in the package that never reads the row first, so nothing
	// downstream notices. Create and Update refuse that instruction
	// when it arrives on a struct; this is the same rule where an op
	// list can express it, which is where a handler building ops out
	// of the fields a request named will put it.
	//
	// An op assigning the ctx tenant's own value is refused too. It is
	// a no-op only by coincidence of the value, and a rule with an
	// exception in it is one a caller can be talked into satisfying.
	if axis, ok := e.tenantAxisColumn(); ok {
		for _, op := range ops {
			if op.column().key() == axis.key() {
				return nil, fmt.Errorf("%w: %s is an axis, not an assignment",
					ErrTenantMismatch, columnPath(axis))
			}
		}
	}
	pred, err := e.pkPredicate(key)
	if err != nil {
		return nil, err
	}
	// The tenant predicate and the authorization guard are the table's
	// context filters and are applied by Exec. This method used to
	// inject the tenant and not the guard, so a patch — a write, and
	// the one write in the package that never reads the row first —
	// reached rows a guarded entity would not have shown the caller.
	return db.Update(e.table).Set(ops...).Where(pred).Exec(ctx)
}

// ownsColumn reports whether c is one of the entity's table's own
// columns.
//
// The lookup goes through the table's name index and only then
// compares [Column.key], so an alias handle for one of the table's
// columns answers yes — an alias is a query-scope rename of the same
// column — while another table's column of the same name answers no.
// Asking key alone cannot tell those two apart: to key, a foreign
// handle is simply unequal to everything, which is indistinguishable
// from "not the column I asked about" and is how a handle that renders
// as the tenant axis came to be read as unrelated to it.
func (e *Entity[T]) ownsColumn(c *Column) bool {
	if c == nil {
		return false
	}
	own := e.table.Col(c.Name())
	return own != nil && own.key() == c.key()
}

// number is the constraint for numeric counter ops.
type number interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 |
		~float32 | ~float64
}

// Inc emits "col = col + delta".
func Inc[T number](col *Col[T], delta T) PatchOp {
	return &incOp[T]{col: col.Column, delta: delta, op: '+'}
}

// Dec emits "col = col - delta".
//
// It renders a subtraction rather than adding a negated delta, because
// [number] admits the unsigned types and negating an unsigned value
// wraps it: Dec(seats, uint32(5)) built as Inc(seats, -5) binds
// 4294967291 and renders a perfect addition of it, so a counter at 100
// asked to fall by five stores 4294967391. Silently — there is nothing
// wrong with the statement.
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
