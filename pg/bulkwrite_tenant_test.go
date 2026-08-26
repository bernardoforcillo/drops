package pg_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/pg"
)

// pg/tenant.go promises that "every statement that reads or writes it
// takes the tenant from ctx", and that forgetting the ctx fails closed.
// CreateMany and UpsertMany did neither: they bound whatever the row's
// tenant field happened to hold, which for a freshly decoded payload is
// the zero value. A row written with tenantId = 0 matches no tenant
// predicate, so it is invisible to every tenant including the one whose
// request created it — and the insert is reported as a success. These
// tests are on the rendered statement rather than a round trip: the
// failure is a wrong bound value, and a fixture holding one tenant's
// rows cannot see it.

// bulkRow is the row type for the bulk-write fixture.
type bulkRow struct {
	ID       int64  `drop:"id"`
	TenantID int64  `drop:"tenantId"`
	Name     string `drop:"name"`
}

// bulkSchema declares the tenant axis through the entity, which is what
// gives CreateMany a column to stamp and UpsertMany a column to gate
// the conflict branch on.
func bulkSchema() *pg.Entity[bulkRow] {
	tbl := pg.NewTable("widgets")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	tenant := pg.Add(tbl, pg.BigInt("tenantId").NotNull())
	pg.Add(tbl, pg.Text("name").NotNull())
	return pg.NewEntity[bulkRow](tbl).ScopeByTenant(tenant)
}

func TestCreateManyStampsTheCtxTenant(t *testing.T) {
	ent := bulkSchema()
	drv := dropstest.New()
	db := pg.New(drv)

	rs := []bulkRow{{Name: "a"}, {Name: "b"}}
	if _, err := ent.CreateMany(db, pg.WithTenant(context.Background(), int64(3)), rs); err != nil {
		t.Fatalf("CreateMany: %v", err)
	}
	want := []any{int64(3), "a", int64(3), "b"}
	if got := drv.Last().Args; !reflect.DeepEqual(got, want) {
		t.Errorf("bound arguments: got = %v, want %v", got, want)
	}
	for i := range rs {
		if got, want := rs[i].TenantID, int64(3); got != want {
			t.Errorf("rs[%d].TenantID after CreateMany: got = %v, want %v", i, got, want)
		}
	}
}

func TestBulkWritesWithoutTenantFailClosed(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
		rows []bulkRow
		want error
	}{
		{
			name: "create many with no tenant on ctx",
			ctx:  context.Background(),
			rows: []bulkRow{{Name: "a"}},
			want: pg.ErrTenantMissing,
		},
		{
			name: "create many with a row owned by another tenant",
			ctx:  pg.WithTenant(context.Background(), int64(3)),
			rows: []bulkRow{{TenantID: 9, Name: "a"}},
			want: pg.ErrTenantMismatch,
		},
		{
			name: "upsert many with no tenant on ctx",
			ctx:  context.Background(),
			rows: []bulkRow{{ID: 1, Name: "a"}},
			want: pg.ErrTenantMissing,
		},
		{
			name: "upsert many with a row owned by another tenant",
			ctx:  pg.WithTenant(context.Background(), int64(3)),
			rows: []bulkRow{{ID: 1, TenantID: 9, Name: "a"}},
			want: pg.ErrTenantMismatch,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ent := bulkSchema()
			drv := dropstest.New()
			db := pg.New(drv)

			var err error
			if strings.HasPrefix(tt.name, "create") {
				_, err = ent.CreateMany(db, tt.ctx, tt.rows)
			} else {
				_, err = ent.UpsertMany(db, tt.ctx, tt.rows)
			}
			if !errors.Is(err, tt.want) {
				t.Errorf("error: got = %v, want %v", err, tt.want)
			}
			// Failing closed means no statement at all, not a
			// statement the server happens to reject.
			if got, want := len(drv.Statements()), 0; got != want {
				t.Errorf("statements sent: got = %v, want %v (%v)", got, want, drv.Statements())
			}
		})
	}
}

// TestUpsertManyRefusesToMoveRowsBetweenTenants covers the half of the
// upsert that stamping alone does not reach. A primary key is unique
// across the table, not per tenant, so the row an INSERT collides with
// may belong to somebody else; "SET every column FROM EXCLUDED" then
// rewrites that row's data and its owner, which is tenant A taking
// tenant B's row by guessing an id.
func TestUpsertManyRefusesToMoveRowsBetweenTenants(t *testing.T) {
	ent := bulkSchema()
	drv := dropstest.New()
	db := pg.New(drv)

	rs := []bulkRow{{ID: 1, Name: "a"}}
	if _, err := ent.UpsertMany(db, pg.WithTenant(context.Background(), int64(3)), rs); err != nil {
		t.Fatalf("UpsertMany: %v", err)
	}
	sql := drv.LastSQL()
	// Only the conflict branch is under test — the INSERT's own column
	// list names the tenant column and always will.
	_, conflict, ok := strings.Cut(sql, " DO UPDATE SET ")
	if !ok {
		t.Fatalf("statement has no conflict update: got = %v, want an ON CONFLICT DO UPDATE", sql)
	}
	set, where, ok := strings.Cut(conflict, " WHERE ")
	if !ok {
		t.Fatalf("conflict update is not gated at all: got = %v, want a WHERE on the DO UPDATE", sql)
	}
	if got, want := strings.Contains(set, `"tenantId"`), false; got != want {
		t.Errorf(`SET list assigns "tenantId": got = %v, want %v (%v)`, got, want, set)
	}
	if got, want := strings.Contains(where, `"tenantId" = EXCLUDED."tenantId"`), true; got != want {
		t.Errorf("conflict update is gated on the tenant: got = %v, want %v (%v)", got, want, where)
	}
}

// TestUpsertManyWithNothingToUpdateDoesNothing pins the edge the
// tenant exclusion creates: on a table whose only non-key column is
// the tenant axis the SET list is empty, and "DO UPDATE SET" with no
// assignments is a syntax error rather than a no-op.
func TestUpsertManyWithNothingToUpdateDoesNothing(t *testing.T) {
	type keyOnly struct {
		ID       int64 `drop:"id"`
		TenantID int64 `drop:"tenantId"`
	}
	tbl := pg.NewTable("memberships")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	tenant := pg.Add(tbl, pg.BigInt("tenantId").NotNull())
	ent := pg.NewEntity[keyOnly](tbl).ScopeByTenant(tenant)

	drv := dropstest.New()
	db := pg.New(drv)
	if _, err := ent.UpsertMany(db, pg.WithTenant(context.Background(), int64(3)), []keyOnly{{ID: 1}}); err != nil {
		t.Fatalf("UpsertMany: %v", err)
	}
	if got, want := strings.Contains(drv.LastSQL(), "DO NOTHING"), true; got != want {
		t.Errorf("conflict branch is DO NOTHING: got = %v, want %v (%v)", got, want, drv.LastSQL())
	}
}
