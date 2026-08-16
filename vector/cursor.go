package vector

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
)

// Backend names the store that issued a cursor. It is stamped into
// every cursor so replaying one against the wrong store fails loudly
// — see [ErrCursorMismatch].
type Backend string

const (
	BackendQdrant     Backend = "qdrant"
	BackendPostgres   Backend = "pg"
	BackendClickHouse Backend = "clickhouse"
)

// Cursor is the decoded form of a [Results.NextCursor] string.
//
// Callers never build one — they pass the opaque string back through
// [QueryBuilder.After]. Adapters do, and each fills in only the fields
// its pagination strategy needs:
//
//   - Keyset backends (pgvector, ClickHouse) set Distance and the ID,
//     and resume with the guard (distance, id) > (Distance, ID).
//     Because the ordering key is fully carried in the cursor, rows
//     inserted between two pages cannot shift a result across the
//     boundary.
//   - Offset backends (Qdrant, whose search API exposes no keyset)
//     set Offset and resume by skipping that many hits.
//
// The ID is stored as text plus a kind tag rather than as a bare JSON
// value: a large int64 ID would otherwise come back through
// encoding/json as a float64 and lose its low bits, silently skipping
// or repeating a row at the page boundary.
type Cursor struct {
	// Backend is the store that issued the cursor.
	Backend Backend `json:"b"`

	// Offset is the number of hits to skip. Offset-paginated backends
	// only.
	Offset int `json:"o,omitempty"`

	// Distance is the last hit's metric distance. Keyset backends
	// only.
	Distance *float64 `json:"d,omitempty"`

	// IDKind and IDText hold the last hit's ID in a form that
	// round-trips exactly. Use [Cursor.SetID] and [Cursor.ID] rather
	// than touching them directly.
	IDKind string `json:"k,omitempty"`
	IDText string `json:"i,omitempty"`
}

// ID kinds. Deliberately single letters: cursors travel in URLs.
const (
	idKindString = "s"
	idKindInt    = "i"
	idKindUint   = "u"
	idKindFloat  = "f"
)

// SetID records the last hit's ID for a keyset cursor, preserving its
// Go type across the round trip. Integer, unsigned, floating-point and
// string IDs are supported — the set every store in this repo can use
// as a primary key. A nil ID clears the field.
func (c *Cursor) SetID(id any) error {
	switch v := id.(type) {
	case nil:
		c.IDKind, c.IDText = "", ""
	case string:
		c.IDKind, c.IDText = idKindString, v
	case []byte:
		c.IDKind, c.IDText = idKindString, string(v)
	case int:
		c.IDKind, c.IDText = idKindInt, strconv.FormatInt(int64(v), 10)
	case int8:
		c.IDKind, c.IDText = idKindInt, strconv.FormatInt(int64(v), 10)
	case int16:
		c.IDKind, c.IDText = idKindInt, strconv.FormatInt(int64(v), 10)
	case int32:
		c.IDKind, c.IDText = idKindInt, strconv.FormatInt(int64(v), 10)
	case int64:
		c.IDKind, c.IDText = idKindInt, strconv.FormatInt(v, 10)
	case uint:
		c.IDKind, c.IDText = idKindUint, strconv.FormatUint(uint64(v), 10)
	case uint8:
		c.IDKind, c.IDText = idKindUint, strconv.FormatUint(uint64(v), 10)
	case uint16:
		c.IDKind, c.IDText = idKindUint, strconv.FormatUint(uint64(v), 10)
	case uint32:
		c.IDKind, c.IDText = idKindUint, strconv.FormatUint(uint64(v), 10)
	case uint64:
		c.IDKind, c.IDText = idKindUint, strconv.FormatUint(v, 10)
	case float32:
		c.IDKind, c.IDText = idKindFloat, strconv.FormatFloat(float64(v), 'g', -1, 32)
	case float64:
		c.IDKind, c.IDText = idKindFloat, strconv.FormatFloat(v, 'g', -1, 64)
	default:
		return fmt.Errorf("drops/vector: cannot put %T in a cursor (want string, integer or float ID)", id)
	}
	return nil
}

// ID returns the ID recorded by [Cursor.SetID], restored to its
// original Go type. The second result is false when the cursor
// carries no ID — the case for an offset-paginated backend.
func (c Cursor) ID() (any, bool, error) {
	switch c.IDKind {
	case "":
		return nil, false, nil
	case idKindString:
		return c.IDText, true, nil
	case idKindInt:
		n, err := strconv.ParseInt(c.IDText, 10, 64)
		if err != nil {
			return nil, false, fmt.Errorf("%w: bad integer id: %w", ErrInvalidCursor, err)
		}
		return n, true, nil
	case idKindUint:
		n, err := strconv.ParseUint(c.IDText, 10, 64)
		if err != nil {
			return nil, false, fmt.Errorf("%w: bad unsigned id: %w", ErrInvalidCursor, err)
		}
		return n, true, nil
	case idKindFloat:
		f, err := strconv.ParseFloat(c.IDText, 64)
		if err != nil {
			return nil, false, fmt.Errorf("%w: bad float id: %w", ErrInvalidCursor, err)
		}
		return f, true, nil
	default:
		return nil, false, fmt.Errorf("%w: unknown id kind %q", ErrInvalidCursor, c.IDKind)
	}
}

// Encode renders the cursor as the opaque, URL-safe string callers
// pass back through [QueryBuilder.After].
func (c Cursor) Encode() (string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// DecodeCursor parses a cursor string and checks it came from the
// expected backend.
//
// An empty string decodes to the zero Cursor with ok=false, which is
// how an adapter recognises a first-page request without the caller
// having to special-case it.
func DecodeCursor(s string, expect Backend) (Cursor, bool, error) {
	var c Cursor
	if s == "" {
		return c, false, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return c, false, fmt.Errorf("%w: not base64: %w", ErrInvalidCursor, err)
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, false, fmt.Errorf("%w: %w", ErrInvalidCursor, err)
	}
	if c.Backend != expect {
		return c, false, fmt.Errorf("%w: cursor is from %q, this store is %q", ErrCursorMismatch, c.Backend, expect)
	}
	return c, true, nil
}

// OffsetCursor builds the cursor an offset-paginated backend hands
// back after serving a page.
func OffsetCursor(b Backend, nextOffset int) Cursor {
	return Cursor{Backend: b, Offset: nextOffset}
}

// KeysetCursor builds the cursor a keyset-paginated backend hands back
// after serving a page, from the last hit on that page.
func KeysetCursor(b Backend, last Hit) (Cursor, error) {
	d := last.Distance
	c := Cursor{Backend: b, Distance: &d}
	if err := c.SetID(last.ID); err != nil {
		return Cursor{}, err
	}
	return c, nil
}
