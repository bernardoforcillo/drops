package clickhouse

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ColumnType describes the SQL type literal of a ClickHouse column —
// the bit that goes right after the column name in DDL.
type ColumnType interface {
	TypeSQL() string
}

// simpleType wraps a single token.
type simpleType string

func (s simpleType) TypeSQL() string { return string(s) }

// paramType renders <base>(<args>).
type paramType struct {
	base string
	args string
}

func (p paramType) TypeSQL() string {
	if p.args == "" {
		return p.base
	}
	return p.base + "(" + p.args + ")"
}

// Type-to-Go mapping (for typed Col[T] helpers below):
//
//	UInt8                       → uint8
//	UInt16                      → uint16
//	UInt32                      → uint32
//	UInt64                      → uint64
//	Int8                        → int8
//	Int16                       → int16
//	Int32                       → int32
//	Int64                       → int64
//	Float32                     → float32
//	Float64                     → float64
//	Decimal(P,S)                → string
//	Boolean                     → bool
//	String / FixedString(N)     → string
//	UUID                        → string
//	Date / Date32               → time.Time
//	DateTime / DateTime64(p)    → time.Time
//	JSON                        → json.RawMessage
//	Array(T) / Nullable(T) / LowCardinality(T)
//	                            → use TypeSlice / TypeNullable /
//	                              TypeLowCardinality + Custom[T]
//
// Wrappers (Array, Nullable, LowCardinality, Map, Tuple, Enum8/16) are
// applied as type-name modifiers since their Go-side representation is
// driver-dependent.

// String / fixed-width / number columns ------------------------------

func String(name string) *Col[string] { return newCol[string](name, simpleType("String")) }

// FixedString returns a FixedString(n) column. n must be > 0.
func FixedString(name string, n int) *Col[string] {
	if n <= 0 {
		n = 1
	}
	return newCol[string](name, paramType{base: "FixedString", args: fmt.Sprintf("%d", n)})
}

func Int8(name string) *Col[int8]   { return newCol[int8](name, simpleType("Int8")) }
func Int16(name string) *Col[int16] { return newCol[int16](name, simpleType("Int16")) }
func Int32(name string) *Col[int32] { return newCol[int32](name, simpleType("Int32")) }
func Int64(name string) *Col[int64] { return newCol[int64](name, simpleType("Int64")) }

func UInt8(name string) *Col[uint8]   { return newCol[uint8](name, simpleType("UInt8")) }
func UInt16(name string) *Col[uint16] { return newCol[uint16](name, simpleType("UInt16")) }
func UInt32(name string) *Col[uint32] { return newCol[uint32](name, simpleType("UInt32")) }
func UInt64(name string) *Col[uint64] { return newCol[uint64](name, simpleType("UInt64")) }

func Float32(name string) *Col[float32] { return newCol[float32](name, simpleType("Float32")) }
func Float64(name string) *Col[float64] { return newCol[float64](name, simpleType("Float64")) }

// Decimal(precision, scale) — represented as string to preserve the
// full arbitrary-precision value.
func Decimal(name string, precision, scale int) *Col[string] {
	return newCol[string](name, paramType{
		base: "Decimal",
		args: fmt.Sprintf("%d, %d", precision, scale),
	})
}

// Boolean ----------------------------------------------------------

func Bool(name string) *Col[bool] { return newCol[bool](name, simpleType("Bool")) }

// Date / time ------------------------------------------------------

func Date(name string) *Col[time.Time]   { return newCol[time.Time](name, simpleType("Date")) }
func Date32(name string) *Col[time.Time] { return newCol[time.Time](name, simpleType("Date32")) }

// DateTime renders DateTime, or DateTime('TZ') when tz is non-empty.
func DateTime(name, tz string) *Col[time.Time] {
	if tz == "" {
		return newCol[time.Time](name, simpleType("DateTime"))
	}
	return newCol[time.Time](name, paramType{
		base: "DateTime",
		args: "'" + strings.ReplaceAll(tz, "'", "''") + "'",
	})
}

// DateTime64 renders DateTime64(precision[, 'TZ']). precision is the
// sub-second precision (0..9).
func DateTime64(name string, precision int, tz string) *Col[time.Time] {
	args := fmt.Sprintf("%d", precision)
	if tz != "" {
		args += ", '" + strings.ReplaceAll(tz, "'", "''") + "'"
	}
	return newCol[time.Time](name, paramType{base: "DateTime64", args: args})
}

// Misc -------------------------------------------------------------

func UUID(name string) *Col[string] { return newCol[string](name, simpleType("UUID")) }
func JSON(name string) *Col[json.RawMessage] {
	return newCol[json.RawMessage](name, simpleType("JSON"))
}

// Custom creates a column with an arbitrary type literal — useful for
// IPv4/IPv6, AggregateFunction(...), or vendor types not covered here.
func Custom[T any](name, typeSQL string) *Col[T] {
	return newCol[T](name, simpleType(typeSQL))
}

// Type wrappers (Array, Nullable, LowCardinality, Map, Tuple, Enum) -

// TypeArray wraps an inner type as Array(T).
func TypeArray(inner ColumnType) ColumnType {
	return simpleType("Array(" + inner.TypeSQL() + ")")
}

// TypeNullable wraps an inner type as Nullable(T).
func TypeNullable(inner ColumnType) ColumnType {
	return simpleType("Nullable(" + inner.TypeSQL() + ")")
}

// TypeLowCardinality wraps an inner type as LowCardinality(T).
func TypeLowCardinality(inner ColumnType) ColumnType {
	return simpleType("LowCardinality(" + inner.TypeSQL() + ")")
}

// TypeMap renders Map(K, V).
func TypeMap(key, value ColumnType) ColumnType {
	return simpleType("Map(" + key.TypeSQL() + ", " + value.TypeSQL() + ")")
}

// TypeTuple renders Tuple(T1, T2, ...).
func TypeTuple(members ...ColumnType) ColumnType {
	parts := make([]string, len(members))
	for i, m := range members {
		parts[i] = m.TypeSQL()
	}
	return simpleType("Tuple(" + strings.Join(parts, ", ") + ")")
}

// TypeEnum8 / TypeEnum16 render the labelled enum types.
//
//	TypeEnum8(map[string]int8{"a": 1, "b": 2})
//
// The output order is sorted by value so the generated DDL is stable
// across runs.
func TypeEnum8(values map[string]int8) ColumnType {
	return simpleType(renderEnum("Enum8", enumPairs8(values)))
}

func TypeEnum16(values map[string]int16) ColumnType {
	return simpleType(renderEnum("Enum16", enumPairs16(values)))
}

type enumPair struct {
	label string
	val   int64
}

func renderEnum(base string, pairs []enumPair) string {
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = fmt.Sprintf("'%s' = %d",
			strings.ReplaceAll(p.label, "'", "''"), p.val)
	}
	return base + "(" + strings.Join(parts, ", ") + ")"
}

func enumPairs8(m map[string]int8) []enumPair {
	out := make([]enumPair, 0, len(m))
	for k, v := range m {
		out = append(out, enumPair{label: k, val: int64(v)})
	}
	sortPairs(out)
	return out
}

func enumPairs16(m map[string]int16) []enumPair {
	out := make([]enumPair, 0, len(m))
	for k, v := range m {
		out = append(out, enumPair{label: k, val: int64(v)})
	}
	sortPairs(out)
	return out
}

func sortPairs(s []enumPair) {
	// Simple insertion sort: enum vocabularies are tiny (a few dozen)
	// and avoiding "sort" keeps the import set lean here.
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1].val > s[j].val; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
