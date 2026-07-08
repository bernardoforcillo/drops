package sqlite_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
	"github.com/bernardoforcillo/drops/sqlite"
)

// --- fake driver ------------------------------------------------------

type fakeRows struct {
	cols []string
	data [][]any
	pos  int
}

func (r *fakeRows) Next() bool {
	if r.pos >= len(r.data) {
		return false
	}
	r.pos++
	return true
}

func (r *fakeRows) Scan(dest ...any) error {
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
func (r *fakeRows) Columns() ([]string, error) { return r.cols, nil }
func (r *fakeRows) Close() error               { return nil }
func (r *fakeRows) Err() error                 { return nil }

type fakeResult struct{}

func (fakeResult) RowsAffected() (int64, error) { return 1, nil }

type fakeDriver struct {
	queries []string
	args    [][]any
	rows    *fakeRows
}

func (f *fakeDriver) Exec(_ context.Context, sql string, args ...any) (drops.Result, error) {
	f.queries = append(f.queries, sql)
	f.args = append(f.args, args)
	return fakeResult{}, nil
}
func (f *fakeDriver) Query(_ context.Context, sql string, args ...any) (drops.Rows, error) {
	f.queries = append(f.queries, sql)
	f.args = append(f.args, args)
	if f.rows != nil {
		return f.rows, nil
	}
	return &fakeRows{}, nil
}
func (f *fakeDriver) Begin(_ context.Context) (drops.Tx, error) { return &fakeTx{f}, nil }

type fakeTx struct{ *fakeDriver }

func (*fakeTx) Commit(_ context.Context) error   { return nil }
func (*fakeTx) Rollback(_ context.Context) error { return nil }

// --- schema fixtures --------------------------------------------------

func mkSchema() (users, orders *sqlite.Table) {
	users = sqlite.NewTable("users")
	uTenant := sqlite.Add(users, sqlite.BigInt("tenantId").NotNull())
	uID := sqlite.Add(users, sqlite.BigInt("id").NotNull())
	sqlite.Add(users, sqlite.Text("email").NotNull().Unique())
	users.PrimaryKey(uTenant, uID)

	orders = sqlite.NewTable("orders")
	sqlite.Add(orders, sqlite.Integer("id").PrimaryKey().AutoIncrement())
	oTenant := sqlite.Add(orders, sqlite.BigInt("tenantId").NotNull())
	oUser := sqlite.Add(orders, sqlite.BigInt("userId").NotNull())
	sqlite.Add(orders, sqlite.Real("amount").NotNull().Default("0"))
	orders.AddCheck("orders_amount_nonneg", "amount >= 0")
	orders.ForeignKeyN(
		[]sqlite.ColRef{oTenant, oUser}, users,
		[]sqlite.ColRef{uTenant, uID}, sqlite.OnDelete("CASCADE"))
	return users, orders
}

// --- DDL --------------------------------------------------------------

func TestCreateTableCompositePK(t *testing.T) {
	users, _ := mkSchema()
	sql, _ := sqlite.ToSQL(sqlite.CreateTable(users))
	want := `CREATE TABLE "users" (
  "tenantId" INTEGER NOT NULL,
  "id" INTEGER NOT NULL,
  "email" TEXT NOT NULL UNIQUE,
  PRIMARY KEY ("tenantId", "id")
)`
	if sql != want {
		t.Errorf("users DDL:\n--- got ---\n%s\n--- want ---\n%s", sql, want)
	}
}

func TestCreateTableInlineCompositeFK(t *testing.T) {
	_, orders := mkSchema()
	sql, _ := sqlite.ToSQL(sqlite.CreateTableIfNotExists(orders))

	// Composite FK must be INLINE (SQLite has no ALTER ADD CONSTRAINT).
	fk := `CONSTRAINT "ordersTenantIdUserIdUsersTenantIdIdFk" FOREIGN KEY ("tenantId", "userId") REFERENCES "users" ("tenantId", "id") ON DELETE CASCADE`
	if !strings.Contains(sql, fk) {
		t.Errorf("missing inline composite FK.\n  want substring: %s\n  got:\n%s", fk, sql)
	}
	for _, want := range []string{
		`CREATE TABLE IF NOT EXISTS "orders"`,
		`"id" INTEGER PRIMARY KEY AUTOINCREMENT`,
		`"amount" REAL NOT NULL DEFAULT 0`,
		`CONSTRAINT "orders_amount_nonneg" CHECK (amount >= 0)`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing %q in:\n%s", want, sql)
		}
	}
}

func TestCreateIndexAndDrop(t *testing.T) {
	users, _ := mkSchema()
	idxSQL, _ := sqlite.ToSQL(sqlite.CreateIndexIfNotExists("idx_users_email", users, users.Col("email")))
	if idxSQL != `CREATE INDEX IF NOT EXISTS "idx_users_email" ON "users" ("email")` {
		t.Errorf("create index: %s", idxSQL)
	}
	dropSQL, _ := sqlite.ToSQL(sqlite.DropIndexIfExists("idx_users_email"))
	if dropSQL != `DROP INDEX IF EXISTS "idx_users_email"` {
		t.Errorf("drop index: %s", dropSQL)
	}
}

// --- CRUD & placeholders ---------------------------------------------

func TestSelectUsesQuestionMarkPlaceholders(t *testing.T) {
	users := sqlite.NewTable("users")
	id := sqlite.Add(users, sqlite.BigInt("id").PrimaryKey())
	name := sqlite.Add(users, sqlite.Text("name").NotNull())

	db := sqlite.New(&fakeDriver{})
	sql, args := db.Select(id, name).From(users).
		Where(id.Gt(10), name.Ne("bob")).
		OrderBy(name).Limit(5).ToSQL()

	want := `SELECT "users"."id", "users"."name" FROM "users" WHERE ("users"."id" > ?) AND ("users"."name" <> ?) ORDER BY "users"."name" LIMIT ?`
	if sql != want {
		t.Errorf("select sql:\n  got:  %s\n  want: %s", sql, want)
	}
	if len(args) != 3 {
		t.Errorf("args: got %d want 3 (%v)", len(args), args)
	}
}

func TestInsertUpdateDelete(t *testing.T) {
	users := sqlite.NewTable("users")
	id := sqlite.Add(users, sqlite.BigInt("id").PrimaryKey())
	name := sqlite.Add(users, sqlite.Text("name").NotNull())
	db := sqlite.New(&fakeDriver{})

	ins, _ := db.Insert(users).Values(id.Val(1), name.Val("alice")).ToSQL()
	if ins != `INSERT INTO "users" ("id", "name") VALUES (?, ?)` {
		t.Errorf("insert: %s", ins)
	}
	upd, _ := db.Update(users).Set(name.Val("bob")).Where(id.Eq(1)).ToSQL()
	if upd != `UPDATE "users" SET "name" = ? WHERE ("users"."id" = ?)` {
		t.Errorf("update: %s", upd)
	}
	del, _ := db.Delete(users).Where(id.Eq(1)).ToSQL()
	if del != `DELETE FROM "users" WHERE ("users"."id" = ?)` {
		t.Errorf("delete: %s", del)
	}
}

// TestDialectSwap verifies the SAME expression renders with pg's $N and
// sqlite's ? — the swappable-connector contract.
func TestDialectSwap(t *testing.T) {
	users := pg.NewTable("users")
	id := pg.Add(users, pg.BigInt("id").PrimaryKey())
	pred := id.Gt(10)

	pgSQL, _ := drops.StringWithDialect(pg.Dialect, pred)
	sqliteSQL, _ := drops.StringWithDialect(sqlite.Dialect, pred)

	if !strings.Contains(pgSQL, "$1") {
		t.Errorf("pg dialect should use $1, got %s", pgSQL)
	}
	if !strings.Contains(sqliteSQL, "?") || strings.Contains(sqliteSQL, "$1") {
		t.Errorf("sqlite dialect should use ?, got %s", sqliteSQL)
	}
}

func TestSelectAllScans(t *testing.T) {
	users := sqlite.NewTable("users")
	id := sqlite.Add(users, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(users, sqlite.Text("name").NotNull())

	drv := &fakeDriver{rows: &fakeRows{
		cols: []string{"id", "name"},
		data: [][]any{{int64(1), "alice"}, {int64(2), "bob"}},
	}}
	db := sqlite.New(drv)

	type user struct {
		ID   int64  `drop:"id"`
		Name string `drop:"name"`
	}
	var out []user
	if err := db.Select(id).From(users).All(context.Background(), &out); err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(out) != 2 || out[0].Name != "alice" || out[1].ID != 2 {
		t.Errorf("scanned rows wrong: %+v", out)
	}
}
