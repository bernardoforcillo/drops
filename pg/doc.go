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
package pg
