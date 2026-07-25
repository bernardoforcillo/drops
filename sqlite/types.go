package sqlite

import (
	"encoding/json"
	"time"
)

// Column type constructors. The names mirror drops/pg so a schema
// ports from PostgreSQL to SQLite by swapping the package; each maps to
// the SQLite declared type with the appropriate type affinity.

// Text is a TEXT column.
func Text(name string) *Col[string] { return newCol[string](name, simpleType("TEXT")) }

// Varchar maps to TEXT — SQLite does not enforce length, but the length
// is accepted for schema parity with PostgreSQL.
func Varchar(name string, _ int) *Col[string] { return newCol[string](name, simpleType("TEXT")) }

// Char maps to TEXT (see Varchar).
func Char(name string, _ int) *Col[string] { return newCol[string](name, simpleType("TEXT")) }

// Integer family — all map to INTEGER affinity.
func SmallInt(name string) *Col[int16] { return newCol[int16](name, simpleType("INTEGER")) }
func Integer(name string) *Col[int32]  { return newCol[int32](name, simpleType("INTEGER")) }
func BigInt(name string) *Col[int64]   { return newCol[int64](name, simpleType("INTEGER")) }

// Serial / BigSerial map to INTEGER. Auto-increment in SQLite is a
// property of an INTEGER PRIMARY KEY (add .PrimaryKey(), and optionally
// .AutoIncrement() for the strict monotonic ROWID), not a distinct
// column type as in PostgreSQL.
func Serial(name string) *Col[int32]    { return newCol[int32](name, simpleType("INTEGER")) }
func BigSerial(name string) *Col[int64] { return newCol[int64](name, simpleType("INTEGER")) }

// Real / DoublePrecision map to REAL affinity.
func Real(name string) *Col[float32]            { return newCol[float32](name, simpleType("REAL")) }
func DoublePrecision(name string) *Col[float64] { return newCol[float64](name, simpleType("REAL")) }

// Numeric maps to NUMERIC affinity. Precision/scale are accepted for
// parity but SQLite does not enforce them.
func Numeric(name string, _, _ int) *Col[string] { return newCol[string](name, simpleType("NUMERIC")) }

// Boolean is stored as INTEGER 0/1; declared as BOOLEAN for readability.
func Boolean(name string) *Col[bool] { return newCol[bool](name, simpleType("BOOLEAN")) }

// Date / Time / Timestamp map to SQLite's date-time storage (TEXT
// affinity via the DATE/TIME/DATETIME declared types).
func Date(name string) *Col[time.Time] { return newCol[time.Time](name, simpleType("DATE")) }
func Time(name string) *Col[time.Time] { return newCol[time.Time](name, simpleType("TIME")) }

// Timestamp maps to DATETIME. withTimeZone is accepted for parity;
// SQLite has no zoned type — store UTC and convert in application code.
func Timestamp(name string, _ bool) *Col[time.Time] {
	return newCol[time.Time](name, simpleType("DATETIME"))
}

// UUID is stored as TEXT.
func UUID(name string) *Col[string] { return newCol[string](name, simpleType("TEXT")) }

// JSON / JSONB are stored as TEXT (SQLite's JSON1 functions operate on
// TEXT). JSONB is an alias for parity with PostgreSQL.
func JSON(name string) *Col[json.RawMessage] {
	return newCol[json.RawMessage](name, simpleType("TEXT"))
}
func JSONB(name string) *Col[json.RawMessage] {
	return newCol[json.RawMessage](name, simpleType("TEXT"))
}

// Blob / Bytea are BLOB columns (Bytea is the PostgreSQL-parity alias).
func Blob(name string) *Col[[]byte]  { return newCol[[]byte](name, simpleType("BLOB")) }
func Bytea(name string) *Col[[]byte] { return newCol[[]byte](name, simpleType("BLOB")) }

// Custom declares a column with a caller-supplied declared type.
func Custom[T any](name, typeSQL string) *Col[T] { return newCol[T](name, simpleType(typeSQL)) }
