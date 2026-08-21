package sqlite_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/sqlite"
)

// Eager-loaded relations carry the axis.
//
// This is the shape that made "declared on the entity" the wrong answer
// and "declared on the table" the right one. A relation loader has no
// Entity to ask — it builds its child query as
// db.Select(cols).From(rel.Target) — so a predicate the Entity methods
// injected filtered the parents and loaded every tenant's children.
// Nothing about the result set says so: the parents are right, the
// children are simply too many, and a single-tenant fixture cannot tell
// the two apart. Hence the assertions here are on the SQL each edge
// sent.

type relParent struct {
	ID       int64        `drop:"id"`
	TenantID int64        `drop:"tenantId"`
	Children []relChild   `dropRel:"children"`
	Tags     []relTagRow  `dropRel:"tags"`
	Owner    *relOwnerRow `dropRel:"owner"`
}

type relChild struct {
	ID       int64 `drop:"id"`
	ParentID int64 `drop:"parentId"`
	TenantID int64 `drop:"tenantId"`
}

type relTagRow struct {
	ID       int64 `drop:"id"`
	TenantID int64 `drop:"tenantId"`
}

type relOwnerRow struct {
	ID       int64 `drop:"id"`
	TenantID int64 `drop:"tenantId"`
}

// A HasMany edge is one extra batched query, and it carries the child
// table's own filters.
func TestHasManyEdgeCarriesTheChildTablesAxis(t *testing.T) {
	parents := sqlite.NewTable("rl_parents")
	parentID := sqlite.Add(parents, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(parents, sqlite.BigInt("tenantId"))

	children := sqlite.NewTable("rl_children")
	sqlite.Add(children, sqlite.BigInt("id").PrimaryKey())
	childParent := sqlite.Add(children, sqlite.BigInt("parentId"))
	childTenant := sqlite.Add(children, sqlite.BigInt("tenantId"))
	children.ContextFilter(sqlite.TenantFilter(childTenant))

	sqlite.NewRelations(parents).HasMany("children", children, parentID, childParent)

	drv := dropstest.New().
		Rows([]string{"id", "tenantId"}, []any{int64(1), scopeTenant}).
		Rows([]string{"id", "parentId", "tenantId"}, []any{int64(9), int64(1), scopeTenant})
	db := sqlite.New(drv)

	var out []relParent
	if err := db.Find(parents).With("children").All(scopeCtx(), &out); err != nil {
		t.Fatalf("All: %v", err)
	}
	stmts := drv.SQL()
	if len(stmts) != 2 {
		t.Fatalf("statements = %v, want 2", stmts)
	}
	want := `SELECT "rl_children"."id", "rl_children"."parentId", "rl_children"."tenantId" ` +
		`FROM "rl_children" WHERE "rl_children"."parentId" IN (?) ` +
		`AND ("rl_children"."tenantId" = ?)`
	if stmts[1] != want {
		t.Errorf("child query = %v, want %v", stmts[1], want)
	}
}

// A scoped child table with no tenant on ctx refuses the edge rather
// than loading every tenant's children.
func TestARelationEdgeWithoutATenantRefuses(t *testing.T) {
	parents := sqlite.NewTable("rn_parents")
	parentID := sqlite.Add(parents, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(parents, sqlite.BigInt("tenantId"))

	children := sqlite.NewTable("rn_children")
	sqlite.Add(children, sqlite.BigInt("id").PrimaryKey())
	childParent := sqlite.Add(children, sqlite.BigInt("parentId"))
	childTenant := sqlite.Add(children, sqlite.BigInt("tenantId"))
	children.ContextFilter(sqlite.TenantFilter(childTenant))

	sqlite.NewRelations(parents).HasMany("children", children, parentID, childParent)

	drv := dropstest.New().Rows([]string{"id", "tenantId"}, []any{int64(1), scopeTenant})
	db := sqlite.New(drv)

	var out []relParent
	err := db.Find(parents).With("children").All(context.Background(), &out)
	if !errors.Is(err, sqlite.ErrTenantMissing) {
		t.Fatalf("err = %v, want %v", err, sqlite.ErrTenantMissing)
	}
	// The root query ran (the parent table is unscoped here); the edge
	// did not, which is the whole point.
	if got := drv.SQL(); len(got) != 1 {
		t.Errorf("statements = %v, want only the root query", got)
	}
}

// A ManyToMany edge is two queries, and the FIRST of them is the one
// that renders its own SQL rather than going through an executor — so
// it is the one that could quietly skip the junction table's filters.
// A junction row is what says which tenant an association belongs to,
// and an unscoped junction read links this tenant's parents to every
// tenant's children through keys the target query cannot question.
func TestManyToManyCarriesTheJunctionAndTargetAxes(t *testing.T) {
	parents := sqlite.NewTable("rm_parents")
	parentID := sqlite.Add(parents, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(parents, sqlite.BigInt("tenantId"))

	tags := sqlite.NewTable("rm_tags")
	tagID := sqlite.Add(tags, sqlite.BigInt("id").PrimaryKey())
	tagTenant := sqlite.Add(tags, sqlite.BigInt("tenantId"))
	tags.ContextFilter(sqlite.TenantFilter(tagTenant))

	junction := sqlite.NewTable("rm_parent_tags")
	jParent := sqlite.Add(junction, sqlite.BigInt("parentId"))
	jTag := sqlite.Add(junction, sqlite.BigInt("tagId"))
	jTenant := sqlite.Add(junction, sqlite.BigInt("tenantId"))
	junction.ContextFilter(sqlite.TenantFilter(jTenant))

	sqlite.NewRelations(parents).
		ManyToMany("tags", tags, junction, jParent, jTag, parentID, tagID)

	drv := dropstest.New().
		Rows([]string{"id", "tenantId"}, []any{int64(1), scopeTenant}).
		Rows([]string{"parentId", "tagId"}, []any{int64(1), int64(4)}).
		Rows([]string{"id", "tenantId"}, []any{int64(4), scopeTenant})
	db := sqlite.New(drv)

	var out []relParent
	if err := db.Find(parents).With("tags").All(scopeCtx(), &out); err != nil {
		t.Fatalf("All: %v", err)
	}
	stmts := drv.SQL()
	if len(stmts) != 3 {
		t.Fatalf("statements = %v, want 3", stmts)
	}
	if !strings.Contains(stmts[1], `("rm_parent_tags"."tenantId" = ?)`) {
		t.Errorf("junction query is unscoped:\n%s", stmts[1])
	}
	if !strings.Contains(stmts[2], `("rm_tags"."tenantId" = ?)`) {
		t.Errorf("target query is unscoped:\n%s", stmts[2])
	}
}

// A BelongsTo edge goes through the same executor and so carries the
// axis without the loader knowing anything about tenants.
func TestBelongsToEdgeCarriesTheTargetAxis(t *testing.T) {
	parents := sqlite.NewTable("rb_parents")
	sqlite.Add(parents, sqlite.BigInt("id").PrimaryKey())
	parentTenant := sqlite.Add(parents, sqlite.BigInt("tenantId"))

	owners := sqlite.NewTable("rb_owners")
	ownerID := sqlite.Add(owners, sqlite.BigInt("id").PrimaryKey())
	ownerTenant := sqlite.Add(owners, sqlite.BigInt("tenantId"))
	owners.ContextFilter(sqlite.TenantFilter(ownerTenant))

	sqlite.NewRelations(parents).BelongsTo("owner", owners, parentTenant, ownerID)

	drv := dropstest.New().
		Rows([]string{"id", "tenantId"}, []any{int64(1), scopeTenant}).
		Rows([]string{"id", "tenantId"}, []any{scopeTenant, scopeTenant})
	db := sqlite.New(drv)

	var out []relParent
	if err := db.Find(parents).With("owner").All(scopeCtx(), &out); err != nil {
		t.Fatalf("All: %v", err)
	}
	stmts := drv.SQL()
	if len(stmts) != 2 {
		t.Fatalf("statements = %v, want 2", stmts)
	}
	if !strings.Contains(stmts[1], `("rb_owners"."tenantId" = ?)`) {
		t.Errorf("BelongsTo edge is unscoped:\n%s", stmts[1])
	}
}
