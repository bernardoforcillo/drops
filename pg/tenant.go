package pg

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/bernardoforcillo/drops"
)

// Multi-tenant SaaS without explicit data isolation is a leak waiting
// to happen — one forgotten WHERE tenantId = $1 and rows cross
// customers. ScopeByTenant + WithTenant make the isolation a property
// of the table rather than the call site: every statement that reads or
// writes it takes the tenant from ctx and carries the predicate.
// Forgetting to set the ctx errors out — bad code path fails closed,
// not open.
//
// "Of the table" is load-bearing and was learned the hard way. While
// the predicate was injected by the Entity methods it reached the
// queries those methods built and nothing else, so an eager-loaded
// relation — whose child query is built by the relation loader, with no
// Entity anywhere in the call — came back holding every tenant's rows.
// Declared on the table, the axis is applied by whatever executor runs
// the statement, which is the same list for a root query and for a
// relation edge.
//
// The axis is per table, because the predicate names a column: an
// entity on `users` cannot say what a row of `posts` belongs to. A
// schema is scoped when each tenant-owning table declares its own axis
// — through its entity, or directly with
// Posts.ContextFilter(pg.TenantFilter(PostTenantID)).
//
//	var Projects = pg.NewAutoEntity[Project]("projects").
//	    ScopeByTenant(ProjectsCols.TenantID)
//
//	ctx = pg.WithTenant(ctx, currentTenant)
//	got, err := Projects.Get(db, ctx, projectID)
//	// SELECT ... WHERE id = $1 AND "tenantId" = $2
//
// Create stamps the tenant on r automatically before insert (or
// rejects if r already carries a different tenant) so a stray
// background job can't silently insert into the wrong tenant.
//
// Three things a reader of this file has to take away with it.
//
// These predicates are not the isolation boundary. PostgreSQL
// row-level security is, and this layer is what makes the common path
// fast, legible and correct on top of it — see "The predicates are not
// the boundary" in the package doc for why no amount of further work
// here changes that, and for what to declare instead.
//
// The predicate reaches every statement drops composed, to any depth —
// a CTE body, a subquery operand, a set-operation operand, an
// eager-loaded edge, the predicate another table's filter answers with.
// Where it stops is listed in the package doc, under "Where the
// automatic scoping stops"; the entries there are the shapes that stay
// the caller's to scope, and a reviewer who has not read them will read
// a raw fragment or a view body as scoped when it is not.
//
// And all of it is pg. mysql and clickhouse have no ctx-resolved filter
// at all, and sqlite's ScopeByTenant is the shape described above as
// the one that was learned the hard way — a
// predicate the Entity methods inject, which a relation loader or a
// bare db.Select goes around. A schema ported across dialects is
// unscoped on arrival, so treat tenant isolation outside pg as
// unimplemented rather than as implemented differently.

type tenantCtxKey int

const tenantKey tenantCtxKey = 1

// WithTenant returns a context that carries tenant. Pass anything
// drivers can bind — a string id, int64 user id, UUID, or struct
// implementing driver.Valuer.
func WithTenant(ctx context.Context, tenant any) context.Context {
	return context.WithValue(ctx, tenantKey, tenant)
}

// TenantFrom returns the tenant on ctx (and ok=false when absent).
func TenantFrom(ctx context.Context) (any, bool) {
	v := ctx.Value(tenantKey)
	return v, v != nil
}

// ErrTenantMissing is returned when a statement reads or writes a table
// that is scoped by tenant and the ctx carries no tenant. Surfacing
// this rather than silently running a cross-tenant query is the whole
// point of the feature.
//
// It names the table rather than the entity, and the difference is not
// pedantry. While the axis lived on the Entity, an entity was the only
// thing that could produce this error; since it moved onto the Table
// the most surprising producer is a bare db.Select().From(Posts) with
// no Entity anywhere in the call — and "entity is tenant-scoped" sent
// that caller looking for an entity they never wrote.
//
// The producers wrap it with the table and column that refused, because
// that is the only diagnostic a caller gets: nothing exported asks a
// *Table which filters it carries, so in a schema where four tables are
// scoped, an unwrapped sentence would say the same thing whichever one
// of them stopped the query. Match it with errors.Is.
var ErrTenantMissing = errors.New("drops/pg: table is tenant-scoped but ctx has no tenant")

// ErrTenantMismatch is returned by Create when r carries a tenant
// value that disagrees with the ctx tenant. Catches the
// "background job stamped the wrong tenant" class of bug.
var ErrTenantMismatch = errors.New("drops/pg: row tenant disagrees with ctx tenant")

// TenantFilter is the canonical [ContextFilterFunc]: it reads the
// tenant off ctx and renders "<col> = $tenant".
//
//	Posts.ContextFilter(pg.TenantFilter(PostTenantID))
//
// It fails closed. A ctx with no tenant produces [ErrTenantMissing] and
// no statement at all, rather than a query that quietly spans every
// customer — which is the only defensible default for a filter whose
// absence is invisible in the result set. A background job that legally
// has no tenant says so with Unscoped() at the query, where a reviewer
// can see it.
//
// col is rendered as given, so pass the handle belonging to the table
// the filter is registered on: an alias handle would qualify with an
// alias the query has no FROM entry for.
//
// The refusal is wrapped as "<table>.<column>", resolved once here
// rather than per call, so a caller who forgot the tenant on one
// request is told which table refused. See [ErrTenantMissing] for why
// that wrapping is the whole diagnostic.
func TenantFilter(col ColRef) ContextFilterFunc {
	c := col.col()
	where := tenantAxisName(c)
	return func(ctx context.Context) (drops.Expression, error) {
		t, ok := TenantFrom(ctx)
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrTenantMissing, where)
		}
		return Eq(c, t), nil
	}
}

// tenantAxisName renders a tenant column as "table.column" for an error
// message, or as the bare column name for a handle that has not been
// added to a table — which is a declaration mistake, and one this
// message should describe rather than panic over.
func tenantAxisName(c *Column) string {
	if c == nil {
		return "?"
	}
	if t := c.Table(); t != nil {
		return t.Name() + "." + c.Name()
	}
	return c.Name()
}

// ScopeByTenant marks col as the entity's tenant axis. Every
// subsequent Get / Query / Update / Delete reads the tenant from
// ctx (via WithTenant) and AND-s "<col> = $tenant" into the
// predicate. Create stamps the tenant onto r automatically.
//
// The axis is installed as a [Table.ContextFilter] rather than injected
// by each Entity method, and that is what makes it a defence rather
// than a habit. An eager-loaded relation has no Entity to ask: its
// child query is built as db.Select().From(rel.To), so a predicate that
// only the entity methods knew about filtered the parents and loaded
// every tenant's children. Registered on the table, the axis is applied
// by whichever executor runs the statement — including the relation
// loaders, the per-parent-limit rewrite, Page and Stream.
//
// The consequence worth stating plainly: from here on *every* query
// against this table needs a tenant on its ctx, including one built
// straight from db.Select(). Queries that legitimately span tenants say
// so with Unscoped().
//
// Panics if col has no matching struct field — fail loudly at
// startup rather than at the first query.
//
// col is matched by Column.key, so a handle taken off a table alias
// names the same axis as the declared one. What is stored is the
// entity's own handle rather than the one passed in: the predicate has
// to qualify with the table this entity queries, and an alias handle
// would qualify with an alias no such query names.
func (e *Entity[T]) ScopeByTenant(col ColRef) *Entity[T] {
	c := col.col()
	for _, cf := range e.colFields {
		if cf.col.key() == c.key() {
			e.tenantCol = cf.col
			e.tenantField = cf.field
			// The filter closes over the entity, not over the column,
			// so there is one source of truth: whatever tenantPredicate
			// answers is what the statement carries.
			e.table.setContextFilter(rowScopeFilterKey(e.rowType, "tenant"), e.tenantPredicate)
			// The write-side half of the same axis. A predicate scopes
			// the statements that have a WHERE clause; an INSERT has
			// none, so what it needs is the column to stamp — see
			// [Table.ScopeWritesByTenant] and [InsertBuilder.ToSQLCtx].
			// Declared on the table rather than kept on the entity for
			// the reason the filter is: db.Insert(e.Table()) has no
			// entity to ask, and it is the spelling the readme shows.
			e.table.setTenantAxis(cf.col)
			return e
		}
	}
	panic("drops/pg: ScopeByTenant column has no matching struct field on " + e.table.Name())
}

// tenantPredicate returns "tenantCol = $ctx-tenant" when the
// entity is scoped, or nil when it isn't. Returns ErrTenantMissing
// when scoped but no tenant is on ctx.
//
// It is registered on the table as a context filter by ScopeByTenant
// and called by the executors; nothing injects it directly any more.
// Two places that build the same predicate would eventually disagree —
// and the way they disagree is that one of them stops being applied to
// a path somebody added later.
func (e *Entity[T]) tenantPredicate(ctx context.Context) (drops.Expression, error) {
	if e.tenantCol == nil {
		return nil, nil
	}
	t, ok := TenantFrom(ctx)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTenantMissing, tenantAxisName(e.tenantCol))
	}
	return Eq(e.tenantCol, t), nil
}

// tenantWriteAxis returns the column and struct field a write stamps
// the ctx tenant onto, and whether this entity has one.
//
// The entity's own axis comes first; failing that the table's, because
// a tenant column is a property of the table — the same reasoning that
// moved the read-side predicate off the entity. A schema that scopes
// its reads with Table.ContextFilter and names its write column with
// [Table.ScopeWritesByTenant] has an axis this entity never declared,
// and a Create that ignored it would bind the struct's zero tenant to
// a column the builder is about to stamp — two answers for one row.
//
// A table axis with no matching field on T is not an error and not a
// gap: the row simply cannot carry a tenant, and the stamping happens
// on the binding instead, in [InsertBuilder.resolveCtx].
func (e *Entity[T]) tenantWriteAxis() (*Column, []int, bool) {
	if e.tenantCol != nil {
		return e.tenantCol, e.tenantField, true
	}
	c := e.table.tenantAxis()
	if c == nil {
		return nil, nil, false
	}
	if f, ok := e.fieldFor(c); ok {
		return c, f, true
	}
	return nil, nil, false
}

// stampTenant ensures r's tenant field matches ctx. Called by
// Create, CreateMany, UpsertMany and CopyFrom — corrects a zero value,
// rejects a mismatching one.
func (e *Entity[T]) stampTenant(ctx context.Context, r *T) error {
	col, field, ok := e.tenantWriteAxis()
	if !ok {
		return nil
	}
	t, ok := TenantFrom(ctx)
	if !ok {
		return fmt.Errorf("%w: %s", ErrTenantMissing, tenantAxisName(col))
	}
	fv := reflect.ValueOf(r).Elem().FieldByIndex(field)
	if fv.IsZero() {
		// Assign — set via reflection. Fields must be settable.
		ctxTenant := reflect.ValueOf(t)
		if !ctxTenant.Type().AssignableTo(fv.Type()) {
			// Try a numeric / string conversion when types differ
			// but are convertible — keeps the API flexible for
			// int64 PKs paired with a tenant value sourced as int.
			if ctxTenant.Type().ConvertibleTo(fv.Type()) {
				ctxTenant = ctxTenant.Convert(fv.Type())
			} else {
				return ErrTenantMismatch
			}
		}
		fv.Set(ctxTenant)
		return nil
	}
	// r already has a tenant — must match ctx.
	if !reflect.DeepEqual(fv.Interface(), t) {
		return ErrTenantMismatch
	}
	return nil
}
