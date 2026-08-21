package sqlite_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/sqlite"
)

// --- money ------------------------------------------------------------

func TestMoney(t *testing.T) {
	a, err := sqlite.MoneyFromString("12.34")
	if err != nil {
		t.Fatal(err)
	}
	if a.Cents() != 1234 {
		t.Errorf("cents: got %d want 1234", a.Cents())
	}
	b := a.Add(sqlite.MoneyFromCents(66))
	if b.String() != "13.00" {
		t.Errorf("add: got %s want 13.00", b.String())
	}
	if got := a.MulRate(0.10).String(); got != "1.23" {
		t.Errorf("mulRate: got %s want 1.23", got)
	}
	// JSON round-trips as a precision-safe string.
	j, _ := json.Marshal(a)
	if string(j) != `"12.34"` {
		t.Errorf("json: got %s", j)
	}
	var m sqlite.Money
	if err := json.Unmarshal([]byte(`"5.50"`), &m); err != nil || m.Cents() != 550 {
		t.Errorf("unmarshal: %v cents=%d", err, m.Cents())
	}
	// driver.Value is the raw cents integer.
	v, _ := a.Value()
	if v.(int64) != 1234 {
		t.Errorf("value: got %v", v)
	}
}

// --- safety -----------------------------------------------------------

func TestSafetyAnalyzer(t *testing.T) {
	sql := strings.Join([]string{
		`DROP TABLE "users"`,
		`ALTER TABLE "users" ADD COLUMN "age" INTEGER NOT NULL`,
		`ALTER TABLE "users" ADD COLUMN "created" TEXT DEFAULT CURRENT_TIMESTAMP`,
		`DELETE FROM "sessions"`,
		`UPDATE "users" SET "x" = 1 WHERE "id" = 1`, // safe — has WHERE
	}, "\n--> statement-breakpoint\n")

	got := map[string]sqlite.SafetySeverity{}
	for _, w := range sqlite.AnalyzeMigration(sql) {
		got[w.Rule] = w.Severity
	}
	for _, rule := range []string{
		"drop-table",
		"add-not-null-column-without-default",
		"add-column-dynamic-default",
		"delete-without-where",
	} {
		if _, ok := got[rule]; !ok {
			t.Errorf("expected rule %q to fire", rule)
		}
	}
	if _, ok := got["update-without-where"]; ok {
		t.Error("update with WHERE should not warn")
	}
	// Suppression works.
	sup := sqlite.AnalyzeMigration(sql, sqlite.SafetyOptions{Ignore: []string{"drop-table"}})
	for _, w := range sup {
		if w.Rule == "drop-table" {
			t.Error("drop-table should have been suppressed")
		}
	}
}

// --- retry ------------------------------------------------------------

func TestRetryPolicy(t *testing.T) {
	p := sqlite.DefaultRetryPolicy()
	if p.MaxAttempts != 3 {
		t.Errorf("attempts: %d", p.MaxAttempts)
	}
	// ExponentialJitter is always positive and non-decreasing-ish.
	bo := sqlite.ExponentialJitter(0, 0)
	if bo(1) <= 0 || bo(5) <= 0 {
		t.Error("backoff must be positive")
	}
}

func TestInTxRetries(t *testing.T) {
	drv := &entDriver{}
	db := sqlite.New(drv).WithRetry(sqlite.RetryPolicy{
		MaxAttempts: 3,
		Errors:      []error{sqlite.ErrBusy},
	})
	calls := 0
	err := db.InTx(context.Background(), func(*sqlite.DB) error {
		calls++
		if calls < 3 {
			return sqlite.ErrBusy
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if calls != 3 {
		t.Errorf("callback ran %d times, want 3", calls)
	}
}

func TestInTxRetryExhausted(t *testing.T) {
	db := sqlite.New(&entDriver{}).WithRetry(sqlite.RetryPolicy{
		MaxAttempts: 2,
		Errors:      []error{sqlite.ErrLocked},
	})
	calls := 0
	err := db.InTx(context.Background(), func(*sqlite.DB) error {
		calls++
		return sqlite.ErrLocked
	})
	if !errors.Is(err, sqlite.ErrLocked) {
		t.Errorf("want ErrLocked, got %v", err)
	}
	if calls != 2 {
		t.Errorf("callback ran %d times, want 2", calls)
	}
}

// --- exists -----------------------------------------------------------

func TestExistsQueries(t *testing.T) {
	drv := &entDriver{}
	db := sqlite.New(drv)
	ctx := context.Background()

	// Empty result set → false, and the right SQL/args are issued.
	ok, err := sqlite.TableExists(ctx, db, "users")
	if err != nil || ok {
		t.Errorf("TableExists empty: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(drv.queries[0], "sqlite_master") || drv.args[0][0] != "users" {
		t.Errorf("TableExists SQL/args: %s %v", drv.queries[0], drv.args[0])
	}

	// A row present → true.
	drv2 := &entDriver{rows: &entRows{cols: []string{"1"}, data: [][]any{{int64(1)}}}}
	db2 := sqlite.New(drv2)
	ok, _ = sqlite.ColumnExists(ctx, db2, "users", "id")
	if !ok {
		t.Error("ColumnExists should be true when a row is returned")
	}
	if !strings.Contains(drv2.queries[0], "pragma_table_info") {
		t.Errorf("ColumnExists SQL: %s", drv2.queries[0])
	}
}

// --- jsonpath ---------------------------------------------------------

func TestJSONPath(t *testing.T) {
	users := sqlite.NewTable("users")
	meta := sqlite.Add(users, sqlite.JSON("meta"))
	_ = meta

	beta := sqlite.JSONField[bool](users.Col("meta"), "settings", "beta")
	sql, args := sqlite.ToSQL(beta.Eq(true))
	want := `(json_extract("users"."meta", ?) = ?)`
	if sql != want {
		t.Errorf("jsonpath eq:\n  got:  %s\n  want: %s", sql, want)
	}
	if args[0] != `$."settings"."beta"` {
		t.Errorf("path arg: got %v", args[0])
	}

	has := sqlite.JSONHasKey(users.Col("meta"), "settings")
	hsql, _ := sqlite.ToSQL(has)
	if hsql != `(json_type("users"."meta", ?) IS NOT NULL)` {
		t.Errorf("hasKey: %s", hsql)
	}
}

// --- patch ------------------------------------------------------------

type acct struct {
	ID      int64 `drop:"id"`
	Balance int64 `drop:"balance"`
}

func TestPatch(t *testing.T) {
	tbl := sqlite.NewTable("accts")
	sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
	balance := sqlite.Add(tbl, sqlite.BigInt("balance").NotNull())
	ent := sqlite.NewEntity[acct](tbl)

	drv := &entDriver{}
	db := sqlite.New(drv)
	_, err := ent.Patch(db, context.Background(), int64(7),
		sqlite.Inc(balance, 5),
		sqlite.SetIfGreater(balance, 100),
	)
	if err != nil {
		t.Fatal(err)
	}
	q := drv.queries[0]
	for _, want := range []string{
		`UPDATE "accts" SET`,
		`"balance" = "accts"."balance" + ?`,
		`"balance" = max("accts"."balance", ?)`,
		`WHERE ("accts"."id" = ?)`,
	} {
		if !strings.Contains(q, want) {
			t.Errorf("patch SQL missing %q in:\n%s", want, q)
		}
	}
}

// [number] admits the unsigned types, so building Dec as Inc(-delta)
// wraps: the rendered statement is a perfect addition of an enormous
// number, and the counter climbs instead of falling.
func TestPatchDecSubtracts(t *testing.T) {
	tbl := sqlite.NewTable("seatmaps")
	sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
	seats := sqlite.Add(tbl, sqlite.Custom[uint32]("seats", "INTEGER").NotNull())
	ent := sqlite.NewEntity[struct {
		ID    int64  `drop:"id"`
		Seats uint32 `drop:"seats"`
	}](tbl)

	drv := &entDriver{}
	db := sqlite.New(drv)
	if _, err := ent.Patch(db, context.Background(), int64(1), sqlite.Dec(seats, uint32(5))); err != nil {
		t.Fatal(err)
	}
	if want := `"seats" = "seatmaps"."seats" - ?`; !strings.Contains(drv.queries[0], want) {
		t.Errorf("Dec must render a subtraction, missing %q in:\n%s", want, drv.queries[0])
	}
	if got := drv.args[0][0]; got != uint32(5) {
		t.Errorf("Dec bound %v, want the delta itself", got)
	}
}

// --- tenant -----------------------------------------------------------

type tRow struct {
	ID       int64  `drop:"id"`
	TenantID int64  `drop:"tenantId"`
	Name     string `drop:"name"`
}

func tenantEntity() (*sqlite.Entity[tRow], *sqlite.Table) {
	tbl := sqlite.NewTable("things")
	sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
	tenant := sqlite.Add(tbl, sqlite.BigInt("tenantId").NotNull())
	sqlite.Add(tbl, sqlite.Text("name").NotNull())
	ent := sqlite.NewEntity[tRow](tbl).ScopeByTenant(tenant)
	return ent, tbl
}

func TestTenantScopeMissing(t *testing.T) {
	ent, _ := tenantEntity()
	db := sqlite.New(&entDriver{})
	if _, err := ent.Get(db, context.Background(), int64(1)); !errors.Is(err, sqlite.ErrTenantMissing) {
		t.Errorf("want ErrTenantMissing, got %v", err)
	}
}

func TestTenantScopeApplied(t *testing.T) {
	ent, _ := tenantEntity()
	drv := &entDriver{}
	db := sqlite.New(drv)
	ctx := sqlite.WithTenant(context.Background(), int64(42))
	_, _ = ent.Get(db, ctx, int64(1))
	q := drv.queries[0]
	if !strings.Contains(q, `("things"."tenantId" = ?)`) {
		t.Errorf("tenant predicate absent:\n%s", q)
	}
	// tenant value is the second bound arg (PK first).
	last := drv.args[0]
	if last[len(last)-1] != int64(42) {
		t.Errorf("tenant arg: got %v", last)
	}
}

func TestTenantStampOnCreate(t *testing.T) {
	ent, _ := tenantEntity()
	drv := &entDriver{}
	db := sqlite.New(drv)
	ctx := sqlite.WithTenant(context.Background(), int64(42))
	r := tRow{ID: 1, Name: "x"} // TenantID zero → stamped
	if err := ent.Create(db, ctx, &r); err != nil {
		t.Fatal(err)
	}
	if r.TenantID != 42 {
		t.Errorf("tenant not stamped: %+v", r)
	}
	// A row carrying a conflicting tenant is rejected.
	bad := tRow{ID: 2, TenantID: 99, Name: "y"}
	if err := ent.Create(db, ctx, &bad); !errors.Is(err, sqlite.ErrTenantMismatch) {
		t.Errorf("want ErrTenantMismatch, got %v", err)
	}
}

// --- page -------------------------------------------------------------

func TestPageFirstPageSQL(t *testing.T) {
	users := entSchema()
	ent := sqlite.NewEntity[entUser](users)
	drv := &entDriver{}
	db := sqlite.New(drv)

	_, err := ent.Page(db).
		OrderBy(sqlite.Asc(users.Col("id"))).
		Limit(10).
		All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	q := drv.queries[0]
	if !strings.Contains(q, `ORDER BY "users"."id" ASC`) {
		t.Errorf("missing ORDER BY:\n%s", q)
	}
	// limit is fetched as limit+1 to detect HasMore.
	if drv.args[0][0] != int64(11) {
		t.Errorf("limit arg: got %v want 11", drv.args[0][0])
	}
}

func TestPageCursorRoundTrip(t *testing.T) {
	users := entSchema()
	ent := sqlite.NewEntity[entUser](users)
	drv := &entDriver{}
	db := sqlite.New(drv)

	// Second page with an After cursor emits the keyset guard.
	p := ent.Page(db).OrderBy(sqlite.Asc(users.Col("id")))
	// Build a valid cursor by paginating a one-row result set first.
	drv.rows = &entRows{
		cols: []string{"id", "name"},
		data: [][]any{{int64(1), "a"}, {int64(2), "b"}},
	}
	pg, err := p.Limit(1).All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !pg.HasMore || pg.NextCursor == "" {
		t.Fatalf("expected HasMore with cursor, got %+v", pg)
	}
	drv2 := &entDriver{}
	db2 := sqlite.New(drv2)
	_, err = ent.Page(db2).
		OrderBy(sqlite.Asc(users.Col("id"))).
		After(pg.NextCursor).
		All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The guard is keysetWhere, the same one SelectBuilder.AfterCursor
	// builds, so a single ordering column renders as a plain strict
	// comparison rather than a one-element row comparison.
	if !strings.Contains(drv2.queries[0], `("users"."id" > ?)`) {
		t.Errorf("keyset guard absent:\n%s", drv2.queries[0])
	}
}
