package mysql

import "time"

// Templates are reusable groups of columns applied to a table. They are
// ordinary functions that accept a *Table, register columns with Add,
// and return a struct of typed *Col[T] handles — the same pattern as
// drops/pg, keeping schema declarations declarative and DRY.
//
//	var (
//	    Users    = mysql.NewTable("users")
//	    UserID   = mysql.Add(Users, mysql.BigInt("id").PrimaryKey())
//	    UserTS   = mysql.Timestamps(Users)   // createdAt, updatedAt
//	    UserSD   = mysql.SoftDelete(Users)   // deletedAt
//	    UserName = mysql.Add(Users, mysql.Text("name").NotNull())
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
		CreatedBy: Add(t, Custom[T]("createdBy", refType).Nullable().References(target)),
		UpdatedBy: Add(t, Custom[T]("updatedBy", refType).Nullable().References(target)),
	}
}

// UUIDPrimaryKeyCols holds the typed handle created by UUIDPrimaryKey.
type UUIDPrimaryKeyCols struct {
	ID *Col[string]
}

// UUIDPrimaryKey appends a TEXT "id" PRIMARY KEY column that defaults to
// a random RFC-4122 v4 UUID. MySQL has no gen_random_uuid(), so the
// default is the canonical randomblob() expression (parenthesised, as
// MySQL requires for expression defaults).
func UUIDPrimaryKey(t *Table) UUIDPrimaryKeyCols {
	// MySQL generates a UUID with UUID(); the expression drops/sqlite
	// uses here builds one out of randomblob and hex because SQLite has
	// no such function, and neither randomblob nor SQLite's || string
	// concatenation exists on MySQL — the ported default was a CREATE
	// TABLE the server would reject.
	//
	// The parentheses are not decoration. MySQL 8.0.13+ accepts a
	// function in DEFAULT only as a parenthesised expression, and
	// MariaDB accepts the same spelling, so this is the one form the
	// whole family takes.
	//
	// UUID() is version 1: time-and-MAC rather than random, so it is
	// sequential-ish and does not carry v4's index-fragmentation
	// problem, but it does encode the host's MAC address. A schema that
	// must not leak that should declare its own default —
	// (UUID_v4()) on MariaDB 10.10+, or generate the value in Go.
	const uuidDefault = "(UUID())"
	return UUIDPrimaryKeyCols{
		ID: Add(t, UUID("id").PrimaryKey().Default(uuidDefault)),
	}
}
