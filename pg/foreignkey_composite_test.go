package pg_test

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/pg"
)

func TestForeignKeyNEmitsCompositeConstraint(t *testing.T) {
	// Parent keyed on a composite PK (tenantId, id).
	users := pg.NewTable("users")
	uTenant := pg.Add(users, pg.BigInt("tenantId").NotNull())
	uID := pg.Add(users, pg.BigSerial("id"))
	users.PrimaryKey(uTenant, uID)

	// Child referencing the parent's composite key.
	orders := pg.NewTable("orders")
	pg.Add(orders, pg.BigSerial("id").PrimaryKey())
	oTenant := pg.Add(orders, pg.BigInt("tenantId").NotNull())
	oUser := pg.Add(orders, pg.BigInt("userId").NotNull())
	orders.ForeignKeyN(
		[]pg.ColRef{oTenant, oUser}, users,
		[]pg.ColRef{uTenant, uID}, pg.OnDelete("CASCADE"))

	stmts := pg.Diff(pg.EmptySnapshot(), pg.BuildSnapshot(pg.NewSchema(users, orders)))
	joined := strings.Join(stmts, "\n")

	want := `ALTER TABLE "orders" ADD CONSTRAINT "ordersTenantIdUserIdUsersTenantIdIdFk" FOREIGN KEY ("tenantId", "userId") REFERENCES "users"("tenantId", "id") ON DELETE cascade ON UPDATE no action;`
	if !strings.Contains(joined, want) {
		t.Errorf("missing composite FK statement.\n  want: %s\n  got:\n%s", want, joined)
	}

	// The composite FK must NOT be inlined into CREATE TABLE.
	for _, s := range stmts {
		if strings.HasPrefix(s, `CREATE TABLE "orders"`) && strings.Contains(s, "FOREIGN KEY") {
			t.Errorf("composite FK must be a separate ALTER, not inlined:\n%s", s)
		}
	}
}

func TestForeignKeyNPanicsOnMismatch(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic on column-count mismatch")
		}
	}()
	users := pg.NewTable("users")
	uID := pg.Add(users, pg.BigSerial("id").PrimaryKey())
	orders := pg.NewTable("orders")
	oA := pg.Add(orders, pg.BigInt("a").NotNull())
	oB := pg.Add(orders, pg.BigInt("b").NotNull())
	orders.ForeignKeyN([]pg.ColRef{oA, oB}, users, []pg.ColRef{uID}) // 2 vs 1
}
