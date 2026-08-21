// Package pg provides PostgreSQL schema declarations and a fluent query
// builder for use with drops.
//
// Tables are declared with NewTable and a sequence of column constructors
// (Text, Integer, Serial, Boolean, Timestamp, UUID, JSONB, ...). Columns
// are themselves drops.Expressions and may be referenced anywhere a SQL
// fragment is expected.
//
// Queries are composed via the methods on DB (Select, Insert, Update,
// Delete). Each builder is immutable in spirit — methods return the
// builder to support chaining — and ends with an executor (All, One,
// Exec, Returning + Scan).
//
// Templates (Timestamps, SoftDelete, Audit, UUIDPrimaryKey) are reusable
// column groups that can be applied to any table — see template.go for
// the function-style pattern, mixin.go for the richer Mixin interface
// that also contributes indexes / lifecycle hooks / default filters.
// Libraries and applications expose their own templates by following
// either recipe.
//
// Lifecycle hooks (OnInsert / OnUpdate / OnDelete on Table) let
// templates extend INSERT / UPDATE / DELETE statements automatically.
// User-supplied values always win, so hooks are safe to register on
// shared tables. Default scopes (Table.DefaultFilter) apply a
// predicate to every SELECT / UPDATE / DELETE against the table
// unless the caller opts out with the builder's Unscoped() method —
// the mechanism behind soft-delete-aware queries.
//
// Entity[T] (see entity.go) binds a Go struct to a Table and exposes
// type-safe CRUD shortcuts: Get / Create / Update / Save / Delete /
// Query. It composes with lifecycle hooks and default scopes, so a
// SoftDelete mixin makes UserEntity.Delete() automatically rewrite
// into an UPDATE that flips deletedAt.
//
// Entity.Validate registers per-row validators that run before
// Create / Update / Save. A column marked with OptimisticLock turns
// Update into a guarded "AND version = current, SET version =
// version + 1" — mismatched versions return ErrStaleObject.
//
// Entity.SetFastScan plugs in a zero-reflection per-row scanner —
// typically the Scan<T> helper emitted by cmd/dropsgen. When set,
// Get / EntityQuery.All / EntityQuery.One skip the reflection path
// entirely. Eager-loaded relations (via With/WithRel) still need
// reflection, so the slow path kicks in automatically for those.
//
// AutoTable[T] / NewAutoEntity[T] derive a Table (and an Entity)
// from extended `drop:"col,primaryKey,autoIncrement,notNull,unique,default=...,version"`
// struct tags — the struct becomes the single source of truth, and
// a working ORM falls out of one declaration:
//
//	var UserEntity = pg.NewAutoEntity[User]("users")
//
// Entity.WithCache plugs in a drops/cache backend so Get /
// EntityQuery.All / EntityQuery.One are read-through and Create /
// Update / Save / Delete are write-invalidate. A built-in
// single-flight group dedupes concurrent identical PK misses so a
// thundering herd resolves to one DB query. Query results are keyed
// by sha256(SQL+args) and rely on the cache's TTL for freshness.
//
// # The predicates are not the boundary
//
// PostgreSQL row-level security is. drops' tenant predicates are the
// application-level layer that makes the common path fast, legible and
// correct; RLS is what makes it a boundary. A schema holding more than
// one tenant's rows wants both, and this section is here because the
// rest of the documentation used to imply that the first alone was
// enough.
//
// That is a finding, and it was reached by trying the other thing.
// Successive audits took twenty-nine scoping defects out of this
// package and left behind a family of checks that read its own source
// so the answers cannot go stale — see the pg/scope*_test.go files, and
// pg/scopecheckself_test.go, which points each check at a package built
// to slip past it. What that work established is that no amount more of
// it produces a boundary, for three reasons none of which is a bug:
//
//   - [github.com/bernardoforcillo/drops.Builder] is an exported escape
//     hatch, by design. What is assembled on one is text whose
//     structure drops never sees, so there is nothing in it to scope.
//     The list below is where that falls;
//   - the checks are an AST walk over one package. That makes them a
//     lint, and a lint is advice to exactly the person it constrains: a
//     change that edits the check passes the check;
//   - [SelectBuilder.Unscoped] exists and has to. Reading across
//     tenants is a real requirement, and a layer the guarded code can
//     switch off is not a layer that code sits behind.
//
// So declare RLS on every table that carries tenant rows —
// [Table.EnableRLS] and [Table.AddPolicy] emit it into the migration —
// write the policy against a setting the connection carries
// (current_setting), and set that setting for the request. Then a
// statement that lost its predicate returns no rows rather than
// somebody else's, and everything in the section below is a question of
// defence in depth rather than a disclosure.
//
// What the predicates still buy, with RLS underneath them, is worth
// stating: the axis is declared once per table instead of written into
// every call site; a request with no tenant on its ctx is refused
// before it reaches the server rather than answered with an empty
// result that reads like "no such row"; and the predicate is in the
// statement, where a query plan and a reviewer can both see it.
//
// # Where the automatic scoping stops
//
// This section describes package pg and nothing else. mysql, sqlite and
// clickhouse have no resolver walk at all: their builders render, and a
// table's DefaultFilters are the whole of what renders automatically.
// sqlite additionally has an Entity-level tenant predicate, which is the
// shape pg had before the axis moved onto the Table — a relation loader
// or a bare db.Select bypasses it. So treat tenant scoping in the other
// three dialects as UNIMPLEMENTED rather than as implemented
// differently: a schema ported across without changing anything is
// unscoped on arrival. (A port of the walk is queued as separate work.
// This paragraph is true the day it is written and has to go when that
// stops being so.)
//
// Everything drops builds, drops walks. A statement written anywhere
// inside a statement drops composed — a CTE body, a subquery operand in
// any clause of any of the four builders, a set-operation operand, the
// predicate a table's own filter answers with, an eager-loaded edge — is
// resolved for the executing ctx: it carries its own table's
// DefaultFilters and ContextFilters, and refuses rather than running
// when they cannot be resolved. The boundary is what drops did not
// build, and a reviewer needs to know exactly where that falls:
//
//   - Raw fragments and drops.ExprFunc bodies. There is nothing inside a
//     [drops.Raw] to walk, and a drops.ExprFunc is a closure the renderer
//     calls with no ctx — so a builder captured in one is written out
//     with its DefaultFilters, none of its ContextFilters, and no refusal
//     on a ctx carrying no tenant at all. Both are the caller's to scope.
//     Hand the builder over as an operand instead and it is walked like
//     any other.
//
//   - A relation drops did not declare. Every FROM, JOIN and USING
//     entry that is a *Table carries that table's filters, wherever it
//     entered from — [SelectBuilder.From], any of the four joins,
//     [DeleteBuilder.Using], [UpdateBuilder.From], and
//     [SelectBuilder.FromExpr], which takes a drops.Expression and
//     recognises a table among them as the relation it is. A FROM
//     source that is NOT a table — a raw fragment naming one, a
//     set-returning function — has no filter list to carry and nothing
//     is added for it.
//
//   - View bodies. [CreateView], [CreateOrReplaceView] and
//     [CreateMaterializedView] deliberately do not resolve theirs, and it
//     is the one place leaving a statement unresolved is the right
//     answer: a view outlives the request that created it, so the
//     creating tenant's predicate would be baked into an object every
//     other tenant reads through — and views are created in migrations,
//     where ctx carries no tenant and resolving would refuse the one
//     statement that has to run. The body's own DefaultFilters still
//     appear, because rendering is all that happens to it.
//
//   - [Table.DefaultFilter] takes no ctx itself, but a statement embedded
//     in one is walked for the executing ctx like any other. The ToSQL
//     path has no ctx to do that with and renders the unresolved list.
//
//   - The [DeleteHook] rewrite path. Hooks run inside WriteSQL, which has
//     no ctx, so whatever a hook returns is rendered ctx-free.
//     [SoftDeleteMixin]'s own rewrite is safe because it is composed from
//     the DeleteBuilder's already-resolved WHERE and RETURNING clauses; a
//     hook that composes anything else — a statement over some other
//     scoped table — has it sent unscoped and without a refusal.
//     [DeleteHookFunc] is exported, so this is reachable from application
//     code. See DeleteBuilder.resolveCtx.
//
//   - ToSQL renders an incomplete statement for any table carrying
//     context filters: the tenant predicate and the authorization guard
//     are missing from it, because there is no ctx to build them from. It
//     is for inspection. ToSQLCtx is what a request would send.
//
//   - Unscoped is statement-local. It reaches into no CTE body, no
//     subquery, no set-operation operand and no eager-loaded edge — which
//     is what makes it usable to widen exactly one relation, and why an
//     edge widens with [RelConfig.Unscoped] instead.
//
//   - A FULL JOIN onto a table carrying context filters is refused with
//     [ErrFullJoinScoped]. There is nowhere correct to put the predicate:
//     the ON clause lets the joined side's unmatched rows through, and
//     the WHERE clause deletes the FROM side's. Join a pre-filtered
//     subquery, or say Unscoped and write the predicates where a reviewer
//     can see them.
//
//   - Eager-load refusal is not atomic. A Find issues the parent query
//     and then one query per edge, so a refusal on a later edge surfaces
//     after earlier statements have already reached the server. Nothing
//     is returned to the caller, but the reads happened. Wrap the call in
//     a transaction if that matters.
package pg
