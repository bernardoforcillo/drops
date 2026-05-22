package pg_test

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// Vector-flavoured schema fixture.
var (
	items         = pg.NewTable("items")
	itemID        = pg.Add(items, pg.BigSerial("id").PrimaryKey())
	itemEmbedding = pg.Add(items, pg.Vector("embedding", 384))
	itemHalfEmbed = pg.Add(items, pg.HalfVec("half_embedding", 384))
)

func TestVectorColumnTypes(t *testing.T) {
	if got := itemEmbedding.Type().TypeSQL(); got != "vector(384)" {
		t.Errorf("vector type: got %q, want vector(384)", got)
	}
	if got := itemHalfEmbed.Type().TypeSQL(); got != "halfvec(384)" {
		t.Errorf("halfvec type: got %q, want halfvec(384)", got)
	}
	if got := pg.SparseVec("s", 16).Type().TypeSQL(); got != "sparsevec(16)" {
		t.Errorf("sparsevec type: got %q", got)
	}
	if got := pg.BitVec("b", 64).Type().TypeSQL(); got != "bit(64)" {
		t.Errorf("bit type: got %q", got)
	}
}

func TestDistanceOperators(t *testing.T) {
	q := []float32{1, 2, 3}
	cases := []struct {
		name string
		expr drops.Expression
		want string
	}{
		{"L2", pg.L2Distance(itemEmbedding, q), `("items"."embedding" <-> $1)`},
		{"IP", pg.InnerProduct(itemEmbedding, q), `("items"."embedding" <#> $1)`},
		{"Cosine", pg.CosineDistance(itemEmbedding, q), `("items"."embedding" <=> $1)`},
		{"L1", pg.L1Distance(itemEmbedding, q), `("items"."embedding" <+> $1)`},
		// method forms.
		{"Cosine method", itemEmbedding.Cosine(q), `("items"."embedding" <=> $1)`},
		{"L2 method", itemEmbedding.L2(q), `("items"."embedding" <-> $1)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, args := drops.String(tc.expr)
			if got != tc.want {
				t.Errorf("sql\n  got:  %s\n  want: %s", got, tc.want)
			}
			if len(args) != 1 {
				t.Errorf("expected 1 arg, got %v", args)
			}
		})
	}
}

func TestNearestNeighbourQuery(t *testing.T) {
	db := pg.New(nil)
	q := []float32{0.1, 0.2, 0.3}
	sql, _ := db.Select(itemID, pg.As(itemEmbedding.Cosine(q), "distance")).
		From(items).
		OrderBy(itemEmbedding.Cosine(q)).
		Limit(10).
		ToSQL()
	want := `SELECT "items"."id", ("items"."embedding" <=> $1) AS "distance" FROM "items" ORDER BY ("items"."embedding" <=> $2) LIMIT $3`
	if sql != want {
		t.Errorf("nn sql\n  got:  %s\n  want: %s", sql, want)
	}
}

func TestHNSWIndexWithOpClass(t *testing.T) {
	idx := pg.NewIndex("items_embedding_hnsw", items, itemEmbedding).
		Using("hnsw").
		OpClass(pg.VectorCosineOps).
		With("m = 16, ef_construction = 64")
	got, _ := drops.String(pg.CreateIndex(idx))
	want := `CREATE INDEX "items_embedding_hnsw" ON "items" USING hnsw ("items"."embedding" vector_cosine_ops) WITH (m = 16, ef_construction = 64)`
	if got != want {
		t.Errorf("hnsw\n  got:  %s\n  want: %s", got, want)
	}
}

func TestIVFFlatIndex(t *testing.T) {
	idx := pg.NewIndex("items_l2_idx", items, itemEmbedding).
		Using("ivfflat").
		OpClass(pg.VectorL2Ops).
		With("lists = 100")
	got, _ := drops.String(pg.CreateIndex(idx))
	if !strings.Contains(got, `USING ivfflat ("items"."embedding" vector_l2_ops)`) ||
		!strings.Contains(got, `WITH (lists = 100)`) {
		t.Errorf("ivfflat: %s", got)
	}
}
