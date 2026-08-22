# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
once a 1.0 is cut.

## [Unreleased]

### Added
- **`pg.DB.InTxAs` and `pg.Session`** — drops could WRITE a row-level
  security policy (`Table.EnableRLS`, `NewPolicy`, `Table.AddPolicy`)
  and could not SATISFY one: nothing in the package made a pooled
  connection carry the identity a policy reads, so the package doc's
  conclusion that RLS is the isolation boundary was an instruction to
  write the other half by hand. `InTxAs` runs a transaction under a
  `Session` — a role established with `SET LOCAL ROLE`, session
  settings established with `set_config(k, v, true)`. `LOCAL` is the
  word that carries it: the server reverts both when the transaction
  ends, so a connection cannot hand one request's identity to the next
  request that draws it, which is how the hand-rolled version breaks
  and it breaks towards more access. A failure to establish the
  identity aborts the transaction and the body never runs — falling
  back to the pool's own user is a fail-open beneath every tenant
  predicate in the package. The role is validated and quoted because
  `SET ROLE` has no placeholder slot; `set_config`'s key and value are
  bound, because it is an ordinary function. New sentinels
  `ErrInvalidRole`, `ErrInvalidSetting` and `ErrEmptySession`. The
  integration suite asks a real server the two questions a rendering
  test cannot: whether the policy filters, and whether `LOCAL` means
  what it says — over a pool of one connection, with a control probe
  that proves the pool does not reset sessions on its own.
- **Version bands in `drops/mirror`** — `Change.Version` decides which
  of two writes to a row the mirror keeps, and a `ReplacingMergeTree`
  never revisits that decision. Two incomparable spaces were in play
  at once and the live one was the application's wall clock: two hosts
  five milliseconds apart inverted two updates one millisecond apart,
  permanently and silently. The `uint64` line is cut into three bands
  instead — a seed band, a clock band for a source with no sequence of
  its own, and a live band that is the outbox event id offset to
  `1<<63`. A `ReplacingMergeTree` written by an older version of this
  package needs no migration: every version it ever stamped was a
  nanosecond reading, so every row it holds sits below every sequenced
  change.
- **`mirror.VersionAwareSink`** — a fill-mode reseed is safe because
  seeded rows lose on version, which is a claim only a sink that reads
  versions can honour. `QdrantSink` cannot: Qdrant's upsert is
  last-write-wins with no compare-and-set. `NewFillReseeder` now
  refuses such a sink by name rather than corrupting it quietly.

- **Typed ad-hoc queries** (`drops.All[T]` / `drops.One[T]`) — an
  entity query was already typed, but an entity is exactly what a
  join, an aggregate or a projection does not have, so the interesting
  queries went through `All(ctx, dest any)` and reported a mismatched
  destination at run time. Both are generic over `drops.RowSource`,
  which is the `Rows(ctx)` method `pg`, `mysql` and `clickhouse`
  builders already carry, so no dialect gained a line of code. `T` may
  be a struct, a `*struct` or — for a single-column result — a scalar,
  `drops.All[int64]`; `One` reports an empty result as
  `drops.ErrNoRows`. See `docs/entities.md`.
- **Scalar slices in `drops.ScanAll`** — `ScanOne` learned scalar
  destinations earlier in this cycle and `ScanAll` did not, so
  `SELECT id` into a `*[]int64` was still "slice element must be
  struct or *struct".
- **Mirror operations** (`drops/mirror`) — a mirror could be started
  and stopped; everything an operator actually has to do to one was
  missing. `Reseeder` replays the source into the sinks, cursored and
  resumable, in a fill mode that only closes holes and a repair mode
  that goes through the outbox and can overwrite a wrong value.
  `Verifier` answers whether the mirror is equal to the source, by
  range digests that narrow only where they disagree and that encode
  both sides in Go rather than trusting two engines to agree on the
  text of a value. `Evolver` reconciles the mirror's *shape* with the
  source's — columns, sorting key and partitioning — adding and
  widening on its own and refusing a drop, a narrowing, a key column,
  a moved sorting key or an unprovable cast by name. See
  `docs/mirror.md`.
- **Scalar destinations in `One`/`All`** — `One(ctx, &n)` for a
  `COUNT(*)` failed with "requires a pointer to struct". A
  single-column query is the most common query there is, and drops
  could not consume its own result.
- **Integration suite against real servers** (`integration/`) — a
  separate Go module, so the drivers it needs cannot reach a user's
  build and drops keeps its zero-dependency property. SQLite's driver
  is pure Go and runs anywhere; Postgres, MySQL, ClickHouse and Qdrant
  run against the bundled compose services and skip with a clear
  message when absent, except in CI where `DROPS_REQUIRE_ALL` turns a
  missing server into a failure. New CI job. See `docs/testing.md` for
  what belongs in which suite.
- **`clickhouse.Dialect`** — pg, sqlite and mysql all exported one;
  without it `drops.StringWithDialect` could not render ClickHouse SQL.
- **`sqlite.Column.Asc/Desc/As`** and **`sqlite.EntityQuery.Unscoped`**
  — both existed in the other dialects. Without Unscoped a
  soft-deleted row was unreachable through the entity at all, so the
  restore flow could not be written.
- **OLTP → OLAP → vector mirroring** (`drops/mirror`) —
  `DeriveClickHouse` makes the analytics schema a function of the
  transactional one rather than a second declaration; `ClickHouseSink`
  and `QdrantSink` apply changes idempotently; `Pump` moves them from a
  durable outbox source, refusing to acknowledge a batch any sink
  rejected. See `docs/mirror.md`.
- **MySQL / MariaDB dialect** (`drops/mysql`) — schema, DDL, indexes,
  the four statement builders, operators, and Entity CRUD with the
  drift check, composite keys and relation handles. Generated keys come
  back through `LastInsertId` because MySQL has no `RETURNING`.
- **Composite primary keys in Entity** (`drops/pg`, `drops/sqlite`) —
  `Get`/`Delete` take the key variadically, `PatchKey` takes it as a
  slice, and wrong arity is `ErrKeyArity` rather than a partial match
  that would address every row sharing a column.
- **Relation handles** (`drops/pg`) — `Table.Rel` resolves a relation
  name once at declaration; `Load`/`LoadRel` take the handle so an
  eager-load is compile-checked, alongside the string-taking
  `With`/`WithRel` for relation sets that arrive from outside the
  program.
- **Schema generation from structs** (`cmd/dropsgen -schema`) — emits
  the typed `Col[T]` declarations from `//drops:schema` tags, so the
  struct is the single source of truth and drift is unrepresentable
  rather than merely detected. Worked example in `examples/schemagen`.
- **Documentation tree** (`docs/`) — getting started, schema, entities,
  dialect comparison, vector search, mirroring. Runnable examples added
  for `drops/mysql` and `drops/sqlite`.
- **Bare-identifier mode on `drops.Builder`** — `BareIdents` /
  `SetBareIdents`, so DDL that defines a table can render unqualified
  column references even when they are nested inside an expression.
- Smaller additions: `pg.SmallSerial`, `sqlite.Column.Asc/Desc/As`,
  `clickhouse.Bind`, `clickhouse.Table.OrderByColumns`,
  `(*Col[T]).Managed` on pg/sqlite/clickhouse.

### Changed
- **`NewEntity` now rejects a column bound to no struct field**
  (`drops/pg`, `drops/sqlite`, `drops/clickhouse`). It used to skip it
  silently, so a renamed field or a mistyped `drop:` tag removed the
  column from every INSERT and UPDATE while everything still compiled
  and every test that did not assert on that column still passed.
  Columns drops itself writes are exempt automatically; the rest must
  be mapped or named through `AllowUnmappedColumns`.

### Fixed
- **The tenant stamp converted the ctx tenant without asking what the
  conversion did to it, and wrote one tenant into a statement that
  addressed another** (`drops/pg`, `drops/sqlite`, `drops/mysql`,
  `drops/clickhouse`). `stampTenant` filled a zero tenant field from
  ctx on `ConvertibleTo` alone, without the string guard `sameTenant` —
  four lines below it in the same file — exists to carry. Go converts
  an integer to a string as a rune, so a ctx tenant of `65` and a TEXT
  tenant column produced `UPDATE "str_rows" SET "tenantId" = ?,
  "title" = ? WHERE ("str_rows"."id" = ?) AND ("str_rows"."tenantId" =
  ?)` bound to `"A", "x", 7, 65`: the row is ASSIGNED to tenant `"A"`
  by a statement that ADDRESSES tenant `65`, which hands it to whoever
  owns `"A"`. `Create` escaped it only because the INSERT is stamped a
  second time from the binding side, which does use `sameTenant`;
  `Update` has no second stamp, so it reached the wire. The stamp now
  converts and then asks `sameTenant` whether what came out names the
  tenant that went in, so the string case and the truncating numeric
  case are both refused — with the axis named, since that is the only
  diagnostic the caller gets. A column whose type disagrees with the
  ctx tenant's is the schema reporting a type confusion, so this
  refuses rather than reaching for `strconv`, which would accept it
  silently.
- **`sameTenant` converted in one direction, so a truncating pair
  compared equal** (`drops/pg`, `drops/sqlite`, `drops/mysql`,
  `drops/clickhouse`). `pg` converted the ctx tenant into the bound
  value's type and the other three converted the bound value into the
  ctx tenant's — identical doc comments over mirrored bodies, and
  neither direction was right. `int64(1<<32|77)` and `int32(77)`
  convert onto each other's type and match in whichever direction
  throws the high bits away, so each dialect accepted the pair the
  other refused and the statement went out carrying a value the ctx
  never named. The comparison is now a round trip — convert, compare,
  convert back, compare again — in all four, in the same words, and
  `pg`'s copy has moved next to `stampTenant` so the four files can be
  diffed against each other.
- **The tenant a row already carried was compared with
  `reflect.DeepEqual`, so the refusal fired on a match** (`drops/pg`,
  `drops/sqlite`, `drops/mysql`). `DeepEqual` is a type comparison as
  much as a value one: `int64(77)` on the column and `int(77)` on ctx
  were `ErrTenantMismatch`, which made `Update` unusable for any caller
  whose tenant does not round-trip through its transport as the
  column's exact type. `clickhouse` was the only dialect that had
  noticed, and its `sameTenant` is now the answer in all four.
- **`sqlite`'s `Update` accepted a row whose key was still zero**
  (`drops/sqlite`). It sent `WHERE "id" = 0`, matched nothing, and
  reported success — a caller left believing a write landed. It now
  returns the new `ErrPKNotSet`, the sentinel `pg` and `mysql` already
  had, and the three doc comments say the same thing: `Save` never
  returns it, because a zero key is how `Save` recognises a row that
  has never been written and routes it to `Create`.
- **`WithTracer` produced no spans inside a transaction** (`drops/pg`).
  `Begin` and `InTx` built the transaction-bound `*DB` as
  `&DB{drv: tx, hook: db.hook}` — twice, in two places — which carried
  the hook and dropped the tracer by not mentioning it. So a trace went
  dark at the `BEGIN` and came back at the `COMMIT`, across exactly the
  span of statements a latency investigation follows it over, and the
  first two statements to vanish were the `SET LOCAL ROLE` and the
  `set_config` `InTxAs` sends to establish the identity everything
  after them runs under. The hook was propagated throughout, so the
  audit trail was intact and this was observability only. Both literals
  are now one `bind` method that says per field what a transactional
  `*DB` carries and why — the retry policy is still deliberately
  dropped, since a nested retry would re-run its body against a
  transaction PostgreSQL has already aborted — and a census fails when
  `DB` grows a field nobody has decided about.
- **A `DefaultFilter` holding a statement drops did not build was never
  offered to the resolver** (`drops/pg`). The walk over a table's
  default filters is guarded by a fast path that asks, without building
  anything, whether walking the list could change it — worth having,
  since the common filter is a soft-delete guard with no statement in
  it. The guard was a copy of `resolveExpr`'s type switch written as a
  list of type names, and it had gone stale in the same direction the
  switch itself once had: it admitted a `*SelectBuilder` and the
  expressions this package wraps around a statement, and knew nothing
  of the arm that handles a statement type a CALLER wrote. That arm is
  what keeps a foreign statement fail-closed — it asks for the ctx form,
  so a filter refusing for want of a tenant refuses the statement around
  it — and the fast path skipped the question. The filter rendered its
  inner statement blind: render-time defaults, no context filters, no
  refusal, on a `context.Background()` with no tenant at all. It now
  dispatches on the same interfaces `resolveExpr` does, and
  `TestNoResolutionEntryPointNamesAStatementType` fails any future copy
  of that decision, wherever it is written and whatever it is called.
- **`Dec` added a negated delta, so an unsigned counter climbed.** In
  all three dialects. The constraint behind `Inc`/`Dec` admits the
  unsigned types, and negating an unsigned value wraps, so
  `Dec(seats, uint32(5))` bound 4294967291 and rendered a flawless
  addition of it: a counter at 100 asked to fall by five stored
  4294967391. No engine objects — the statement is correct SQL, it just
  computes the opposite of the method's name. `Dec` renders a
  subtraction now.
- **`pg.Table.As` handed back the table's own columns, so a PostgreSQL
  self-join could not be written.** The copy was one struct assignment
  deep: the alias shared its column slice, its `byName` map and every
  other map with the table it was aliased from, so `users.As("u").
  Col("id")` was the package-level handle and still rendered
  `"users"."id"`. A join whose `FROM` names only the alias was rejected
  with 42P01; a self-join was accepted and read both sides from the
  un-aliased instance. The shared maps leaked in the other direction
  too — a relation, `CHECK`, `UNIQUE` or policy declared against the
  alias was written into the base table.

  `As` now copies the columns and binds them to the alias, and gives
  the copy its own maps, with the near side of each relation and the
  table-level key, unique and foreign-key column lists rebound. So that
  the copy stays *the same column* everywhere identity is what is being
  asked, a column carries the one it was copied from (`Column.origin`)
  and every consumer that matched on the handle now matches on that:
  the INSERT column list and row alignment, both hook contexts' `Has` /
  `Set` / `SetExpr`, `Entity`'s key columns, `ScopeByTenant`, `Page`'s
  ordering columns and cursor, the `CREATE TABLE` body and
  `BuildSnapshot`. Left unmatched, each of those failed in its own way
  and most of them quietly: a row bound through an alias rendered as
  all-`DEFAULT` and stored `NULL`s, a `CREATE TABLE` came out with no
  `PRIMARY KEY` and the server took it, a snapshot recorded the key
  column as nullable and fed that to `Diff` and `Push`. `As` also
  validates its argument now, like every other identifier entry point
  in the package.

  Two things `As` deliberately does not do, both now in its doc
  comment. Nothing the caller built and drops only re-emits is
  rewritten — a predicate, and a `Patch` operation: a default filter
  installed by `SoftDeleteMixin`, or an `authz` guard built from the
  package-level columns, is an already-built expression closed over the
  handles it was given, so an aliased query needs `Unscoped` and an
  explicit predicate, and an aliased entity needs a `Patch` built from
  the alias's handles. And the copy is a snapshot — a column or
  relation declared after `As` returned does not reach the alias, which
  matters because Go initialises package-level vars before it runs
  `init`.
- **`pg.Entity.Page` ordered by the handle it was handed, not by the
  one the query names.** `Asc` / `Desc` are free functions, so an
  ordering column arrives as whichever handle the caller held, and it
  is rendered twice — into the `ORDER BY` and into the cursor guard's
  row comparison. An entity on an alias paged by the package-level
  column, or an entity on the base table paged by an alias handle,
  emitted a statement naming a relation with no `FROM` entry, and
  PostgreSQL answered 42P01. `Page` now restates each ordering column
  as the handle its own table hands out, the way `ScopeByTenant`
  already did with the tenant axis. An ordering column with no struct
  field is also rejected before the query runs rather than at the first
  page boundary: a cursor is built out of the row's field values, so
  such a column could never produce one.
- **An aliased `mysql` column was a stranger to the column it names.**
  `mysql.Table.As` copied its columns and bound them to the alias, so
  the rendering was right, but nothing recorded that the copy *is* the
  declared column. Every site that decided by pointer therefore
  answered wrong, and the worst of them silently: `alignRow` matched a
  row's bindings against the INSERT column list through a
  `map[*Column]`, so a row bound through an alias fell to the `DEFAULT`
  fill — on a defaulted column MariaDB accepts the statement and writes
  `('anon', 0)`, and on a `NOT NULL` column with no default the same
  statement is error 1364, which is to say one schema change separates
  a silent corruption from a hard failure. A column now carries the one
  it was copied from (`Column.origin`) and a `*Table` carries the same
  (`Table.origin`), and `alignRow`, `Entity`'s key columns and
  `Table.AddIndex` match on that — `AddIndex`'s panic also names the
  alias, having printed the base name on both sides of "cannot be added
  to".

  Where such a handle is *rendered* rather than looked up, identity is
  not enough and the reference is restated. `Entity.Page` ordered by
  the handle it was handed, into both the `ORDER BY` and the cursor
  guard; a `CursorSpec` driven through `SelectBuilder` did the same,
  and its `FROM` may arrive after the spec, so that one is restated at
  render time — and left alone whenever drops cannot be sure the
  statement names one instance of the table. A self-join names two on
  purpose. So can a source added with `FromExpr`, which drops re-emits
  without reading: a comma join on the alias is a second instance it
  cannot count. Restating there picks the wrong instance and picks it
  silently, because both relations exist and the statement stays valid
  SQL over the wrong rows. `Patch` and `(*UpdateBuilder).Set` render an
  operation's column on the right of the assignment —
  `SET age = age + ?` — as does `ON DUPLICATE KEY UPDATE`; each is now
  restated against the relation the statement names, which for an
  `INSERT` is always the table, its `INTO` clause having no `AS` to
  carry. Every one of these was error 1054 on the live server, in both
  directions: once a `FROM` carries an alias the base name stops being
  a legal qualifier, and without one the alias never was. Cursor tokens
  are unaffected — the ordering fingerprint identifies a column by
  name, so a token stamped under one handle still spends under the
  other.

  `As` also copies what it used to share. The `checks` map was shared
  by reference, so a check added to either handle appeared on both and
  the doc's "the copy is a snapshot" was false; `indexes` and
  `defaultFilters` were copied as slice headers at full capacity, so
  two aliases of one table appended into the same spare slot and the
  second overwrote the first.

- **`qdrant.HasID()` with an empty id set matched every point.** The
  field carries `omitempty`, so an empty set left a condition with no
  clauses in it, and Qdrant reads a condition that constrains nothing
  as one that matches everything. The route is the shortest there is —
  look up ids, find none, delete what you found — and it empties the
  collection while reporting success. `In` and `NotIn` had the same
  hole in milder form.
- **A SQLite table rebuild destroyed every index and trigger on the
  table.** SQLite cannot `ALTER` most things, so `drops/sqlite` renders
  those changes as a rebuild — create the new shape, copy, `DROP
  TABLE`, rename — and `DROP TABLE` takes the table's indexes and
  triggers with it. `TableSnapshot` carried no record of either, so
  they were gone, silently, and a migration that dropped the index a
  hot query depends on reported success. `Introspect` now reads the
  stored `CREATE INDEX` / `CREATE TRIGGER` text out of `sqlite_master`
  and the rebuild replays it after the rename. What a rebuild cannot
  preserve is now said out loud rather than assumed: an index keyed on
  a removed column is dropped with the column and reported
  (`rebuild-drops-index`); an index that reaches a removed column
  through a partial predicate or an expression is replayed and the
  engine's rejection fails the migration; a trigger is replayed
  verbatim, because SQLite does not resolve a trigger body until it
  fires, and one naming a removed column is reported
  (`rebuild-stale-trigger`). `Diff` still emits no `CREATE INDEX` or
  `DROP INDEX` of its own — the schema DSL cannot declare an index, so
  every index in a database is undeclared and diffing them would drop
  all of them. The replay reaches as far as the previous snapshot does,
  which means `Push` and `DetectDrift`, both of which diff against a
  live `Introspect`. `GenerateMigration` diffs two snapshot files and a
  snapshot file records no index, so a generated rebuild still destroys
  them — and introspecting at generation time would only be a guess
  about the server the file is applied to later. That rebuild now
  carries a comment saying so, reported as `rebuild-loses-indexes`.
  `DetectDrift` correspondingly does not report a hand-made index as an
  unauthorised change; it used to report a hand-made *unique* index and
  not a plain one, and its proposed remedy was a rebuild that would
  have destroyed it.
- **Every SQLite table with a `UNIQUE` constraint rebuilt itself on
  every push.** SQLite does not store a `UNIQUE` constraint's name — an
  inline `email TEXT UNIQUE`, a `UNIQUE (email)` and a `CONSTRAINT c
  UNIQUE (email)` all leave one anonymous index, which `PRAGMA
  index_list` reports as `sqlite_autoindex_users_1`. Comparing names
  therefore reported a constraint change against the table's own
  declaration, forever, and on SQLite a constraint change means a full
  table rebuild. Unique constraints are now compared as a set of column
  tuples, which is the only part the engine remembers. `Introspect` also
  stops filing a standalone `CREATE UNIQUE INDEX` as a table
  constraint; it is an index, and it is preserved as one.
- **The shared scanner mapped a column to the wrong field whenever two
  fields could claim it**, and which one won depended on declaration
  order. An embedded `Key.ID` displaced the outer `ID` when the
  embedded field was declared second; a field's camelCase form
  displaced another field's explicit `drop:` tag when it came later.
  Names now resolve by depth first — the shallower field wins, as it
  does for ordinary Go field access — and, at equal depth, tag before
  field name before camelCase form, with a genuine tie going to the
  field declared first.
- **The shared scanner skipped every field promoted out of an
  unexported embedded struct**, so a row type that factors its
  bookkeeping columns into an `audit` mixin silently scanned none of
  them. Those fields are exported and settable through reflection; they
  are now walked, as `encoding/json` walks them. An embedded
  `time.Time` or `sql.Scanner`, conversely, is no longer walked into:
  it receives a column, which is what `IsScalarDest` already claimed.
- **`ScanOne` sent a `**struct` down the single-column path**, where it
  became either "needs a single-column result" or a driver-level
  conversion error, neither of which names the mistake. `IsScalarDest`
  now looks through pointers, so a pointer to a struct is a struct.
- **Composite primary keys could not create their own table**
  (`drops/pg`, `drops/sqlite`) — an inline `PRIMARY KEY` was emitted on
  every marked column, so a two-column key rendered two of them.
  PostgreSQL rejects that outright and so does SQLite. The support
  added earlier in this cycle could query such a table but never build
  one. Both now emit the table-level clause MySQL already did.
- **SQLite `Entity.Create` bound the zero primary key**, so every row
  claimed id 0 and the second insert failed on the key; and it never
  read the generated key back, leaving the caller holding a row it
  could not address. Zero auto-increment and defaulted columns are now
  omitted, and the key comes back through `RETURNING`.
- **A SQLite primary key was not `NOT NULL`** — treated as redundant
  next to `PRIMARY KEY`, which PostgreSQL implies and SQLite does not.
  Measured against the engine: a `TEXT PRIMARY KEY` without it accepts
  and stores a NULL key.
- **SQLite introspection never detected `AUTOINCREMENT`**, so every
  declared auto-increment column diffed against the live table forever.
- **A SQLite table with no `CHECK` constraints diffed against itself** —
  `BuildSnapshot` initialises the constraint maps, `Introspect` left
  one nil, and `reflect.DeepEqual` reports nil and empty as different.
  On SQLite a constraint change means a full table rebuild, so a schema
  that already matched its declaration copied itself on every deploy.
- **ClickHouse `CREATE TABLE` emitted table-qualified column names** in
  ORDER BY / PRIMARY KEY / PARTITION BY, which the server rejects — it
  cannot resolve `"docs"."id"` against a table that does not exist yet.
  The existing test had pinned the broken output.
- **ClickHouse's event store and matview interpolated table names into
  a literal `"%s"`** instead of quoting them as identifiers.
- **CI had been red since PR #5.** `.golangci.yml` was still v1 format
  while the action installs v2, so the linter failed on config load and
  never ran; gocritic ran with a `style` tag this codebase does not
  follow; `sloppyReassign` was actively wrong against the named-return
  pattern the hooks depend on. Now green, with the two real findings it
  had been hiding fixed.
- **SQLite entity queries never used the attached cache** — `queryKey`
  was dead code because the query-result caching pg has was never
  wired up. `All`/`One` now read through it with the same single-flight
  stampede protection the PK path had.


## [0.6.0] - 2026-08-16

### Added
- **Portable vector search** (`drops/vector`) — one search vocabulary
  shared by pgvector, ClickHouse and Qdrant, replacing three
  backend-specific ones:
  - **`Filter`** — a portable predicate tree (`And`/`Or`/`Not` over
    `Eq`, `Ne`, `In`, `NotIn`, `Lt`/`Lte`/`Gt`/`Gte`, `Between`,
    `IsNull`, `MatchText`, `HasID`, `GeoWithin`), compiled to each
    backend's own representation through a generic `Compile`/`Visitor`
    pair so the traversal exists once.
  - **`Query` / `QueryBuilder`** — query vector, `TopK`, `Metric`,
    filter, `MaxDistance` (always in the metric's units), payload and
    vector inclusion, cursor, and a `Params` bag for backend-specific
    tuning that other backends ignore.
  - **`Hit` / `Results`** — every hit carries both `Distance` (lower is
    closer) and `Score` (higher is better), converted in one place;
    `HasMore` is decided by a `TopK+1` probe, never a second query.
  - **`Cursor`** — one opaque, URL-safe cursor across backends, stamped
    with the issuing backend so a cross-store replay is
    `ErrCursorMismatch` rather than a wrong page. IDs travel as text
    plus a kind tag, so an `int64` past 2^53 round-trips exactly.
  - **`Store`** — the one-method interface the three adapters implement.
- **Vector-store adapters** for the three backends:
  - **`pg.NewVectorStore`** — pgvector distance operators for all six
    metrics, filter fields resolved to mapped columns or a jsonb
    payload accessor, keyset pagination on `(distance, id)`, PostGIS
    `ST_Within` for geo filters via `WithGeoColumn`. `FormatVector` /
    `ParseVector` / `FormatBitVector` are exported for hand-written
    statements.
  - **`clickhouse.NewVectorStore`** — `cosineDistance` / `L2Distance` /
    `L1Distance` / negated `dotProduct` over `Array(Float32)` (no
    extension required), `JSONExtract*` payload accessors with
    `JSONHas` null tests, SETTINGS forwarded from `Params`, and the
    same keyset pagination. The query vector is rendered once and
    referenced by alias in `WHERE` and `ORDER BY`.
  - **`(*qdrant.Client).Store`** — portable filters compiled to Qdrant's
    Must/Should/MustNot tree (negations routed through `must_not`,
    `IsNull` covering both `is_null` and `is_empty`), offset
    pagination, and score-to-distance normalisation that accounts for
    Qdrant's per-metric score semantics. `qdrant.CompileFilter` is
    exported so portable filters can also drive `Scroll`,
    `Recommend` and `DeleteByFilter`.

### Fixed
- **Every PostGIS helper emitted invalid SQL** (`drops/pg`, `geo.go`) —
  `Within`, `DistanceFrom`, `NearestFrom` and `WithinRadius` each wrote
  `$1`, `$2`, … into the SQL text by hand *and* called `AddArg`, which
  writes the placeholder itself. Each helper therefore emitted its
  placeholders twice, the second set dangling after the closing
  parenthesis:

      ST_Within(…, ST_MakeEnvelope($1, $2, $3, $4, 4326))$1$2$3$4

  That is a syntax error unconditionally, not merely mis-numbered
  parameters, so PostGIS support has never worked. All four now bind
  through `AddArg` alone, which also makes their numbering follow the
  Builder — a geo predicate that is not first in a statement now binds
  `$2`, `$3`, … correctly. The existing tests missed this because they
  asserted on substrings and argument counts, which the broken output
  satisfied; the new regression test pins the whole rendered string.

## [0.5.0] - 2026-07-25

### Added
- **Tiered cache** (`drops/cache/tiered`) — two-level L1+L2 read-through /
  write-through cache with `GetOrLoad` singleflight stampede protection.
- **Memcached cache backend** (`drops/cache/memcached`) — stdlib-only
  backend implementing `cache.Cache` / `cache.MultiCache`.
- **OpenTelemetry hook instrumentation** (`drops/otel`) — spans + RED
  metrics adapter for all backends without importing OTel in core packages.
- **SQLite keyset pagination and soft delete parity** (`drops/sqlite`) —
  `Entity.Page`, `Table.DefaultFilter`, `SoftDelete` helpers, and
  `UpdateBuilder.SetExpr`.
- **drizzle-kit interop for SQLite** (`drops/sqlite`) —
  - **DrizzleMigrator** (`drizzle.go`) — applies a drizzle-kit migration
    directory (journal + hashed `.sql` files, statement-breakpoint
    splitting, `BeforeEach`/`AfterEach` hooks). Adapted to SQLite: the
    `__drizzle_migrations` history table is unqualified (no schema), and
    the journal dialect must be `sqlite`.
  - **GenerateMigration** (`generate.go`) — diffs the Go schema against
    the latest snapshot and writes a new drizzle-kit migration set
    (`<tag>.sql`, `meta/<idx>_snapshot.json`, updated `_journal.json`),
    with optional `WithDown` rollback SQL. No-op when the schema is
    unchanged.
- **Query-plan capture for SQLite** (`drops/sqlite`, `explain.go`) —
  `Explain` runs `EXPLAIN QUERY PLAN`, parsing it into `PlanStep`s with
  `SeqScans` (full-table scans), `UsedIndexes`, a stable `Fingerprint`
  for regression detection, and a tree `String`.
- **Audit, authorization and caching for SQLite** (`drops/sqlite`) — the
  cross-cutting Entity concerns from pg, wired into SQLite's Entity CRUD:
  - **Audit** (`audit.go`) — `NewAuditLog`/`NewAuditTable`/`WithAudit`,
    `WithActor`/`ActorFrom`; Create/Update/Delete write an audit row in
    the same transaction as the mutation.
  - **Authorization** (`authz.go`) — `Guard` + `OwnerGuard`/
    `MembershipGuard`/`CustomGuard` + `AnyOf`/`AllOf`, `WithSubject`/
    `SubjectFrom`, `(*Entity).AuthorizeWith`; the guard predicate is
    AND-ed into Get/Query/Update/Delete and fails closed with
    `ErrSubjectMissing`.
  - **Cache** (`cache.go`) — `(*Entity).WithCache` read-through cache
    over the `drops/cache` backend, with a single-flight group,
    PK-entry invalidation on write/delete, and gob-encoded entries.
- **Schema push & diagram for SQLite** (`drops/sqlite`) —
  - **Push** (`push.go`) — `Push` introspects the live DB, diffs it
    against a Go `Schema`, and applies (or `DryRun`-previews) the diff in
    one transaction; `PushResult`/`PushOptions`/`ErrSchemaRequired`.
  - **Mermaid ER diagram** (`diagram.go`) — `MermaidDiagram` renders a
    schema (tables + relations) as a Mermaid `erDiagram`.

  `objects.go` (sequences, RLS policies, materialized views) is not
  ported — those are Postgres-specific.
- **Reflection, PII and drift for SQLite** (`drops/sqlite`) —
  - **AutoTable** (`autotable.go`) — `AutoTable[T]` / `NewAutoEntity[T]`
    derive a Table from `drop` struct tags (primaryKey, autoIncrement,
    notNull, unique, pii, default), mapping Go types to SQLite affinities
    (`sqlite.Money` → INTEGER).
  - **PII redaction** (`pii.go`) — `PII`/`IsPII`/`(*Col).AsPII`; Exec and
    Query unwrap the marker for the driver while hooks/loggers see
    `<redacted>`, and entity bindings wrap PII columns automatically.
  - **Drift detection** (`drift.go`) — `DetectDrift` computes the two-way
    Snapshot diff into a `DriftReport` (`PendingMigrations`,
    `UnauthorizedChanges`, `InSync`).
- **Dev & schema tooling for SQLite** (`drops/sqlite`) —
  - **Factory** (`factory.go`) — `NewFactory`/`Build`/`BuildN`/`Create`/
    `CreateN`/`With`/`Reset` test-data factories (backed by the new
    `Entity.CreateMany` batch insert).
  - **Seeder** (`seed.go`) — `NewSeeder` + `SeedAdd`/`SeedAddCreate`/
    `SeedDo` + transactional `Apply`.
  - **Test transaction** (`testing.go`) — `TestTx` runs a test body in a
    rolled-back transaction via the `TB` interface.
  - **N+1 detector** (`n1.go`) — `WithN1Detector` + `N1Hook` +
    `N1Report`/`N1Pattern` to flag repeated query skeletons.
  - **Keyset cursor** (`cursor.go`) — `CursorSpec`/`OrderKey`,
    `EncodeCursor`/`Cursor.Decode`, and `SelectBuilder.OrderByCursor`/
    `AfterCursor`/`BeforeCursor` (NULLS defaults documented for SQLite).
  - **Enum** (`enum.go`) — `NewEnum`/`AddTo`/`EnumCol` emulate a
    PostgreSQL enum as a `TEXT` column plus an `IN (...)` CHECK
    constraint (SQLite has no enum type).
  - `Entity.CreateMany` — multi-row batch insert with tenant stamping.
- **Transactional outbox for SQLite** (`drops/sqlite`, `outbox.go`) —
  `Outbox` / `NewOutboxTable` / `Emit` / `EmitWith`, `Drain`,
  `MarkPublished`, `MarkFailed`, `Cleanup`, and `OutboxWorker`
  (`OnEvent`/`OnBatch`, `WithInterval`/`WithBatch`/`WithMaxAttempts`/
  `WithBackoff`, `Run`/`Tick`). SQLite has no LISTEN/NOTIFY, SKIP LOCKED
  or advisory locks, so it is a poll-based single-worker outbox with
  INTEGER Unix-second timestamps; the pg per-aggregate advisory-lock
  ordering mode is omitted. Delivery is at-least-once.
- **Event sourcing & saga for SQLite** (`drops/sqlite`) —
  - **Event store** (`eventstore.go`) — `EventStore` / `NewEventStoreTable`
    with `Append` (optimistic concurrency via the UNIQUE
    aggregate/version constraint → `ErrConcurrencyConflict`, detected
    from SQLite's "UNIQUE constraint failed" message), `Load`, `Stream`,
    `LatestVersion`, plus snapshots (`NewSnapshotTable`, `SaveSnapshot`
    via `ON CONFLICT DO UPDATE`, `LoadSnapshot`).
  - **Saga** (`saga.go`) — `NewSaga`/`Step`/`Run` orchestration with
    reverse-order compensation, typed `SagaState` (`SagaStateGet[T]`),
    and `SagaError`/`IsSagaError`.
- **Transactional store patterns for SQLite** (`drops/sqlite`) —
  - **Idempotency keys** (`idempotency.go`) — `IdempotencyStore` /
    `NewIdempotencyTable` / `Run` / `RunJSON` / `Cleanup` / `SweepEvery`.
    SQLite has no `SELECT ... FOR UPDATE`; concurrent `Run` calls
    serialise on the write-transaction lock instead. Time comparisons
    bind Go times (not `CURRENT_TIMESTAMP`) to avoid datetime-format
    mismatch.
  - **Chunked backfill** (`backfill.go`) — `NewBackfill` with
    `ChunkSize`/`Throttle`/`Fetch`/`Process`/`OnProgress`, resumable via a
    persisted state table (`NewBackfillStateTable`, timestamps stored as
    INTEGER Unix seconds), upserting through `ON CONFLICT DO UPDATE`. The
    pg replica-lag gate is omitted (SQLite has no replication).
- **Lifecycle hooks, templates and mixins for SQLite** (`drops/sqlite`) —
  the pg hook/mixin subsystem, adapted to SQLite:
  - **Hooks** (`hooks.go` + builder wiring) — `Table.OnInsert` /
    `OnUpdate` / `OnDelete` and `DefaultFilter`, applied by the INSERT /
    UPDATE / DELETE / SELECT builders; `Unscoped()` on Select/Update/
    Delete bypasses default scopes. User-supplied values always win over
    hook-supplied ones.
  - **Templates** (`template.go`) — `Timestamps`, `SoftDelete`, `Audit`,
    `UUIDPrimaryKey` column groups returning typed handles. SQLite
    adaptations: `CURRENT_TIMESTAMP` defaults, and a `randomblob()`-based
    RFC-4122 v4 UUID default for `UUIDPrimaryKey` (SQLite has no
    `gen_random_uuid()`).
  - **Mixins** (`mixin.go`) — `ApplyMixins` + `TimestampsMixin`
    (bumps `updatedAt` on UPDATE), `SoftDeleteMixin` (default-scopes
    queries and rewrites DELETE into UPDATE `deletedAt`), `AuditMixin`,
    `UUIDPrimaryKeyMixin`.
- **Higher-level pg feature parity for SQLite** (`drops/sqlite`) — the
  portable feature patterns that previously lived only in `drops/pg` are
  now available on SQLite, adapted to SQLite semantics:
  - **Money** (`money.go`) — precision-safe integer-cents monetary type
    (`Money`, `MoneyFromString`/`MoneyFromCents`/`MoneyFromUnits`, `Add`,
    `Sub`, `MulRate` with banker's rounding, JSON string round-trip,
    `driver.Valuer`/`sql.Scanner`).
  - **Cursor pagination** (`page.go`) — `Entity.Page` with opaque
    keyset cursors (`Asc`/`Desc`, `Page[T]`, `HasMore`/`NextCursor`),
    using SQLite row-value comparison for the keyset guard.
  - **Patch** (`patch.go`) — `Entity.Patch` with SQL-side ops `Inc`,
    `Dec`, `Set`, `SetIfGreater`/`SetIfLess` (via `max`/`min`) and
    `SetIfChanged` (via NULL-safe `IS NOT`).
  - **Tenant scoping** (`tenant.go`) — `Entity.ScopeByTenant` +
    `WithTenant`/`TenantFrom`; Get/Query/Update/Delete auto-apply the
    tenant predicate and Create stamps it, failing closed with
    `ErrTenantMissing` / `ErrTenantMismatch`.
  - **Typed JSON path** (`jsonpath.go`) — `JSONField[T]` typed accessor
    over `json_extract` with comparison/`In`/`IsNull`/`Like` operators,
    plus `JSONHasKey` via `json_type`.
  - **Retry** (`retry.go`) — `RetryPolicy` + `DB.WithRetry`;
    transaction-level retry on `SQLITE_BUSY`/`SQLITE_LOCKED`
    (`ErrBusy`/`ErrLocked`, matched by `errors.Is` or driver message),
    `ExponentialJitter`, `DefaultRetryPolicy`.
  - **Tracing** (`tracing.go`) — `Tracer`/`Span` contract + `WithTracer`
    wired into every Exec/Query span (dependency-free OTel-shaped API).
  - **Existence checks** (`exists.go`) — `TableExists`, `ColumnExists`,
    `IndexExists`, `TriggerExists` over `sqlite_master` / `pragma_table_info`.
  - **Migration safety analyzer** (`safety.go`) — `AnalyzeMigration`
    with SQLite-tuned rules (drop-table, drop/rename-column,
    add-NOT-NULL-without-default, non-constant ADD COLUMN default,
    DELETE/UPDATE without WHERE).
  - **Logger hook alias** (`hook_logger.go`) — `sqlite.LoggerHook` for
    symmetry with the pg/clickhouse dialects.

  Postgres-specific features remain pg-only where SQL cannot express
  them (LISTEN/NOTIFY, pgvector, materialized views, COPY, PostGIS,
  advisory locks, streaming replication, `CREATE INDEX CONCURRENTLY`,
  table-partitioned time series).
- **Portable SQL expression layer for SQLite** (`drops/sqlite`) — the
  SQLite dialect gains the full set of standard-SQL expression builders
  that previously lived only in `drops/pg`, so anything expressible in
  portable SQL is now available on SQLite too. New helpers:
  - **Operators / predicates** (`op.go`): free-standing `Eq`, `Ne`, `Gt`,
    `Gte`, `Lt`, `Lte`, `Not`, `In`, `NotIn`, `IsNull`, `IsNotNull`,
    `Between`, `NotBetween`, `Like`, `NotLike`, `LikeEscape`, plus the
    SQLite-native `Glob`, `Regexp`, and the NULL-safe `IsDistinctFrom` /
    `IsNotDistinctFrom` (rendered via SQLite `IS` / `IS NOT`).
  - **Aggregates / scalars** (`funcs.go`): `Count`, `CountAll`,
    `CountDistinct`, `Sum`, `Avg`, `Min`, `Max`, `SumDistinct`,
    `AvgDistinct`, `Filter`, plus SQLite's `Total`, `GroupConcat`,
    `Coalesce`, `IfNull`, `NullIf`, `Lower`, `Upper`, `As`, `Func`.
  - **Math** (`math.go`): `Abs`, `Round`, `Ceil`, `Floor`, `Trunc`,
    `Mod` (via `%`), `Power` (`pow`), `Sqrt`, `Sign`, `Exp`, `Ln`, `Log`,
    `Greatest`/`Least` (via multi-arg `max`/`min`), trig functions,
    `Random`, and the `Plus`/`Minus`/`Mul`/`Div` operators.
  - **Strings** (`strings.go`): `ConcatOp` (`||`), `Concat`, `ConcatWS`,
    `Length`, `OctetLength`, `Substr`, `Trim`/`LTrim`/`RTrim`, `Replace`,
    `Instr`, `Hex`/`Unhex`, `Quote`, `Chr`, `Unicode`, `Format`/`Printf`.
  - **Cast / Case** (`cast.go`): `CastAs`/`Cast` (SQLite has only the
    `CAST(x AS T)` form) and the `Case`/`CaseOn` builder.
  - **Subqueries** (`subquery.go`): `Exists`, `NotExists`, `Subquery`,
    `InSub`, `NotInSub`.
  - **CTEs** (`cte.go`): `With` / `WithRecursive` on the SELECT builder
    plus `CTEDef` (WITH / WITH RECURSIVE, supported since SQLite 3.8.3).
  - **Window functions** (`window.go`): `Over`, `WindowSpec`,
    `RowNumber`, `Rank`, `DenseRank`, `PercentRank`, `CumeDist`, `Ntile`,
    `Lag`, `Lead`, `FirstValue`, `LastValue`, `NthValue`.
  - **JSON1** (`json.go`): `JSONExtract`, `JSONGet` (`->`),
    `JSONGetText` (`->>`), `JSONArrayLength`, `JSONType`, `JSONValid`,
    `JSONQuote`, `JSONObject`, `JSONArray`, `JSONSet`/`JSONInsert`/
    `JSONReplace`, `JSONRemove`, `JSONPatch`, `JSONGroupArray`,
    `JSONGroupObject`.
  - **Date/time** (`datetime.go`): `Now`, `CurrentDate`, `CurrentTime`,
    `CurrentTimestamp`, `DateOf`, `TimeOf`, `DateTime`, `JulianDay`,
    `UnixEpoch`, `StrfTime`.
- **Portable SQL expression layer for ClickHouse** (`drops/clickhouse`)
  — the standard-SQL structural helpers ClickHouse supports with
  identical syntax, mirroring the SQLite/pg surface: `CastAs`/`Cast` and
  `Case`/`CaseOn` (`cast.go`); `Exists`, `NotExists`, `Subquery`,
  `InSub`, `NotInSub` (`subquery.go`); `With` / `CTEDef` (`cte.go`); and
  window functions `Over`, `WindowSpec`, `RowNumber`, `Rank`,
  `DenseRank`, `FirstValue`, `LastValue`, `NthValue`, plus `Lag`/`Lead`
  emitting ClickHouse's `lagInFrame`/`leadInFrame` (`window.go`).

## [0.4.1] - 2026-07-14

### Fixed
- **Qdrant missing-collection 404 classification** (`drops/qdrant`) — the
  `Client.Do` 404 check used a case-sensitive substring match on
  `"not found"`, which does not match real Qdrant's response body
  (``Not found: Collection `x` doesn't exist!`` — capital "Not found",
  "doesn't exist"). A missing collection therefore surfaced as a plain
  `HTTPError` instead of `ErrCollectionMissing`, so `CollectionExists`
  returned an error rather than `(false, nil)` and callers never reached
  their auto-create branch — collections were silently never created.
  The 404 is now classified case-insensitively and also accepts
  `"doesn't exist"` / `"does not exist"`. The test mock, which previously
  matched a lowercase body no live server emits, now uses Qdrant's real
  format, and a table-driven test pins the variants.

## [0.4.0] - 2026-07-08

### Added
- **Swappable SQL dialect abstraction** (`drops`) — a new `drops.Dialect`
  interface (`Name`, `Placeholder`, `QuoteIdent`, `SupportsReturning`)
  that a `Builder` carries. `drops.WithDialect(d)` reroutes placeholder
  rendering and identifier quoting through the dialect, so the same
  builder chain targets any SQL-like backend by swapping the dialect and
  driver. A Builder with no dialect keeps the previous PostgreSQL
  behaviour byte-for-byte (`$N` placeholders, `"…"` identifiers), so this
  is fully backward compatible. `pg.Dialect` and `sqlite.Dialect` are the
  two implementations; `drops.StringWithDialect` renders an expression a
  dialect's way.
- **SQLite dialect** (`drops/sqlite`) — a new package mirroring
  `drops/pg`'s API surface (Table / Column / DB / DDL / Select / Insert /
  Update / Delete) over the shared `drops.Driver`, emitting SQLite SQL:
  `?` placeholders, SQLite type affinities, `INSERT OR IGNORE/REPLACE`,
  and — the key dialect difference — **all constraints rendered inline in
  `CREATE TABLE`** (SQLite has no `ALTER TABLE ADD CONSTRAINT`). Type
  constructors share pg's names (`Text`, `BigInt`, `Timestamp`, …) so a
  schema ports with a package swap.
- **Composite (N-column) foreign keys** (`drops/pg`, `drops/sqlite`) —
  `Table.ForeignKeyN(cols, target, targetCols, opts…)` declares a
  multi-column FK (`FOREIGN KEY (a,b) REFERENCES t (x,y)`). In pg it is
  wired through the snapshot/diff generator and emitted as a separate
  `ALTER TABLE ADD CONSTRAINT`; in sqlite it is emitted inline. Column
  counts must match (panics at declaration otherwise).
- **Shared reflection row-scanner** (`drops`) — `drops.ScanOne` /
  `drops.ScanAll` moved the dialect-agnostic struct↔column mapping into
  the root package so every dialect scans rows identically. `drops.StructFields`
  exposes the column→field map for entity binding. `drops/sqlite` uses
  both; `drops/pg` keeps its own wrappers.
- **SQLite full ORM parity** (`drops/sqlite`) — the dialect now covers:
  - **Entities** — `Entity[T]` typed CRUD (`Get` / `Create` / `Update` /
    `Delete`) and a fluent `Query` (`Where` / `OrderBy` / `Limit` /
    `Offset` / `All` / `One`).
  - **Relations & eager loading** — `NewRelations(t).HasMany / HasOne /
    BelongsTo / ManyToMany`, loaded via `db.Find(t).With(names…)` with one
    batched query per edge (no N+1) stitched into `dropRel` struct fields.
  - **Migrations** — a versioned `Migrator` (`Add` / `AddSQL` / `AddFS` /
    `Up` / `Down` / `Status`) with `BeforeEach` / `AfterEach` in-transaction
    data hooks, mirroring `pg.Migrator`.
  - **Snapshot & diff** — `BuildSnapshot` / `Diff` generate SQLite
    migration SQL, honouring SQLite semantics: `ALTER TABLE ADD COLUMN`
    where possible, and the standard **table-rebuild sequence**
    (`CREATE t_new` → `INSERT … SELECT` → `DROP` → `RENAME`) for column
    type changes, drops, and constraint changes that SQLite cannot alter
    in place.
  - **Introspection** — `Introspect(ctx, db)` reconstructs a `Snapshot`
    from a live database via `sqlite_master` and the `table_info` /
    `foreign_key_list` / `index_list` PRAGMAs.

## [0.3.0] - 2026-07-04

### Added
- **Migration data hooks** (`drops/pg`) — both migrators now expose
  `BeforeEach` / `AfterEach` hooks that run inside each migration's
  transaction, the seam for data migrations that must run between
  schema migrations (backfilling a new column, copying rows into a
  split-out table, rewriting a value before an old column is dropped).
  On the native `Migrator`, `MigrationHook` receives the tx-scoped
  `*DB`, the `Migration`, and a `MigrationDirection` (`DirectionUp` /
  `DirectionDown`) so a data step can be scoped to a specific version
  and direction; hooks fire around both `Up` and `Down`. On
  `DrizzleMigrator` — where migration files are pure SQL and there is
  otherwise no place for Go logic — `DrizzleHook` receives the
  tx-scoped `*DB` and the `DrizzleEntry`, letting a backfill run
  atomically with the file's statements. A hook that returns an error
  aborts the migration and the whole transaction rolls back.
- **Nested (deep) relation eager-loading** (`drops/pg`) — `Find().With`
  now accepts dot paths such as `With("posts.comments")` to load
  relations of relations to arbitrary depth. Each relation edge still
  costs exactly one batched query (no N+1), and paths sharing a prefix
  are merged so the shared edge is fetched once
  (`With("posts.comments", "posts.tags")` runs three queries, not four).
  Nested rows are stitched in place onto the live result structs via
  pointers into the parent data. Works across `HasMany`, `HasOne`,
  `BelongsTo`, and `ManyToMany` intermediates. The entire `With` graph
  is validated against the schema before any query runs, so a typo at
  any depth fails fast with an `unknown relation` error; malformed
  paths (e.g. `"posts..comments"`) report `invalid relation path`.
- **Per-relation filtering & ordering on eager loads** (`drops/pg`) —
  new `Find().WithRel(name, func(*pg.RelConfig))`. The `RelConfig`
  callback exposes `Where` (AND-ed onto the relation's batched query),
  `OrderBy` (sorts each parent's loaded slice), and `With`/`WithRel`
  for configuring deeper relations — mirroring drizzle's
  `with: { posts: { where, orderBy } }`. Still one query per edge.
  For `ManyToMany`, `OrderBy` re-sorts each parent's slice into target
  order (default remains junction-row order). `WithRel` and `With`
  merge when they name the same edge, so it is fetched once. Per-parent
  `LIMIT`/`OFFSET` is intentionally not yet offered (a single `LIMIT`
  caps the whole batch, not each parent — needs a window-function
  rewrite).
- **`drops.CallHook(h, ctx, e)`** — the safe entrypoint every dialect
  now uses to emit observability events. Tolerates nil hooks and
  recovers panics, so a buggy user-supplied `Hook` (nil deref in a
  formatter, out-of-bounds in a metric label, …) can no longer crash
  the caller's request goroutine. `drops.ChainHooks` also continues
  to the next hook after a panicking one. Wired into pg, clickhouse,
  qdrant, cache/memory, cache/redis.
- `.gitignore` — coverage / profile / OS / editor / env / build
  artefacts kept out of the tree.
- **Cache abstraction** (`drops/cache`) — driver-agnostic interface
  (`Get` / `Set` / `Delete` / `Exists` / `TTL` / `Ping` / `Close`) with
  `MultiCache` for batch operations. Sentinels: `ErrNotFound`,
  `ErrClosed`, `ErrInvalidKey`.
- **In-memory cache** (`drops/cache/memory`) — concurrent-safe,
  TTL-aware, with an optional janitor goroutine and FIFO eviction once
  `MaxEntries` is reached. Defensive copies on Get/Set so callers can't
  mutate stored bytes.
- **Redis cache** (`drops/cache/redis`) — production backend with a
  bundled minimal RESP2 client and a bounded connection pool. Zero
  external dependencies (`net.Conn` + `bufio` only). Supports legacy
  and ACL `AUTH`, `SELECT db`, key prefixes, context-deadline
  propagation onto the wire, and the `drops.Hook` contract for
  observability. `Cache` and `MultiCache` interfaces both implemented.
- **Redis production hardening**:
  - Channel-based pool replaces the spin-wait loop; `Get` honours ctx
    cancellation natively, no CPU burn under contention.
  - `MinIdleConns` pre-dials connections at startup so the first
    request after a cold start doesn't pay a full TCP+AUTH RTT.
  - `MaxLifetime` recycles connections past an age cap regardless of
    idle status — critical when AUTH tokens rotate or a load balancer
    wants to drain old conns.
  - `ReadTimeout` / `WriteTimeout` (defaults: 3s each) apply when the
    caller's ctx has no deadline so a hung server can't stall the
    goroutine forever. Set negative to disable.
  - `MaxRetries` (default 1) retries on transient transport errors
    (EOF, `net.Error`, `ErrProtocol`) with a fresh connection;
    app-level `-ERR` replies are never retried.
  - `ShutdownTimeout` (default 5s) lets `Close` drain in-flight ops
    before forcing socket closure.
  - `ClientName` (default `"drops"`) is sent via `CLIENT SETNAME` on
    connect so the connection is identifiable in `CLIENT LIST` /
    `SLOWLOG` / `MONITOR`.
  - `Cache.Stats()` returns a `PoolStats` snapshot for metrics
    emitters: `TotalConns`, `Hits`, `Misses`, `Timeouts`,
    `StaleClosed`, `WaitCount`, `WaitDuration`, `Retries`.
- **Redis auth & transport**:
  - `redis.CredentialsProvider func(ctx) (Credentials, error)` is
    called per new connection so short-lived tokens (AWS ElastiCache
    IAM, Azure AAD, OIDC, Vault leases) can be refreshed without
    restarting the cache. Provider errors fail the dial cleanly.
  - `redis.StaticCredentials(user, pass)` helper for the simple case.
  - `Options.TLS *tls.Config` enables in-transit encryption; the
    default dialer is wrapped with a `tls.Dialer` so callers don't
    have to plumb their own.
  - `redis.ParseURL("redis[s]://[user:pass@]host[:port][/db]")` lifts
    a connection string into Options — and rediss:// pre-populates a
    sensible `tls.Config` (`ServerName` = host, MinVersion = TLS1.2).
  - Existing `Username`/`Password` fields are kept as the static
    shorthand; if `Credentials` is non-nil it wins.
- **Qdrant client** (`drops/qdrant`) — focused HTTP client for the Qdrant
  vector database. Zero external deps (net/http + encoding/json only):
  - `Client` with `WithAPIKey` / `WithHTTPClient` / `WithTimeout` options;
    Qdrant Cloud (`api-key`) and self-hosted (`Authorization: Bearer`)
    auth headers are set in lock-step
  - Collections: `CreateCollection`, `DeleteCollection`,
    `CollectionExists`, `CollectionInfo`, `ListCollections`
  - Points: `Upsert`, `DeleteByIDs`, `DeleteByFilter`, `Retrieve`, `Count`
  - Search: `Search` (single vector), `Recommend` (positive/negative
    examples), `Scroll` (deterministic pagination cursor)
  - Filter DSL: `Must` / `Should` / `MustNot` blocks with
    `Eq` / `In` / `NotIn` / `MatchText` / `Range` / `HasID` / `IsEmpty` /
    `IsNull` / `GeoIn` / `Nest` conditions
  - `HTTPError` carries `Status` / `StatusText` / `Body`; missing
    collections wrap `ErrCollectionMissing` so `errors.Is` works
- **pgvector** support in `drops/pg`:
  - Column types: `Vector(name, dim) *Col[[]float32]`,
    `HalfVec(name, dim) *Col[[]float32]`, `SparseVec(name, dim) *Col[string]`,
    `BitVec(name, dim) *Col[string]`
  - Distance operators: `L2Distance` (`<->`), `InnerProduct` (`<#>`),
    `CosineDistance` (`<=>`), `L1Distance` (`<+>`), `HammingDistance` (`<~>`),
    `JaccardDistance` (`<%>`); plus convenience methods
    `c.L2(v)` / `c.IP(v)` / `c.Cosine(v)` / `c.L1(v)` on `*Col[T]`
  - Index op-class hints (`VectorL2Ops`, `VectorCosineOps`, `HalfVecIPOps`,
    `BitHammingOps`, …) plus `Index.OpClass(...)` / `Index.With(...)` so
    HNSW and IVFFlat indexes render with the correct operator class
    and tuning parameters
  - The existing `CreateExtensionIfNotExists("vector")` is the install
    step — no new helper needed

### Added (still under [Unreleased])
- **ClickHouse dialect** (`drops/clickhouse`):
  - Typed columns: `String`, `FixedString`, `Int{8,16,32,64}`, `UInt{8,16,32,64}`,
    `Float{32,64}`, `Decimal`, `Bool`, `Date`, `Date32`, `DateTime(tz)`,
    `DateTime64(prec, tz)`, `UUID`, `JSON`, `Custom[T]`
  - Type wrappers: `TypeArray`, `TypeNullable`, `TypeLowCardinality`,
    `TypeMap`, `TypeTuple`, `TypeEnum8/16` plus chainable `.Nullable()` /
    `.LowCardinality()` / `.Default(sql)` / `.Codec(...)` / `.TTL(...)` /
    `.Comment(...)` on `*Col[T]`
  - Engines: `MergeTree`, `ReplacingMergeTree`, `SummingMergeTree`,
    `AggregatingMergeTree`, `CollapsingMergeTree`,
    `VersionedCollapsingMergeTree`, `ReplicatedMergeTree`, `Memory`, `Log`,
    `TinyLog`, `StripeLog`, `Null`, plus `Raw` for distributed / kafka /
    custom engines
  - `Table.Engine(...) / OrderBy / PartitionBy / PrimaryKey / SampleBy /
    TTL / Setting(...)` builder
  - DDL: `CreateTable[IfNotExists]`, `DropTable[IfExists]`, `TruncateTable`,
    `OptimizeTable(final)`, `CreateDatabase[IfNotExists]`,
    `DropDatabase[IfExists]`; `CreateTableErr` returns `ErrEngineRequired`
  - Query builder: `Select` with `From`, `Final`, `SampleBy`, joins
    (`Join` / `LeftJoin` / `AnyJoin` / `AllJoin` / `AsofJoin` / `FullJoin`),
    `Prewhere`, `Where`, `GroupBy`, `Having`, `OrderBy`, `Limit/Offset`,
    `Distinct`, `Setting`, plus `Count(ctx)`
  - `Insert(t).Row(...).Rows(...).Columns(...).Exec(ctx)` for batch INSERTs
  - Aggregates: `Uniq`, `UniqExact`, `UniqHLL12`, `AnyAgg`, `AnyLast`,
    `AnyHeavy`, `Quantile`, `QuantileExact`, `QuantileTiming`, `GroupArray`,
    `GroupUniqArray`, `ArgMax`, `ArgMin`, plus the usual `Count/Sum/Avg/Min/Max`
  - Date helpers: `ToDate`, `ToDateTime`, `ToStartOf{Day,Hour,Minute,Month}`,
    `ToYYYYMM`, `ToYYYYMMDD`, `DateDiff`
  - `DB` with `Hook` / `WithHook` / `Ping` / `Close` / `Begin` / `InTx`
    (context-safe rollback) — same surface as `pg.DB`
  - `Placeholder` exported so callers can render any drops expression
    with `?` placeholders via `clickhouse.ToSQL(expr)`
  - Identifier validation (`ErrInvalidIdentifier`) on construction
- `drops.BuilderOption` / `drops.WithPlaceholder` lets dialects override
  the `$N` placeholder rendering — used by ClickHouse to emit `?` and
  available to anyone building another dialect.
- `DB.Close()` releases the underlying driver if it implements `io.Closer`.
  The bundled `stdlib` adapter implements `Close` so `defer db.Close()`
  in user code now propagates to `*sql.DB.Close()`.
- `SelectBuilder.Count(ctx)` returns `int64` for the current SELECT,
  wrapping the existing query as a subquery — paginated UIs and admin
  dashboards usually need a total alongside their listing.
- `LoggerOptions.Redact func(args []any) []any` lets `LoggerHook` strip
  passwords, tokens and PII before logging when `LogArgs: true`. The
  redactor receives a copy so it can't mutate the caller's args.
- Go example tests (`ExampleAdd`, `ExampleDB_Select`, `ExampleDB_Insert`,
  `ExampleDB_WithHook`, `ExampleCol_Eq`) render in pkg.go.dev.
- `drops.Hook` interface + `drops.QueryEvent` for per-operation observability
  (kind, SQL, args, duration, error). Compose via `drops.ChainHooks`.
- `DB.WithHook(h)` to attach a hook; the hook is propagated into the
  transaction-bound DBs returned by `Begin` / `InTx`. `InTx` emits
  `begin` / `commit` / `rollback` events automatically.
- `pg.LoggerHook(log, opts)` convenience that wires any `LoggerFunc`
  (e.g. `log.Printf`, `slog.Info`) into the hook surface with
  `SlowQuery` threshold and `LogArgs` / `MaxSQLLength` options.
- `DB.Ping(ctx)` health check that issues `SELECT 1` and emits a
  `ping` event.
- Sentinel errors checkable with `errors.Is`:
  `ErrReturningRequired`, `ErrNoRowsToInsert`, `ErrNoUpdateAssignments`,
  `ErrSchemaRequired`, `ErrInvalidIdentifier`.
- Identifier validation at construction time (`NewTable`,
  `NewSchemaTable`, every column constructor) — rejects empty strings,
  non-UTF8 sequences and NUL bytes. Bad identifiers fail fast at
  startup rather than at the first query.
- GitHub Actions CI workflow: `go vet`, `go build`, `go test`,
  `go test -race`, `staticcheck`, `govulncheck` across Go 1.22 / 1.23 /
  1.24.
- MIT license (`license.md`).

### Changed
- **Migration diff generator never inlines constraints into `CREATE
  TABLE`** (`drops/pg`) — `Diff` now emits every composite primary
  key, UNIQUE, FOREIGN KEY and CHECK constraint as its own raw SQL
  `ALTER TABLE … ADD CONSTRAINT` statement, and enums as a separate
  `CREATE TYPE`. Previously UNIQUE constraints were rendered inline
  in the `CREATE TABLE` body; new tables now produce a bare column-only
  `CREATE TABLE` followed by the constraint statements (matching how
  composite PKs, FKs and CHECKs were already handled). This keeps each
  constraint independently diffable and re-orderable across migrations.
- `InTx` (both the root `drops.InTx` helper and `pg.DB.InTx`) now uses a
  detached context with a 5-second timeout for the deferred `Rollback`,
  so a cancelled or expired caller-ctx no longer prevents the cleanup
  path from running. The detached ctx still inherits values (trace IDs,
  request IDs) from the parent.
- All query builders (`Select`, `Insert`, `Update`, `Delete`) now route
  through `DB.Exec` / `DB.Query` so hook events fire uniformly,
  whether the SQL came from a builder or from raw `Exec`/`Query` calls.
- Errors that used to be unique `fmt.Errorf("…")` instances are now the
  sentinel values above. `errors.Is` works as expected.
- `drops.Raw` is now `type Raw string` (was a struct with a misleading
  `Args` field that never renumbered placeholders). Pure SQL text.
- Empty `In(col)` / `NotIn(col)` no longer emits the invalid
  `(col IN ())`. `In` returns `(false)`, `NotIn` returns `(true)` —
  matching set-theoretic semantics.

### Fixed
- **`CREATE INDEX` rendered table-qualified column names, producing
  invalid DDL** (`drops/pg`) — `NewIndex(...)` built from column handles
  emitted its column list as `("table"."column")`, which PostgreSQL
  rejects inside an index column list with `syntax error at or near
  ")"` (SQLSTATE 42601). Column references in the index column list now
  render as bare identifiers (`("column")`); functional/expression
  indexes are unaffected, and `WHERE` predicates (ordinary expressions)
  stay qualified. This also corrects pgvector `USING hnsw/ivfflat`
  index DDL. The bug was latent because the builder's tests only
  string-compared the rendered SQL and never executed it.

### Removed
- `drops.MustString` and `drops.Errorf` re-exports (unused).
