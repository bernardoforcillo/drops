package integration_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/mysql"
	"github.com/bernardoforcillo/drops/stdlib"
)

func openMySQL(t *testing.T) *mysql.DB {
	t.Helper()
	dsn := integration.DSN(t, integration.EnvMySQL)
	sqlDB, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := sqlDB.PingContext(context.Background()); err != nil {
		t.Fatalf("ping %s: %v", dsn, err)
	}
	return mysql.New(stdlib.New(sqlDB))
}

func execMySQL(t *testing.T, db *mysql.DB, e drops.Expression) {
	t.Helper()
	if _, err := db.ExecExpr(context.Background(), e); err != nil {
		text, args := drops.StringWithDialect(mysql.Dialect, e)
		t.Fatalf("MySQL rejected the statement: %v\n%s\nargs: %v", err, text, args)
	}
}

func dropMySQL(t *testing.T, db *mysql.DB, tbl *mysql.Table) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = db.ExecExpr(context.Background(), mysql.DropTableIfExists(tbl))
	})
}

func TestMySQLEveryColumnTypeIsAccepted(t *testing.T) {
	db := openMySQL(t)
	tbl := mysql.NewTable(integration.UniqueName(t, "kitchen"))
	dropMySQL(t, db, tbl)

	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(tbl, mysql.Varchar("a_varchar", 255))
	mysql.Add(tbl, mysql.Char("a_char", 8))
	mysql.Add(tbl, mysql.Text("a_text"))
	mysql.Add(tbl, mysql.LongText("a_longtext"))
	mysql.Add(tbl, mysql.TinyInt("a_tinyint"))
	mysql.Add(tbl, mysql.SmallInt("a_smallint"))
	mysql.Add(tbl, mysql.Integer("a_int"))
	mysql.Add(tbl, mysql.BigInt("a_bigint"))
	mysql.Add(tbl, mysql.UnsignedInt("a_uint"))
	mysql.Add(tbl, mysql.UnsignedBigInt("a_ubigint"))
	mysql.Add(tbl, mysql.Real("a_float"))
	mysql.Add(tbl, mysql.DoublePrecision("a_double"))
	mysql.Add(tbl, mysql.Numeric("a_decimal", 10, 2))
	mysql.Add(tbl, mysql.Boolean("a_bool"))
	mysql.Add(tbl, mysql.Date("a_date"))
	mysql.Add(tbl, mysql.Time("a_time"))
	mysql.Add(tbl, mysql.Timestamp("a_datetime", false))
	mysql.Add(tbl, mysql.Timestamp("a_timestamp", true))
	mysql.Add(tbl, mysql.UUID("a_uuid"))
	mysql.Add(tbl, mysql.JSON("a_json"))
	mysql.Add(tbl, mysql.Blob("a_blob"))
	mysql.Add(tbl, mysql.LongBlob("a_longblob"))
	mysql.Add(tbl, mysql.Enum("a_enum", "draft", "live"))
	mysql.Add(tbl, mysql.Custom[string]("a_custom", "BINARY(16)"))

	execMySQL(t, db, mysql.CreateTable(tbl))
}

// The generated key comes back through LastInsertId, since MySQL has
// no RETURNING. That is a claim about the driver as much as about
// drops, and only a real driver can settle it.
func TestMySQLCreateReadsBackTheGeneratedKey(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl := mysql.NewTable(integration.UniqueName(t, "users"))
	dropMySQL(t, db, tbl)

	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	name := mysql.Add(tbl, mysql.Varchar("name", 255).NotNull())
	execMySQL(t, db, mysql.CreateTable(tbl))

	type user struct {
		ID   int64
		Name string
	}
	ent := mysql.NewEntity[user](tbl)

	u := user{Name: "Ada"}
	if err := ent.Create(db, ctx, &u); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if u.ID == 0 {
		t.Fatal("no generated key came back; LastInsertId is not reaching the row")
	}

	got, err := ent.Get(db, ctx, u.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "Ada" {
		t.Errorf("got %+v", got)
	}

	// A second insert must get a different key, or the first was a
	// coincidence.
	v := user{Name: "Grace"}
	if err := ent.Create(db, ctx, &v); err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if v.ID == u.ID {
		t.Errorf("both rows report key %d", u.ID)
	}
	_ = name
}

// ON DUPLICATE KEY UPDATE, and the fact that it fires on ANY unique
// index rather than a named conflict target — the divergence from
// PostgreSQL the package doc warns about.
func TestMySQLUpsertFiresOnAnyUniqueIndex(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl := mysql.NewTable(integration.UniqueName(t, "accounts"))
	dropMySQL(t, db, tbl)

	id := mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	email := mysql.Add(tbl, mysql.Varchar("email", 191).NotNull().Unique())
	label := mysql.Add(tbl, mysql.Varchar("label", 255).NotNull())
	execMySQL(t, db, mysql.CreateTable(tbl))

	ins := func(pk int64, mail, lab string) error {
		_, err := db.Insert(tbl).
			Row(id.Val(pk), email.Val(mail), label.Val(lab)).
			OnDuplicateKeyUpdateAll().
			Exec(ctx)
		return err
	}
	if err := ins(1, "a@example.com", "first"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Different primary key, same unique email: MySQL treats this as a
	// duplicate and updates, where PostgreSQL's ON CONFLICT (id) would
	// have raised.
	if err := ins(2, "a@example.com", "second"); err != nil {
		t.Fatalf("upsert on the unique column: %v", err)
	}

	var rows []struct {
		ID    int64
		Email string
		Label string
	}
	if err := db.Select(id, email, label).From(tbl).All(ctx, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected the upsert to collapse into one row, got %d: %+v", len(rows), rows)
	}
	if rows[0].Label != "second" {
		t.Errorf("the update did not apply: %+v", rows[0])
	}
}

// MySQL cannot index a TEXT column without a prefix length. The
// builder can express one; this checks the server agrees, and that the
// unprefixed form really is rejected — which is why Prefix exists.
func TestMySQLTextIndexNeedsAPrefix(t *testing.T) {
	db := openMySQL(t)
	tbl := mysql.NewTable(integration.UniqueName(t, "docs"))
	dropMySQL(t, db, tbl)

	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	body := mysql.Add(tbl, mysql.Text("body"))
	execMySQL(t, db, mysql.CreateTable(tbl))

	ctx := context.Background()
	unprefixed := mysql.NewIndex(integration.UniqueName(t, "np"), tbl, body)
	if _, err := db.ExecExpr(ctx, unprefixed); err == nil {
		t.Error("MySQL accepted a TEXT index with no prefix; Prefix would then be unnecessary")
	} else if !strings.Contains(strings.ToLower(err.Error()), "key") {
		t.Logf("rejected, though not with the expected message: %v", err)
	}

	execMySQL(t, db, mysql.NewIndex(integration.UniqueName(t, "p"), tbl, body).Prefix(body, 64))
}

func TestMySQLCompositePrimaryKeyIsAccepted(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl := mysql.NewTable(integration.UniqueName(t, "memberships"))
	dropMySQL(t, db, tbl)

	mysql.Add(tbl, mysql.BigInt("orgId").PrimaryKey())
	mysql.Add(tbl, mysql.BigInt("userId").PrimaryKey())
	mysql.Add(tbl, mysql.Varchar("role", 32).NotNull())
	execMySQL(t, db, mysql.CreateTable(tbl))

	type membership struct {
		OrgID  int64 `drop:"orgId"`
		UserID int64 `drop:"userId"`
		Role   string
	}
	ent := mysql.NewEntity[membership](tbl)

	m := membership{OrgID: 1, UserID: 2, Role: "admin"}
	if err := ent.Create(db, ctx, &m); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := ent.Get(db, ctx, int64(1), int64(2))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Role != "admin" {
		t.Errorf("got %+v", got)
	}
	if _, err := ent.Delete(db, ctx, int64(1), int64(2)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

// ORDER BY and LIMIT on UPDATE and DELETE are MySQL extensions the
// builder emits; a server has to accept the clause order.
func TestMySQLBatchedUpdateAndDelete(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl := mysql.NewTable(integration.UniqueName(t, "queue"))
	dropMySQL(t, db, tbl)

	id := mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	done := mysql.Add(tbl, mysql.Boolean("done").NotNull().Default("0"))
	execMySQL(t, db, mysql.CreateTable(tbl))

	for i := 0; i < 5; i++ {
		if _, err := db.Insert(tbl).Row(done.Val(false)).Exec(ctx); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if _, err := db.Update(tbl).Set(done.Val(true)).OrderBy(id.Asc()).Limit(2).Exec(ctx); err != nil {
		t.Fatalf("batched UPDATE: %v", err)
	}
	if _, err := db.Delete(tbl).Where(done.Eq(true)).OrderBy(id.Asc()).Limit(1).Exec(ctx); err != nil {
		t.Fatalf("batched DELETE: %v", err)
	}

	var n int64
	if err := db.Select(mysql.CountAll()).From(tbl).One(ctx, &n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("after updating 2 and deleting 1 of them, %d rows remain, want 4", n)
	}
}
