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

// SoftDelete registers a nullable "deletedAt" column on t plus a filter
// named [FilterSoftDelete] (deletedAt IS NULL), mirroring drops/pg's
// SoftDeleteMixin. After this, every Select / Update / Delete against t
// automatically excludes soft-deleted rows unless the builder names the
// guard — IgnoreFilters(sqlite.FilterSoftDelete) — or drops every
// filter at once with Unscoped.
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
	col := Add(t, Timestamp("deletedAt", false).Nullable().Managed())
	t.AddFilter(FilterSoftDelete, col.IsNull())
	return SoftDeleteCols{DeletedAt: col}
}

// SoftDeleteByID marks the row whose primary key equals id as
// soft-deleted by setting deletedAt to CURRENT_TIMESTAMP. It steps
// around the soft-delete guard by name so it also works idempotently on
// an already-hidden row — and only that guard, so a tenancy or
// authorisation filter on the same table still narrows which row this
// statement is allowed to touch.
func (e *Entity[T]) SoftDeleteByID(db *DB, ctx context.Context, id any, sd SoftDeleteCols) (drops.Result, error) {
	pred, err := e.pkPredicate([]any{id})
	if err != nil {
		return nil, err
	}
	return db.Update(e.table).IgnoreFilters(FilterSoftDelete).
		SetExpr(sd.DeletedAt.Column, drops.Raw("CURRENT_TIMESTAMP")).
		Where(pred).
		Exec(ctx)
}

// Restore clears deletedAt (SET deletedAt = NULL) for the row whose
// primary key equals id, un-hiding it. It ignores the soft-delete guard
// — the row is, by definition, currently filtered out by it — and
// nothing else, so the table's other filters still apply.
func (e *Entity[T]) Restore(db *DB, ctx context.Context, id any, sd SoftDeleteCols) (drops.Result, error) {
	pred, err := e.pkPredicate([]any{id})
	if err != nil {
		return nil, err
	}
	// SetNull rather than the literal token: this is the one
	// statement in drops that used to have to splice a NULL, because
	// nothing typed could produce one.
	return db.Update(e.table).IgnoreFilters(FilterSoftDelete).
		Set(sd.DeletedAt.SetNull()).
		Where(pred).
		Exec(ctx)
}
