package mysql

import (
	"encoding/json"
	"fmt"
	"time"
)

// Column type constructors. The names mirror drops/pg so a schema
// ports between the two by swapping the package; each maps to the
// closest MySQL type.

type parametrisedType struct {
	base string
	args string
}

func (p parametrisedType) TypeSQL() string { return p.base + "(" + p.args + ")" }

// Text is a TEXT column.
//
// Note that MySQL cannot index a TEXT column without a prefix length,
// so a column you intend to index or make UNIQUE wants [Varchar]
// instead. That is a real constraint of the engine, not a drops
// limitation, and it is the most common surprise when porting a
// PostgreSQL schema where every string is `text`.
func Text(name string) *Col[string] { return newCol[string](name, simpleType("TEXT")) }

// LongText is a LONGTEXT column, for values past TEXT's 64KB.
func LongText(name string) *Col[string] { return newCol[string](name, simpleType("LONGTEXT")) }

// Varchar is a VARCHAR(n) column. A non-positive n defaults to 255,
// the length MySQL schemas conventionally use.
func Varchar(name string, n int) *Col[string] {
	if n <= 0 {
		n = 255
	}
	return newCol[string](name, parametrisedType{base: "VARCHAR", args: fmt.Sprintf("%d", n)})
}

// Char is a CHAR(n) column.
func Char(name string, n int) *Col[string] {
	if n <= 0 {
		n = 1
	}
	return newCol[string](name, parametrisedType{base: "CHAR", args: fmt.Sprintf("%d", n)})
}

// Integer family.
func TinyInt(name string) *Col[int8]   { return newCol[int8](name, simpleType("TINYINT")) }
func SmallInt(name string) *Col[int16] { return newCol[int16](name, simpleType("SMALLINT")) }
func Integer(name string) *Col[int32]  { return newCol[int32](name, simpleType("INT")) }
func BigInt(name string) *Col[int64]   { return newCol[int64](name, simpleType("BIGINT")) }

// Unsigned variants. MySQL has no PostgreSQL equivalent for these, so
// they exist only here.
func UnsignedInt(name string) *Col[uint32] {
	return newCol[uint32](name, simpleType("INT UNSIGNED"))
}
func UnsignedBigInt(name string) *Col[uint64] {
	return newCol[uint64](name, simpleType("BIGINT UNSIGNED"))
}

// Serial / BigSerial are the PostgreSQL-parity spellings of an
// auto-incrementing key. MySQL has no serial *type*: it is an ordinary
// integer marked AUTO_INCREMENT, which these constructors do for you.
func Serial(name string) *Col[int32] {
	c := newCol[int32](name, simpleType("INT"))
	c.Column.autoInc = true
	return c
}

func BigSerial(name string) *Col[int64] {
	c := newCol[int64](name, simpleType("BIGINT"))
	c.Column.autoInc = true
	return c
}

// Floating point.
func Real(name string) *Col[float32]            { return newCol[float32](name, simpleType("FLOAT")) }
func DoublePrecision(name string) *Col[float64] { return newCol[float64](name, simpleType("DOUBLE")) }

// Numeric is DECIMAL(precision, scale). A non-positive precision
// yields a bare DECIMAL, which MySQL reads as DECIMAL(10,0) — an
// integer. Say what you mean if fractions matter.
func Numeric(name string, precision, scale int) *Col[string] {
	if precision <= 0 {
		return newCol[string](name, simpleType("DECIMAL"))
	}
	return newCol[string](name, parametrisedType{
		base: "DECIMAL",
		args: fmt.Sprintf("%d,%d", precision, scale),
	})
}

// Boolean is MySQL's BOOLEAN, itself an alias for TINYINT(1).
func Boolean(name string) *Col[bool] { return newCol[bool](name, simpleType("BOOLEAN")) }

// Date / time.
func Date(name string) *Col[time.Time] { return newCol[time.Time](name, simpleType("DATE")) }
func Time(name string) *Col[time.Time] { return newCol[time.Time](name, simpleType("TIME")) }

// Timestamp maps to DATETIME(6) — microsecond precision, matching what
// PostgreSQL stores.
//
// withTimeZone selects TIMESTAMP(6) instead, and the name is a
// half-truth MySQL is responsible for: TIMESTAMP does not store a zone
// but converts to UTC on write and back to the session zone on read,
// so the value you get depends on the connection's time_zone. DATETIME
// stores exactly what you gave it. Prefer DATETIME with UTC values
// unless you need TIMESTAMP's auto-update behaviour.
func Timestamp(name string, withTimeZone bool) *Col[time.Time] {
	if withTimeZone {
		return newCol[time.Time](name, simpleType("TIMESTAMP(6)"))
	}
	return newCol[time.Time](name, simpleType("DATETIME(6)"))
}

// UUID is CHAR(36) — the portable choice. MySQL 8 has BIN_TO_UUID for
// a packed BINARY(16) form, which is smaller and faster to index but
// unreadable in a query result; declare that with [Custom] when you
// want it.
func UUID(name string) *Col[string] {
	return newCol[string](name, parametrisedType{base: "CHAR", args: "36"})
}

// JSON is a JSON column. JSONB is the PostgreSQL-parity alias: MySQL's
// JSON type is already stored in a parsed binary form, so there is
// only one.
func JSON(name string) *Col[json.RawMessage] {
	return newCol[json.RawMessage](name, simpleType("JSON"))
}

func JSONB(name string) *Col[json.RawMessage] { return JSON(name) }

// Blob / Bytea are binary columns (Bytea is the PostgreSQL-parity
// alias).
func Blob(name string) *Col[[]byte]  { return newCol[[]byte](name, simpleType("BLOB")) }
func Bytea(name string) *Col[[]byte] { return newCol[[]byte](name, simpleType("BLOB")) }

// LongBlob is LONGBLOB, for payloads past BLOB's 64KB.
func LongBlob(name string) *Col[[]byte] { return newCol[[]byte](name, simpleType("LONGBLOB")) }

// Enum declares MySQL's ENUM type with the given members.
func Enum(name string, members ...string) *Col[string] {
	quoted := make([]string, len(members))
	for i, m := range members {
		quoted[i] = "'" + quoteLiteral(m) + "'"
	}
	return newCol[string](name, parametrisedType{base: "ENUM", args: joinComma(quoted)})
}

// Custom declares a column with a caller-supplied type literal.
func Custom[T any](name, typeSQL string) *Col[T] { return newCol[T](name, simpleType(typeSQL)) }

// quoteLiteral escapes a string for a single-quoted SQL literal. Used
// only for schema text (enum members, defaults), never for values —
// those are always bound.
func quoteLiteral(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'', '\\':
			out = append(out, '\\', s[i])
		default:
			out = append(out, s[i])
		}
	}
	return string(out)
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
