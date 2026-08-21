package clickhouse

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/internal/vecsql"
	"github.com/bernardoforcillo/drops/vector"
)

// VectorStore adapts a ClickHouse table to
// [github.com/bernardoforcillo/drops/vector.Store] — the third
// backend behind the same portable query as pgvector and Qdrant.
//
// ClickHouse needs no extension for this: cosineDistance, L2Distance,
// L1Distance and dotProduct are built in and operate on an
// Array(Float32) column, which is all an embedding is here.
//
//	store := clickhouse.NewVectorStore(db, Docs, DocID, DocEmbedding,
//	    clickhouse.WithPayloadColumn(DocMeta),   // String holding JSON
//	    clickhouse.WithField("lang", DocLang),
//	)
//
// # What it is good at, and what it isn't
//
// Without a vector-similarity index this is an exact brute-force scan:
// every row's distance is computed. That is genuinely fast on
// ClickHouse for the sizes analytical tables reach, and unlike an ANN
// index it cannot miss a match — filters and deep pages stay exact, so
// the caveat pgvector carries about selective filters does not apply
// here. It is, however, linear: a billion-row collection wants either
// a MergeTree partition key that prunes most parts, or Qdrant.
//
// # Filter fields
//
// Field resolution matches the pgvector store: [WithField] mappings
// first, then a JSON accessor into the payload column, then
// [vector.ErrUnknownField]. Dotted names walk nested JSON.
type VectorStore struct {
	db      *DB
	table   *Table
	id      *Column
	vec     *Column
	payload *Column
	fields  map[string]*Column
}

// VectorStoreOption configures a [VectorStore].
type VectorStoreOption func(*VectorStore)

// WithPayloadColumn names the String column holding each record's
// metadata as JSON. It becomes the source of [vector.Hit.Payload] and
// the fallback for filter fields no [WithField] mapping covers.
func WithPayloadColumn(c ColRef) VectorStoreOption {
	return func(s *VectorStore) { s.payload = c.col() }
}

// WithField maps a filter field name onto a real column — cheaper and
// better typed than reaching into the JSON payload.
func WithField(name string, c ColRef) VectorStoreOption {
	return func(s *VectorStore) {
		if s.fields == nil {
			s.fields = map[string]*Column{}
		}
		s.fields[name] = c.col()
	}
}

// NewVectorStore returns a portable [vector.Store] over a ClickHouse
// table whose embedding column is an Array(Float32).
func NewVectorStore(db *DB, t *Table, id, embedding ColRef, opts ...VectorStoreOption) *VectorStore {
	s := &VectorStore{db: db, table: t, id: id.col(), vec: embedding.col()}
	for _, o := range opts {
		o(s)
	}
	return s
}

// distanceAlias is the output name given to the distance expression.
// ClickHouse resolves aliases in WHERE as well as ORDER BY, so the
// query vector appears exactly once in the statement — which matters
// when it is a 1536-component literal.
const distanceAlias = "_drops_distance"

// Search implements [vector.Store].
//
// Pagination is keyset on (distance, id), the same strategy the
// pgvector store uses: the cursor carries the last hit's distance and
// ID, and the next page is guarded by
// (distance, id) > (lastDistance, lastID). ClickHouse compares tuples
// lexicographically, so the guard is one expression rather than an
// unrolled disjunction.
func (s *VectorStore) Search(ctx context.Context, q vector.Query) (*vector.Results, error) {
	q = q.Normalized()
	if err := q.Validate(); err != nil {
		return nil, err
	}

	dist, err := s.distanceExpr(q.Metric, q.Vector)
	if err != nil {
		return nil, err
	}
	distRef := drops.Raw(drops.StdQuoteIdent(distanceAlias))

	cols := []drops.Expression{s.id, As(dist, distanceAlias)}
	if q.IncludePayload && s.payload != nil {
		cols = append(cols, s.payload)
	}
	if q.IncludeVector {
		cols = append(cols, s.vec)
	}

	sel := s.db.Select(cols...).From(s.table)

	if pred, ok, cErr := s.visitor().Compile(q.Filter); cErr != nil {
		return nil, cErr
	} else if ok {
		sel.Where(pred)
	}
	if q.MaxDistance != nil {
		sel.Where(Lte(distRef, *q.MaxDistance))
	}

	cur, hasCursor, err := vector.DecodeCursor(q.Cursor, vector.BackendClickHouse)
	if err != nil {
		return nil, err
	}
	if hasCursor {
		guard, gErr := s.cursorGuard(distRef, cur)
		if gErr != nil {
			return nil, gErr
		}
		sel.Where(guard)
	}

	for k, v := range settingsFrom(q) {
		sel.Setting(k, v)
	}

	sel.OrderBy(drops.Raw(drops.StdQuoteIdent(distanceAlias)+" ASC"), s.id.Asc())
	sel.Limit(int64(q.TopK) + 1) // +1 probes for a further page

	rows, err := sel.Rows(ctx)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := &vector.Results{}
	for rows.Next() {
		var (
			id      any
			d       float64
			payload string
			vec     []float32
		)
		dest := []any{&id, &d}
		if q.IncludePayload && s.payload != nil {
			dest = append(dest, &payload)
		}
		if q.IncludeVector {
			dest = append(dest, &vec)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		hit := vector.Hit{
			ID:       normalizeID(id),
			Distance: d,
			Score:    vector.ScoreFromDistance(q.Metric, d),
			Vector:   vec,
		}
		if payload != "" {
			if err := json.Unmarshal([]byte(payload), &hit.Payload); err != nil {
				return nil, fmt.Errorf("drops/clickhouse: decode payload for id %v: %w", hit.ID, err)
			}
		}
		res.Hits = append(res.Hits, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	res.HasMore = len(res.Hits) > q.TopK
	if res.HasMore {
		res.Hits = res.Hits[:q.TopK]
		next, cErr := vector.KeysetCursor(vector.BackendClickHouse, res.Hits[len(res.Hits)-1])
		if cErr != nil {
			return nil, cErr
		}
		enc, cErr := next.Encode()
		if cErr != nil {
			return nil, cErr
		}
		res.NextCursor = enc
	}
	return res, nil
}

// cursorGuard renders (distance, id) > (?, ?).
func (s *VectorStore) cursorGuard(dist drops.Expression, cur vector.Cursor) (drops.Expression, error) {
	if cur.Distance == nil {
		return nil, fmt.Errorf("%w: keyset cursor carries no distance", vector.ErrInvalidCursor)
	}
	id, ok, err := cur.ID()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: keyset cursor carries no id", vector.ErrInvalidCursor)
	}
	lastDist, lastID := *cur.Distance, id
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		b.Append(dist)
		b.WriteString(", ")
		s.id.WriteSQL(b)
		b.WriteString(") > (")
		b.AddArg(lastDist)
		b.WriteString(", ")
		b.AddArg(lastID)
		b.WriteByte(')')
	}), nil
}

// distanceExpr renders the ClickHouse distance function for the
// requested metric.
func (s *VectorStore) distanceExpr(m vector.Metric, v []float32) (drops.Expression, error) {
	lit := VectorLiteral(v)
	switch m {
	case vector.Cosine:
		return Func("cosineDistance", s.vec, lit), nil
	case vector.L2:
		return Func("L2Distance", s.vec, lit), nil
	case vector.L1:
		return Func("L1Distance", s.vec, lit), nil
	case vector.InnerProduct:
		// The portable convention is that smaller is closer, so the
		// distance is the negated dot product — matching what
		// pgvector's <#> operator returns.
		return Func("negate", Func("dotProduct", s.vec, lit)), nil
	default:
		// Hamming and Jaccard are bit-vector metrics; ClickHouse has
		// no equivalent over Array(Float32).
		return nil, fmt.Errorf("%w: clickhouse has no %s distance over Array(Float32)",
			vector.ErrUnsupportedMetric, m)
	}
}

// VectorLiteral renders an embedding as CAST([…] AS Array(Float32)).
//
// It is a literal rather than a bound parameter deliberately: array
// binding is driver-specific, while a literal built from
// strconv-formatted floats renders identically through every driver
// and carries no injection surface — a float32 has no textual form
// that is not a number.
func VectorLiteral(v []float32) drops.Expression {
	var sb strings.Builder
	sb.WriteString("CAST([")
	for i, f := range v {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatFloat(float64(f), 'f', -1, 32))
	}
	sb.WriteString("] AS Array(Float32))")
	return drops.Raw(sb.String())
}

// settingsFrom pulls ClickHouse SETTINGS out of the portable Params
// bag. Only string-renderable scalars are forwarded; keys meant for
// another backend are ignored, so one Query stays runnable everywhere.
//
// A key that is not a setting name is dropped rather than rendered.
// This is the one place a SETTINGS key can reach the SQL without an
// author having written it — Params is a map the caller fills, and a
// caller who fills it from a request would otherwise be writing the
// clause. Dropping it keeps the query runnable, which is what the rest
// of this function does with everything it cannot render.
func settingsFrom(q vector.Query) map[string]string {
	var out map[string]string
	for k, v := range q.Params {
		rest, ok := strings.CutPrefix(k, "clickhouse.")
		if !ok {
			continue
		}
		if validateSettingKey(rest) != nil {
			continue
		}
		s, ok := settingValue(v)
		if !ok {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[rest] = s
	}
	return out
}

func settingValue(v any) (string, bool) {
	switch n := v.(type) {
	case string:
		// The backslash goes first: escaping only the quote leaves a
		// value ending in one — "tier\" — escaping the closing quote
		// instead of standing for itself, and the literal open over
		// everything the renderer writes after it.
		esc := strings.ReplaceAll(n, `\`, `\\`)
		return "'" + strings.ReplaceAll(esc, "'", `\'`) + "'", true
	case bool:
		if n {
			return "1", true
		}
		return "0", true
	case int:
		return strconv.Itoa(n), true
	case int64:
		return strconv.FormatInt(n, 10), true
	case float64:
		return strconv.FormatFloat(n, 'f', -1, 64), true
	default:
		return "", false
	}
}

// normalizeID converts a driver's []byte representation of a text ID
// into a string, so [vector.Hit.ID] prints and round-trips through a
// cursor as the value it is.
func normalizeID(v any) any {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return v
}

// Filter compilation ------------------------------------------------

func (s *VectorStore) visitor() vecsql.Visitor {
	return vecsql.Visitor{
		Ops: vecsql.Ops{
			Eq: Eq, Ne: Ne, Lt: Lt, Lte: Lte, Gt: Gt, Gte: Gte,
			In: In, NotIn: NotIn,
			And: And, Or: Or, Not: Not,
			IsNull: IsNull, IsNotNull: IsNotNull,
		},
		Resolve:  s.fieldExpr,
		IDColumn: s.id,
		TextMatcher: func(col drops.Expression, text string) drops.Expression {
			return Gt(Func("positionCaseInsensitive", col, text), 0)
		},
		GeoCompiler: s.geoWithin,
		NullTest:    s.nullTest,
	}
}

// fieldExpr resolves a filter field to an expression: a mapped column
// if there is one, otherwise a JSON accessor into the payload column.
//
// sample decides which accessor: JSONExtractFloat for a numeric
// operand, JSONExtractString otherwise — ClickHouse's extractors are
// typed at the call site rather than by a cast.
func (s *VectorStore) fieldExpr(field string, sample any) (drops.Expression, error) {
	if col, ok := s.fields[field]; ok {
		return col, nil
	}
	if s.payload == nil {
		return nil, fmt.Errorf("%w: %q (no WithField mapping and no payload column)", vector.ErrUnknownField, field)
	}
	fn := "JSONExtractString"
	if vecsql.IsNumeric(sample) {
		fn = "JSONExtractFloat"
	}
	return jsonAccessor(fn, s.payload, strings.Split(field, ".")), nil
}

// jsonAccessor renders JSONExtractX(payload, 'a', 'b', 'c').
func jsonAccessor(fn string, col *Column, path []string) drops.Expression {
	args := make([]any, 0, len(path)+1)
	args = append(args, col)
	for _, p := range path {
		args = append(args, p)
	}
	return Func(fn, args...)
}

// nullTest renders "this field holds no usable value".
//
// For a mapped column that is IS NULL. For a payload field it is
// JSONHas: ClickHouse's extractors return a zero value — "" or 0 —
// for a key that is absent, so IS NULL on an accessor is never true
// and would make IsNull silently match nothing.
func (s *VectorStore) nullTest(field string, col drops.Expression) drops.Expression {
	if _, mapped := s.fields[field]; mapped || s.payload == nil {
		return IsNull(col)
	}
	args := make([]any, 0, 4)
	args = append(args, s.payload)
	for _, p := range strings.Split(field, ".") {
		args = append(args, p)
	}
	return Not(Func("JSONHas", args...))
}

// geoWithin compiles a bounding-box test into four comparisons on the
// field's lat / lon sub-keys — the shape Qdrant stores a geo payload
// in, so the same portable filter means the same thing on both.
func (s *VectorStore) geoWithin(field string, box vector.GeoBox) (drops.Expression, error) {
	lat, err := s.fieldExpr(field+".lat", 0.0)
	if err != nil {
		return nil, err
	}
	lon, err := s.fieldExpr(field+".lon", 0.0)
	if err != nil {
		return nil, err
	}
	return And(
		Lte(lat, box.TopLeft.Lat),
		Gte(lat, box.BottomRight.Lat),
		Gte(lon, box.TopLeft.Lon),
		Lte(lon, box.BottomRight.Lon),
	), nil
}

var _ vector.Store = (*VectorStore)(nil)
