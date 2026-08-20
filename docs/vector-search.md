# Vector search

pgvector, ClickHouse and Qdrant all do similarity search, and each
wanted the query written its own way: a `drops.Expression` predicate
and a distance operator for pgvector, a JSON `must`/`should`/`must_not`
tree for Qdrant, nothing at all for ClickHouse. Moving a collection
between them, or querying two at once, meant rewriting the search.

`drops/vector` is that query written once. No SQL and no HTTP in it: a
portable filter tree, a query, a `Store` interface, and the score and
distance conventions the three backends are normalised onto.

## One query

```go
import "github.com/bernardoforcillo/drops/vector"

q := vector.Search(embedding).
    TopK(20).
    Metric(vector.Cosine).
    Where(vector.And(
        vector.Eq("lang", "it"),
        vector.Gte("published_at", 1700000000),
        vector.Not(vector.In("status", "draft", "archived")),
    )).
    WithPayload().
    Build()

res, err := store.Search(ctx, q)
```

## Three stores

```go
// pgvector — a table with a vector(N) column
store := pg.NewVectorStore(db, Docs, DocID, DocEmbedding,
    pg.WithPayloadColumn(DocMeta),   // jsonb
    pg.WithField("lang", DocLang))   // a real column beats jsonb

// ClickHouse — an Array(Float32) column, no extension needed
store := clickhouse.NewVectorStore(chdb, Docs, DocID, DocEmbedding,
    clickhouse.WithPayloadColumn(DocMeta))

// Qdrant — a collection
store := cli.Store("embeddings", qdrant.WithMetric(vector.Cosine))
```

## Filter fields

A filter names fields as strings, because Qdrant payloads have no
schema. The SQL stores resolve them in two steps: a `WithField`
mapping compiles to that column, keeping the predicate typed and
index-friendly; otherwise the name becomes a JSON accessor into the
payload column, with dotted names walking nested objects
(`"author.name"` → `meta -> 'author' ->> 'name'`).

A field that matches neither is `vector.ErrUnknownField` — better a
clear error than a predicate that quietly matches nothing.

## Distance and score

Every `Hit` carries both:

- **`Distance`** is in the metric's own units, and smaller is closer.
- **`Score`** is a ranking value where larger is better.

The conversion lives in one place, so `MaxDistance(0.25)` means the
same thing whether it becomes a `<=` on a pgvector expression or
Qdrant's `score_threshold`. Qdrant's own score is a similarity for
Cosine and Dot but the raw distance for Euclid and Manhattan; that
asymmetry is normalised in the adapter rather than leaking to callers.

## Pagination

One opaque cursor, whatever is underneath:

```go
for cursor := ""; ; {
    res, err := store.Search(ctx, vector.Search(v).TopK(50).After(cursor).Build())
    if err != nil {
        return err
    }
    handle(res.Hits)
    if !res.HasMore {
        break
    }
    cursor = res.NextCursor
}
```

The SQL stores paginate by keyset on `(distance, id)`, so the next page
is guarded by `(distance, id) > (lastDistance, lastID)` and concurrent
inserts cannot shift a row across a boundary. Qdrant's search API has
no keyset, so its cursor carries an offset instead. Both encode to the
same opaque string, and each is stamped with the backend that issued
it: replaying a Qdrant cursor against pgvector returns
`vector.ErrCursorMismatch` rather than serving the wrong page.

`HasMore` costs no extra round trip — every backend asks for `TopK+1`
and trims.

## Two honest caveats

**pgvector's HNSW and IVFFlat indexes are approximate**, and a `WHERE`
clause is applied on top of what the index returned. A very selective
filter or a deep page can therefore come back with fewer than `TopK`
rows even though more matches exist. That is pgvector's behaviour, not
this adapter's; widen the search per query:

```go
vector.Search(v).TopK(20).Param("hnsw.ef_search", 200)
```

**ClickHouse without a vector index is an exact brute-force scan.** It
cannot miss a match, which is the opposite trade — but it is linear, so
a billion-row collection wants either a partition key that prunes most
parts, or Qdrant.

## Adding a backend

Implement `vector.Store` — one method. Compile the filter by handing a
`vector.Visitor` to `vector.Compile`, which owns the traversal so each
backend only supplies the leaves.
