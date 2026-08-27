# Making the planner do what you meant

PostgreSQL's planner is very good and occasionally wrong, and when it
is wrong the usual cause is not a bad choice — it is a good choice made
from a bad row estimate. This page is the three tools drops gives you
for that: measuring how selective a predicate really is, telling the
planner what to do when it will not be talked out of it, and holding
the result to a test.

## Is this predicate worth an index?

The question behind most "should we add an index" discussions, and it
has a number:

```go
s, err := pg.SketchColumn(ctx, db, "orders", "status", pg.SketchOptions{})

s.Selectivity("pending")   // 0.24
```

A quarter of the table. An index on `status` will not help — the
planner will read the table and it will be right to.

Rules of thumb, and they are only that:

| selectivity | what happens |
|---|---|
| below ~0.05 | an index scan pays |
| 0.05 – 0.2 | it depends on the row width and the correlation |
| above ~0.2 | the planner reads the table, whatever indexes exist |

`SketchColumn` is a full scan unless you sample it, so it belongs in a
maintenance job rather than on a request path:

```go
s, err := pg.SketchColumn(ctx, db, "events", "tenant_id",
    pg.SketchOptions{Sample: 0.01})   // TABLESAMPLE SYSTEM (1)
```

Store the answer rather than recomputing it:

```go
blob, _ := s.MarshalBinary()      // a few tens of KB, whatever the cardinality
// ... later, at start-up
var s pg.Sketch
_ = s.UnmarshalBinary(blob)
```

### What the structure is, and why that one

A [Count-Min sketch](https://en.wikipedia.org/wiki/Count%E2%80%93min_sketch):
a fixed table of counters, constant space whatever the cardinality.
InstantDB keeps one per attribute and uses it to order joins.

The property that makes it the *right* structure here rather than a
convenient one: **it never undercounts.** Hash collisions add other
values' counts to yours, so an estimate is an upper bound — exact for
the values that matter and inflated for the rare ones.

That asymmetry lands the error on the safe side. Over-estimating
selectivity makes you decline an index you might have wanted: a missed
optimisation, visible in a latency graph. Under-estimating would make
you declare a predicate selective when it is not, which is the mistake
that produces the bad plan in the first place.

`ExpectedError()` reports the over-count the dimensions admit — the row
total divided by the width. An estimate within that of zero is noise
rather than evidence.

It also **merges exactly**, so a sketch can be built per shard or per
partition and folded together, and the result is identical to one
built in a single pass.

What it cannot do is enumerate what it holds — it stores counters, not
values. `TopValues` takes the candidates from you:

```go
for _, c := range s.TopValues("pending", "shipped", "cancelled") {
    fmt.Println(c.Value, c.Estimate, c.Selectivity)
}
```

That list is usually one you already have: the enum's labels, the
tenants on record, the statuses the application defines.

## Telling the planner what to do

PostgreSQL has no hint syntax. The
[`pg_hint_plan`](https://github.com/ossc-db/pg_hint_plan) extension
adds one, reading directives from a block comment at the *front* of
the statement. drops already builds a comment per statement for
[query tags](../querytag.go) — but that one goes at the end, because
MySQL executes a leading `/*+` and five dialects share the plumbing.
So hints are a second, PostgreSQL-only path with the opposite
placement:

```go
ctx = pg.WithPlanHints(ctx, pg.IndexScan("orders", "orders_customer_created_idx"))
rows, err := Orders.Query(db).Where(pg.Eq(OrderCustomer, id)).All(ctx)
```

```sql
/*+ IndexScan(orders orders_customer_created_idx) */
SELECT ... FROM "orders" WHERE "customer_id" = $1 /*controller='orders'*/
```

The full set:

| group | constructors |
|---|---|
| scan | `SeqScan` `NoSeqScan` `IndexScan` `NoIndexScan` `IndexOnlyScan` `NoIndexOnlyScan` `BitmapScan` `NoBitmapScan` `TidScan` `NoTidScan` |
| join | `NestLoop` `NoNestLoop` `HashJoin` `NoHashJoin` `MergeJoin` `NoMergeJoin` |
| order | `Leading` |
| estimates | `RowsAbsolute` `RowsMultiply` |
| other | `Parallel` `SetGUC` |

### Scope the context to one query

Hints live on the context, so they apply to every statement issued
with it. `pg_hint_plan` ignores a hint naming a relation the query does
not contain, so a stray hint is usually harmless — but "usually
harmless" is not a contract, and a hint that silently applies to the
wrong statement is worse than no hint.

```go
hinted := pg.WithPlanHints(ctx, pg.NestLoop("a", "b"))
rows, err := db.Query(hinted, sql, args...)   // this one, nothing else
```

Build the context immediately before the call it is for. Do not pass
it down. `pg.WithPlanHints(ctx)` with no arguments clears the set,
which is how a sub-query opts out of one it inherited.

### Prefer the negative hint

`NoSeqScan("orders")` says what not to do and leaves the planner free
among the rest. `IndexScan("orders", "orders_customer_idx")` pins the
plan to an object a migration can rename or drop, at which point the
hint silently stops applying.

`RowsAbsolute` and `RowsMultiply` are better still where they fit: they
correct the *estimate*, which is the actual cause, and leave every
decision to the planner. When that works it keeps working as the data
changes, which `Leading` — which removes the search entirely — does
not.

### Identifiers are validated, not escaped

A quoted identifier absorbs a double quote. Nothing absorbs a `*/`, and
PostgreSQL nests block comments, so an embedded `/*` moves the end too.
A name carrying either is refused outright and the hint is dropped:

```go
if bad, ok := pg.HintsValid(hints); !ok {
    log.Fatalf("unrenderable plan hint: %q", bad)
}
```

Fail-closed is the only defensible default in the statement path — a
query that loses a hint runs slowly, a query that gains a broken
comment does not run at all — but that makes the mistake silent, so
`HintsValid` exists to catch it at start-up or in a test.

### It is silent when it does not apply

The extension may not be loaded. The relation name may not match the
alias the planner sees. The index may have been renamed. In every case
the comment is just a comment, the query runs unhinted, and nothing
reports it.

Which is why the next section exists.

## Holding the plan to a test

`ExplainPlan.Fingerprint()` answers "did the plan change?" — the right
question for a regression alert and the wrong one for a test. It is
opaque: it tells you something moved, not whether the query does what
you meant, and a test written against one has to be updated whenever
anything shifts, including the shifts that are fine.

So read the plan instead:

```go
plan, err := pg.Explain(db, ctx, sql, args...)

if err := plan.Expect(
    pg.UsesIndex("orders", "orders_customer_created_idx"),
    pg.NoSeqScanOn("orders"),
    pg.NoNestedLoopOver("order_lines"),
    pg.MaxCost(5000),
); err != nil {
    t.Error(err)
}
```

Every expectation is evaluated, not just the first: when a plan is
wrong it is usually wrong in more than one way, and a test that
reports one failure per run turns a five-minute fix into five runs.
The messages name what was actually seen, so the fix does not need a
second run either.

| expectation | catches |
|---|---|
| `UsesIndex(rel, idx)` | the hint stopped applying; the index was dropped |
| `UsesAnyIndex(rel)` | the same, without pinning to a name |
| `NoSeqScanOn(rel)` | a predicate that stopped being sargable |
| `NoNestedLoopOver(rel)` | the shape that is fast on a fixture and quadratic in production |
| `NoNodeType("Materialize")` | anything you have decided you do not want |
| `MaxCost(n)` | a ratchet against an order-of-magnitude regression |

Two notes on using these honestly.

**`NoSeqScanOn` is an assertion about a large table.** PostgreSQL is
right to scan twenty rows sequentially, so on a small fixture this will
fail for a query that is perfectly correct. Populate the table, or
assert on a query against one the fixture fills properly.

**Cost units are arbitrary.** `MaxCost` is a ratchet, not a threshold:
record what the query costs today, set the limit somewhat above it,
and let the test catch the change that makes it ten times worse.

### Reading a plan without a database

`ExplainPlan.JSON` carries the raw `EXPLAIN (FORMAT JSON)` payload
verbatim, and `ParseExplainJSON` reads it back:

```go
plan, err := pg.ParseExplainJSON(stored)
```

So a plan captured in production next to a slow query can be asserted
on offline, or diffed against today's.

`IndexesUsed()` is the accessor most tests actually want, and it earns
its keep on the awkward case: a `Bitmap Index Scan` names the index and
leaves the relation to the `Bitmap Heap Scan` above it, so attributing
one to the other takes a walk up the tree.

## Putting the three together

The loop this is meant to support:

1. A query is slow. `pg.Explain` shows a sequential scan.
2. `pg.SketchColumn` says the predicate matches 0.3% of rows, so an
   index should be winning.
3. The estimate is the problem — `RowsAbsolute` or `NoSeqScan` fixes
   the plan.
4. `plan.Expect(pg.UsesIndex(...))` in a test, so that when the hint
   stops applying somebody finds out.

Step 4 is the one people skip, and it is the one that makes the other
three worth doing more than once.
