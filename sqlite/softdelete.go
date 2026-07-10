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
	col := Add(t, Timestamp("deletedAt", false))
	t.DefaultFilter(col.IsNull())
	return SoftDeleteCols{DeletedAt: col}
}

// SoftDeleteByID marks the row whose primary key equals id as
// soft-deleted by setting deletedAt to CURRENT_TIMESTAMP. It runs
// Unscoped so it also works idempotently on an already-hidden row.
func (e *Entity[T]) SoftDeleteByID(db *DB, ctx context.Context, id any, sd SoftDeleteCols) (drops.Result, error) {
	return db.Update(e.table).Unscoped().
		SetExpr(sd.DeletedAt.Column, drops.Raw("CURRENT_TIMESTAMP")).
		Where(cmp(e.pk, "=", id)).
		Exec(ctx)
}

// Restore clears deletedAt (SET deletedAt = NULL) for the row whose
// primary key equals id, un-hiding it. Runs Unscoped because the row is,
// by definition, currently filtered out.
func (e *Entity[T]) Restore(db *DB, ctx context.Context, id any, sd SoftDeleteCols) (drops.Result, error) {
	return db.Update(e.table).Unscoped().
		SetExpr(sd.DeletedAt.Column, drops.Raw("NULL")).
		Where(cmp(e.pk, "=", id)).
		Exec(ctx)
}
