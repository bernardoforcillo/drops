package sqlite

import "time"

// Templates are reusable groups of columns applied to a table. They are
// ordinary functions that accept a *Table, register columns with Add,
// and return a struct of typed *Col[T] handles — the same pattern as
// drops/pg, keeping schema declarations declarative and DRY.
//
//	var (
//	    Users    = sqlite.NewTable("users")
//	    UserID   = sqlite.Add(Users, sqlite.BigInt("id").PrimaryKey())
//	    UserTS   = sqlite.Timestamps(Users)   // createdAt, updatedAt
//	    UserSD   = sqlite.SoftDelete(Users)   // deletedAt
//	    UserName = sqlite.Add(Users, sqlite.Text("name").NotNull())
//	)

// TimestampsCols holds the typed handles created by Timestamps.
type TimestampsCols struct {
	CreatedAt *Col[time.Time]
	UpdatedAt *Col[time.Time]
}

// Timestamps appends NOT NULL "createdAt" / "updatedAt" DATETIME columns
// defaulting to CURRENT_TIMESTAMP to t.
func Timestamps(t *Table) TimestampsCols {
	return TimestampsCols{
		CreatedAt: Add(t, Timestamp("createdAt", false).NotNull().Default("CURRENT_TIMESTAMP").Managed()),
		UpdatedAt: Add(t, Timestamp("updatedAt", false).NotNull().Default("CURRENT_TIMESTAMP").Managed()),
	}
}

// AuditCols holds the typed handles created by Audit.
type AuditCols[T any] struct {
	CreatedBy *Col[T]
	UpdatedBy *Col[T]
}

// Audit appends nullable "createdBy" / "updatedBy" columns to t and
// declares foreign keys against target — typically a users.id PK. The
// referencing columns adopt the target column's declared type.
func Audit[T any](t *Table, target *Col[T]) AuditCols[T] {
	refType := target.Type().TypeSQL()
	return AuditCols[T]{
		CreatedBy: Add(t, Custom[T]("createdBy", refType).References(target)),
		UpdatedBy: Add(t, Custom[T]("updatedBy", refType).References(target)),
	}
}

// UUIDPrimaryKeyCols holds the typed handle created by UUIDPrimaryKey.
type UUIDPrimaryKeyCols struct {
	ID *Col[string]
}

// UUIDPrimaryKey appends a TEXT "id" PRIMARY KEY column that defaults to
// a random RFC-4122 v4 UUID. SQLite has no gen_random_uuid(), so the
// default is the canonical randomblob() expression (parenthesised, as
// SQLite requires for expression defaults).
func UUIDPrimaryKey(t *Table) UUIDPrimaryKeyCols {
	const uuidV4 = `(lower(hex(randomblob(4)) || '-' || hex(randomblob(2)) || '-4' || ` +
		`substr(hex(randomblob(2)),2) || '-' || substr('89ab',abs(random()) % 4 + 1,1) || ` +
		`substr(hex(randomblob(2)),2) || '-' || hex(randomblob(6))))`
	return UUIDPrimaryKeyCols{
		ID: Add(t, UUID("id").PrimaryKey().Default(uuidV4)),
	}
}
