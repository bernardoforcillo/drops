package sqlite_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/sqlite"
)

// --- fake driver (renamed to avoid clashing with sqlite_test.go) ------

type mgRows struct {
	cols []string
	data [][]any
	pos  int
}

func (r *mgRows) Next() bool {
	if r.pos >= len(r.data) {
		return false
	}
	r.pos++
	return true
}

func (r *mgRows) Scan(dest ...any) error {
	row := r.data[r.pos-1]
	for i, d := range dest {
		switch p := d.(type) {
		case *int64:
			*p = row[i].(int64)
		case *string:
			*p = row[i].(string)
		}
	}
	return nil
}
func (r *mgRows) Columns() ([]string, error) { return r.cols, nil }
func (r *mgRows) Close() error               { return nil }
func (r *mgRows) Err() error                 { return nil }

type mgResult struct{}

func (mgResult) RowsAffected() (int64, error) { return 1, nil }

type mgDriver struct {
	queries []string
	args    [][]any
	rows    *mgRows
}

func (f *mgDriver) Exec(_ context.Context, sql string, args ...any) (drops.Result, error) {
	f.queries = append(f.queries, sql)
	f.args = append(f.args, args)
	return mgResult{}, nil
}
func (f *mgDriver) Query(_ context.Context, sql string, args ...any) (drops.Rows, error) {
	f.queries = append(f.queries, sql)
	f.args = append(f.args, args)
	if f.rows != nil {
		return f.rows, nil
	}
	return &mgRows{}, nil
}
func (f *mgDriver) Begin(_ context.Context) (drops.Tx, error) { return &mgTx{f}, nil }

type mgTx struct{ *mgDriver }

func (*mgTx) Commit(_ context.Context) error   { return nil }
func (*mgTx) Rollback(_ context.Context) error { return nil }

// containsQuery reports whether any recorded query contains sub.
func containsQuery(qs []string, sub string) bool {
	for _, q := range qs {
		if strings.Contains(q, sub) {
			return true
		}
	}
	return false
}

// --- (a) Up runs pending SQL migrations and records history ----------

func TestMigratorUpRunsAndRecords(t *testing.T) {
	drv := &mgDriver{}
	db := sqlite.New(drv)
	m := sqlite.NewMigrator(db).
		AddSQL("0001", "create_widgets",
			`CREATE TABLE "widgets" ("id" INTEGER PRIMARY KEY)`,
			`DROP TABLE "widgets"`)

	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}

	// History table created.
	if !containsQuery(drv.queries, `CREATE TABLE IF NOT EXISTS "_drops_sqlite_migrations"`) {
		t.Errorf("history table not created; queries: %v", drv.queries)
	}
	// The migration body ran.
	if !containsQuery(drv.queries, `CREATE TABLE "widgets"`) {
		t.Errorf("migration up SQL did not run; queries: %v", drv.queries)
	}
	// History row inserted with the version arg.
	if !containsQuery(drv.queries, `INSERT INTO "_drops_sqlite_migrations"`) {
		t.Errorf("history row not inserted; queries: %v", drv.queries)
	}
	found := false
	for _, a := range drv.args {
		if len(a) == 2 && a[0] == "0001" && a[1] == "create_widgets" {
			found = true
		}
	}
	if !found {
		t.Errorf("history insert args missing; args: %v", drv.args)
	}
}

// --- (b) Before/AfterEach data hooks fire with the right direction ---

func TestMigratorHooksDirection(t *testing.T) {
	drv := &mgDriver{}
	db := sqlite.New(drv)

	var beforeDir, afterDir sqlite.MigrationDirection
	var beforeVersion string
	beforeSeen, afterSeen := false, false

	m := sqlite.NewMigrator(db).
		BeforeEach(func(_ context.Context, tx *sqlite.DB, mig sqlite.Migration, dir sqlite.MigrationDirection) error {
			beforeSeen = true
			beforeDir = dir
			beforeVersion = mig.Version
			return nil
		}).
		AfterEach(func(_ context.Context, tx *sqlite.DB, mig sqlite.Migration, dir sqlite.MigrationDirection) error {
			afterSeen = true
			afterDir = dir
			return nil
		}).
		AddSQL("0001", "seed", `INSERT INTO "t" DEFAULT VALUES`, "")

	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if !beforeSeen || !afterSeen {
		t.Fatalf("hooks did not fire: before=%v after=%v", beforeSeen, afterSeen)
	}
	if beforeDir != sqlite.DirectionUp || afterDir != sqlite.DirectionUp {
		t.Errorf("wrong direction: before=%s after=%s (want up)", beforeDir, afterDir)
	}
	if beforeDir.String() != "up" {
		t.Errorf("DirectionUp.String() = %q, want up", beforeDir.String())
	}
	if beforeVersion != "0001" {
		t.Errorf("hook migration version = %q, want 0001", beforeVersion)
	}
}

// --- schema helpers ---------------------------------------------------

func mgUsersOrders() *sqlite.Schema {
	users := sqlite.NewTable("users")
	uTenant := sqlite.Add(users, sqlite.BigInt("tenantId").NotNull())
	uID := sqlite.Add(users, sqlite.BigInt("id").NotNull())
	sqlite.Add(users, sqlite.Text("email").NotNull().Unique())
	users.PrimaryKey(uTenant, uID)

	orders := sqlite.NewTable("orders")
	sqlite.Add(orders, sqlite.Integer("id").PrimaryKey().AutoIncrement())
	oTenant := sqlite.Add(orders, sqlite.BigInt("tenantId").NotNull())
	oUser := sqlite.Add(orders, sqlite.BigInt("userId").NotNull())
	sqlite.Add(orders, sqlite.Real("amount").NotNull().Default("0"))
	orders.AddCheck("orders_amount_nonneg", "amount >= 0")
	orders.ForeignKeyN(
		[]sqlite.ColRef{oTenant, oUser}, users,
		[]sqlite.ColRef{uTenant, uID}, sqlite.OnDelete("CASCADE"))

	return sqlite.NewSchema(users, orders)
}

// --- (c) Diff from empty emits CREATE TABLE with inline constraints --

func TestDiffFromEmptyCreatesInlineConstraints(t *testing.T) {
	cur := sqlite.BuildSnapshot(mgUsersOrders())
	stmts := sqlite.Diff(sqlite.EmptySnapshot(), cur)

	joined := strings.Join(stmts, "\n")
	for _, want := range []string{
		`CREATE TABLE "users"`,
		`PRIMARY KEY ("tenantId", "id")`,
		`"email" TEXT NOT NULL UNIQUE`,
		`CREATE TABLE "orders"`,
		`"id" INTEGER PRIMARY KEY AUTOINCREMENT`,
		`CONSTRAINT "orders_amount_nonneg" CHECK (amount >= 0)`,
		`FOREIGN KEY ("tenantId", "userId") REFERENCES "users" ("tenantId", "id") ON DELETE CASCADE`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in diff:\n%s", want, joined)
		}
	}
	// No ALTER TABLE ADD CONSTRAINT — SQLite constraints must be inline.
	if strings.Contains(joined, "ADD CONSTRAINT") {
		t.Errorf("diff must not emit ADD CONSTRAINT:\n%s", joined)
	}
}

// --- (d) Diff add-column emits ALTER TABLE ADD COLUMN ----------------

func TestDiffAddColumn(t *testing.T) {
	prevTbl := sqlite.NewTable("items")
	sqlite.Add(prevTbl, sqlite.Integer("id").PrimaryKey())
	sqlite.Add(prevTbl, sqlite.Text("name").NotNull())
	prev := sqlite.BuildSnapshot(sqlite.NewSchema(prevTbl))

	curTbl := sqlite.NewTable("items")
	sqlite.Add(curTbl, sqlite.Integer("id").PrimaryKey())
	sqlite.Add(curTbl, sqlite.Text("name").NotNull())
	sqlite.Add(curTbl, sqlite.Text("nickname")) // nullable → ALTER-addable
	cur := sqlite.BuildSnapshot(sqlite.NewSchema(curTbl))

	stmts := sqlite.Diff(prev, cur)
	if len(stmts) != 1 {
		t.Fatalf("expected 1 statement, got %d: %v", len(stmts), stmts)
	}
	want := `ALTER TABLE "items" ADD COLUMN "nickname" TEXT;`
	if stmts[0] != want {
		t.Errorf("add column:\n  got:  %s\n  want: %s", stmts[0], want)
	}
}

// --- (e) Diff of a type change emits the table-rebuild sequence ------

func TestDiffTypeChangeRebuild(t *testing.T) {
	prevTbl := sqlite.NewTable("metrics")
	sqlite.Add(prevTbl, sqlite.Integer("id").PrimaryKey())
	sqlite.Add(prevTbl, sqlite.Integer("score").NotNull())
	prev := sqlite.BuildSnapshot(sqlite.NewSchema(prevTbl))

	curTbl := sqlite.NewTable("metrics")
	sqlite.Add(curTbl, sqlite.Integer("id").PrimaryKey())
	sqlite.Add(curTbl, sqlite.Real("score").NotNull()) // INTEGER -> REAL
	cur := sqlite.BuildSnapshot(sqlite.NewSchema(curTbl))

	stmts := sqlite.Diff(prev, cur)
	joined := strings.Join(stmts, "\n")

	for _, want := range []string{
		`-- rebuild "metrics":`,
		`CREATE TABLE "metrics_new"`,
		`INSERT INTO "metrics_new" ("id", "score") SELECT "id", "score" FROM "metrics";`,
		`DROP TABLE "metrics";`,
		`ALTER TABLE "metrics_new" RENAME TO "metrics";`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in rebuild:\n%s", want, joined)
		}
	}
	// The new table must carry the changed type.
	if !strings.Contains(joined, `"score" REAL NOT NULL`) {
		t.Errorf("rebuilt table missing new column type:\n%s", joined)
	}
	// Must not attempt an in-place column alter.
	if strings.Contains(joined, "ALTER COLUMN") {
		t.Errorf("SQLite cannot ALTER COLUMN; rebuild expected:\n%s", joined)
	}
}
