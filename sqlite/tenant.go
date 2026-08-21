package sqlite

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/bernardoforcillo/drops"
)

// Multi-tenant SaaS without explicit data isolation is a leak waiting
// to happen — one forgotten WHERE tenantId = ? and rows cross
// customers. ScopeByTenant + WithTenant make the isolation a property
// of the TABLE rather than of the call site: every statement that reads
// or writes it takes the tenant from ctx and carries the predicate.
// Forgetting to set the ctx errors out — the bad code path fails
// closed, not open.
//
// "Of the table" is load-bearing and this dialect learned it the hard
// way twice over. While the predicate was injected by the Entity
// methods it reached the queries those methods built and nothing else:
//
//   - an eager-loaded relation, whose child query is built by the
//     relation loader in find.go with no Entity anywhere in the call,
//     came back holding every tenant's rows;
//   - db.Select().From(Posts) — the spelling the readme shows first —
//     carried nothing at all, on a ctx with no tenant, without
//     refusing;
//   - the query cache keyed its entries on the rendered SQL and its
//     arguments, so with the tenant no longer bound into the statement
//     two tenants asking one question shared one entry.
//
// Declared on the table, the axis is applied by whichever executor runs
// the statement, which is the same list for a root query, a relation
// edge, a CTE body, a subquery operand, an UPDATE and a DELETE.
//
// The axis is per table, because the predicate names a column: an
// entity on `users` cannot say what a row of `posts` belongs to. A
// schema is scoped when each tenant-owning table declares its own axis
// — through its entity, or directly with
// Posts.ContextFilter(sqlite.TenantFilter(PostTenantID)).
//
//	var Projects = sqlite.NewEntity[Project](projects).
//	    ScopeByTenant(projects.Col("tenantId"))
//
//	ctx = sqlite.WithTenant(ctx, currentTenant)
//	got, err := Projects.Get(db, ctx, projectID)
//	// SELECT … WHERE ("id" = ?) AND ("tenantId" = ?)
//
// Create stamps the tenant on the row automatically before insert (or
// rejects a row that already carries a different one), so a stray
// background job cannot silently insert into the wrong tenant.
//
// # Where the automatic scoping stops
//
// These predicates are not an isolation boundary the way a server-side
// policy is, and SQLite has no equivalent of PostgreSQL row-level
// security to put underneath them: there are no roles, no policies, and
// a process that can open the file can read every byte in it. So the
// list below is not a footnote, it is the whole of what is left when
// the predicates do not reach:
//
//   - a raw statement, through [DB.Exec] or [DB.Query], carries what
//     the caller wrote and nothing else;
//   - a [drops.Raw] fragment, or a caller's own [drops.ExprFunc], is
//     text drops never parses — including one used as a subquery body,
//     a CTE body, or a filter's predicate;
//   - a view: SQLite evaluates the body the CREATE VIEW stored, and
//     drops sees only the view's name;
//   - a statement said Unscoped(), which is the point of the method;
//   - an INSERT into a table that declared a read filter and no write
//     column — see [Table.ScopeWritesByTenant] for why drops will not
//     guess the column.
//
// A reviewer who has not read that list will read a raw fragment or a
// view body as scoped when it is not.

type tenantCtxKey int

const tenantKey tenantCtxKey = 1

// WithTenant returns a context carrying tenant. Pass anything the driver
// can bind — a string id, int64, UUID string, etc.
func WithTenant(ctx context.Context, tenant any) context.Context {
	return context.WithValue(ctx, tenantKey, tenant)
}

// TenantFrom returns the tenant on ctx (ok=false when absent).
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
// scoped an unwrapped sentence would say the same thing whichever one
// of them stopped the query. Match it with errors.Is.
var ErrTenantMissing = errors.New("drops/sqlite: table is tenant-scoped but ctx has no tenant")

// ErrTenantMismatch is returned when a row or a binding carries a
// tenant value that disagrees with the ctx tenant. Catches the
// "background job stamped the wrong tenant" class of bug.
var ErrTenantMismatch = errors.New("drops/sqlite: row tenant disagrees with ctx tenant")

// TenantFilter is the canonical [ContextFilterFunc]: it reads the
// tenant off ctx and renders "<col> = ?".
//
//	Posts.ContextFilter(sqlite.TenantFilter(PostTenantID))
//
// It fails closed. A ctx with no tenant produces [ErrTenantMissing] and
// no statement at all, rather than a query that quietly spans every
// customer — which is the only defensible default for a filter whose
// absence is invisible in the result set. A background job that legally
// has no tenant says so with Unscoped() at the query, where a reviewer
// can see it.
//
// col is rendered as given, so pass the handle belonging to the table
// the filter is registered on; an alias handle would qualify with an
// alias the query has no FROM entry for. A statement that names the
// table under an alias is handled the other way round, by
// Table.resolveFilterExprs.
//
// The refusal is wrapped as "<table>.<column>", resolved once here
// rather than per call, so a caller who forgot the tenant on one
// request is told which table refused.
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

// ScopeByTenant marks col as the entity's tenant axis. Every subsequent
// Get / Query / Update / Delete reads the tenant from ctx (via
// WithTenant) and AND-s "<col> = ?" into the predicate; Create stamps
// the tenant onto the row.
//
// The axis is installed as a [Table.ContextFilter] rather than injected
// by each Entity method, and that is what makes it a defence rather
// than a habit — see the file comment for the three shapes that went
// around the injected version, one of which was the query cache.
//
// The consequence worth stating plainly: from here on *every* query
// against this table needs a tenant on its ctx, including one built
// straight from db.Select(). Queries that legitimately span tenants say
// so with Unscoped().
//
// Panics if col has no matching struct field — fail loudly at startup
// rather than at the first query.
func (e *Entity[T]) ScopeByTenant(col ColRef) *Entity[T] {
	c := col.col()
	for _, cf := range e.colFields {
		if cf.col == c {
			e.tenantCol = c
			e.tenantField = cf.field
			// The filter closes over the entity, not over the column,
			// so there is one source of truth: whatever tenantPredicate
			// answers is what the statement carries.
			e.table.setContextFilter(rowScopeFilterKey(e.rowType, "tenant"), e.tenantPredicate)
			// The write-side half of the same axis. A predicate scopes
			// the statements that have a WHERE clause; an INSERT has
			// none, so what it needs is the column to stamp — see
			// [Table.ScopeWritesByTenant] and [InsertBuilder.ToSQLCtx].
			// Declared on the table rather than kept on the entity
			// because db.Insert(e.Table()) has no entity to ask.
			e.table.setTenantAxis(c)
			return e
		}
	}
	panic("drops/sqlite: ScopeByTenant column has no matching struct field on " + e.table.Name())
}

// tenantPredicate returns "tenantCol = <ctx tenant>" when the entity is
// scoped, nil when it isn't, or ErrTenantMissing when scoped without a
// ctx tenant.
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
	for _, cf := range e.colFields {
		if cf.col == c {
			return c, cf.field, true
		}
	}
	return nil, nil, false
}

// stampTenant ensures r's tenant field matches ctx — assigns a zero
// value, rejects a mismatching one.
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
		ctxTenant := reflect.ValueOf(t)
		if !ctxTenant.Type().AssignableTo(fv.Type()) {
			if ctxTenant.Type().ConvertibleTo(fv.Type()) {
				ctxTenant = ctxTenant.Convert(fv.Type())
			} else {
				return ErrTenantMismatch
			}
		}
		fv.Set(ctxTenant)
		return nil
	}
	if !reflect.DeepEqual(fv.Interface(), t) {
		return ErrTenantMismatch
	}
	return nil
}

// sameTenant reports whether a bound tenant value and the ctx tenant
// name the same tenant.
//
// The conversion mirrors [Entity.stampTenant]: a tenant sourced as an
// int and a column typed int64 are the same tenant, and refusing that
// pairing would reject the very rows the entity methods stamp. The
// string guard is not decoration — Go converts an integer to a string
// as a rune, so without it tenant 65 and tenant "A" would compare
// equal, and a numeric tenant would be accepted as the owner of a text
// tenant column's row.
func sameTenant(bound, want any) bool {
	if reflect.DeepEqual(bound, want) {
		return true
	}
	bv, wv := reflect.ValueOf(bound), reflect.ValueOf(want)
	if !bv.IsValid() || !wv.IsValid() {
		return false
	}
	if (bv.Kind() == reflect.String) != (wv.Kind() == reflect.String) {
		return false
	}
	if !bv.Type().ConvertibleTo(wv.Type()) {
		return false
	}
	return reflect.DeepEqual(bv.Convert(wv.Type()).Interface(), want)
}
