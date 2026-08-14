// Package vector is the backend-neutral vector-search layer shared by
// drops/pg (pgvector), drops/clickhouse and drops/qdrant.
//
// Before this package each backend had its own search vocabulary:
// Qdrant took a JSON Filter tree with Must / Should / MustNot blocks,
// pgvector took drops.Expression predicates and a distance operator,
// and ClickHouse took neither. Application code that wanted to move a
// collection from one store to the other — or fan a query out across
// two of them — had to be rewritten around the new vocabulary.
//
// The types here are that vocabulary, written once:
//
//   - [Filter] is a portable predicate tree (And / Or / Not over Eq,
//     In, Range, IsNull, MatchText, HasID, GeoWithin).
//   - [Query] is a similarity search: query vector, TopK, [Metric],
//     filter, distance ceiling, cursor.
//   - [Hit] and [Results] are the answer, with both a distance (lower
//     is closer) and a score (higher is better) whatever the metric.
//   - [Store] is the one-method interface every backend implements.
//
// A backend turns a Filter into its own representation by handing a
// [Visitor] to [Compile]; the traversal, the error handling and the
// empty/degenerate cases live here rather than being re-derived three
// times.
//
// # Using it
//
//	q := vector.Search(embedding).
//	    TopK(20).
//	    Metric(vector.Cosine).
//	    Where(vector.And(
//	        vector.Eq("lang", "it"),
//	        vector.Gte("published_at", 1700000000),
//	    )).
//	    WithPayload().
//	    Build()
//
//	res, err := store.Search(ctx, q)   // store is any vector.Store
//
// The same q runs against a [github.com/bernardoforcillo/drops/qdrant]
// collection, a pgvector table or a ClickHouse table — only the value
// bound to store changes.
//
// # Pagination
//
// Every backend paginates through one opaque [Cursor] string. Callers
// never build or parse one: pass [Results.NextCursor] to
// [QueryBuilder.After] to get the following page, and stop when
// [Results.HasMore] is false.
//
//	for cursor := ""; ; {
//	    res, err := store.Search(ctx, vector.Search(v).TopK(50).After(cursor).Build())
//	    if err != nil {
//	        return err
//	    }
//	    handle(res.Hits)
//	    if !res.HasMore {
//	        break
//	    }
//	    cursor = res.NextCursor
//	}
//
// What the cursor carries underneath differs, because the backends
// genuinely differ:
//
//   - SQL backends (pgvector, ClickHouse) use a keyset on
//     (distance, id). The next page's guard is
//     (distance, id) > (lastDistance, lastID), so concurrent inserts
//     never shift rows across a page boundary the way OFFSET does.
//   - Qdrant's search API has no keyset — it exposes an integer
//     offset — so the cursor carries that offset instead.
//
// Both encode to the same opaque base64 string, and each cursor is
// stamped with the backend that issued it: replaying a Qdrant cursor
// against a pgvector store fails with [ErrCursorMismatch] rather than
// silently returning the wrong page.
//
// Detecting a further page costs nothing extra — every backend asks
// for TopK+1 rows and trims.
package vector
