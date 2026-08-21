# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
once a 1.0 is cut.

## [Unreleased]

### Added
- **`dropsgen -rels`: the struct an eager load fills, generated.**
  Rows mode emits what a `SELECT` of one table hands back. What was
  still hand-written was the other shape — a parent with its children
  attached — and it is the one most likely to be wrong, because four
  things in it have to agree with declarations that live somewhere
  else: the field name, the slice-versus-pointer choice, the `dropRel`
  tag and the nested type. Three of the four fail silently. A relation
  that does not load reads exactly like a parent with no children.

  `dropsgen -rels ./db/schema -shape users:posts.comments` writes one
  struct per shape, embedding the `<Table>Row` rows mode already emits
  and adding one field per relation. The cardinalities follow the
  loader rather than the declaration, because where the two could be
  read differently `pg/find.go` is the one that decides: `HasMany`,
  `ManyToMany` and `MorphMany` become a slice, and a parent with no
  children is assigned an empty non-nil one, so `len()` is the answer
  and a nil means nobody loaded it; `HasOne` and `BelongsTo` become a
  pointer, because the loader leaves the field untouched when nothing
  matches and a zeroed struct is indistinguishable from a real row of
  zeros; `MorphTo` becomes an `any`, and a path through one is refused
  here exactly as the loader refuses to descend past one.

  The `-shape` paths are the same strings `With()` takes, so the shape
  and the query that fills it are written from one spelling — and they
  are the whole of how deep it goes. There is no "every relation": a
  schema with a back-reference makes that an infinite type, and a
  struct that declares a relation the query does not load is refused
  outright under `StrictLoading`, so generating more than was asked
  for would break the query it was generated for. A shape is named for
  its table and its paths (`UsersWithPostsComments`), which keeps a
  self-referential chain apart, and `-shape 'AuthorWithBooks=...'`
  names one outright. A name the package already declares is skipped
  with a note in the header, as in rows mode — but only when the
  struct already there carries the fields this run would have written,
  because the other generated structs nest it. A declaration that
  disagrees is a collision and is refused, and that holds across
  separate runs as well as within one: a derived name erases where a
  path was split, so `users:posts.tags` and `users:posts,tags` arrive
  at one name carrying different fields, and `-o` lets two runs write
  two files into one package. The package's own declarations are read
  back and compared, which costs nothing to check in and leaves the
  names no longer than they were. The output is byte-stable, including
  across a different arrangement of the same arguments.

  `examples/schemagen` now declares one relation of every kind — a
  profile for the `HasOne`, a junction for the `ManyToMany`, a notes
  table that is both halves of the polymorphic pair — and generates a
  shape for each, so the integration suite loads all six from
  generated structs against live PostgreSQL rather than from
  hand-written mirrors, including the empty case for every
  cardinality. Four of the six previously had the generator's half and
  the loader's half asserted separately, which is two expectations
  written by one author and nothing mechanical tying them together.

  Each relation field also carries `drop:"-"`. It is not decoration: a
  relation field sits at depth 0 and would out-rank a real column of
  the same name promoted from the embedded row struct, so the column
  would scan into the slice. And `-rows` now moves any
  `*_drops_rels.go` aside while it compiles the package, because a
  shape only compiles while the row structs it names exist — without
  that, generating the first shape made regenerating the row structs
  impossible.

- **`(*pg.Table).RelationNames`** lists the relations a table declares,
  sorted — the counterpart of `FilterNames`, and what a tool that
  walks a schema needs, since nothing outside the package can range
  over the map.

- **A rename is a rename, and drops stops rather than guess.** A
  structural diff can see that `email` is gone and `emailAddress` has
  arrived. It cannot see whether that was one rename or a drop and an
  add, and until now it picked the second — `DROP COLUMN "email"` and
  `ADD COLUMN "emailAddress"`, which is not a migration but data loss
  with a green checkmark. `AnalyzeMigration` flagged the DROP as
  destructive, which told the operator the migration was dangerous
  rather than that it was wrong.

  `DetectRenames` now finds the ambiguous pairs — a dropped column and
  an added one on the same table whose types are in the same family, a
  dropped table and an added one carrying the same columns — and
  `GenerateMigration` returns `*RenameAmbiguityError` and writes
  nothing while any of them is unanswered. That refusal is the part
  that matters: CI has no terminal, and the answer drops would have to
  invent without one is the `DROP COLUMN`. The family test is the
  middle of the two useless extremes: identical types would miss a
  rename that widened the column in the same step, and no test at all
  would ask whether a dropped timestamp had become a boolean.

  An answer is a `RenameDecision`, given through `GenerateOptions.Renames`
  or on the command line — `--rename-column users.email=emailAddress`,
  `--rename-table users=people`, `--drop-column users.email`,
  `--drop-table users` — or, under `drops generate --interactive`, one
  question per pair on stdin. Prompting is opt-in rather than inferred
  from whether stdin looks like a terminal, because the portable form of
  that question says yes to `/dev/null`, which is what a program with no
  stdin is handed. Every answer is recorded in
  `<dir>/meta/_renames.json`, next to the snapshots and the journal, so
  the question is asked once and replayed after that: a colleague
  generating from the same directory gets the same migration, and CI
  finds an answer already there. drizzle-kit reads the journal and the
  snapshots by name and ignores everything else, so the directory stays
  shared.

  The answer, once given, is applied by rewriting the previous snapshot
  as if the rename had already run and diffing that, so a rename
  combined with a type change comes out as the `RENAME` and the type
  change rather than as a drop and an add. `DiffOptions.Renames` carries
  it in all three dialects: PostgreSQL emits `ALTER TABLE ... RENAME
  COLUMN`, MySQL emits `RENAME COLUMN` on a server that has it (8.0,
  MariaDB 10.5.2) and `CHANGE COLUMN` on one that does not, and SQLite
  emits `RENAME COLUMN` in front of everything else — including a
  rebuild, whose `INSERT ... SELECT` then finds the column under the
  name it is copying. SQLite was the worst of the three: a rebuild that
  had not been told about a rename copied the table without that
  column and dropped the original in the same breath, with no `DROP
  COLUMN` anywhere in the file to warn anybody.

- **Push asks the rename question too, and the schema is where the
  answer lives.** Rename detection reached `GenerateMigration` and
  stopped there. All three `Push` implementations went on calling
  `Diff` with no renames and no ambiguity check, so `Push` was a
  second door into exactly the data loss the feature exists to close —
  and the quieter door, because a push has no migration file for
  anybody to read before it runs. `drops push` at least refused the
  `DROP COLUMN` as destructive, but `--allow-destructive` then applied
  the drop-and-add without ever mentioning that a rename was possible;
  a library caller of `pg.Push` or `mysql.Push` got no refusal at all;
  and `sqlite.Push` was worse than either, because its rebuild copies
  only the columns both sides name, so the renamed column's data went
  with **no `DROP COLUMN` anywhere in the statement list** for the
  destructive guard to catch.

  All three now run the same check the generator runs and return
  `*RenameAmbiguityError` before executing anything.

  The answer comes from the schema. `(*Col[T]).RenamedFrom("email")`
  and `(*Table).RenamedFrom("users")` state that a column or table is
  the one that used to be called that — a fact about the schema's
  history, kept in the thing `Push` already reads, rather than in a
  migration directory a push does not have. Stated there it answers
  every database the schema is pushed to instead of the one whoever
  typed a flag was pointed at, and it goes inert on its own:
  `renameStillPending` applies it only while the old name is in the
  database and the new one is not, so it can be left in place until
  every deployment has caught up. `DeclaredRenames` exposes what a
  schema states, and `GenerateMigration` reads it too — but does not
  copy it into `meta/_renames.json`, because that file records answers
  somebody gave and a declaration is not one. `PushOptions.Renames`
  takes the per-run answer, which is also the only way to say the
  other thing — that the column really is being dropped — since once
  such a drop has been pushed the question never comes back.

  `drops push` grows `--rename-column`, `--rename-table`,
  `--drop-column`, `--drop-table` and `--interactive`, the same five
  `drops generate` has, and refuses with the same exit code of 3.
  `--allow-destructive` is deliberately not one of them: whether a
  column is being renamed or dropped is a claim about what a change
  means, whether a change that destroys data may run is a permission
  about consequences, and one flag answering both would have granting
  the second silently grant the first.

  Two smaller things fell out of it. `sqlite.Push` and `mysql.Push`
  narrow the live side to the tables the schema declares, which
  filtered out the very table a declared table rename is about — the
  push then built the new table empty beside the old one and applied
  the rename to nothing; a table the schema names as a former name is
  now kept. And the refusal's wording follows its audience: the
  message still names the columns, but a push does not print the
  `--rename-column` lines or point at a rename log it has not got.

  Two things about the precedence between the two answers. A refusal
  given for this run now outranks a `RenamedFrom` naming the same
  object, which is what `PushOptions.Renames` and the refusal's own
  wording had been promising: the two answers are not the same shape —
  a rename names a pair, a refusal names only the object that is going
  — so the refusal could not outrank the declaration by carrying the
  same key, and was simply not heard. An operator who had decided the
  column really was going had nothing to say, and against a database
  holding both names the replayed declaration failed with advice about
  writing a migration, which a push has not got either.

  And a table rename's claim on the old name now expires. A
  declaration is meant to be left in the source until every deployment
  has caught up, so it outlives the rename; the hole it opens in
  `sqlite.Push`'s and `mysql.Push`'s narrowing must not. Once the
  database carries the new name, whatever answers to the old one is
  somebody else's table — and offering it to the diff as the previous
  life of a declared table made the push either ask an unanswerable
  question pairing the wrong table's columns, or, once told the old
  name really was going, drop it.

- **A rename decided on a run that changed nothing is still
  recorded.** `GenerateMigration` returned its `NoOp` result before
  writing `meta/_renames.json`, so an answer given on a run whose diff
  came to nothing was dropped on the floor and had to be given again —
  by a run that may have had nobody to give it. The log is now written
  first. Nothing is written when no answer was given this run, so a
  settled schema still writes nothing at all.

- **`drops lint`: three query mistakes, caught before the query
  runs.** Drizzle ships an ESLint plugin whose flagship rule is
  "delete without a where clause", and it is the single most-cited
  reason people install it. ESLint has to guess from a method name.
  `go/analysis` is handed the type checker's answers whether it asks
  or not, so a rule knows the value is a `*pg.DeleteBuilder` and knows
  which package-level `*pg.Table` the statement targets.

  `drops lint ./...` runs three analyzers, exported from
  `cmd/drops/dropslint` so they also work under golangci-lint or
  `go vet -vettool`. `unfilteredwrite` reports a DELETE or UPDATE
  executed with nothing to bound it — MySQL's `LIMIT` on a write
  counts as a bound; `Unscoped()` pointedly does not, since it removes
  predicates rather than adding one. `unboundedread` reports an `All`
  that reads every row: no `Where`, no `Limit`, and no
  `Budget.MaxRows`, whose vocabulary it borrows deliberately — the two
  are the build-time and run-time answers to one question, and
  `MaxRows` is enforced by injecting a LIMIT on exactly the call the
  rule watches. `loopload` reports a query that eager-loads a relation
  and executes inside a loop: the N+1 `pg.WithN1Detector` reports at
  run time, reported at build time instead.

  A linter is judged on its false positives, so each rule says where
  it stops. All three follow a builder within one function body and no
  further, flow-insensitively — a `Where` anywhere on the value
  counts, which misses the bug hidden under an `if` and never
  mis-reads the careful spelling. A builder that is returned, passed
  to a call, or aliased goes unreported. A statement is reported where
  it *executes*, never where `ToSQL` renders it. `unboundedread` asks
  the most before speaking: whole-row reads only, `All` only, and only
  when it can name the table — a table built inside a function is one
  whose size the analyzer cannot know and to which nobody can attach a
  budget, so a finding there would be a finding with no fix.

  A deliberate offender says so: `//drops:lint ignore <rule> — reason`
  on the line, the line above, or the enclosing function's doc
  comment. A table that really is small says so once on its
  declaration, `//drops:lint lookup`, which travels as an analysis
  fact to every package that imports the schema.

  Running it over drops itself moved the rules twice. A MySQL batched
  `UPDATE … LIMIT 2` was reported and should not have been; `Limit`
  now bounds a write. Seventy-odd unit tests reading three-row fixture
  tables built inside the test function were reported and should not
  have been — the rule was answering a question about table size for
  tables whose size it had no way to know, which is the requirement
  that a target be nameable. The tree is clean now but for two
  directives in the integration suite, where a test deliberately
  rewrites a one-row table. `docs/lint.md` has the whole story.
- **`drops.Multi`: a transaction as an inspectable value.** A
  multi-step transaction has been an opaque closure passed to
  `InTx`, and when step four fails at three in the morning the
  closure returns one error and nothing else — not which step, not
  what had already run. `drops.NewMulti().Run("team", …).Run("audit",
  …)` describes the same transaction as an ordered list of named
  steps; `Exec` runs them in one transaction, and a failure comes back
  as a `*MultiError` carrying the step's name, its position, its
  cause, and the `Results` of every step that had already succeeded.
  Results are keyed by step name and read out typed via
  `drops.StepResult[T]`; declaring a step with `drops.Step[T]` instead
  of `Run` makes the writer's and the reader's types the same thing to
  the compiler, so the run-time check cannot fail.

  The sharper argument for it is `pg.RetryPolicy`, which re-runs an
  arbitrary callback on a serialization failure. That is safe only by
  convention — nothing stops a closure from having sent an email on
  the attempt that got rolled back. A Multi is a list of steps that
  can be enumerated with `Names()` before it ever runs, so "this
  script touches these four things, in this order" becomes a fact CI
  can assert rather than a comment. It does not make a step body pure;
  it puts the shape of the transaction where a reviewer and a test can
  see it.

  `Multi` is not a second saga, and the doc says where the line is. A
  Multi is *one* transaction: all steps or none, no compensations
  because a failure leaves no trace, bounded to work short enough to
  hold a transaction open. A saga is one transaction per step, durable
  as it goes, and in exchange every step needs a compensating action
  and those compensations are best-effort. A saga can leave the world
  half-undone; a Multi cannot. It lives in the root package and takes
  a `drops.Driver`, so it works for every dialect through the one
  interface.

- **`And` and `Or` drop nil predicates, in every dialect.** Building a
  filter that depends on a request parameter meant accumulating into a
  `[]drops.Expression` and appending under an `if`, because passing a
  nil predicate was a nil-pointer dereference (pg, mysql, clickhouse)
  or a dangling `( AND x AND )` that no server would parse (sqlite).
  Nil now means what the caller meant by it — no restriction — so
  `pg.And(nil, x, nil)` is `x`, and the `if` and the slice go away.
  With nothing left to join, `And()` renders the identity `TRUE` and
  `Or()` renders `FALSE`; SQLite's `And()` used to render `()`, which
  was a syntax error.

  The same rule is applied one layer up, where the nil actually
  arrives: `Where`, `Having` and ClickHouse's `Prewhere` ignore nil
  predicates, and a clause that is left with nothing is omitted rather
  than emitted empty. A join's `ON` cannot be omitted — the grammar
  demands a predicate — so a nil condition there renders the same
  identity, `ON TRUE`, instead of the `ON` followed by nothing it
  produced before. That the three live engines accept `TRUE`, `FALSE`
  and `ON TRUE`, and agree on what they mean, is asserted against
  PostgreSQL, MariaDB and SQLite in the integration suite; SQLite only
  learned the two keywords in 3.23. `vector.And` / `vector.Or` already
  dropped the zero `Filter`, which is the value-typed version of the
  same idea — with one deliberate difference documented there: an empty
  `vector.Or()` is *no constraint*, not `FALSE`, because that API
  declines to express an unsatisfiable predicate at all.

  The rule cuts the other way on a write, and the doc comments now say
  so: `Delete(t).Where(nil)` used to be a nil dereference and is now a
  DELETE with no `WHERE`. What still bounds it is the table's global
  filters, which are merged in at render time and are unaffected by
  what the caller passed — asserted against a live PostgreSQL with a
  second tenant's row and a soft-deleted row in the table, since a
  lost guard shows up as a row and not as a string difference.

- **Query tagging from context (SQLCommenter).** A slow statement in
  `pg_stat_statements` or a MySQL slow log says what ran and says
  nothing about which line of application code ran it; the usual
  recovery is to eyeball the top-20 and grep the codebase for
  something shaped like them. `drops.WithQueryTags` puts the answer in
  the statement. Tags on the context are appended as a trailing
  comment in the SQLCommenter format Rails and EF Core emit —
  `SELECT … /*action='show',controller='users',request_id='7f3a'*/` —
  and the comment is the one part of a statement that survives into
  every log, view and proxy downstream. `context.Context` is a
  strictly better carrier for this than the thread-locals Rails uses,
  because it already follows the request across goroutines.

  This is not a `Hook`, and could not be: a hook fires after the
  operation and its return value is discarded, so it can observe a
  statement but never rewrite one. Tagging is therefore plumbed into
  the statement path itself — `DB.Exec` and `DB.Query` in all four
  dialects, which is where every builder's rendered SQL passes on its
  way to the driver — rather than exposed as general SQL-rewriting
  middleware, which drops deliberately refuses. The grammar is closed:
  key-sorted encoded pairs in a trailing comment, nothing else.

  Three decisions worth stating. The comment goes at the **end**, per
  the spec: too much tooling switches on a statement's first token —
  MySQL executes `/*!` and reads `/*+` as an optimizer hint, proxies
  route reads and writes by leading keyword — and the usual argument
  for a leading comment (a trailing one is lost when the statement is
  wrapped in a subquery) does not apply, because the comment is
  appended to a finished statement on its way to the driver, with no
  composition left to happen. Keys and values are **percent-encoded**
  to RFC 3986's unreserved set: they are application strings landing
  inside a SQL comment, and a value containing `*/` would close it and
  hand the rest of itself to the parser, so `*`, `/`, `'`, newline,
  `!` and `+` all escape and the rendered comment can only ever
  contain `[A-Za-z0-9-._~%'=,]`. Integration tests feed exactly that
  payload — aimed at dropping the table the same statement reads — to
  live PostgreSQL, MariaDB and SQLite, and confirm against
  `pg_stat_activity` and `information_schema.processlist` that what
  the server received is the comment that was written.

  Query arguments are **not taggable**, structurally rather than by
  documentation: a `Tag` holds two strings, with no `any`, `Stringer`
  or `Valuer` to convert a bound value through, and
  `drops.TagStatement` is handed the statement text and the context
  only — the argument slice is not in scope where the comment is
  built. Bound arguments are user data, and a tracing backend is not
  where user data goes.

  Cost is one `ctx.Value` lookup on a statement with no tags, which is
  what the untagged path is: `BenchmarkExecUntagged` is unchanged
  before and after (pg: ~207ns/2 allocs → ~201ns/2 allocs, within
  noise). Tag values do become part of the statement text, so a
  high-cardinality tag like a request id defeats statement caching for
  as long as it is set — documented rather than forbidden, since
  matching one logged statement to one trace is exactly what it is
  for.

- **Nullability, stated once and enforced from both ends.** Writing a
  NULL through the builder had no typed spelling: `(*Col[T]).Val`
  takes a `T`, so callers reached for `drops.Raw("NULL")` — which
  splices a literal, turns one statement into two for the plan cache,
  and routes the value around `AddArg`, where PII redaction, hooks and
  tracers live — or re-declared the column as `Custom[*string]`, which
  also re-types the operators and makes `Eq(nil)` a predicate that
  compiles, binds NULL and is unconditionally false. In SQLite there
  was no spelling at all: `Col[T]` has no `Expr`, the package has no
  untyped `Bind`, and `ColumnValue`'s methods are unexported, so no
  caller could put a NULL into an INSERT. `SetNull()`, `ValPtr(*T)`
  and `ValNull(sql.Null[T])` now exist on `Col[T]` in all four
  dialects; each binds a parameter, unwrapped to the value type `Val`
  would have bound, so hooks and drivers see one thing rather than
  two.

  Reading was the mirror image and worse, because nothing checked it:
  a nullable column bound to a `string` field was accepted at
  declaration and failed at the first NULL row, in a scan, with
  `database/sql`'s "converting NULL to string is unsupported" and a
  column index. The type system cannot see it — a column's `T` is the
  *operand* type its comparisons take, and the scan destination is a
  struct field drops reaches only by reflection — so the check went
  where both are in scope: `NewEntity` now refuses a column that
  admits NULL bound to a field that cannot receive one, naming the
  column, the field, its type and both fixes. It fires on whether the
  column admits NULL rather than on whether it said so, because a bare
  `pg.Text("bio")` is exactly the shape that has been accepting NULLs
  nobody declared. The rule is one-directional: a NOT NULL column
  bound to a `*T` is the legitimate "distinguish unset from zero"
  idiom and passes. `AllowNullableColumns(names...)` and
  `AllowAnyNullableColumn()` are the escape hatches, shaped like the
  unmapped-column ones.

  `Nullable()` and `IsNullable()` join `NotNull()` / `IsNotNull()` in
  pg, sqlite and mysql — ClickHouse already had both, and is the
  dialect that had this right all along: nullability there is an
  opt-in that changes the column's type, so no existing ClickHouse
  schema trips the check. `Nullable()` renders nothing in the other
  three, where a column admits NULL unless it says otherwise, so no
  snapshot churns and no migration appears.

- **Row and insert structs, generated from the table declaration**
  (`dropsgen -rows ./db/schema`). drizzle-orm's most-praised ergonomic
  feature is `$inferSelect` / `$inferInsert`: the row type is derived
  from the column table so it cannot drift, and the insert type omits
  the columns the database will fill. Go cannot infer a struct from a
  value, so the equivalent has to be generated — `dropsgen -schema`
  already ran struct to table, and this is the inverse. `UsersRow` has
  one field per column, the Go type that column's `*pg.Col[T]`
  carries, and the `drop:` tag that binds it; `UsersInsert` is the
  same minus a serial key, a column with a `DEFAULT`, a generated
  column and anything `Managed()` marks as written by drops.

  Nullability landing is what makes this worth generating rather than
  writing. `pg.NewEntity` now refuses a column that admits NULL bound
  to a field that cannot receive one, and refuses it at package-var
  init — so the mistake a hand-written struct earns is a panic at
  process start, or a scan failure in production on the first row that
  happens to be NULL. A generator that reads nullability off the
  declaration cannot emit that pairing: the nullable columns come out
  as pointers, and the struct is correct by construction.

  It reads the table by compiling one. A CLI cannot read a Go
  variable, and parsing `pg.Add` calls out of the source would recover
  the column names but not the Go types — a column's `T` lives in the
  `*Col[T]` handle and appears in no field of it. So `-rows` reuses
  the bridge `drops generate` and `drops push` already use: a
  throwaway program that imports the schema package, calls its
  `Schema()` function and prints what it finds as JSON, compiled by
  the real compiler against the real declarations. `(*pg.Column).GoType`
  is the one thing that had to be added for it, returning the value
  type the handle carries — nil for an `AutoTable` column, which was
  derived from a struct that already exists.

  Two decisions the mode had to make. A struct whose name the package
  already declares is **skipped**, with the name and its file recorded
  in the generated file's header: two declarations of one name in one
  package do not compile, so emitting a second one is not an option,
  and overwriting a struct somebody maintains is not a generator's
  call. And the output is byte-stable — including across the run that
  reads the previous run's output, which is why the previous file is
  moved aside before the bridge compiles the package, a step that also
  keeps a stale generated file from breaking the run that would have
  fixed it. A test runs the whole thing twice and compares bytes.

  What it will not write, it refuses to write. A generated file that
  does not compile is worse than no file, so the two shapes that would
  produce one are errors naming the column: an instantiated generic
  over a type from another package (`sql.Null[time.Time]` — reflect
  reports the argument by package *name*, and there is no path left to
  import), and two packages that share a name, which cannot both be
  imported unqualified. Where a qualifier *can* be written it is the
  package's own name rather than the last element of its import path,
  and the import carries that name when the two differ — every module
  past v1 ends its path in a version element, so `github.com/gofrs/`
  `uuid/v5` is package `uuid`.

  `examples/schemagen` now runs both directions over one table, and
  its tests close the loop: the struct that generated the table and
  the struct generated from that table agree field for field.

- **Global filters carry names, and a query bypasses them one at a
  time.** A table's implicit predicates — the soft-delete guard, a
  tenancy axis, an authorisation rule — were anonymous, and the only
  way past any of them was `Unscoped()`, which drops all of them. So a
  report that legitimately wanted soft-deleted rows silently lost its
  tenancy predicate: the caller asked to see deleted rows and got
  another customer's. `Table.AddFilter(name, pred)` registers a filter
  under a name and `IgnoreFilters(names...)` — on Select, Update,
  Delete, Find and EntityQuery — drops only what it names. EF Core
  shipped named query filters in 2025 for the same reason, which is
  the field conceding that one anonymous global filter was a design
  error; fixing it now, before there are users, is the cheap moment.

  `SoftDeleteMixin` registers under `FilterSoftDelete` and
  `ScopeByTenant` under `FilterTenant`, both constants rather than
  strings a caller retypes. `Unscoped` stays, documented as the blunt
  instrument: it drops every filter the *table* carries, which is what
  a migration wants and almost never what a query does. It does not
  reach the `ScopeByTenant` guard, which is built per query from the
  ctx and is not a table filter — losing isolation as a side effect of
  an unrelated `Unscoped` is the accident the feature exists to
  prevent, so crossing tenants has to be said out loud with
  `IgnoreFilters(FilterTenant)`. `DefaultFilter` still registers an
  anonymous filter, and is still only reachable by `Unscoped`.
  Available in pg, sqlite and mysql.

- **An unloaded relation stops reading as an empty one.** Forget
  `Load(UserPosts)` and `user.Posts` is `nil`, which is
  indistinguishable from "this user has no posts" — a silent wrong
  answer, and the Go ORM bug nobody in Go guards against. SQLAlchemy
  has `raiseload` and Rails `strict_loading`, but both work by
  intercepting the attribute read, and nothing in Go can interpose on
  a struct field. So drops refuses the *query* instead:
  `db.StrictLoading()` (or `.Strict()` on one query) walks the
  destination struct against the table's declared relations and fails
  before the SELECT runs, naming the relation, the struct, and both
  the call that would have loaded it and the call that waives it.

  The waiver is `NoLoad(rels...)` / `Without(names...)` — SQLAlchemy's
  `noload`, meaning not "do not load", which is already the default,
  but "I know it is not loaded and I will not read it". A destination
  struct with no relation field is never refused; neither is
  `Entity.Get`, which addresses a row by primary key and has no
  relation-loading vocabulary, so the check there would be an
  unconditional refusal rather than a signal about the query. The
  check is off by default and meant for development and test builds,
  where the mistake surfaces as a failing test rather than a failing
  request. It does not overlap `N1Hook`: that counts SQL that already
  ran, to catch the query a caller *did* write in a loop; this one
  costs no round trip and catches the query a caller did not write.
  pg descends the whole nested load tree; sqlite's `Find` loads one
  level, so the check looks at one level.

- **The `drops` CLI** — every step of the migration loop existed as a
  library function and had no front end, so a project that wanted
  drizzle-kit's workflow had to write its own `main` for each of them.
  `drops generate`, `migrate` (with `migrate down`), `push`, `drift`,
  `pull`, `baseline` and `status` now drive `pg.GenerateMigration`,
  `pg.DrizzleMigrator`, `pg.Push`, `pg.Introspect` and
  `pg.DetectDrift` directly. A CLI cannot read a Go variable, so the
  schema-aware commands generate a program that imports the schema
  package, call `func Schema() *pg.Schema`, and run it with `go run` —
  the real compiler evaluating the real declarations. `push`,
  `migrate` and `migrate down` route their statements through
  `pg.AnalyzeMigration` and refuse the destructive ones without
  `--allow-destructive`, naming each statement they held back;
  `drift` exits 3 so it can gate a pipeline. The gate classifies a
  statement collapsed to one line: the safety analyser's patterns join
  their two halves with `.*`, which does not cross a newline, so a
  wrapped `ALTER TABLE ... DROP COLUMN` — drizzle-kit's spelling, or an
  edited file's — would otherwise be applied unremarked. `push` and
  `baseline` refuse a `--pg-schema` other than `public`, because
  `pg.Diff` writes unqualified identifiers and would put the tables in
  whatever `search_path` points at while reporting success.
- **`cmd/drops` is a module of its own** — the CLI needs a connection
  and drops has no dependencies, so the binary began by speaking the
  PostgreSQL v3 wire protocol itself, SCRAM-SHA-256 included: ~1,500
  lines of hand-written, security-critical network code that one
  author wrote and nobody audited. The constraint had a better answer.
  `cmd/drops` now has its own `go.mod`, exactly as `integration/`
  does, and links `github.com/jackc/pgx/v5` behind `drops/stdlib` like
  any other program; the library's promise is untouched and CI still
  proves it. Two things follow. `go install
  github.com/bernardoforcillo/drops/cmd/drops@latest` is unchanged,
  but a release of the CLI is tagged `cmd/drops/vX.Y.Z` — a nested
  module carries its directory in the tag — and the `replace` that
  builds it against the checkout has to come out first. And `drops
  push`, alone among the commands, needs pgx in *your* `go.mod`: it is
  the one whose generated program opens the connection, and `go run`
  resolves that program's imports in your module rather than in the
  binary's. It checks, and names the `go get`. `generate`, `drift` and
  `status --schema` compile the same program without the connection
  and need nothing but drops.
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
- **ClickHouse introspection, snapshots, `Diff` and `Push`** —
  `clickhouse.Introspect` reads a table's real shape out of
  `system.tables` and `system.columns` (columns and types, the engine
  and its parameters, the sorting, primary, partition and sampling
  keys, the table TTL, the SETTINGS, and which columns take part in
  which key), `BuildSnapshot` derives the same shape from a
  `clickhouse.Schema`, `Diff` produces the statements between the two
  and `Push` applies them. The package doc said all four were planned;
  `drops/mirror` had meanwhile grown its own `InspectMirror` over
  `system.columns` because the dialect offered nothing, which put the
  hole in the load-bearing part of the library.

  `Diff` returns a `Plan` rather than the `[]string` the other
  dialects return, because ClickHouse is the dialect where a schema
  difference does not always have a statement behind it: there is no
  `ALTER` for a table's engine, its partitioning, its primary key or —
  beyond appending columns the same statement adds — its sorting key,
  and none for a column taking part in any of them. Those come back as
  `Refusal` values naming the remedy. On a `ReplacingMergeTree` the
  sorting key is worth the emphasis: the engine collapses rows that
  share it, so changing it is not a schema tweak but a change to what
  "the same row" means.

  A second category is reported rather than emitted. Where
  introspection cannot read back what a declaration says — a column
  TTL, which `system.columns` does not report — or where the server
  re-renders an expression in its own spelling (`ts + INTERVAL 30 DAY`
  reads back as `ts + toIntervalDay(30)`), drops withholds the
  statement as a `Notice` carrying the SQL rather than re-emitting it
  on every push for ever. Settings are compared only where the
  declaration names them, since ClickHouse materialises an engine's
  defaults into the metadata and a live `index_granularity` nobody
  declared is not evidence that anyone removed it.

  `clickhouse.Analyze` grades the statements a plan does carry —
  metadata, a background rewrite of every part, or a deletion with no
  way back — and it matters more here than in the other dialects
  because a ClickHouse `ALTER` returns before its work is done:
  `mutations_sync` defaults to 0, so a statement the server accepted
  may have hours of rewriting still ahead of it in `system.mutations`.
- **`clickhouse.ClassifyTypeChange`** — whether a column's type change
  widens, is unprovable, or is refused outright, together with the
  type a `MODIFY COLUMN` should actually set once a live
  `LowCardinality` wrapper is carried onto it. `drops/mirror`'s
  `Evolver` had this analysis to itself; it now calls the dialect for
  it and keeps only the judgements that are about mirroring — chiefly
  that a column losing its `Nullable` is merely unprovable for a table
  in general and certain to fail for a mirror, because
  `ClickHouseSink` writes NULL into every non-key column of every
  tombstone.
- **Queue time is reported separately from query time.** A statement's
  `Duration` has always been two measurements added together — the
  wait for a connection from the pool and the time the database took —
  and they have opposite remedies, so their sum tells an operator
  which question to ask only by accident. `drops.QueryEvent` now
  carries `WaitDuration` and `WaitKnown`, and `QueryDuration()`
  returns the difference.

  drops cannot take that measurement on its own: `drops.Driver` is
  three methods and acquisition happens inside whatever the caller
  plugged in. So the split arrives through an optional interface a
  pool may implement — `pg.ConnAcquirer`, one method that checks out a
  single connection — with `pg.QueueTimed` putting a clock around the
  checkout and nothing else, holding the connection until `Rows.Close`
  or `Commit`, and reporting through `drops.ReportConnWait`. A driver
  that measures the wait internally can report it directly.

  A driver that reports nothing leaves `WaitKnown` false, and every
  consumer treats that as unknown rather than zero: `drops/otel`
  records the new `db.client.connection.wait_time` and
  `db.client.operation.query_time` histograms only for events that
  carry a real measurement, because a queue-time gauge reading zero
  because nobody was counting looks exactly like a pool under no
  pressure. `db.client.operation.duration` still means the total.
- **`pg.Replicated` gained a post-write delay, and read routing gained
  teeth.** Rails' `DatabaseSelector` has two things drops did not.
  Half of the first turned out to exist under another name —
  `pg.WithReadYourWrites` is the window, armed by a write and checked
  by every read on the same context — but it was armed only by
  `Replicated.Exec`, so a write committed through `InTx`, which is how
  most writes worth reading back are made, left it unarmed and the
  next read went to a possibly-lagging replica. It now arms at commit,
  for transactions that actually wrote.

  The same blind spot ran deeper: a write carrying a `RETURNING` clause
  is issued through `Query`, not `Exec` — `InsertBuilder.Scan`,
  `UpdateBuilder.Scan` and `DeleteBuilder.Scan` all do it, and it is
  how a generated key comes back — and `Query` is the method that
  routes to a replica. `INSERT ... RETURNING id` was therefore sent to
  a replica and left the window unarmed. `Replicated.Query` now routes
  by what the statement does rather than by the method it arrived on:
  a statement whose leading keyword writes (or a `WITH` / `EXPLAIN`
  containing one) goes to the primary and arms the window exactly as
  `Exec` does.

  `Replicated.WithWriteDelay` is the other half: for the given
  duration after a write, that session's reads go to the primary
  regardless of replication position. It is the floor `WithLSNTracking`
  lacked — a caught-up replay position proves the row just written is
  there and says nothing about what else the write set in motion — and
  it widens a caller's window when it is longer, while leaving a
  window explicitly cleared with `d=0` cleared.

  `DB.InReadTx` makes "this only reads" enforced rather than assumed,
  by the one party that can enforce it: PostgreSQL refuses writes
  inside a read-only transaction with SQLSTATE 25006. `drops.Driver`
  cannot ask for one — `Begin` takes a context and nothing else — so
  drops issues `SET TRANSACTION READ ONLY` as the transaction's first
  statement, and `pg.ReadOnlyBeginner` is the one-method extension a
  driver can implement to save the round trip. When neither works the
  call fails with `pg.ErrNotReadOnly` rather than handing back a
  transaction that can write.
- Smaller additions: `pg.SmallSerial`, `sqlite.Column.Asc/Desc/As`,
  `clickhouse.Bind`, `clickhouse.Table.OrderByColumns`,
  `(*Col[T]).Managed` on pg/sqlite/clickhouse.
- **`clickhouse.ValidateSetting`** returns what
  `clickhouse.Table.Setting` panics on, for code that has to answer
  for a `SETTINGS` pair it did not write. `mirror.WithSetting` is the
  first such caller: its arguments arrive from outside the program and
  `DeriveClickHouse` reports everything else about its options as an
  error, so a bad pair now comes back as one instead of unwinding the
  caller.

- **The safety analyser names the shape a lost rename makes.**
  `AnalyzeStatements` now also reads a migration as a whole, not only
  one statement at a time, and reports `unstated-table-rename` when a
  DROP TABLE shares a migration with a CREATE TABLE of another name,
  and `unstated-column-rename` when one table loses a column and gains
  one (`drops/pg`, `drops/sqlite`, `drops/mysql`).

  These are the two ambiguities `DetectRenames` cannot see and so
  cannot refuse: a table rename where every column was renamed with it
  leaves the two snapshots with no column names in common, and a column
  rename that crosses a type family is not a candidate at all. Both
  come out of the generator as the destructive pair, silently.

  Both rules are graded `SeverityInfo` on purpose. `drop-table` is
  already an error and `drop-column` an error or a warning depending on
  the dialect, so a second finding at that level adds urgency to
  nothing and would be the first rule anyone put in `Ignore`; and both
  need *both* halves present, because a warning that fires on every
  genuine drop is one people learn to skip. A rename the migration
  states out loud accounts for the pair — which is what keeps the rule
  off SQLite's table rebuild, the CREATE/copy/DROP/RENAME sequence
  every column change on that dialect produces.

### Changed
- **`NewEntity` now refuses a nullable column bound to a field that
  cannot hold NULL, and it refuses at package-var init time — so an
  upgraded program does not start until the schema says what it
  means.** This is the breaking half of the nullability work above and
  it will fire on ordinary existing code: a column declared
  `pg.Text("name")` with neither `.NotNull()` nor `.Nullable()` emits
  a nullable column, and a `Name string` field cannot receive what
  that column is allowed to store. The old code was not terser, it was
  wrong — the database really did accept NULL there — but the upgrade
  is not silent and it is not lazy. The panic names every column, its
  field, and both fixes; `AllowNullableColumns(names...)` waives it
  per column and `AllowAnyNullableColumn()` waives it wholesale, for a
  schema that is not ready to decide today.
- **The two schema generators state nullability instead of leaving it
  implicit.** `pg.AutoTable`'s doc comment claimed pointer types made
  a column nullable "by default", which asserts that a non-pointer
  does not — and the code did no such thing, leaving every untagged
  column nullable. Both `AutoTable` (pg and sqlite) and
  `dropsgen -schema` now read nullability off the field's type: a
  pointer or an `sql.Null[T]` declares a nullable column, everything
  else NOT NULL, with a new `null` tag option as the escape hatch for
  a field whose type cannot say it. Nothing either generator emits can
  be the unstated shape `NewEntity` now rejects. This changes the DDL
  they produce for non-pointer untagged fields, from nullable to NOT
  NULL: regenerate, read the schema diff, and expect `SET NOT NULL` to
  fail where the column already holds NULLs — which is the latent bug
  surfacing in the safest available place.
- `dropsgen -introspect` gives a nullable column a pointer field, so
  introspecting a live schema and regenerating its declaration returns
  the schema it started from.
- **`NewEntity` now rejects a column bound to no struct field**
  (`drops/pg`, `drops/sqlite`, `drops/clickhouse`). It used to skip it
  silently, so a renamed field or a mistyped `drop:` tag removed the
  column from every INSERT and UPDATE while everything still compiled
  and every test that did not assert on that column still passed.
  Columns drops itself writes are exempt automatically; the rest must
  be mapped or named through `AllowUnmappedColumns`.

- **The two outbox dialects agreed on failure bookkeeping.** `drops/pg`
  wrote a handler failure through the draining transaction and
  `drops/mysql` deliberately through the pool, each with a coherent
  argument, and they cannot both be the library's answer. The pg answer
  wins and `drops/mysql` now matches it: the per-aggregate paths record
  the attempts bump on their own transaction via `failOneOn`.

  The deciding argument is the pool. Writing the failure through
  `Outbox.MarkFailed` asks the pool for a connection that
  `DrainAggregate`'s own transaction is holding, so a worker on a pool
  of one stopped at the first handler error and never came back — and
  on MySQL that transaction also holds the aggregate's session-scoped
  `GET_LOCK`, so the stall kept every other worker out of that
  aggregate too. The losing argument — that a rollback must not be able
  to undo an attempts bump, or a poison event never reaches
  MaxAttempts — is recorded in the doc comment on `failOneOn` in both
  packages, along with why it does not apply: both in-transaction
  callers record the failure and immediately return nil, so the
  transaction carrying the bump is the one that commits.

### Fixed
- **The `dropRel` tag's fallback is documented as the hazard it is.**
  `relationTargetField` looks a relation's field up by its `dropRel`
  tag and then, failing that, by a case-insensitive field-name match.
  Every shape the generator emits names its field after its relation,
  so every live test passed with the tag deleted and nothing said what
  the tag was for. The fallback claims a field because of what it is
  *called*: an untagged `Posts []PostsRow` that the caller meant as
  their own cache is filled by `With("posts")` all the same, and the
  walk reaches through embedded structs into the row struct, where the
  fields are columns. The doc comment now says so, and a live test
  discriminates — a field named `Articles` carrying `dropRel:"posts"`
  loads, the same field without the tag is refused.

- **ClickHouse's keyset pagination walked off the end at the first NULL
  key.** `clickhouse/cursor.go` still rendered a plain `col > ?` bound
  to the cursor value, so a page whose last row had a NULL in a key
  column produced a predicate that matched nothing — an empty page that
  reads exactly like the end of the result set, on every page from then
  on. `drops/pg` and `drops/sqlite` were made NULL-aware; ClickHouse
  was left because its NULL ordering needed a decision rather than a
  mechanical port.

  The decision: ClickHouse's default is NULLS LAST in *both*
  directions. PostgreSQL reaches its default by sorting NULL as the
  largest value and SQLite and MySQL by sorting it as the smallest, so
  on all three the placement flips with the direction. ClickHouse
  instead carries a `nulls_direction` beside the sort direction and
  defaults it to "same as direction, i.e. NULLS LAST", flipping only
  for an explicit `NULLS FIRST` — so a descending walk leaves the NULLs
  at the end where ascending put them. The guard is built against that,
  honours an explicit `NullsFirst`/`NullsLast`, and reverses both
  together when paging backward. `EncodeCursor` also follows a pointer
  to its pointee (a nil one being the NULL) and unwraps a
  `driver.Valuer`, as pg's does, so a Nullable key column can be a page
  boundary at all.

- **A ClickHouse cursor on a NaN or an infinity silently became a
  cursor on zero.** `encodeCursorValue` discarded the marshal error on
  the float branch, and JSON has no spelling for either, so the payload
  carried a bare `null` that decoded straight back to `0` — a page that
  ended on a NaN resumed from zero and replayed every row above it.
  `EncodeCursor` now returns the error. ClickHouse sorts NaN into the
  gap between the values and the NULLs, so this is reachable on any
  `Float64` key.

- **A MorphMany bound to a non-slice field was reported as a HasMany.**
  The slice guard in `pg/find.go` accepts `HasManyKind` and
  `MorphManyKind` but hardcoded the name of the first, so the one fact
  the error stated about the relation was wrong and sent the reader
  looking for a declaration that does not exist. The message now names
  the kind it saw. The sibling dialects were checked and are correct:
  `drops/sqlite` has no MorphMany, and both `ManyToMany` guards are
  reachable only for that kind.

- **A per-parent `Limit` on an eager-loaded relation dropped that
  relation's global filters.** An edge of a `Load` tree is normally a
  `Select().From(rel.To)`, so it carries the related table's guards for
  free. Asking for at most N children per parent takes a different
  route — `RelConfig.Limit` rewrites the edge into a hand-written
  `ROW_NUMBER() OVER (PARTITION BY …)` statement — and that writer
  never spelled the guards out. So adding `.Limit(n)` to a load, a
  change that reads as "give me fewer of these", silently widened what
  came back: on a live server, an author whose books table carried a
  soft-delete guard and a tenancy guard returned the soft-deleted book
  and the other tenant's book once the cap was added, and neither
  without it. The rewrite now AND-s in `rel.To`'s filters, so a capped
  load and an uncapped one scope identically.
- **`mysql.EntityQuery` had no `IgnoreFilters`.** The builders got it
  and the entity layer did not, which left an entity query on a
  doubly-guarded table holding only `Unscoped` — the all-or-nothing the
  named filters exist to replace.
- **`pg.EntityQuery.Stream` and `pg.Entity.Page` read every tenant's
  rows.** `ScopeByTenant` promises that every read on the entity is
  narrowed to the ctx tenant, and Get / Query.All / Query.One / Update
  / Delete all honoured it. Stream reached straight for the
  `SelectBuilder` and Page built its own statement, so both skipped the
  tenant predicate and the `AuthorizeWith` guard entirely — which made
  an export or a paginated listing the one way to read across every
  customer without asking for it, and made a missing ctx tenant a
  silent full-table read instead of `ErrTenantMissing`. Against a live
  server with two tenants in the table, both returned all of them.
  drops/sqlite's Page had this right already.
- **`sqlite`'s `SoftDeleteByID` and `Restore` wrote past every filter
  on the table, not just the one they meant to.** Both have to reach a
  row the soft-delete guard is hiding, and the only tool for that was
  `Unscoped()` — which also drops the tenancy or authorisation filter
  that decides *which* row the statement may touch. Against a live
  engine, soft-deleting id 2 on a table filtered to tenant 7 hid
  tenant 8's row. They now name the guard they are stepping around,
  `IgnoreFilters(FilterSoftDelete)`, and stay inside the rest.
  `SoftDeleteMixin` also stopped registering a second copy of the
  filter `SoftDelete` had already installed, which had been doubling
  the predicate in every rendered WHERE.
- **`clickhouse.Analyze` graded a statement by the text of its column
  comment.** Every keyword the rules match is also an ordinary English
  word, and the matcher read the whole statement including its
  literals. A `MODIFY COLUMN` carrying `COMMENT 'we remove this in Q3'`
  matched the `REMOVE` rule and came back as a metadata change that
  "touches no data", which is the opposite of what a type change does
  and is said reassuringly; an `ADD COLUMN` whose comment mentioned a
  drop table raised a destructive finding and cried wolf. Literals are
  emptied out before the rules run now.
- **A ClickHouse column comment could escape its own string literal.**
  `drops/clickhouse` quoted literals the way `drops/pg` does, by
  doubling the single quote, and ClickHouse's lexer is not PostgreSQL's:
  it reads C-style escapes inside a literal. A comment ending in a
  backslash therefore escaped its own closing quote and ran on into the
  rest of the statement, and a comment of `\' OR 1=1 --` left the
  literal altogether — through `CreateTable`, through `Diff`'s
  `COMMENT COLUMN`, and through the `MODIFY COLUMN` restatement that
  carries a comment. A comment is the one string in a schema statement
  that drops did not write itself. The backslash is escaped first now.
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
- **`pg`'s scanner disagreed with the root one about embedded
  unexported structs.** `pg/scan.go` carried a fork of the reflection
  walk that skipped unexported fields before it reached the anonymous
  ones, so a row type factoring its timestamps into an unexported
  `audit` lost those columns — scanned into a discard sink, zero values
  in the struct, no error. The root scanner walks the embedded type
  first and explains why. `pg` now uses `drops.StructFields` rather
  than a second copy of the rules, which also brings pg the root's
  collision rule: a name reachable at two depths belongs to the
  shallower field, and an embedded `time.Time` or `sql.Scanner`
  receives a column instead of lending its fields. `sqlite` and
  `mysql` were already on the root scanner; `clickhouse` still carries
  the same fork.
- **A keyset cursor sitting on a NULL paged nothing, forever.**
  `pg.EncodeCursor(spec, nil, …)` rendered `note > $1` bound to nil,
  which under three-valued logic matches no row — so every page after
  the first came back empty and looked exactly like the end of the
  result set. The keyset guard is now NULL-aware, as `mysql`'s already
  was: it is written against the `NULLS FIRST` / `NULLS LAST`
  placement the spec asks for, the equality on leading keys uses
  `IS NULL`, and paging backward reverses the placement along with the
  direction. `EncodeCursor` also follows a pointer, since a nullable
  column arrives from the last row of a page as a `*T`.
- **Two racing migrators failed inside PostgreSQL's catalogue.**
  `CREATE TABLE IF NOT EXISTS` and `CREATE SCHEMA IF NOT EXISTS` are
  not atomic against a concurrent `CREATE`, so a rolling deploy that
  ran the migrator once per replica reported a duplicate key on
  `pg_type_typname_nsp_index` or `pg_namespace_nspname_index` — names
  that tell an operator nothing. `Migrator.Up`/`Down` and
  `DrizzleMigrator.Up` now hold a PostgreSQL advisory lock for the
  whole run, so the loser waits and then finds nothing to do. It
  matters most for the drizzle migrator, whose history is keyed by
  hash with no unique constraint: unsynchronised, two runs would both
  apply a pending file and both record it. `WithLockTimeout` turns the
  wait into `ErrMigrationLocked`, `WithoutLock` opts out for a pool too
  small to lend the lock a connection, and `LockKey` names the key to
  look for in `pg_locks`.
- **The outbox's per-aggregate worker delivered events out of order.**
  `OrderingPerAggregate` ended each tick by falling through to the
  unordered drain, which selects every pending row regardless of
  aggregate — so the two cases where the ordered pass delivers nothing,
  an aggregate whose advisory lock another worker holds and an
  aggregate parked behind a failed event, were exactly the cases where
  the same tick delivered that aggregate's events anyway. The fallback
  is now `DrainUnaggregated`, which sees only the events that carry no
  aggregate and therefore no ordering promise. `DrainAggregate` also
  stops at the first event that is not available yet rather than
  stepping over it, since a failed event pushed into the future by its
  backoff was letting the events emitted behind it go first.
  `mysql/outbox.go` is a copy of the same design and still has both.
- **`drops.TagStatement(ctx, "")` returned `" /*…*/"`.** The separator
  keeps the comment off the end of the statement; with no statement
  there is nothing to separate it from.
- **`sqlite.NewEntity` could not see a key declared on the table.** A
  SQLite primary key arrives two ways — `(*Col[T]).PrimaryKey()` or
  `Table.PrimaryKey(a, b)` — and `NewEntity` read only the first, so a
  table declared the other way panicked "table has no PRIMARY KEY"
  beside a `CreateTable` that had been reading both spellings all
  along. It now reads both. `Table.PrimaryKey` also states NOT NULL on
  each member, as the column spelling already did: SQLite's
  table-level PRIMARY KEY does not enforce it — the legacy bug it
  keeps for compatibility — so on a live engine the key column really
  did take a NULL, and took a second identical NULL row after it. The
  two spellings now describe the same table, and drops/pg's `inKey`
  exemption in the nullability check is deliberately *not* copied
  here, because in SQLite it is not true.
- **`mysql.CreateTable` silently dropped every `CHECK` constraint.**
  The migration path emits `ALTER TABLE … ADD CONSTRAINT … CHECK`
  for a new table too, so one declaration built two different tables
  depending on which layer built it: against live MariaDB 10.11 the
  `CreateTable` one accepted rows the `Push` one rejects. `CHECK`
  clauses are now rendered inside the table body, in name order, after
  the keys.
- **`clickhouse.Push` sent a `CREATE TABLE` whose `ENGINE` was a Go
  comment.** A table nobody called `.Engine(…)` on renders a marker
  where the engine belongs, which `CreateTableErr` exists to catch and
  `Push` was not calling. `Push` now returns `ErrEngineRequired`,
  naming the table, before it reads the server — so `DryRun` is
  refused on the same terms, and `AllowRefused` does not waive it,
  that flag being about changes ClickHouse will not make rather than
  statements it cannot parse.
- **A ClickHouse `SETTINGS` value was concatenated unchecked.**
  `SETTINGS` is a comma-separated list and `MODIFY SETTING` is the
  same list of one, so a value carrying a comma outside a literal set
  this setting and whatever the rest of the string named; a comment
  opener did worse, ending the list while leaving the statement valid,
  so every setting after it was silently never applied.
  `Table.Setting` and `SelectBuilder.Setting` now panic on a pair that
  is not one setting — declaration-time loudness, like `mustIdent` —
  and the check is narrow enough that `'tier_a,tier_b'` and
  `disk(name = 'd', type = cache)` still go through. The reachable
  caller was `vector.Query.Params`, a map the caller fills: a key that
  is not a setting name is now dropped rather than rendered, and a
  string parameter ending in a backslash no longer escapes its own
  closing quote.
- **`clickhouse` snapshots flagged columns the partition key merely
  contains the name of.** `columnsNamedBy` was a substring search, so
  a column named `s` came back `inPartitionKey: true` under
  `toYYYYMM(ts)` — written into the snapshot file the next reader
  believes. The rendering is now split into identifier-shaped words
  and the name has to equal one; quoting falls away with the
  delimiters, so it works for `drops.Raw` expressions too.


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
