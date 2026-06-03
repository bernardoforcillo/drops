package clickhouse

import "time"

// Templates are reusable groups of columns applied to a table. A
// template is an ordinary function that accepts a *Table, registers
// columns via Add, and returns a struct of typed *Col[T] handles. The
// recipe keeps schema declarations declarative and reusable:
//
//	var (
//	    Events   = clickhouse.NewTable("events").Engine(clickhouse.MergeTree())
//	    EventID  = clickhouse.Add(Events, clickhouse.UUID("id"))
//	    EventTS  = clickhouse.Timestamps(Events) // created_at, updated_at
//	    EventName = clickhouse.Add(Events, clickhouse.String("name"))
//	)
//
// External libraries follow the same recipe to expose custom templates
// — there is no registration step, only a function returning typed
// column handles.

// TimestampsCols holds the typed handles created by Timestamps.
type TimestampsCols struct {
	CreatedAt *Col[time.Time]
	UpdatedAt *Col[time.Time]
}

// Timestamps appends "created_at" and "updated_at" DateTime columns
// defaulting to now() to t.
func Timestamps(t *Table) TimestampsCols {
	return TimestampsCols{
		CreatedAt: Add(t, DateTime("created_at", "").Default("now()")),
		UpdatedAt: Add(t, DateTime("updated_at", "").Default("now()")),
	}
}

// SoftDeleteCols holds the typed handle created by SoftDelete.
type SoftDeleteCols struct {
	DeletedAt *Col[time.Time]
}

// SoftDelete appends a Nullable(DateTime) "deleted_at" column to t. A
// row is treated as live while deleted_at IS NULL.
func SoftDelete(t *Table) SoftDeleteCols {
	return SoftDeleteCols{
		DeletedAt: Add(t, DateTime("deleted_at", "").Nullable()),
	}
}

// AuditCols holds the typed handles created by Audit. The type
// parameter mirrors the type of the supplied identity column.
type AuditCols[T any] struct {
	CreatedBy *Col[T]
	UpdatedBy *Col[T]
}

// Audit appends "created_by" and "updated_by" columns to t, mirroring
// target's SQL type. ClickHouse has no foreign-key enforcement, so the
// columns are plain scalars; the typed handles let queries still
// compare them against target safely.
func Audit[T any](t *Table, target *Col[T]) AuditCols[T] {
	typeSQL := target.Type().TypeSQL()
	return AuditCols[T]{
		CreatedBy: Add(t, Custom[T]("created_by", typeSQL)),
		UpdatedBy: Add(t, Custom[T]("updated_by", typeSQL)),
	}
}

// UUIDPrimaryKeyCols holds the typed handle created by UUIDPrimaryKey.
type UUIDPrimaryKeyCols struct {
	ID *Col[string]
}

// UUIDPrimaryKey appends an "id" UUID column defaulting to
// generateUUIDv4(). ClickHouse has no primary-key constraint; pair the
// column with the table's ORDER BY (or PRIMARY KEY clause on
// MergeTree-family engines) to make it the row identifier.
func UUIDPrimaryKey(t *Table) UUIDPrimaryKeyCols {
	return UUIDPrimaryKeyCols{
		ID: Add(t, UUID("id").Default("generateUUIDv4()")),
	}
}
