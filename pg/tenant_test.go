package pg_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

type tenantUser struct {
	ID       int64  `drop:"id,primaryKey,autoIncrement"`
	TenantID int64  `drop:"tenantId,notNull"`
	Name     string `drop:"name,notNull"`
}

func tenantSchema(t *testing.T) (*pg.Table, *pg.Entity[tenantUser]) {
	t.Helper()
	tbl := pg.AutoTable[tenantUser]("users")
	ent := pg.NewEntity[tenantUser](tbl).ScopeByTenant(tbl.Col("tenantId"))
	return tbl, ent
}

func TestScopeByTenantInjectsPredicateOnGet(t *testing.T) {
	tbl, ent := tenantSchema(t)
	_ = tbl
	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{cols: []string{"id", "tenantId", "name"}, data: [][]any{{int64(1), int64(99), "Alice"}}}, nil
	}}
	db := pg.New(fd)
	ctx := pg.WithTenant(context.Background(), int64(99))
	if _, err := ent.Get(db, ctx, int64(1)); err != nil {
		t.Fatalf("Get: %v", err)
	}
	sql := fd.queries[0]
	if !strings.Contains(sql, `"tenantId" = $`) {
		t.Errorf("Get must AND tenantId = $: %s", sql)
	}
}

func TestScopeByTenantRequiresCtxTenant(t *testing.T) {
	_, ent := tenantSchema(t)
	db := pg.New(&fakeDriver{})
	_, err := ent.Get(db, context.Background(), int64(1))
	if !errors.Is(err, pg.ErrTenantMissing) {
		t.Errorf("Get without WithTenant must error, got %v", err)
	}
}

func TestScopeByTenantOnQueryAll(t *testing.T) {
	tbl, ent := tenantSchema(t)
	_ = tbl
	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{cols: []string{"id", "tenantId", "name"}}, nil
	}}
	db := pg.New(fd)
	ctx := pg.WithTenant(context.Background(), int64(42))
	if _, err := ent.Query(db).All(ctx); err != nil {
		t.Fatalf("All: %v", err)
	}
	sql := fd.queries[0]
	if !strings.Contains(sql, `"tenantId" = $`) {
		t.Errorf("Query.All must AND tenantId = $: %s", sql)
	}
	// Args: tenant should be present.
	if fd.args[0][0] != int64(42) {
		t.Errorf("tenant arg: %v", fd.args[0])
	}
}

func TestScopeByTenantOnUpdate(t *testing.T) {
	_, ent := tenantSchema(t)
	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{cols: []string{"id", "tenantId", "name"}, data: [][]any{{int64(1), int64(7), "Alice"}}}, nil
	}}
	db := pg.New(fd)
	ctx := pg.WithTenant(context.Background(), int64(7))
	u := tenantUser{ID: 1, TenantID: 7, Name: "Alice"}
	if err := ent.Update(db, ctx, &u); err != nil {
		t.Fatalf("Update: %v", err)
	}
	sql := fd.queries[0]
	if !strings.Contains(sql, `"tenantId" = $`) {
		t.Errorf("Update must AND tenantId in WHERE: %s", sql)
	}
}

func TestScopeByTenantOnDelete(t *testing.T) {
	_, ent := tenantSchema(t)
	fd := &fakeDriver{}
	db := pg.New(fd)
	ctx := pg.WithTenant(context.Background(), int64(7))
	if _, err := ent.Delete(db, ctx, int64(1)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	sql := fd.queries[0]
	if !strings.HasPrefix(sql, "DELETE FROM") {
		t.Errorf("expected DELETE, got: %s", sql)
	}
	if !strings.Contains(sql, `"tenantId" = $`) {
		t.Errorf("Delete must AND tenantId in WHERE: %s", sql)
	}
}

func TestScopeByTenantCreateStampsTenant(t *testing.T) {
	_, ent := tenantSchema(t)
	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{cols: []string{"id", "tenantId", "name"}, data: [][]any{{int64(7), int64(42), "Alice"}}}, nil
	}}
	db := pg.New(fd)
	ctx := pg.WithTenant(context.Background(), int64(42))
	u := tenantUser{Name: "Alice"} // no tenant set
	if err := ent.Create(db, ctx, &u); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if u.TenantID != 42 {
		t.Errorf("Create should stamp tenant onto r, got %d", u.TenantID)
	}
}

func TestScopeByTenantCreateRejectsMismatch(t *testing.T) {
	_, ent := tenantSchema(t)
	db := pg.New(&fakeDriver{})
	ctx := pg.WithTenant(context.Background(), int64(42))
	u := tenantUser{Name: "Alice", TenantID: 99} // wrong tenant
	err := ent.Create(db, ctx, &u)
	if !errors.Is(err, pg.ErrTenantMismatch) {
		t.Errorf("Create must reject mismatching tenant, got %v", err)
	}
}

func TestScopeByTenantUnscopedEntityIsUnaffected(t *testing.T) {
	_, ent := entUsersSchema() // not tenant-scoped
	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{cols: []string{"id", "name", "email"}, data: [][]any{{int64(1), "X", "x@x"}}}, nil
	}}
	db := pg.New(fd)
	if _, err := ent.Get(db, context.Background(), int64(1)); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if strings.Contains(fd.queries[0], "tenant") {
		t.Errorf("unscoped entity should not emit tenant predicate: %s", fd.queries[0])
	}
}

func TestWithTenantContextRoundTrip(t *testing.T) {
	ctx := pg.WithTenant(context.Background(), "tenant-7")
	v, ok := pg.TenantFrom(ctx)
	if !ok || v != "tenant-7" {
		t.Errorf("tenant from ctx: %v, %v", v, ok)
	}
}
