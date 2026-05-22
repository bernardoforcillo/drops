package pg

import (
	"encoding/json"
	"fmt"
	"time"
)

// simpleType is a ColumnType whose SQL form is a single literal token.
type simpleType string

func (s simpleType) TypeSQL() string { return string(s) }

// parametrisedType is a ColumnType with a length/precision parameter.
type parametrisedType struct {
	base string
	args string
}

func (p parametrisedType) TypeSQL() string {
	if p.args == "" {
		return p.base
	}
	return p.base + "(" + p.args + ")"
}

// Type-to-Go mapping
//
//	smallint                 → int16
//	integer / serial         → int32
//	bigint / bigserial       → int64
//	real                     → float32
//	double precision         → float64
//	numeric                  → string
//	boolean                  → bool
//	text / varchar / char    → string
//	uuid                     → string
//	date / time / timestamp  → time.Time
//	json / jsonb             → json.RawMessage
//	bytea                    → []byte
//
// Nullability is represented orthogonally — the Go type is the value
// type; use IsNull / IsNotNull to test for NULL. Pointer wrappers can be
// declared via Custom[*string] when the application wants nullable
// scanning.

// Text columns -----------------------------------------------------------

func Text(name string) *Col[string] { return newCol[string](name, simpleType("text")) }

func Varchar(name string, n int) *Col[string] {
	if n <= 0 {
		return newCol[string](name, simpleType("varchar"))
	}
	return newCol[string](name, parametrisedType{base: "varchar", args: fmt.Sprintf("%d", n)})
}

func Char(name string, n int) *Col[string] {
	if n <= 0 {
		n = 1
	}
	return newCol[string](name, parametrisedType{base: "char", args: fmt.Sprintf("%d", n)})
}

// Numeric columns --------------------------------------------------------

func SmallInt(name string) *Col[int16]  { return newCol[int16](name, simpleType("smallint")) }
func Integer(name string) *Col[int32]   { return newCol[int32](name, simpleType("integer")) }
func BigInt(name string) *Col[int64]    { return newCol[int64](name, simpleType("bigint")) }
func Serial(name string) *Col[int32]    { return newCol[int32](name, simpleType("serial")) }
func BigSerial(name string) *Col[int64] { return newCol[int64](name, simpleType("bigserial")) }

func Real(name string) *Col[float32] { return newCol[float32](name, simpleType("real")) }
func DoublePrecision(name string) *Col[float64] {
	return newCol[float64](name, simpleType("double precision"))
}

// Numeric returns NUMERIC(precision, scale) — represented as string so
// arbitrary-precision values aren't truncated. precision=0 means
// unconstrained NUMERIC.
func Numeric(name string, precision, scale int) *Col[string] {
	if precision <= 0 {
		return newCol[string](name, simpleType("numeric"))
	}
	return newCol[string](name, parametrisedType{
		base: "numeric",
		args: fmt.Sprintf("%d,%d", precision, scale),
	})
}

// Boolean ---------------------------------------------------------------

func Boolean(name string) *Col[bool] { return newCol[bool](name, simpleType("boolean")) }

// Date / time -----------------------------------------------------------

func Date(name string) *Col[time.Time] { return newCol[time.Time](name, simpleType("date")) }
func Time(name string) *Col[time.Time] { return newCol[time.Time](name, simpleType("time")) }

// Timestamp creates a TIMESTAMP column (TIMESTAMPTZ if withTimeZone).
func Timestamp(name string, withTimeZone bool) *Col[time.Time] {
	if withTimeZone {
		return newCol[time.Time](name, simpleType("timestamptz"))
	}
	return newCol[time.Time](name, simpleType("timestamp"))
}

func Interval(name string) *Col[string] { return newCol[string](name, simpleType("interval")) }

// Misc ------------------------------------------------------------------

func UUID(name string) *Col[string] { return newCol[string](name, simpleType("uuid")) }

func JSONB(name string) *Col[json.RawMessage] {
	return newCol[json.RawMessage](name, simpleType("jsonb"))
}
func JSON(name string) *Col[json.RawMessage] {
	return newCol[json.RawMessage](name, simpleType("json"))
}

func Bytea(name string) *Col[[]byte] { return newCol[[]byte](name, simpleType("bytea")) }

// Custom creates a column with an arbitrary type literal. Specify the Go
// value type as the type parameter — e.g. pg.Custom[string]("status",
// "user_status_enum").
func Custom[T any](name, typeSQL string) *Col[T] { return newCol[T](name, simpleType(typeSQL)) }
