package pg

import (
	"context"
	"errors"
	"reflect"

	"github.com/bernardoforcillo/drops"
)

// Multi-tenant SaaS without explicit data isolation is a leak waiting
// to happen — one forgotten WHERE tenantId = $1 and rows cross
// customers. ScopeByTenant + WithTenant make the isolation a property
// of the entity rather than the call site: every Get / Query /
// Update / Delete on the entity reads the tenant from ctx and
// auto-injects the predicate. Forgetting to set the ctx errors out
// — bad code path fails closed, not open.
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

// ErrTenantMissing is returned when an entity is scoped by tenant
// but ctx lacks one. Surfacing this error rather than silently
// running a cross-tenant query is the whole point of the feature.
var ErrTenantMissing = errors.New("drops/pg: entity is tenant-scoped but ctx has no tenant")

// ErrTenantMismatch is returned by Create when r carries a tenant
// value that disagrees with the ctx tenant. Catches the
// "background job stamped the wrong tenant" class of bug.
var ErrTenantMismatch = errors.New("drops/pg: row tenant disagrees with ctx tenant")

// ScopeByTenant marks col as the entity's tenant axis. Every
// subsequent Get / Query / Update / Delete reads the tenant from
// ctx (via WithTenant) and AND-s "<col> = $tenant" into the
// predicate. Create stamps the tenant onto r automatically.
//
// Panics if col has no matching struct field — fail loudly at
// startup rather than at the first query.
func (e *Entity[T]) ScopeByTenant(col ColRef) *Entity[T] {
	c := col.col()
	for _, cf := range e.colFields {
		if cf.col == c {
			e.tenantCol = c
			e.tenantField = cf.field
			return e
		}
	}
	panic("drops/pg: ScopeByTenant column has no matching struct field on " + e.table.Name())
}

// tenantPredicate returns "tenantCol = $ctx-tenant" when the
// entity is scoped, or nil when it isn't. Returns ErrTenantMissing
// when scoped but no tenant is on ctx.
func (e *Entity[T]) tenantPredicate(ctx context.Context) (drops.Expression, error) {
	if e.tenantCol == nil {
		return nil, nil
	}
	t, ok := TenantFrom(ctx)
	if !ok {
		return nil, ErrTenantMissing
	}
	return Eq(e.tenantCol, t), nil
}

// stampTenant ensures r's tenant field matches ctx. Called by
// Create — corrects a zero value, rejects a mismatching one.
func (e *Entity[T]) stampTenant(ctx context.Context, r *T) error {
	if e.tenantCol == nil {
		return nil
	}
	t, ok := TenantFrom(ctx)
	if !ok {
		return ErrTenantMissing
	}
	fv := reflect.ValueOf(r).Elem().FieldByIndex(e.tenantField)
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
