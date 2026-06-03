package pg

import "time"

// Templates are reusable groups of columns applied to a table. They are
// nothing more than ordinary functions that accept a *Table, register
// columns with Add, and return a struct of typed *Col[T] handles. The
// pattern keeps schema declarations declarative and DRY:
//
//	var (
//	    Users   = pg.NewTable("users")
//	    UserID  = pg.Add(Users, pg.BigSerial("id").PrimaryKey())
//	    UserTS  = pg.Timestamps(Users)   // created_at, updated_at
//	    UserSD  = pg.SoftDelete(Users)   // deleted_at
//	    UserName = pg.Add(Users, pg.Text("name").NotNull())
//	)
//
//	// Later, the typed handles are usable as ordinary columns:
//	db.Select(UserID, UserName, UserTS.CreatedAt).
//	    From(Users).
//	    Where(UserSD.DeletedAt.IsNull())
//
// External libraries and applications follow the same recipe to expose
// custom templates — there is no registration step, just a function:
//
//	type LocalisedCols struct {
//	    Locale *pg.Col[string]
//	}
//	func Localised(t *pg.Table) LocalisedCols {
//	    return LocalisedCols{
//	        Locale: pg.Add(t, pg.Varchar("locale", 8).NotNull().Default("'en'")),
//	    }
//	}

// TimestampsCols holds the typed handles created by Timestamps.
type TimestampsCols struct {
	CreatedAt *Col[time.Time]
	UpdatedAt *Col[time.Time]
}

// Timestamps appends NOT NULL "created_at" and "updated_at" TIMESTAMPTZ
// columns defaulting to now() to t.
func Timestamps(t *Table) TimestampsCols {
	return TimestampsCols{
		CreatedAt: Add(t, Timestamp("created_at", true).NotNull().Default("now()")),
		UpdatedAt: Add(t, Timestamp("updated_at", true).NotNull().Default("now()")),
	}
}

// SoftDeleteCols holds the typed handle created by SoftDelete.
type SoftDeleteCols struct {
	DeletedAt *Col[time.Time]
}

// SoftDelete appends a nullable "deleted_at" TIMESTAMPTZ column. A
// record is treated as live while deleted_at IS NULL.
func SoftDelete(t *Table) SoftDeleteCols {
	return SoftDeleteCols{
		DeletedAt: Add(t, Timestamp("deleted_at", true)),
	}
}

// AuditCols holds the typed handles created by Audit. The type
// parameter mirrors the referenced PK column.
type AuditCols[T any] struct {
	CreatedBy *Col[T]
	UpdatedBy *Col[T]
}

// Audit appends nullable "created_by" and "updated_by" columns to t and
// declares foreign keys against target — typically a users.id PK. The
// referencing columns' SQL type is derived from target; serial-family
// types are mapped to their underlying integer type (bigserial→bigint,
// serial→integer, smallserial→smallint).
func Audit[T any](t *Table, target *Col[T]) AuditCols[T] {
	refType := referenceTypeOf(target.Type().TypeSQL())
	return AuditCols[T]{
		CreatedBy: Add(t, Custom[T]("created_by", refType).References(target)),
		UpdatedBy: Add(t, Custom[T]("updated_by", refType).References(target)),
	}
}

// referenceTypeOf returns the SQL type a referencing column should
// declare when the target uses a serial-family type. Non-serial types
// are returned unchanged.
func referenceTypeOf(typeSQL string) string {
	switch typeSQL {
	case "bigserial":
		return "bigint"
	case "serial":
		return "integer"
	case "smallserial":
		return "smallint"
	}
	return typeSQL
}

// UUIDPrimaryKeyCols holds the typed handle created by UUIDPrimaryKey.
type UUIDPrimaryKeyCols struct {
	ID *Col[string]
}

// UUIDPrimaryKey appends an "id" UUID PRIMARY KEY column defaulting to
// gen_random_uuid() — the function shipped by the pgcrypto extension
// and built into PostgreSQL 13+.
func UUIDPrimaryKey(t *Table) UUIDPrimaryKeyCols {
	return UUIDPrimaryKeyCols{
		ID: Add(t, UUID("id").PrimaryKey().Default("gen_random_uuid()")),
	}
}
