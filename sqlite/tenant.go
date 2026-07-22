package sqlite

import (
	"context"
	"errors"
	"reflect"

	"github.com/bernardoforcillo/drops"
)

// Multi-tenant isolation as a property of the entity rather than the
// call site: ScopeByTenant + WithTenant make every Get / Query /
// Update / Delete / Create read the tenant from ctx and auto-apply the
// predicate. Forgetting to set the ctx fails closed with
// ErrTenantMissing instead of leaking a cross-tenant query.
//
//	var Projects = sqlite.NewEntity[Project](projectsTable).
//	    ScopeByTenant(projectsTable.Col("tenantId"))
//
//	ctx = sqlite.WithTenant(ctx, currentTenant)
//	got, err := Projects.Get(db, ctx, projectID)
//	// SELECT ... WHERE ("id" = ?) AND ("tenantId" = ?)

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

// ErrTenantMissing is returned when a tenant-scoped entity is used with
// a ctx that carries no tenant.
var ErrTenantMissing = errors.New("drops/sqlite: entity is tenant-scoped but ctx has no tenant")

// ErrTenantMismatch is returned by Create when the row carries a tenant
// value disagreeing with the ctx tenant.
var ErrTenantMismatch = errors.New("drops/sqlite: row tenant disagrees with ctx tenant")

// ScopeByTenant marks col as the entity's tenant axis. Every subsequent
// Get / Query / Update / Delete AND-s "<col> = <ctx-tenant>" into the
// predicate; Create stamps the tenant onto the row.
//
// Panics if col has no matching struct field — fail loudly at startup.
func (e *Entity[T]) ScopeByTenant(col ColRef) *Entity[T] {
	c := col.col()
	for _, cf := range e.colFields {
		if cf.col == c {
			e.tenantCol = c
			e.tenantField = cf.field
			return e
		}
	}
	panic("drops/sqlite: ScopeByTenant column has no matching struct field on " + e.table.Name())
}

// tenantPredicate returns "tenantCol = <ctx tenant>" when the entity is
// scoped, nil when it isn't, or ErrTenantMissing when scoped without a
// ctx tenant.
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

// stampTenant ensures r's tenant field matches ctx — assigns a zero
// value, rejects a mismatching one.
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
