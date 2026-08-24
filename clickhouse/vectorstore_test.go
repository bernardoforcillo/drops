package clickhouse_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/clickhouse"
	"github.com/bernardoforcillo/drops/vector"
)

// A ClickHouse-shaped embeddings table.
var (
	vsDocs  = clickhouse.NewTable("docs")
	vsID    = clickhouse.Add(vsDocs, clickhouse.Int64("id"))
	vsEmbed = clickhouse.Add(vsDocs, clickhouse.Custom[[]float32]("embedding", "Array(Float32)"))
	vsLang  = clickhouse.Add(vsDocs, clickhouse.String("lang"))
	vsMeta  = clickhouse.Add(vsDocs, clickhouse.String("meta"))
)

// vecRows is a canned drops.Rows; the package's existing fakeRows is
// empty by construction and these tests need real values back.
type vecRows struct {
	cols []string
	data [][]any
	pos  int
}

func (r *vecRows) Next() bool {
	if r.pos >= len(r.data) {
		return false
	}
	r.pos++
	return true
}

func (r *vecRows) Scan(dest ...any) error {
	row := r.data[r.pos-1]
	for i, d := range dest {
		switch p := d.(type) {
		case *any:
			*p = row[i]
		case *float64:
			*p = row[i].(float64)
		case *string:
			*p = row[i].(string)
		case *[]float32:
			*p = row[i].([]float32)
		}
	}
	return nil
}

func (r *vecRows) Columns() ([]string, error) { return r.cols, nil }
func (r *vecRows) Close() error               { return nil }
func (r *vecRows) Err() error                 { return nil }

// vecDriver records statements and replies with canned rows.
type vecDriver struct {
	queries []string
	args    [][]any
	rows    func() drops.Rows
}

func (d *vecDriver) Exec(context.Context, string, ...any) (drops.Result, error) {
	return nil, errors.New("unexpected Exec")
}

func (d *vecDriver) Query(_ context.Context, sql string, args ...any) (drops.Rows, error) {
	d.queries = append(d.queries, sql)
	d.args = append(d.args, args)
	if d.rows == nil {
		return &vecRows{}, nil
	}
	return d.rows(), nil
}

func (d *vecDriver) Begin(context.Context) (drops.Tx, error) {
	return nil, errors.New("unexpected Begin")
}

func newVectorStore(rows func() drops.Rows, opts ...clickhouse.VectorStoreOption) (*clickhouse.VectorStore, *vecDriver) {
	d := &vecDriver{rows: rows}
	return clickhouse.NewVectorStore(clickhouse.New(d), vsDocs, vsID, vsEmbed, opts...), d
}

func searchSQL(t *testing.T, q vector.Query, opts ...clickhouse.VectorStoreOption) (string, []any) {
	t.Helper()
	store, d := newVectorStore(nil, opts...)
	if _, err := store.Search(context.Background(), q); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(d.queries) != 1 {
		t.Fatalf("expected 1 query, got %d", len(d.queries))
	}
	return d.queries[0], d.args[0]
}

func TestVectorSearchBaseSQL(t *testing.T) {
	sql, args := searchSQL(t, vector.Search([]float32{1, 0, -0.5}).TopK(4).Build())

	for _, want := range []string{
		`cosineDistance("docs"."embedding", CAST([1,0,-0.5] AS Array(Float32)))`,
		`AS "_drops_distance"`,
		`FROM "docs"`,
		`ORDER BY "_drops_distance" ASC, "docs"."id" ASC`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("SQL missing %s\ngot %s", want, sql)
		}
	}
	if got := args[len(args)-1]; got != int64(5) {
		t.Errorf("LIMIT arg = %v want TopK+1 = 5", got)
	}
}

// The query vector is expensive to repeat — at 1536 components it
// dwarfs the rest of the statement — so it must appear exactly once
// even when WHERE also constrains the distance.
func TestVectorSearchRendersTheVectorOnce(t *testing.T) {
	sql, _ := searchSQL(t, vector.Search([]float32{1, 0, -0.5}).MaxDistance(0.4).Build())
	if n := strings.Count(sql, "CAST(["); n != 1 {
		t.Errorf("query vector rendered %d times, want 1\n%s", n, sql)
	}
	if !strings.Contains(sql, `"_drops_distance" <=`) {
		t.Errorf("MaxDistance should reuse the alias: %s", sql)
	}
}

func TestVectorSearchMetricFunctions(t *testing.T) {
	cases := map[vector.Metric]string{
		vector.Cosine:       "cosineDistance(",
		vector.L2:           "L2Distance(",
		vector.L1:           "L1Distance(",
		vector.InnerProduct: "negate(dotProduct(",
	}
	for m, fn := range cases {
		sql, _ := searchSQL(t, vector.Search([]float32{1, 0, 1}).Metric(m).Build())
		if !strings.Contains(sql, fn) {
			t.Errorf("%s: SQL missing %s\ngot %s", m, fn, sql)
		}
	}
}

// Hamming and Jaccard are bit-vector metrics with no ClickHouse
// equivalent over Array(Float32) — better a clear error than ranking
// by something else.
func TestVectorSearchRejectsBitMetrics(t *testing.T) {
	for _, m := range []vector.Metric{vector.Hamming, vector.Jaccard} {
		store, _ := newVectorStore(nil)
		_, err := store.Search(context.Background(), vector.Search([]float32{1}).Metric(m).Build())
		if !errors.Is(err, vector.ErrUnsupportedMetric) {
			t.Errorf("%s: err = %v want ErrUnsupportedMetric", m, err)
		}
	}
}

func TestVectorSearchMappedFieldBeatsPayload(t *testing.T) {
	sql, _ := searchSQL(t,
		vector.Search([]float32{1}).Where(vector.Eq("lang", "it")).Build(),
		clickhouse.WithField("lang", vsLang),
		clickhouse.WithPayloadColumn(vsMeta),
	)
	if !strings.Contains(sql, `"docs"."lang" =`) {
		t.Errorf("expected the mapped column, got %s", sql)
	}
	if strings.Contains(sql, "JSONExtract") {
		t.Errorf("payload accessor used despite a mapping: %s", sql)
	}
}

// ClickHouse types its JSON extractors at the call site rather than
// with a cast, so the operand's type picks the function.
func TestVectorSearchPayloadExtractorFollowsOperandType(t *testing.T) {
	sql, _ := searchSQL(t,
		vector.Search([]float32{1}).Where(vector.And(
			vector.Eq("author.name", "ada"),
			vector.Gte("year", 2020),
		)).Build(),
		clickhouse.WithPayloadColumn(vsMeta),
	)
	if !strings.Contains(sql, `JSONExtractString("docs"."meta", 'author', 'name')`) &&
		!strings.Contains(sql, `JSONExtractString("docs"."meta", ?, ?)`) {
		t.Errorf("nested string accessor missing from %s", sql)
	}
	if !strings.Contains(sql, "JSONExtractFloat") {
		t.Errorf("numeric accessor missing from %s", sql)
	}
}

// A missing JSON key extracts as "" or 0, never as NULL — so IsNull
// has to ask JSONHas or it would match nothing at all.
func TestVectorSearchIsNullUsesJSONHas(t *testing.T) {
	sql, _ := searchSQL(t,
		vector.Search([]float32{1}).Where(vector.IsNull("author")).Build(),
		clickhouse.WithPayloadColumn(vsMeta),
	)
	if !strings.Contains(sql, "JSONHas") {
		t.Errorf("expected JSONHas for a payload field, got %s", sql)
	}
}

func TestVectorSearchIsNullOnMappedColumnUsesSQLNull(t *testing.T) {
	sql, _ := searchSQL(t,
		vector.Search([]float32{1}).Where(vector.IsNull("lang")).Build(),
		clickhouse.WithField("lang", vsLang),
		clickhouse.WithPayloadColumn(vsMeta),
	)
	if !strings.Contains(sql, "IS NULL") || strings.Contains(sql, "JSONHas") {
		t.Errorf("expected IS NULL on a real column, got %s", sql)
	}
}

func TestVectorSearchMatchText(t *testing.T) {
	sql, args := searchSQL(t,
		vector.Search([]float32{1}).Where(vector.MatchText("lang", "ital")).Build(),
		clickhouse.WithField("lang", vsLang),
	)
	if !strings.Contains(sql, "positionCaseInsensitive") {
		t.Errorf("expected positionCaseInsensitive, got %s", sql)
	}
	found := false
	for _, a := range args {
		if a == "ital" {
			found = true
		}
	}
	if !found {
		t.Errorf("search text was not bound as a parameter: %v", args)
	}
}

func TestVectorSearchUnknownField(t *testing.T) {
	store, _ := newVectorStore(nil)
	_, err := store.Search(context.Background(),
		vector.Search([]float32{1}).Where(vector.Eq("nope", 1)).Build())
	if !errors.Is(err, vector.ErrUnknownField) {
		t.Errorf("err = %v want ErrUnknownField", err)
	}
}

func TestVectorSearchForwardsSettings(t *testing.T) {
	sql, _ := searchSQL(t, vector.Search([]float32{1}).
		Param("clickhouse.max_threads", 8).
		Param("hnsw_ef", 256). // meant for Qdrant
		Build())
	if !strings.Contains(sql, "SETTINGS max_threads = 8") {
		t.Errorf("setting not forwarded: %s", sql)
	}
	if strings.Contains(sql, "hnsw_ef") {
		t.Errorf("another backend's param leaked through: %s", sql)
	}
}

// --- Pagination -----------------------------------------------------

func TestVectorSearchKeysetPagination(t *testing.T) {
	rows := func() drops.Rows {
		return &vecRows{
			cols: []string{"id", "_drops_distance"},
			data: [][]any{
				{int64(1), 0.10},
				{int64(2), 0.20},
				{int64(3), 0.30},
			},
		}
	}
	store, d := newVectorStore(rows)
	ctx := context.Background()

	first, err := store.Search(ctx, vector.Search([]float32{1}).TopK(2).Build())
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first.Hits) != 2 || !first.HasMore || first.NextCursor == "" {
		t.Fatalf("first page = %d hits HasMore=%v cursor=%q", len(first.Hits), first.HasMore, first.NextCursor)
	}

	if _, err := store.Search(ctx, vector.Search([]float32{1}).TopK(2).After(first.NextCursor).Build()); err != nil {
		t.Fatalf("second page: %v", err)
	}
	sql, args := d.queries[1], d.args[1]
	if !strings.Contains(sql, `") > (`) {
		t.Errorf("expected a (distance, id) > (...) guard, got %s", sql)
	}
	var sawDist, sawID bool
	for _, a := range args {
		if a == 0.20 {
			sawDist = true
		}
		if a == int64(2) {
			sawID = true
		}
	}
	if !sawDist || !sawID {
		t.Errorf("guard did not bind the last hit (dist=%v id=%v): %v", sawDist, sawID, args)
	}
}

func TestVectorSearchRejectsForeignCursor(t *testing.T) {
	enc, err := vector.OffsetCursor(vector.BackendQdrant, 10).Encode()
	if err != nil {
		t.Fatal(err)
	}
	store, _ := newVectorStore(nil)
	_, err = store.Search(context.Background(), vector.Search([]float32{1}).After(enc).Build())
	if !errors.Is(err, vector.ErrCursorMismatch) {
		t.Errorf("err = %v want ErrCursorMismatch", err)
	}
}

func TestVectorSearchDecodesPayloadAndVector(t *testing.T) {
	rows := func() drops.Rows {
		return &vecRows{
			cols: []string{"id", "_drops_distance", "meta", "embedding"},
			data: [][]any{{
				int64(7), 0.5,
				`{"lang":"it","year":2021}`,
				[]float32{1, 0.5, -1},
			}},
		}
	}
	store, _ := newVectorStore(rows, clickhouse.WithPayloadColumn(vsMeta))
	res, err := store.Search(context.Background(),
		vector.Search([]float32{1}).WithPayload().WithVector().Build())
	if err != nil {
		t.Fatal(err)
	}
	hit := res.Hits[0]
	if hit.Payload["lang"] != "it" {
		t.Errorf("payload = %v", hit.Payload)
	}
	if len(hit.Vector) != 3 || hit.Vector[1] != 0.5 {
		t.Errorf("vector = %v", hit.Vector)
	}
	if hit.Score != vector.ScoreFromDistance(vector.Cosine, 0.5) {
		t.Errorf("Score = %v", hit.Score)
	}
}

// Params is a bag the caller fills, and a caller who fills it from a
// request is the one place a SETTINGS key reaches the SQL without an
// author having written it. A key that is not a setting name is
// dropped: the search still runs, and the clause is still drops's.
func TestVectorSearchDropsASettingKeyThatIsNotAName(t *testing.T) {
	sql, _ := searchSQL(t, vector.Search([]float32{1, 0}).
		Param("clickhouse.max_threads = 1, allow_nullable_key", true).
		Param("clickhouse.max_threads", 4).
		Build())
	if !strings.Contains(sql, "max_threads = 4") {
		t.Errorf("the well-formed setting should still be there:\n%s", sql)
	}
	if strings.Contains(sql, "allow_nullable_key") {
		t.Errorf("a key the caller wrote as SQL reached the clause:\n%s", sql)
	}
}

// A string param is quoted, and the quoting has to survive a value
// that ends in a backslash — which would otherwise escape the closing
// quote and leave the literal open over the rest of the clause.
func TestVectorSearchQuotesASettingValueEndingInABackslash(t *testing.T) {
	sql, _ := searchSQL(t, vector.Search([]float32{1, 0}).
		Param("clickhouse.storage_policy", `tier\`).
		Build())
	if !strings.Contains(sql, `storage_policy = 'tier\\'`) {
		t.Errorf("the backslash is not escaped:\n%s", sql)
	}
}
