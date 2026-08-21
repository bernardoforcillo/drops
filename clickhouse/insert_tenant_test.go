package clickhouse_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/clickhouse"
	"github.com/bernardoforcillo/drops/dropstest"
)

// The write side. An INSERT has no WHERE clause, so a ctx cannot filter
// it: the only two things it can do are STAMP the tenant onto every row
// and REFUSE when it cannot say which tenant that is.
//
// In this dialect that is the whole of the write side — there is no
// UPDATE and no DELETE to carry a predicate — and it is also the whole
// of the bulk path, since a batch INSERT is what a ClickHouse write is.

type scHit struct {
	ID       uint64    `drop:"id"`
	TenantID string    `drop:"tenantId"`
	Path     string    `drop:"path"`
	TS       time.Time `drop:"ts"`
}

// scHitEntity binds the struct to the table WITHOUT ScopeByTenant: the
// axis on scHits is declared on the table, and this entity is the proof
// that an entity built over it inherits both halves without saying so.
var scHitEntity = clickhouse.NewEntity[scHit](scHits)

// scOrders is the entity-declared axis, kept on its own table so that
// ScopeByTenant's registration cannot stack a second filter onto the
// table the rest of this package's tests read.
type scOrder struct {
	ID       uint64 `drop:"id"`
	TenantID string `drop:"tenantId"`
}

var (
	scOrders      = clickhouse.NewTable("orders")
	scOrderID     = clickhouse.Add(scOrders, clickhouse.UInt64("id"))
	scOrderTen    = clickhouse.Add(scOrders, clickhouse.String("tenantId"))
	scOrderEntity = clickhouse.NewEntity[scOrder](scOrders).ScopeByTenant(scOrderTen)
)

func init() { scOrders.Engine(clickhouse.MergeTree()).OrderBy(scOrderTen, scOrderID) }

// A raw builder on a table with a write axis stamps the ctx tenant. It
// used to insert whatever the caller bound — typically nothing for the
// tenant column — landing the row on NULL, where the very next SELECT
// could not see it and the INSERT was reported as a success.
func TestInsertStampsTheCtxTenant(t *testing.T) {
	db := clickhouse.New(nil)
	sql, args, err := db.Insert(scHits).
		Row(scHitID.Val(1), scHitPath.Val("/")).
		ToSQLCtx(tenantCtx("acme"))
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	checkSQL(t, sql, `INSERT INTO "hits" ("id", "path", "tenantId") VALUES (?, ?, ?)`)
	checkArgs(t, args, uint64(1), "/", "acme")
}

// Fail closed, and before a byte is rendered.
func TestInsertRefusesWithoutTenant(t *testing.T) {
	drv := dropstest.New()
	db := clickhouse.New(drv)
	_, err := db.Insert(scHits).Row(scHitID.Val(1)).Exec(context.Background())
	if !errors.Is(err, clickhouse.ErrTenantMissing) {
		t.Fatalf("got %v, want ErrTenantMissing", err)
	}
	if len(drv.Statements()) != 0 {
		t.Errorf("statement sent despite refusal: %v", drv.SQL())
	}
}

// A row that already names this tenant is accepted and not duplicated;
// one that names another is refused. This is the "background job
// stamped the wrong tenant" class of bug.
func TestInsertChecksAnAlreadyBoundTenant(t *testing.T) {
	db := clickhouse.New(nil)
	sql, args, err := db.Insert(scHits).
		Row(scHitID.Val(1), scHitTen.Val("acme")).
		ToSQLCtx(tenantCtx("acme"))
	if err != nil {
		t.Fatalf("matching tenant: %v", err)
	}
	checkSQL(t, sql, `INSERT INTO "hits" ("id", "tenantId") VALUES (?, ?)`)
	checkArgs(t, args, uint64(1), "acme")

	_, _, err = db.Insert(scHits).
		Row(scHitID.Val(1), scHitTen.Val("globex")).
		ToSQLCtx(tenantCtx("acme"))
	if !errors.Is(err, clickhouse.ErrTenantMismatch) {
		t.Fatalf("mismatching tenant: got %v, want ErrTenantMismatch", err)
	}
}

// A tenant column bound to an expression cannot be compared with the
// ctx tenant, so it is refused rather than waved through — the
// permissive default would be a hole shaped exactly like the feature.
func TestInsertRefusesAnOpaqueTenantBinding(t *testing.T) {
	db := clickhouse.New(nil)
	_, _, err := db.Insert(scHits).
		Row(scHitID.Val(1), scHitTen.Expr(drops.Raw("currentUser()"))).
		ToSQLCtx(tenantCtx("acme"))
	if !errors.Is(err, clickhouse.ErrTenantMismatch) {
		t.Fatalf("got %v, want ErrTenantMismatch", err)
	}
}

// Every row of the batch, not just the one the column list came from.
// A batch is the normal size of a ClickHouse write, so "the first eight
// hundred rows landed" is the outcome being ruled out.
func TestInsertStampsEveryRowOfABatch(t *testing.T) {
	db := clickhouse.New(nil)
	ins := db.Insert(scHits)
	for i := uint64(1); i <= 3; i++ {
		ins.Row(scHitID.Val(i), scHitPath.Val("/"))
	}
	sql, args, err := ins.ToSQLCtx(tenantCtx("acme"))
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	checkSQL(t, sql, `INSERT INTO "hits" ("id", "path", "tenantId") VALUES (?, ?, ?), (?, ?, ?), (?, ?, ?)`)
	checkArgs(t, args, uint64(1), "/", "acme", uint64(2), "/", "acme", uint64(3), "/", "acme")
}

// Unscoped is the escape for a backfill that writes other tenants'
// rows and says so by binding the column itself.
func TestInsertUnscopedStampsNothing(t *testing.T) {
	db := clickhouse.New(nil)
	sql, args, err := db.Insert(scHits).Unscoped().
		Row(scHitID.Val(1), scHitTen.Val("globex")).
		ToSQLCtx(context.Background())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	checkSQL(t, sql, `INSERT INTO "hits" ("id", "tenantId") VALUES (?, ?)`)
	checkArgs(t, args, uint64(1), "globex")
}

// An INSERT builder is a value too: stamping into its rows would leave
// the first request's tenant bound in the second request's statement.
func TestInsertBuilderIsReusableAcrossRequests(t *testing.T) {
	db := clickhouse.New(nil)
	ins := db.Insert(scHits).Row(scHitID.Val(1))
	_, first, err := ins.ToSQLCtx(tenantCtx("acme"))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	sql, second, err := ins.ToSQLCtx(tenantCtx("globex"))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	checkSQL(t, sql, `INSERT INTO "hits" ("id", "tenantId") VALUES (?, ?)`)
	checkArgs(t, first, uint64(1), "acme")
	checkArgs(t, second, uint64(1), "globex")
}

// ToSQL is honest about being incomplete on a table with a write axis.
func TestInsertToSQLOmitsTheStamp(t *testing.T) {
	db := clickhouse.New(nil)
	sql, args := db.Insert(scHits).Row(scHitID.Val(1)).ToSQL()
	checkSQL(t, sql, `INSERT INTO "hits" ("id") VALUES (?)`)
	checkArgs(t, args, uint64(1))
}

// A hook is an operand position no call site shows. What it binds goes
// through the same walk and the same tenant check as a caller's value,
// and a hook that binds the tenant column is verified rather than
// stamped over.
func TestInsertHookBindingsAreWalkedAndChecked(t *testing.T) {
	db := clickhouse.New(nil)

	hooked := clickhouse.NewTable("hooked")
	hid := clickhouse.Add(hooked, clickhouse.UInt64("id"))
	hten := clickhouse.Add(hooked, clickhouse.String("tenantId"))
	hcnt := clickhouse.Add(hooked, clickhouse.UInt64("n"))
	hooked.Engine(clickhouse.MergeTree()).OrderBy(hten).
		ContextFilter(clickhouse.TenantFilter(hten)).
		ScopeWritesByTenant(hten)
	hooked.OnInsert(clickhouse.InsertHookFunc(func(c *clickhouse.InsertHookCtx) {
		c.SetExpr(hcnt.Column, clickhouse.Subquery(db.Select(scSessionID).From(scSessions)))
	}))

	sql, args, err := db.Insert(hooked).Row(hid.Val(1)).ToSQLCtx(tenantCtx("acme"))
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	if !strings.Contains(sql, `FROM "sessions" WHERE ("sessions"."tenantId" = ?)`) {
		t.Errorf("the hook's statement went out unscoped: %s", sql)
	}
	checkArgs(t, args, uint64(1), "acme", "acme")

	// And the refusal reaches through the hook too.
	if _, _, err := db.Insert(hooked).Row(hid.Val(1)).ToSQLCtx(context.Background()); !errors.Is(err, clickhouse.ErrTenantMissing) {
		t.Errorf("got %v, want ErrTenantMissing", err)
	}
}

// A statement written into a value position reads as much as any SELECT
// does, and used to read every tenant's rows to compute a value stored
// in this tenant's row.
func TestInsertValueSubqueryIsScoped(t *testing.T) {
	db := clickhouse.New(nil)
	sql, args, err := db.Insert(scHits).
		Row(scHitID.Expr(clickhouse.Subquery(db.Select(scSessionID).From(scSessions)))).
		ToSQLCtx(tenantCtx("acme"))
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	if !strings.Contains(sql, `FROM "sessions" WHERE ("sessions"."tenantId" = ?)`) {
		t.Errorf("value subquery went out unscoped: %s", sql)
	}
	checkArgs(t, args, "acme", "acme")
}

// ----------------------------------------------------------------------
// The engine, which is where ClickHouse puts the upsert hazard
// ----------------------------------------------------------------------

// PostgreSQL's cross-tenant overwrite needs a statement — an upsert
// whose conflict target is the primary key. ClickHouse's needs none: a
// merging engine folds rows that share a SORTING KEY, in the
// background, and if the tenant column is not part of that key the fold
// crosses tenants. Stamping the row does not help, because the tenant
// column is not what the engine compares.
func TestMergingEngineWithoutTheTenantInTheSortingKeyIsRefused(t *testing.T) {
	db := clickhouse.New(nil)

	cases := []struct {
		name   string
		engine clickhouse.Engine
		want   bool // want refusal
	}{
		{"MergeTree", clickhouse.MergeTree(), false},
		{"ReplacingMergeTree", clickhouse.ReplacingMergeTree(""), true},
		{"CollapsingMergeTree", clickhouse.CollapsingMergeTree("sign"), true},
		{"VersionedCollapsingMergeTree", clickhouse.VersionedCollapsingMergeTree("sign", "v"), true},
		{"SummingMergeTree", clickhouse.SummingMergeTree(), true},
		{"AggregatingMergeTree", clickhouse.AggregatingMergeTree(), true},
		// A production schema spells its engine through Raw far more
		// often than through a constructor, and the replication prefix
		// composes with the family name. Both have to be seen.
		{"Raw/ReplicatedReplacingMergeTree",
			clickhouse.Raw("ReplicatedReplacingMergeTree('/clickhouse/t', '{replica}')"), true},
		{"Raw/GraphiteMergeTree", clickhouse.Raw("GraphiteMergeTree('graphite_rollup')"), true},
		{"Raw/ReplicatedMergeTree",
			clickhouse.Raw("ReplicatedMergeTree('/clickhouse/t', '{replica}')"), false},
		{"Raw/Distributed", clickhouse.Raw("Distributed(c, db, t, rand())"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tbl := clickhouse.NewTable("folded")
			id := clickhouse.Add(tbl, clickhouse.UInt64("id"))
			ten := clickhouse.Add(tbl, clickhouse.String("tenantId"))
			tbl.Engine(tc.engine).OrderBy(id).ScopeWritesByTenant(ten)

			_, _, err := db.Insert(tbl).Row(id.Val(1)).ToSQLCtx(tenantCtx("acme"))
			got := errors.Is(err, clickhouse.ErrTenantNotInSortingKey)
			if got != tc.want {
				t.Fatalf("refused=%v want=%v (err=%v)", got, tc.want, err)
			}
			if tc.want && !strings.Contains(err.Error(), "hits.tenantId") &&
				!strings.Contains(err.Error(), "folded.tenantId") {
				t.Errorf("refusal does not name the axis: %v", err)
			}
		})
	}
}

// Putting the tenant column in the sorting key is the fix, and it is
// what the refusal points at.
func TestMergingEngineWithTheTenantInTheSortingKeyIsAccepted(t *testing.T) {
	db := clickhouse.New(nil)
	tbl := clickhouse.NewTable("folded2")
	id := clickhouse.Add(tbl, clickhouse.UInt64("id"))
	ten := clickhouse.Add(tbl, clickhouse.String("tenantId"))
	tbl.Engine(clickhouse.ReplacingMergeTree("")).OrderBy(ten, id).ScopeWritesByTenant(ten)

	sql, args, err := db.Insert(tbl).Row(id.Val(1)).ToSQLCtx(tenantCtx("acme"))
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	checkSQL(t, sql, `INSERT INTO "folded2" ("id", "tenantId") VALUES (?, ?)`)
	checkArgs(t, args, uint64(1), "acme")
}

// A merging engine with no sorting key at all folds every row of every
// tenant into one, so it is the worst case rather than an exemption.
func TestMergingEngineWithNoSortingKeyIsRefused(t *testing.T) {
	db := clickhouse.New(nil)
	tbl := clickhouse.NewTable("folded3")
	id := clickhouse.Add(tbl, clickhouse.UInt64("id"))
	ten := clickhouse.Add(tbl, clickhouse.String("tenantId"))
	tbl.Engine(clickhouse.ReplacingMergeTree("")).ScopeWritesByTenant(ten)

	_, _, err := db.Insert(tbl).Row(id.Val(1)).ToSQLCtx(tenantCtx("acme"))
	if !errors.Is(err, clickhouse.ErrTenantNotInSortingKey) {
		t.Fatalf("got %v, want ErrTenantNotInSortingKey", err)
	}
	if !strings.Contains(err.Error(), "sorting key is empty") {
		t.Errorf("refusal does not say the key is empty: %v", err)
	}
	// Unscoped is the deliberate way in.
	if _, _, err := db.Insert(tbl).Unscoped().Row(id.Val(1)).ToSQLCtx(context.Background()); err != nil {
		t.Fatalf("Unscoped: %v", err)
	}
}

// Reads are unaffected: the guard on a SELECT is correct whatever the
// engine does, and refusing them would take a working query away over a
// hazard only writes carry.
func TestMergingEngineDoesNotRefuseReads(t *testing.T) {
	db := clickhouse.New(nil)
	tbl := clickhouse.NewTable("folded4")
	id := clickhouse.Add(tbl, clickhouse.UInt64("id"))
	ten := clickhouse.Add(tbl, clickhouse.String("tenantId"))
	tbl.Engine(clickhouse.ReplacingMergeTree("")).OrderBy(id).
		ContextFilter(clickhouse.TenantFilter(ten)).ScopeWritesByTenant(ten)

	sql, _ := mustSQL(t, db.Select(id).From(tbl), tenantCtx("acme"))
	checkSQL(t, sql, `SELECT "folded4"."id" FROM "folded4" WHERE ("folded4"."tenantId" = ?)`)
}

// ----------------------------------------------------------------------
// Entity
// ----------------------------------------------------------------------

// Create stamps the STRUCT as well as the row, so the caller's value
// comes back carrying the tenant it was written under.
func TestEntityCreateStampsTheStructAndTheRow(t *testing.T) {
	drv := dropstest.New()
	db := clickhouse.New(drv)

	r := scOrder{ID: 1}
	if _, err := scOrderEntity.Create(db, tenantCtx("acme"), &r); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if r.TenantID != "acme" {
		t.Errorf("struct not stamped: %+v", r)
	}
	checkSQL(t, drv.LastSQL(), `INSERT INTO "orders" ("id", "tenantId") VALUES (?, ?)`)
	checkArgs(t, drv.Last().Args, uint64(1), "acme")

	// A struct that already names another tenant is refused, and
	// nothing is sent.
	drv.Reset()
	other := scOrder{ID: 2, TenantID: "globex"}
	if _, err := scOrderEntity.Create(db, tenantCtx("acme"), &other); !errors.Is(err, clickhouse.ErrTenantMismatch) {
		t.Fatalf("got %v, want ErrTenantMismatch", err)
	}
	if len(drv.Statements()) != 0 {
		t.Errorf("statement sent despite refusal: %v", drv.SQL())
	}
}

// CreateMany stamps every row before any SQL is built, so a batch is
// either wholly owned or wholly refused.
func TestEntityCreateManyStampsEveryRowOrRefuses(t *testing.T) {
	drv := dropstest.New()
	db := clickhouse.New(drv)

	rows := []scOrder{{ID: 1}, {ID: 2}, {ID: 3}}
	if _, err := scOrderEntity.CreateMany(db, tenantCtx("acme"), rows); err != nil {
		t.Fatalf("CreateMany: %v", err)
	}
	for i, r := range rows {
		if r.TenantID != "acme" {
			t.Errorf("row %d not stamped: %+v", i, r)
		}
	}
	checkSQL(t, drv.LastSQL(),
		`INSERT INTO "orders" ("id", "tenantId") VALUES (?, ?), (?, ?), (?, ?)`)
	checkArgs(t, drv.Last().Args, uint64(1), "acme", uint64(2), "acme", uint64(3), "acme")

	drv.Reset()
	mixed := []scOrder{{ID: 1}, {ID: 2, TenantID: "globex"}}
	if _, err := scOrderEntity.CreateMany(db, tenantCtx("acme"), mixed); !errors.Is(err, clickhouse.ErrTenantMismatch) {
		t.Fatalf("got %v, want ErrTenantMismatch", err)
	}
	if len(drv.Statements()) != 0 {
		t.Errorf("partial batch sent: %v", drv.SQL())
	}
}

// ScopeByTenant declares both halves at once: the read filter on the
// table and the write axis.
func TestScopeByTenantDeclaresBothHalves(t *testing.T) {
	db := clickhouse.New(nil)
	tbl := clickhouse.NewTable("both")
	id := clickhouse.Add(tbl, clickhouse.UInt64("id"))
	ten := clickhouse.Add(tbl, clickhouse.String("tenantId"))
	tbl.Engine(clickhouse.MergeTree()).OrderBy(ten)

	type both struct {
		ID       uint64 `drop:"id"`
		TenantID string `drop:"tenantId"`
	}
	clickhouse.NewEntity[both](tbl).ScopeByTenant(ten)

	// Read: a bare db.Select carries the axis, with no entity in the
	// call at all.
	sql, _ := mustSQL(t, db.Select(id).From(tbl), tenantCtx("acme"))
	checkSQL(t, sql, `SELECT "both"."id" FROM "both" WHERE ("both"."tenantId" = ?)`)

	// Write: so does a bare db.Insert.
	isql, iargs, err := db.Insert(tbl).Row(id.Val(1)).ToSQLCtx(tenantCtx("acme"))
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	checkSQL(t, isql, `INSERT INTO "both" ("id", "tenantId") VALUES (?, ?)`)
	checkArgs(t, iargs, uint64(1), "acme")
}

// An entity rebuilt per request replaces its own filter rather than
// stacking one more onto the shared table — the WHERE clause that
// otherwise grows without bound.
func TestRebuildingAnEntityDoesNotStackFilters(t *testing.T) {
	db := clickhouse.New(nil)
	tbl := clickhouse.NewTable("perrequest")
	id := clickhouse.Add(tbl, clickhouse.UInt64("id"))
	ten := clickhouse.Add(tbl, clickhouse.String("tenantId"))
	tbl.Engine(clickhouse.MergeTree()).OrderBy(ten)

	type row struct {
		ID       uint64 `drop:"id"`
		TenantID string `drop:"tenantId"`
	}
	for i := 0; i < 5; i++ {
		clickhouse.NewEntity[row](tbl).ScopeByTenant(ten)
	}
	sql, args := mustSQL(t, db.Select(id).From(tbl), tenantCtx("acme"))
	if n := strings.Count(sql, "tenantId"); n != 1 {
		t.Errorf("%d tenant predicates: %s", n, sql)
	}
	checkArgs(t, args, "acme")
}

// The column list decides which bindings are rendered at all, so the
// tenant column is added to it whether or not stamping had to add the
// binding. Columns() fixing a list without it would otherwise drop a
// tenant bound on every row, and the batch would land owned by nobody —
// invisible in the SQL, which names only the columns it writes.
func TestExplicitColumnListStillCarriesTheTenant(t *testing.T) {
	db := clickhouse.New(nil)
	sql, args, err := db.Insert(scHits).
		Columns(scHitID, scHitPath).
		Row(scHitID.Val(1), scHitPath.Val("/"), scHitTen.Val("acme")).
		ToSQLCtx(tenantCtx("acme"))
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	checkSQL(t, sql, `INSERT INTO "hits" ("id", "path", "tenantId") VALUES (?, ?, ?)`)
	checkArgs(t, args, uint64(1), "/", "acme")
}

// A batch whose first row omits the tenant and whose later rows bind it
// aligns by column rather than by position, so no row loses a value to
// the NULL fill.
func TestBatchWithMixedTenantBindingsAlignsByColumn(t *testing.T) {
	db := clickhouse.New(nil)
	sql, args, err := db.Insert(scHits).
		Row(scHitID.Val(1)).
		Row(scHitTen.Val("acme"), scHitID.Val(2)).
		ToSQLCtx(tenantCtx("acme"))
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	checkSQL(t, sql, `INSERT INTO "hits" ("id", "tenantId") VALUES (?, ?), (?, ?)`)
	checkArgs(t, args, uint64(1), "acme", uint64(2), "acme")
}
