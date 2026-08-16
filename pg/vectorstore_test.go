package pg_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
	"github.com/bernardoforcillo/drops/vector"
)

// A small pgvector-shaped schema shared by the tests below.
var (
	vsDocs  = pg.NewTable("docs")
	vsID    = pg.Add(vsDocs, pg.BigInt("id").PrimaryKey())
	vsEmbed = pg.Add(vsDocs, pg.Vector("embedding", 3))
	vsLang  = pg.Add(vsDocs, pg.Text("lang"))
	vsMeta  = pg.Add(vsDocs, pg.JSONB("meta"))
)

func newVectorStore(t *testing.T, rows func(sql string, args []any) (drops.Rows, error), opts ...pg.VectorStoreOption) (*pg.VectorStore, *fakeDriver) {
	t.Helper()
	fd := &fakeDriver{handler: rows}
	db := pg.New(fd)
	return pg.NewVectorStore(db, vsDocs, vsID, vsEmbed, opts...), fd
}

// searchSQL runs a query against a driver that returns nothing and
// hands back the SQL and args the store produced.
func searchSQL(t *testing.T, q vector.Query, opts ...pg.VectorStoreOption) (string, []any) {
	t.Helper()
	store, fd := newVectorStore(t, nil, opts...)
	if _, err := store.Search(context.Background(), q); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(fd.queries) != 1 {
		t.Fatalf("expected 1 query, got %d", len(fd.queries))
	}
	return fd.queries[0], fd.args[0]
}

func TestVectorSearchBaseSQL(t *testing.T) {
	sql, args := searchSQL(t, vector.Search([]float32{1, 0, -0.5}).TopK(4).Build())

	for _, want := range []string{
		`"docs"."id"`,
		`("docs"."embedding" <=> $1::vector)`,
		`AS "_drops_distance"`,
		`FROM "docs"`,
		`ORDER BY "_drops_distance" ASC, "docs"."id" ASC`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("SQL missing %s\ngot %s", want, sql)
		}
	}
	// The vector is bound, not spliced, and rendered in pgvector's
	// text form.
	if got, want := args[0], "[1,0,-0.5]"; got != want {
		t.Errorf("bound vector = %v want %v", got, want)
	}
	// TopK+1 is how HasMore is decided without a second query.
	if got := args[len(args)-1]; got != int64(5) {
		t.Errorf("LIMIT arg = %v want 5", got)
	}
}

func TestVectorSearchMetricOperators(t *testing.T) {
	cases := map[vector.Metric]string{
		vector.Cosine:       "<=>",
		vector.L2:           "<->",
		vector.InnerProduct: "<#>",
		vector.L1:           "<+>",
		vector.Hamming:      "<~>",
		vector.Jaccard:      "<%>",
	}
	for m, op := range cases {
		sql, args := searchSQL(t, vector.Search([]float32{1, 0, 1}).Metric(m).Build())
		if !strings.Contains(sql, op) {
			t.Errorf("%s: SQL missing operator %s\ngot %s", m, op, sql)
		}
		// The bit metrics take a bit(N) literal rather than a vector.
		if m == vector.Hamming || m == vector.Jaccard {
			if !strings.Contains(sql, "::bit(3)") {
				t.Errorf("%s: expected a bit(3) cast, got %s", m, sql)
			}
			if args[0] != "101" {
				t.Errorf("%s: bit literal = %v want 101", m, args[0])
			}
		}
	}
}

func TestVectorSearchMappedFieldBeatsPayload(t *testing.T) {
	sql, _ := searchSQL(t,
		vector.Search([]float32{1}).Where(vector.Eq("lang", "it")).Build(),
		pg.WithField("lang", vsLang),
		pg.WithPayloadColumn(vsMeta),
	)
	if !strings.Contains(sql, `"docs"."lang" =`) {
		t.Errorf("expected the mapped column, got %s", sql)
	}
	if strings.Contains(sql, `"meta"`) {
		t.Errorf("payload column used despite a mapping: %s", sql)
	}
}

func TestVectorSearchPayloadFallbackAndCasts(t *testing.T) {
	sql, _ := searchSQL(t,
		vector.Search([]float32{1}).Where(vector.And(
			vector.Eq("author.name", "ada"),
			vector.Gte("year", 2020),
		)).Build(),
		pg.WithPayloadColumn(vsMeta),
	)
	// Dotted names walk nested objects; the last hop uses ->> so the
	// cast lands on a scalar.
	if !strings.Contains(sql, `"docs"."meta" -> 'author' ->> 'name'`) {
		t.Errorf("nested accessor missing from %s", sql)
	}
	// A numeric operand gets a numeric comparison rather than a
	// lexical one.
	if !strings.Contains(sql, `->> 'year')::numeric`) {
		t.Errorf("numeric cast missing from %s", sql)
	}
}

func TestVectorSearchUnknownField(t *testing.T) {
	store, _ := newVectorStore(t, nil)
	_, err := store.Search(context.Background(),
		vector.Search([]float32{1}).Where(vector.Eq("nope", 1)).Build())
	if !errors.Is(err, vector.ErrUnknownField) {
		t.Errorf("err = %v want ErrUnknownField", err)
	}
}

// SQL's three-valued logic would drop rows with a NULL field from a
// Ne, which is not what the portable filter means — and not what
// Qdrant does with the same filter.
func TestVectorSearchNeKeepsNulls(t *testing.T) {
	sql, _ := searchSQL(t,
		vector.Search([]float32{1}).Where(vector.Ne("lang", "it")).Build(),
		pg.WithField("lang", vsLang),
	)
	if !strings.Contains(sql, "IS NULL") {
		t.Errorf("Ne should also admit rows with no value: %s", sql)
	}
}

func TestVectorSearchMatchTextBindsTheNeedle(t *testing.T) {
	sql, args := searchSQL(t,
		vector.Search([]float32{1}).Where(vector.MatchText("lang", "ital")).Build(),
		pg.WithField("lang", vsLang),
	)
	if !strings.Contains(sql, "ILIKE ('%' || $") {
		t.Errorf("expected wildcards concatenated onto a bound param, got %s", sql)
	}
	// The needle stays a parameter — no pattern splicing.
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

func TestVectorSearchMaxDistance(t *testing.T) {
	sql, args := searchSQL(t, vector.Search([]float32{1}).MaxDistance(0.3).Build())
	if !strings.Contains(sql, "<=") {
		t.Errorf("MaxDistance should render a bound: %s", sql)
	}
	found := false
	for _, a := range args {
		if a == 0.3 {
			found = true
		}
	}
	if !found {
		t.Errorf("distance ceiling not bound: %v", args)
	}
}

// --- Pagination -----------------------------------------------------

func TestVectorSearchKeysetPagination(t *testing.T) {
	// Three rows available, page size two.
	rows := func(string, []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"id", "_drops_distance"},
			data: [][]any{
				{int64(1), 0.10},
				{int64(2), 0.20},
				{int64(3), 0.30},
			},
		}, nil
	}
	store, fd := newVectorStore(t, rows)
	ctx := context.Background()

	first, err := store.Search(ctx, vector.Search([]float32{1}).TopK(2).Build())
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first.Hits) != 2 {
		t.Fatalf("first page has %d hits, want 2", len(first.Hits))
	}
	if !first.HasMore || first.NextCursor == "" {
		t.Fatalf("expected a further page, got HasMore=%v cursor=%q", first.HasMore, first.NextCursor)
	}
	if first.Hits[0].ID != int64(1) || first.Hits[0].Distance != 0.10 {
		t.Errorf("first hit = %+v", first.Hits[0])
	}

	if _, err := store.Search(ctx, vector.Search([]float32{1}).TopK(2).After(first.NextCursor).Build()); err != nil {
		t.Fatalf("second page: %v", err)
	}
	sql, args := fd.queries[1], fd.args[1]
	// The guard is the row-comparison form, which PostgreSQL
	// evaluates lexicographically.
	if !strings.Contains(sql, `") > (`) {
		t.Errorf("expected a (distance, id) > (...) guard, got %s", sql)
	}
	// The last page's distance and id are what it resumes from.
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

func TestVectorSearchTerminalPageHasNoCursor(t *testing.T) {
	rows := func(string, []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"id", "_drops_distance"},
			data: [][]any{{int64(1), 0.1}},
		}, nil
	}
	store, _ := newVectorStore(t, rows)
	res, err := store.Search(context.Background(), vector.Search([]float32{1}).TopK(5).Build())
	if err != nil {
		t.Fatal(err)
	}
	if res.HasMore || res.NextCursor != "" {
		t.Errorf("HasMore=%v cursor=%q, want a terminal page", res.HasMore, res.NextCursor)
	}
}

func TestVectorSearchRejectsForeignCursor(t *testing.T) {
	enc, err := vector.OffsetCursor(vector.BackendQdrant, 10).Encode()
	if err != nil {
		t.Fatal(err)
	}
	store, _ := newVectorStore(t, nil)
	_, err = store.Search(context.Background(), vector.Search([]float32{1}).After(enc).Build())
	if !errors.Is(err, vector.ErrCursorMismatch) {
		t.Errorf("err = %v want ErrCursorMismatch", err)
	}
}

// --- Payload / vector decoding --------------------------------------

func TestVectorSearchDecodesPayloadAndVector(t *testing.T) {
	rows := func(string, []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"id", "_drops_distance", "meta", "embedding"},
			data: [][]any{{
				int64(7), 0.5,
				[]byte(`{"lang":"it","year":2021}`),
				[]byte("[1,0.5,-1]"),
			}},
		}, nil
	}
	store, _ := newVectorStore(t, rows, pg.WithPayloadColumn(vsMeta))
	res, err := store.Search(context.Background(),
		vector.Search([]float32{1}).WithPayload().WithVector().Build())
	if err != nil {
		t.Fatal(err)
	}
	hit := res.Hits[0]
	if hit.Payload["lang"] != "it" {
		t.Errorf("payload = %v", hit.Payload)
	}
	want := []float32{1, 0.5, -1}
	if len(hit.Vector) != len(want) {
		t.Fatalf("vector = %v", hit.Vector)
	}
	for i := range want {
		if hit.Vector[i] != want[i] {
			t.Errorf("vector[%d] = %v want %v", i, hit.Vector[i], want[i])
		}
	}
	// Score follows the shared convention, whichever metric ran.
	if hit.Score != vector.ScoreFromDistance(vector.Cosine, 0.5) {
		t.Errorf("Score = %v", hit.Score)
	}
}

func TestFormatParseVectorRoundTrip(t *testing.T) {
	in := []float32{0, 1.5, -2.25, 1e-3}
	got, err := pg.ParseVector(pg.FormatVector(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(in) {
		t.Fatalf("len %d vs %d", len(got), len(in))
	}
	for i := range in {
		if got[i] != in[i] {
			t.Errorf("[%d] = %v want %v", i, got[i], in[i])
		}
	}
}

func TestParseVectorRejectsGarbage(t *testing.T) {
	if _, err := pg.ParseVector("[1,two,3]"); err == nil {
		t.Error("expected a parse error")
	}
}

func TestFormatBitVector(t *testing.T) {
	if got := pg.FormatBitVector([]float32{1, 0, 0.5, 0}); got != "1010" {
		t.Errorf("got %q want 1010", got)
	}
}
