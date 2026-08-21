package sqlite

import (
	"context"
	"time"

	"github.com/bernardoforcillo/drops"
)

// SoftDeleteCols holds the column(s) a SoftDelete registers.
type SoftDeleteCols struct {
	// DeletedAt is the nullable timestamp column: NULL means live, a
	// timestamp means soft-deleted.
	DeletedAt *Col[time.Time]
}

// SoftDelete registers a nullable "deletedAt" column on t plus a
// DefaultFilter (deletedAt IS NULL), mirroring drops/pg's SoftDeleteMixin.
// After this, every Select / Update / Delete against t automatically
// excludes soft-deleted rows unless the builder calls Unscoped.
//
// Declare it during schema setup, before NewEntity, so the entity's
// column mapping includes deletedAt:
//
//	posts := sqlite.NewTable("posts")
//	sqlite.Add(posts, sqlite.BigInt("id").PrimaryKey())
//	sd := sqlite.SoftDelete(posts)
//	postEntity := sqlite.NewEntity[Post](posts)
//	...
//	postEntity.SoftDeleteByID(db, ctx, id, sd) // hide the row
//	postEntity.Restore(db, ctx, id, sd)        // bring it back
func SoftDelete(t *Table) SoftDeleteCols {
	col := Add(t, Timestamp("deletedAt", false).Managed())
	t.DefaultFilter(col.IsNull())
	return SoftDeleteCols{DeletedAt: col}
}

// SoftDeleteByID marks the row whose primary key equals id as
// soft-deleted by setting deletedAt to CURRENT_TIMESTAMP. It runs
// Unscoped so it also works idempotently on an already-hidden row — and
// then puts the table's context filters back, so it stays inside the
// ctx tenant. See markDeletedAt.
func (e *Entity[T]) SoftDeleteByID(db *DB, ctx context.Context, id any, sd SoftDeleteCols) (drops.Result, error) {
	return e.markDeletedAt(db, ctx, id, sd, drops.Raw("CURRENT_TIMESTAMP"))
}

// Restore clears deletedAt (SET deletedAt = NULL) for the row whose
// primary key equals id, un-hiding it. Runs Unscoped because the row is,
// by definition, currently filtered out — and keeps the tenant, for the
// reason [Entity.SoftDeleteByID] does.
func (e *Entity[T]) Restore(db *DB, ctx context.Context, id any, sd SoftDeleteCols) (drops.Result, error) {
	return e.markDeletedAt(db, ctx, id, sd, drops.Raw("NULL"))
}

// markDeletedAt is the shared body of SoftDeleteByID and Restore: an
// UPDATE of the soft-delete column, addressed by primary key, that sees
// hidden rows and only this tenant's.
//
// Both halves of that need saying, because they pull against each
// other. Unscoped is what lets these two methods work at all: the
// table's DefaultFilter is "deletedAt IS NULL", so a scoped UPDATE
// could never touch an already-hidden row and Restore would be a no-op
// by construction.
//
// But Unscoped is statement-wide — deliberately, see
// [SelectBuilder.Unscoped] — so it clears the table's CONTEXT filters
// too, and those are where the tenant axis and the authorization guard
// live. An id is unique across the whole table rather than per tenant,
// so a plain Unscoped().Where(id = ?) hides or restores whichever
// tenant's row happens to carry that id, on a ctx carrying no tenant at
// all, reported as one row affected. That is a cross-tenant write
// through a method whose whole purpose is to be the safe alternative to
// DELETE.
//
// So the filters are resolved and AND-ed back explicitly. That is the
// same escape the Unscoped doc points callers at — say what this
// statement's authority is, then say which rows it means — written here
// once rather than left for every caller of these two methods to
// remember. A ctx with no tenant is refused before anything is sent,
// because resolveContextFilters refuses.
func (e *Entity[T]) markDeletedAt(db *DB, ctx context.Context, id any, sd SoftDeleteCols, val drops.Expression) (drops.Result, error) {
	pred, err := e.pkPredicate([]any{id})
	if err != nil {
		return nil, err
	}
	scope, err := e.table.resolveContextFilters(ctx)
	if err != nil {
		return nil, err
	}
	upd := db.Update(e.table).Unscoped().
		SetExpr(sd.DeletedAt.Column, val).
		Where(pred)
	upd.Where(scope...)
	return upd.Exec(ctx)
}
