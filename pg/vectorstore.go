package pg

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

// VectorStore adapts a pgvector table to
// [github.com/bernardoforcillo/drops/vector.Store], so the portable
// query that runs against a Qdrant collection runs here too.
//
// A store needs to know three things: the table, the column holding
// the record ID, and the column holding the embedding. Everything
// else — where filterable metadata lives, which pgvector type the
// column is — is an option:
//
//	store := pg.NewVectorStore(db, Docs, DocID, DocEmbedding,
//	    pg.WithPayloadColumn(DocMeta),      // jsonb
//	    pg.WithField("lang", DocLang),      // a real column beats jsonb
//	)
//
// # Filter fields
//
// A [vector.Filter] names fields as strings, because Qdrant payloads
// have no schema. Resolution walks two steps:
//
//  1. a field registered with [WithField] compiles to that column,
//     which keeps the predicate typed and index-friendly;
//  2. otherwise, if a payload column is set, the field compiles to a
//     JSON accessor into it — dotted names walk nested objects, so
//     "author.name" becomes meta -> 'author' ->> 'name'.
//
// A field that matches neither is [vector.ErrUnknownField]: better a
// clear error than a predicate that silently matches nothing.
//
// # ANN indexes and filtering
//
// pgvector's HNSW and IVFFlat indexes answer "ORDER BY distance LIMIT
// k" approximately. A WHERE clause — a filter, a MaxDistance, or the
// cursor guard on page two — is applied on top of what the index
// returns, so a very selective filter or a deep page can come back
// with fewer rows than TopK even though more matches exist. That is
// pgvector's behaviour, not this adapter's; the fix is to widen the
// index search with hnsw.ef_search / ivfflat.probes, which you can
// pass per query:
//
//	vector.Search(v).TopK(20).Param("hnsw.ef_search", 200)
type VectorStore struct {
	db      *DB
	table   *Table
	id      *Column
	vec     *Column
	payload *Column
	fields  map[string]*Column
	geoCols map[string]*Column
	cast    string
}

// VectorStoreOption configures a [VectorStore].
type VectorStoreOption func(*VectorStore)

// WithPayloadColumn names the jsonb column holding record metadata.
// It becomes both the source of [vector.Hit.Payload] and the fallback
// target for filter fields that no [WithField] mapping covers.
func WithPayloadColumn(c ColRef) VectorStoreOption {
	return func(s *VectorStore) { s.payload = c.col() }
}

// WithField maps a filter field name onto a real column. Prefer it
// over the jsonb fallback wherever a column exists: the predicate
// stays typed and can use an ordinary index.
func WithField(name string, c ColRef) VectorStoreOption {
	return func(s *VectorStore) {
		if s.fields == nil {
			s.fields = map[string]*Column{}
		}
		s.fields[name] = c.col()
	}
}

// WithGeoColumn maps a filter field onto a PostGIS geography column,
// so [vector.GeoWithin] on that field compiles to ST_Within rather
// than to a pair of lat/lon comparisons.
func WithGeoColumn(name string, c ColRef) VectorStoreOption {
	return func(s *VectorStore) {
		if s.geoCols == nil {
			s.geoCols = map[string]*Column{}
		}
		s.geoCols[name] = c.col()
	}
}

// WithVectorType names the pgvector type the query vector is cast to
// before it meets the column — "vector" (the default), "halfvec" or
// "sparsevec". The bit-distance metrics ignore it and cast to bit(N)
// instead, N being the query vector's length.
func WithVectorType(name string) VectorStoreOption {
	return func(s *VectorStore) {
		if name != "" {
			s.cast = name
		}
	}
}

// NewVectorStore returns a portable [vector.Store] over a pgvector
// table.
func NewVectorStore(db *DB, t *Table, id, embedding ColRef, opts ...VectorStoreOption) *VectorStore {
	s := &VectorStore{
		db:    db,
		table: t,
		id:    id.col(),
		vec:   embedding.col(),
		cast:  "vector",
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// distanceAlias is the output name the distance expression gets, and
// what ORDER BY refers to so the query vector is bound once rather
// than once per clause.
const distanceAlias = "_drops_distance"

// Search implements [vector.Store].
//
// Pagination is keyset, on (distance, id): the cursor carries the last
// hit's distance and ID, and the next page is guarded by
// (distance, id) > (lastDistance, lastID). Rows inserted between two
// requests therefore cannot shift a result across a page boundary the
// way OFFSET would. The id tie-break is what makes the order total —
// two records at the same distance still have a defined position.
func (s *VectorStore) Search(ctx context.Context, q vector.Query) (*vector.Results, error) {
	q = q.Normalized()
	if err := q.Validate(); err != nil {
		return nil, err
	}

	dist, err := s.distanceExpr(q.Metric, q.Vector)
	if err != nil {
		return nil, err
	}

	cols := []drops.Expression{s.id, aliased(dist, distanceAlias)}
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
		sel.Where(Lte(dist, *q.MaxDistance))
	}

	cur, hasCursor, err := vector.DecodeCursor(q.Cursor, vector.BackendPostgres)
	if err != nil {
		return nil, err
	}
	if hasCursor {
		guard, gErr := s.cursorGuard(dist, cur)
		if gErr != nil {
			return nil, gErr
		}
		sel.Where(guard)
	}

	// ORDER BY refers to the output alias so the query vector is
	// bound once; PostgreSQL resolves output names in ORDER BY.
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
			payload []byte
			vecText []byte
		)
		dest := []any{&id, &d}
		if q.IncludePayload && s.payload != nil {
			dest = append(dest, &payload)
		}
		if q.IncludeVector {
			dest = append(dest, &vecText)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		hit := vector.Hit{
			ID:       normalizeID(id),
			Distance: d,
			Score:    vector.ScoreFromDistance(q.Metric, d),
		}
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &hit.Payload); err != nil {
				return nil, fmt.Errorf("drops/pg: decode payload for id %v: %w", hit.ID, err)
			}
		}
		if len(vecText) > 0 {
			v, pErr := ParseVector(string(vecText))
			if pErr != nil {
				return nil, fmt.Errorf("drops/pg: decode vector for id %v: %w", hit.ID, pErr)
			}
			hit.Vector = v
		}
		res.Hits = append(res.Hits, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	res.HasMore = len(res.Hits) > q.TopK
	if res.HasMore {
		res.Hits = res.Hits[:q.TopK]
		next, cErr := vector.KeysetCursor(vector.BackendPostgres, res.Hits[len(res.Hits)-1])
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

// cursorGuard renders (distance, id) > ($d, $id) — the row-comparison
// form, which PostgreSQL evaluates lexicographically and which is
// therefore exactly the "strictly after the last hit" test the keyset
// needs.
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

// distanceExpr renders "<embedding> <op> <query vector>" for the
// requested metric.
func (s *VectorStore) distanceExpr(m vector.Metric, v []float32) (drops.Expression, error) {
	op, ok := distanceOperator(m)
	if !ok {
		return nil, fmt.Errorf("%w: pgvector has no %s operator", vector.ErrUnsupportedMetric, m)
	}
	// The bit metrics take a bit(N) literal; everything else takes a
	// pgvector text literal cast to the column's type.
	if m == vector.Hamming || m == vector.Jaccard {
		lit, cast := FormatBitVector(v), fmt.Sprintf("bit(%d)", len(v))
		return binOp(s.vec, op, castParam(lit, cast)), nil
	}
	return binOp(s.vec, op, castParam(FormatVector(v), s.cast)), nil
}

// distanceOperator maps a portable metric onto the pgvector operator
// that computes it.
func distanceOperator(m vector.Metric) (string, bool) {
	switch m {
	case vector.L2:
		return "<->", true
	case vector.InnerProduct:
		return "<#>", true
	case vector.Cosine:
		return "<=>", true
	case vector.L1:
		return "<+>", true
	case vector.Hamming:
		return "<~>", true
	case vector.Jaccard:
		return "<%>", true
	default:
		return "", false
	}
}

// castParam binds v and casts the placeholder — "$3::vector". Binding
// the vector as text rather than as a Go slice is what keeps the
// adapter driver-agnostic: pgx and lib/pq both send a string happily,
// while neither maps []float32 without pgvector's own codec
// registered.
func castParam(v string, cast string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.AddArg(v)
		b.WriteString("::")
		b.WriteString(cast)
	})
}

// aliased renders `<expr> AS "<name>"`.
func aliased(e drops.Expression, name string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.Append(e)
		b.WriteString(" AS ")
		b.WriteIdent(name)
	})
}

// FormatVector renders an embedding in pgvector's text form,
// "[1,2.5,-3]". Exported because it is equally what you want when
// binding a vector to a hand-written INSERT.
func FormatVector(v []float32) string {
	var sb strings.Builder
	sb.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatFloat(float64(f), 'f', -1, 32))
	}
	sb.WriteByte(']')
	return sb.String()
}

// FormatBitVector renders an embedding as the "0101" literal a bit(N)
// column takes. Any non-zero component is a 1.
func FormatBitVector(v []float32) string {
	var sb strings.Builder
	sb.Grow(len(v))
	for _, f := range v {
		if f != 0 {
			sb.WriteByte('1')
		} else {
			sb.WriteByte('0')
		}
	}
	return sb.String()
}

// ParseVector reads pgvector's text form back into a slice. It is the
// inverse of [FormatVector].
func ParseVector(s string) ([]float32, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]float32, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return nil, fmt.Errorf("drops/pg: bad vector component %q: %w", p, err)
		}
		out[i] = float32(f)
	}
	return out, nil
}

// normalizeID converts the driver's representation of an ID into a
// value a cursor can carry. Drivers hand text columns back as []byte
// often enough that leaving it unconverted would make [vector.Hit.ID]
// print as a byte slice.
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
		// PostgreSQL has a real case-insensitive LIKE, so a portable
		// MatchText is a containment test with the wildcards
		// concatenated onto the bound parameter — which keeps the
		// caller's text a parameter rather than splicing it into the
		// pattern.
		TextMatcher: func(col drops.Expression, text string) drops.Expression {
			return ILike(col, drops.ExprFunc(func(b *drops.Builder) {
				b.WriteString("('%' || ")
				b.AddArg(text)
				b.WriteString(" || '%')")
			}))
		},
		GeoCompiler: s.geoWithin,
	}
}

// fieldExpr resolves a filter field to an expression.
//
// sample is the value the field is about to be compared against; it
// decides the cast applied to a jsonb accessor. A numeric operand
// yields `(meta ->> 'k')::numeric` so the comparison is numeric, while
// anything else stays text — which is also the right answer for
// ISO-8601 timestamps, whose lexical and chronological orders agree.
func (s *VectorStore) fieldExpr(field string, sample any) (drops.Expression, error) {
	if col, ok := s.fields[field]; ok {
		return col, nil
	}
	if s.payload == nil {
		return nil, fmt.Errorf("%w: %q (no WithField mapping and no payload column)", vector.ErrUnknownField, field)
	}
	path := strings.Split(field, ".")
	cast := ""
	if vecsql.IsNumeric(sample) {
		cast = "numeric"
	}
	return jsonAccessor(s.payload, path, cast), nil
}

// jsonAccessor renders meta -> 'a' -> 'b' ->> 'c', optionally cast.
func jsonAccessor(col *Column, path []string, cast string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		col.WriteSQL(b)
		for i := 0; i < len(path)-1; i++ {
			b.WriteString(" -> ")
			writeJSONLiteral(b, path[i])
		}
		b.WriteString(" ->> ")
		writeJSONLiteral(b, path[len(path)-1])
		b.WriteByte(')')
		if cast != "" {
			b.WriteString("::")
			b.WriteString(cast)
		}
	})
}

// geoWithin compiles a bounding-box test.
//
// With a PostGIS column registered via [WithGeoColumn] it is
// ST_Within. Otherwise the field is treated the way Qdrant stores a
// geo payload — an object with lat and lon keys — and the box becomes
// four ordinary comparisons on "<field>.lat" and "<field>.lon", which
// resolve through the normal field rules.
func (s *VectorStore) geoWithin(field string, box vector.GeoBox) (drops.Expression, error) {
	if col, ok := s.geoCols[field]; ok {
		return Within(col, Box{
			SW: Point{Lat: box.BottomRight.Lat, Lon: box.TopLeft.Lon},
			NE: Point{Lat: box.TopLeft.Lat, Lon: box.BottomRight.Lon},
		}), nil
	}
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
