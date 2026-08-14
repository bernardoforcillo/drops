package vector_test

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/vector"
)

// sexpr is a Visitor that renders a filter as an s-expression. It
// exists so the traversal itself can be asserted on without dragging a
// SQL dialect or an HTTP client into the test.
type sexpr struct{}

func (sexpr) And(parts []string) (string, error) { return join("and", parts), nil }
func (sexpr) Or(parts []string) (string, error)  { return join("or", parts), nil }
func (sexpr) Not(part string) (string, error)    { return "(not " + part + ")", nil }

func (sexpr) Compare(op vector.Op, field string, value any) (string, error) {
	return fmt.Sprintf("(%s %s %v)", op, field, value), nil
}

func (sexpr) Set(op vector.Op, field string, values []any) (string, error) {
	return fmt.Sprintf("(%s %s %v)", op, field, values), nil
}

func (sexpr) Null(op vector.Op, field string) (string, error) {
	return fmt.Sprintf("(%s %s)", op, field), nil
}

func (sexpr) MatchText(field, text string) (string, error) {
	return fmt.Sprintf("(match %s %q)", field, text), nil
}

func (sexpr) HasID(ids []any) (string, error) { return fmt.Sprintf("(id %v)", ids), nil }

func (sexpr) GeoWithin(field string, box vector.GeoBox) (string, error) {
	return fmt.Sprintf("(geo %s %v %v)", field, box.TopLeft, box.BottomRight), nil
}

func join(op string, parts []string) string {
	return "(" + op + " " + strings.Join(parts, " ") + ")"
}

func compile(t *testing.T, f vector.Filter) (string, bool) {
	t.Helper()
	out, ok, err := vector.Compile[string](f, sexpr{})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return out, ok
}

func TestZeroFilterCompilesToNothing(t *testing.T) {
	if out, ok := compile(t, vector.Filter{}); ok {
		t.Errorf("zero filter should not compile to a predicate, got %q", out)
	}
}

// A connective that ends up with no meaningful children has to
// disappear entirely rather than compile to an empty node — that is
// what lets callers assemble optional predicates with And.
func TestEmptyConnectivesCollapseToZero(t *testing.T) {
	for name, f := range map[string]vector.Filter{
		"And()":           vector.And(),
		"Or()":            vector.Or(),
		"And(zero, zero)": vector.And(vector.Filter{}, vector.Filter{}),
		"Not(zero)":       vector.Not(vector.Filter{}),
	} {
		if !f.IsZero() {
			t.Errorf("%s should be the zero filter, got %+v", name, f)
		}
	}
}

func TestSingleChildConnectiveCollapses(t *testing.T) {
	got, ok := compile(t, vector.And(vector.Eq("lang", "it"), vector.Filter{}))
	if !ok {
		t.Fatal("expected a predicate")
	}
	if want := "(eq lang it)"; got != want {
		t.Errorf("got %q want %q — And should not wrap a lone child", got, want)
	}
}

func TestCompileNesting(t *testing.T) {
	f := vector.And(
		vector.Eq("lang", "it"),
		vector.Or(
			vector.In("topic", "go", "rust"),
			vector.Not(vector.IsNull("author")),
		),
		vector.Gte("year", 2020),
	)
	got, ok := compile(t, f)
	if !ok {
		t.Fatal("expected a predicate")
	}
	want := "(and (eq lang it) (or (in topic [go rust]) (not (is_null author))) (gte year 2020))"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// Between is sugar, not a node: every backend gets it for free
// because it expands to two bounds before compilation.
func TestBetweenExpandsToBounds(t *testing.T) {
	got, _ := compile(t, vector.Between("year", 2000, 2020))
	want := "(and (gte year 2000) (lte year 2020))"
	if got != want {
		t.Errorf("got %s want %s", got, want)
	}
}

func TestInExpandsLoneSlice(t *testing.T) {
	f := vector.In("topic", []string{"go", "rust"})
	if len(f.Values) != 2 {
		t.Fatalf("expected the slice to be expanded, got %v", f.Values)
	}
	if f.Values[0] != "go" || f.Values[1] != "rust" {
		t.Errorf("values = %v", f.Values)
	}
}

func TestCompileRejectsUnknownOp(t *testing.T) {
	_, _, err := vector.Compile[string](vector.Filter{Op: "nope"}, sexpr{})
	if err == nil {
		t.Fatal("expected an error for an unknown op")
	}
}

func TestGeoWithinNeedsABox(t *testing.T) {
	_, _, err := vector.Compile[string](vector.Filter{Op: vector.OpGeoWithin, Field: "loc"}, sexpr{})
	if err == nil {
		t.Fatal("expected an error for a boxless GeoWithin")
	}
}

// --- Cursors --------------------------------------------------------

func TestCursorRoundTrip(t *testing.T) {
	d := 0.25
	in := vector.Cursor{Backend: vector.BackendPostgres, Distance: &d}
	if err := in.SetID("doc-42"); err != nil {
		t.Fatal(err)
	}
	enc, err := in.Encode()
	if err != nil {
		t.Fatal(err)
	}
	out, ok, err := vector.DecodeCursor(enc, vector.BackendPostgres)
	if err != nil || !ok {
		t.Fatalf("Decode: %v ok=%v", err, ok)
	}
	if out.Distance == nil || *out.Distance != d {
		t.Errorf("distance = %v", out.Distance)
	}
	id, ok, err := out.ID()
	if err != nil || !ok {
		t.Fatalf("ID: %v ok=%v", err, ok)
	}
	if id != "doc-42" {
		t.Errorf("id = %v (%T)", id, id)
	}
}

// The reason IDs travel as text plus a kind tag rather than as bare
// JSON: an int64 past 2^53 would come back from encoding/json as a
// lossy float64, which at a page boundary silently skips or repeats a
// row.
func TestCursorPreservesLargeIntegerID(t *testing.T) {
	const big = int64(9007199254740993) // 2^53 + 1
	var c vector.Cursor
	c.Backend = vector.BackendClickHouse
	if err := c.SetID(big); err != nil {
		t.Fatal(err)
	}
	enc, err := c.Encode()
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := vector.DecodeCursor(enc, vector.BackendClickHouse)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := out.ID()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := id.(int64)
	if !ok {
		t.Fatalf("id came back as %T, want int64", id)
	}
	if got != big {
		t.Errorf("id = %d want %d (lost %d)", got, big, big-got)
	}
}

func TestCursorRejectsForeignBackend(t *testing.T) {
	enc, err := vector.OffsetCursor(vector.BackendQdrant, 20).Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := vector.DecodeCursor(enc, vector.BackendPostgres); !errors.Is(err, vector.ErrCursorMismatch) {
		t.Errorf("err = %v, want ErrCursorMismatch", err)
	}
}

func TestEmptyCursorIsFirstPage(t *testing.T) {
	c, ok, err := vector.DecodeCursor("", vector.BackendQdrant)
	if err != nil {
		t.Fatalf("empty cursor should not be an error, got %v", err)
	}
	if ok {
		t.Errorf("empty cursor should report ok=false, got %+v", c)
	}
}

func TestCursorRejectsGarbage(t *testing.T) {
	for _, s := range []string{"not base64!!", "YWJj"} { // valid base64, invalid JSON
		if _, _, err := vector.DecodeCursor(s, vector.BackendQdrant); !errors.Is(err, vector.ErrInvalidCursor) {
			t.Errorf("DecodeCursor(%q) err = %v, want ErrInvalidCursor", s, err)
		}
	}
}

func TestCursorRejectsUnsupportedIDType(t *testing.T) {
	var c vector.Cursor
	if err := c.SetID(struct{ A int }{1}); err == nil {
		t.Error("expected SetID to reject a struct ID")
	}
}

// --- Score / distance ----------------------------------------------

func TestScoreDistanceAreInverses(t *testing.T) {
	metrics := []vector.Metric{
		vector.Cosine, vector.L2, vector.InnerProduct,
		vector.L1, vector.Hamming, vector.Jaccard,
	}
	for _, m := range metrics {
		for _, d := range []float64{0, 0.5, 1, 7.25} {
			s := vector.ScoreFromDistance(m, d)
			back := vector.DistanceFromScore(m, s)
			if math.Abs(back-d) > 1e-9 {
				t.Errorf("%s: distance %v -> score %v -> %v", m, d, s, back)
			}
		}
	}
}

// Whatever the metric, a nearer hit must score higher — that is the
// entire contract of the Score field.
func TestScoreOrdersLikeDistance(t *testing.T) {
	for _, m := range []vector.Metric{vector.Cosine, vector.L2, vector.InnerProduct, vector.L1} {
		near := vector.ScoreFromDistance(m, 0.1)
		far := vector.ScoreFromDistance(m, 0.9)
		if near <= far {
			t.Errorf("%s: near scored %v, far scored %v — order inverted", m, near, far)
		}
	}
}

// --- Query builder --------------------------------------------------

func TestQueryDefaults(t *testing.T) {
	q := vector.Search([]float32{1, 2, 3}).Build()
	if q.TopK != vector.DefaultTopK {
		t.Errorf("TopK = %d want %d", q.TopK, vector.DefaultTopK)
	}
	if q.Metric != vector.Cosine {
		t.Errorf("Metric = %q want %q", q.Metric, vector.Cosine)
	}
}

func TestQueryValidateRejectsEmptyVector(t *testing.T) {
	if err := (vector.Query{}).Validate(); !errors.Is(err, vector.ErrNoVector) {
		t.Errorf("err = %v want ErrNoVector", err)
	}
}

// Where is additive so a query can be narrowed in stages — a handler
// adding filters as it inspects the request shouldn't have to collect
// them first.
func TestWhereAccumulates(t *testing.T) {
	q := vector.Search([]float32{1}).
		Where(vector.Eq("a", 1)).
		Where(vector.Eq("b", 2)).
		Build()
	got, ok := compile(t, q.Filter)
	if !ok {
		t.Fatal("expected a predicate")
	}
	if want := "(and (eq a 1) (eq b 2))"; got != want {
		t.Errorf("got %s want %s", got, want)
	}
}

func TestBuilderCarriesEverything(t *testing.T) {
	q := vector.Search([]float32{1, 2}).
		TopK(7).
		Metric(vector.L2).
		MaxDistance(1.5).
		WithVector().
		WithPayload().
		After("cur").
		Param("hnsw_ef", 128).
		Build()

	if q.TopK != 7 || q.Metric != vector.L2 || !q.IncludeVector || !q.IncludePayload {
		t.Errorf("query = %+v", q)
	}
	if q.MaxDistance == nil || *q.MaxDistance != 1.5 {
		t.Errorf("MaxDistance = %v", q.MaxDistance)
	}
	if q.Cursor != "cur" {
		t.Errorf("Cursor = %q", q.Cursor)
	}
	if v, ok := q.Param("hnsw_ef"); !ok || v != 128 {
		t.Errorf("Param = %v ok=%v", v, ok)
	}
}

func TestTopKIgnoresNonPositive(t *testing.T) {
	q := vector.Search([]float32{1}).TopK(5).TopK(0).TopK(-3).Build()
	if q.TopK != 5 {
		t.Errorf("TopK = %d, want the last positive value 5", q.TopK)
	}
}
