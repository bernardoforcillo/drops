// Package pg provides PostgreSQL schema declarations and a fluent query
// builder for use with drops.
//
// Tables are declared with NewTable and a sequence of column constructors
// (Text, Integer, Serial, Boolean, Timestamp, UUID, JSONB, ...). Columns
// are themselves drops.Expressions and may be referenced anywhere a SQL
// fragment is expected.
//
// Queries are composed via the methods on DB (Select, Insert, Update,
// Delete). Each builder is immutable in spirit — methods return the
// builder to support chaining — and ends with an executor (All, One,
// Exec, Returning + Scan).
//
// Templates (Timestamps, SoftDelete, Audit, UUIDPrimaryKey) are reusable
// column groups that can be applied to any table — see template.go for
// the function-style pattern, mixin.go for the richer Mixin interface
// that also contributes indexes / lifecycle hooks / default filters.
// Libraries and applications expose their own templates by following
// either recipe.
//
// Lifecycle hooks (OnInsert / OnUpdate / OnDelete on Table) let
// templates extend INSERT / UPDATE / DELETE statements automatically.
// User-supplied values always win, so hooks are safe to register on
// shared tables. Default scopes (Table.DefaultFilter) apply a
// predicate to every SELECT / UPDATE / DELETE against the table
// unless the caller opts out with the builder's Unscoped() method —
// the mechanism behind soft-delete-aware queries.
//
// Entity[T] (see entity.go) binds a Go struct to a Table and exposes
// type-safe CRUD shortcuts: Get / Create / Update / Save / Delete /
// Query. It composes with lifecycle hooks and default scopes, so a
// SoftDelete mixin makes UserEntity.Delete() automatically rewrite
// into an UPDATE that flips deletedAt.
//
// Entity.Validate registers per-row validators that run before
// Create / Update / Save. A column marked with OptimisticLock turns
// Update into a guarded "AND version = current, SET version =
// version + 1" — mismatched versions return ErrStaleObject.
//
// Entity.SetFastScan plugs in a zero-reflection per-row scanner —
// typically the Scan<T> helper emitted by cmd/dropsgen. When set,
// Get / EntityQuery.All / EntityQuery.One skip the reflection path
// entirely. Eager-loaded relations (via With/WithRel) still need
// reflection, so the slow path kicks in automatically for those.
//
// AutoTable[T] / NewAutoEntity[T] derive a Table (and an Entity)
// from extended `drop:"col,primaryKey,autoIncrement,notNull,unique,default=...,version"`
// struct tags — the struct becomes the single source of truth, and
// a working ORM falls out of one declaration:
//
//	var UserEntity = pg.NewAutoEntity[User]("users")
package pg
