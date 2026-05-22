package pg

import (
	"fmt"

	"github.com/bernardoforcillo/drops"
)

// pgvector support.
//
// pgvector (https://github.com/pgvector/pgvector) is the standard
// PostgreSQL extension for vector similarity search. Install it once
// per database with:
//
//	db.ExecExpr(ctx, pg.CreateExtensionIfNotExists("vector"))
//
// Then declare columns with Vector / HalfVec / SparseVec / BitVec, and
// query with the distance helpers below.

// Vector returns a vector(N) column. The Go value type is []float32 —
// the same shape pgvector exposes through pgx and lib/pq's text codec.
func Vector(name string, dim int) *Col[[]float32] {
	if dim <= 0 {
		dim = 1
	}
	return newCol[[]float32](name, simpleType(fmt.Sprintf("vector(%d)", dim)))
}

// HalfVec returns a halfvec(N) column (half-precision float). The Go
// value type stays []float32; the driver converts on the wire.
func HalfVec(name string, dim int) *Col[[]float32] {
	if dim <= 0 {
		dim = 1
	}
	return newCol[[]float32](name, simpleType(fmt.Sprintf("halfvec(%d)", dim)))
}

// SparseVec returns a sparsevec(N) column. Encoding is driver-specific;
// represented as string for portability.
func SparseVec(name string, dim int) *Col[string] {
	if dim <= 0 {
		dim = 1
	}
	return newCol[string](name, simpleType(fmt.Sprintf("sparsevec(%d)", dim)))
}

// BitVec returns a bit(N) column for binary-vector similarity search.
func BitVec(name string, dim int) *Col[string] {
	if dim <= 0 {
		dim = 1
	}
	return newCol[string](name, simpleType(fmt.Sprintf("bit(%d)", dim)))
}

// Distance operators ----------------------------------------------

// L2Distance renders <col> <-> <vec> — Euclidean distance.
// The right-hand side is typically a Go []float32 (bound as a param)
// or another vector column.
func L2Distance(left, right any) drops.Expression { return binOp(left, "<->", right) }

// InnerProduct renders <col> <#> <vec> — pgvector's negative inner
// product. Smaller is better; for raw inner product, negate the
// expression or use the cosine helper if your vectors are normalised.
func InnerProduct(left, right any) drops.Expression { return binOp(left, "<#>", right) }

// CosineDistance renders <col> <=> <vec>.
func CosineDistance(left, right any) drops.Expression { return binOp(left, "<=>", right) }

// L1Distance renders <col> <+> <vec> — Manhattan distance.
func L1Distance(left, right any) drops.Expression { return binOp(left, "<+>", right) }

// HammingDistance renders <col> <~> <vec> — bit-vector Hamming
// distance (BitVec columns only).
func HammingDistance(left, right any) drops.Expression { return binOp(left, "<~>", right) }

// JaccardDistance renders <col> <%> <vec> — bit-vector Jaccard
// distance.
func JaccardDistance(left, right any) drops.Expression { return binOp(left, "<%>", right) }

// Convenience methods on the typed Vector / HalfVec columns. These
// let queries read `Embedding.L2(query)` rather than the free-function
// form.

// L2 is the method form of L2Distance.
func (c *Col[T]) L2(v any) drops.Expression { return L2Distance(c.Column, v) }

// IP is the method form of InnerProduct.
func (c *Col[T]) IP(v any) drops.Expression { return InnerProduct(c.Column, v) }

// Cosine is the method form of CosineDistance.
func (c *Col[T]) Cosine(v any) drops.Expression { return CosineDistance(c.Column, v) }

// L1 is the method form of L1Distance.
func (c *Col[T]) L1(v any) drops.Expression { return L1Distance(c.Column, v) }

// Index op classes (used with NewIndex(...).Using("hnsw") + WithOpClass)
// ----------------------------------------------------------------

// VectorOpClass is one of the per-distance-metric operator classes
// pgvector exposes for indexing. Pass the relevant value to
// (*Index).OpClass() on a Vector column.
type VectorOpClass string

const (
	VectorL2Ops     VectorOpClass = "vector_l2_ops"
	VectorIPOps     VectorOpClass = "vector_ip_ops"
	VectorCosineOps VectorOpClass = "vector_cosine_ops"
	VectorL1Ops     VectorOpClass = "vector_l1_ops"

	HalfVecL2Ops     VectorOpClass = "halfvec_l2_ops"
	HalfVecIPOps     VectorOpClass = "halfvec_ip_ops"
	HalfVecCosineOps VectorOpClass = "halfvec_cosine_ops"

	BitHammingOps VectorOpClass = "bit_hamming_ops"
	BitJaccardOps VectorOpClass = "bit_jaccard_ops"
)

// OpClass attaches a per-column operator class hint to the most recent
// column in the index. Use it alongside Using("hnsw") or
// Using("ivfflat") on a vector index:
//
//	idx := pg.NewIndex("items_embedding_idx", Items, Embedding).
//	    Using("hnsw").
//	    OpClass(pg.VectorCosineOps).
//	    With("m = 16, ef_construction = 64")
//
// (Index.With and OpClass are pgvector additions to the Index type;
// see index.go for the underlying fields.)
func (i *Index) OpClass(class VectorOpClass) *Index {
	i.opClass = string(class)
	return i
}

// With attaches a `WITH (key = value, ...)` clause — the conventional
// place for pgvector index parameters (m, ef_construction, lists).
func (i *Index) With(spec string) *Index {
	i.with = spec
	return i
}
