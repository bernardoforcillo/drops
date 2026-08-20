package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"

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

// A secondary index over a TEXT column: MySQL refuses one without a
// prefix length (error 1170), MariaDB takes it and silently narrows the
// index to the longest prefix its engine allows. The two servers do not
// agree, so the test pins what each one did rather than one server's
// answer — and either way an index over the *whole* value is what
// nobody offers, which is what Prefix exists to say out loud.
func TestMySQLTextIndexNeedsAPrefix(t *testing.T) {
	db := openMySQL(t)
	tbl := mysql.NewTable(integration.UniqueName(t, "docs"))
	dropMySQL(t, db, tbl)

	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	body := mysql.Add(tbl, mysql.Text("body"))
	execMySQL(t, db, mysql.CreateTable(tbl))

	ctx := context.Background()
	npName := integration.UniqueName(t, "np")
	if _, err := db.ExecExpr(ctx, mysql.NewIndex(npName, tbl, body)); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "key length") {
			t.Errorf("rejected, but not for the missing key length: %v", err)
		}
	} else {
		sub := indexPrefixLength(t, db, tbl.Name(), npName)
		if sub == 0 {
			t.Errorf("index %s covers the whole TEXT value, which no MySQL engine can do", npName)
		}
		t.Logf("this server accepted the unprefixed index and imposed a %d-byte prefix itself", sub)
	}

	execMySQL(t, db, mysql.NewIndex(integration.UniqueName(t, "p"), tbl, body).Prefix(body, 64))
}

// indexPrefixLength reads the prefix an index actually ended up with,
// which is the only way to tell a server that imposed one from a server
// that indexed the whole column.
func indexPrefixLength(t *testing.T, db *mysql.DB, table, index string) int64 {
	t.Helper()
	rows, err := db.Query(context.Background(),
		"SELECT COALESCE(SUB_PART, 0) AS subPart FROM information_schema.STATISTICS"+
			" WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND INDEX_NAME = ?",
		table, index)
	if err != nil {
		t.Fatalf("information_schema: %v", err)
	}
	var out []struct{ SubPart int64 }
	if err := drops.ScanAll(rows, &out); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("index %s on %s is not in information_schema", index, table)
	}
	return out[0].SubPart
}

// A TEXT column in the PRIMARY KEY or a UNIQUE KEY is the one place
// both servers agree: without a prefix length it is error 1170, and
// CreateTable has nowhere but the column to learn the length from.
func TestMySQLTextKeyCarriesItsPrefixLength(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()

	bare := mysql.NewTable(integration.UniqueName(t, "bare"))
	dropMySQL(t, db, bare)
	mysql.Add(bare, mysql.Text("slug").PrimaryKey())
	if _, err := db.ExecExpr(ctx, mysql.CreateTable(bare)); err == nil {
		t.Fatal("the server took a TEXT primary key with no key length; the premise of KeyPrefix is gone")
	} else if !strings.Contains(strings.ToLower(err.Error()), "key length") {
		t.Fatalf("rejected, but not for the missing key length: %v", err)
	}

	tbl := mysql.NewTable(integration.UniqueName(t, "keyed"))
	dropMySQL(t, db, tbl)
	slug := mysql.Add(tbl, mysql.Text("slug").KeyPrefix(64).PrimaryKey())
	mysql.Add(tbl, mysql.Text("body").KeyPrefix(32).Unique())
	execMySQL(t, db, mysql.CreateTable(tbl))

	if _, err := db.Insert(tbl).Row(slug.Val("a-post")).Exec(ctx); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := db.Insert(tbl).Row(slug.Val("a-post")).Exec(ctx); err == nil {
		t.Error("the prefixed primary key did not enforce uniqueness")
	}
}

// Both sides of a self-join have to be addressable at once: the alias
// copy must qualify its columns with the alias, or the join condition
// silently reads from the un-aliased table and the query returns the
// wrong rows without erroring.
func TestMySQLSelfJoinResolvesThroughTheAlias(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl := mysql.NewTable(integration.UniqueName(t, "staff"))
	dropMySQL(t, db, tbl)

	id := mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	name := mysql.Add(tbl, mysql.Varchar("name", 64).NotNull())
	managerID := mysql.Add(tbl, mysql.BigInt("managerId"))
	execMySQL(t, db, mysql.CreateTable(tbl))

	for _, r := range []struct {
		id      int64
		name    string
		manager int64
	}{{1, "Ada", 0}, {2, "Grace", 1}, {3, "Alan", 1}} {
		row := []mysql.ColumnValue{id.Val(r.id), name.Val(r.name)}
		if r.manager != 0 {
			row = append(row, managerID.Val(r.manager))
		}
		if _, err := db.Insert(tbl).Row(row...).Exec(ctx); err != nil {
			t.Fatalf("seed %s: %v", r.name, err)
		}
	}

	mgr := tbl.As("mgr")
	var rows []struct {
		Staff   string `drop:"staff"`
		Manager string `drop:"manager"`
	}
	err := db.Select(name.As("staff"), mgr.Col("name").As("manager")).
		From(tbl).
		Join(mgr, mysql.Eq(managerID, mgr.Col("id"))).
		OrderBy(name.Asc()).
		All(ctx, &rows)
	if err != nil {
		t.Fatalf("self-join: %v", err)
	}
	want := map[string]string{"Alan": "Ada", "Grace": "Ada"}
	if len(rows) != len(want) {
		t.Fatalf("self-join returned %d rows, want %d: %+v", len(rows), len(want), rows)
	}
	for _, r := range rows {
		if want[r.Staff] != r.Manager {
			t.Errorf("%s reports to %q, want %q", r.Staff, r.Manager, want[r.Staff])
		}
	}
}

// The alias has to reach past SELECT. A shallow copy was the disease
// and the self-join was only the first symptom: UPDATE and DELETE
// qualify their predicates through the same column handles, and MariaDB
// answers 1064 to "DELETE FROM t AS a", so the multi-table spelling is
// the one both servers take.
func TestMySQLAliasReachesUpdateAndDelete(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl := mysql.NewTable(integration.UniqueName(t, "staff"))
	dropMySQL(t, db, tbl)

	id := mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	name := mysql.Add(tbl, mysql.Varchar("name", 64).NotNull())
	execMySQL(t, db, mysql.CreateTable(tbl))

	for _, r := range []struct {
		id   int64
		name string
	}{{1, "Ada"}, {2, "Grace"}, {3, "Alan"}} {
		if _, err := db.Insert(tbl).Row(id.Val(r.id), name.Val(r.name)).Exec(ctx); err != nil {
			t.Fatalf("seed %s: %v", r.name, err)
		}
	}

	u := tbl.As("u")
	if _, err := db.Update(u).
		Set(mysql.Bind(u.Col("name"), "Ada L")).
		Where(mysql.Eq(u.Col("id"), int64(1))).
		Exec(ctx); err != nil {
		t.Fatalf("UPDATE through the alias: %v", err)
	}
	if _, err := db.Delete(u).Where(mysql.Eq(u.Col("id"), int64(3))).Exec(ctx); err != nil {
		t.Fatalf("DELETE through the alias: %v", err)
	}

	var rows []struct {
		ID   int64  `drop:"id"`
		Name string `drop:"name"`
	}
	if err := db.Select(id, name).From(tbl).OrderBy(id.Asc()).All(ctx, &rows); err != nil {
		t.Fatal(err)
	}
	want := []struct {
		ID   int64
		Name string
	}{{1, "Ada L"}, {2, "Grace"}}
	if len(rows) != len(want) {
		t.Fatalf("%d rows left, want %d: %+v", len(rows), len(want), rows)
	}
	for i, w := range want {
		if rows[i].ID != w.ID || rows[i].Name != w.Name {
			t.Errorf("row %d = %+v, want %+v", i, rows[i], w)
		}
	}
}

// A self-join has to reach two instances of one table, and the alias
// is what separates them. The failure is not a syntax error the server
// would catch: a predicate built from the un-aliased handles on both
// sides is still valid SQL, it just compares one instance of the table
// to itself and quietly returns the wrong rows.
//
// mysql has no relations, so the edge is spelled at the query site.
func TestMySQLAliasedSelfJoinJoinsTheRightInstance(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl := mysql.NewTable(integration.UniqueName(t, "staff"))
	dropMySQL(t, db, tbl)

	id := mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	name := mysql.Add(tbl, mysql.Varchar("name", 64).NotNull())
	managerID := mysql.Add(tbl, mysql.BigInt("managerId"))
	execMySQL(t, db, mysql.CreateTable(tbl))

	for _, r := range []struct {
		id      int64
		name    string
		manager int64
	}{{1, "Ada", 0}, {2, "Grace", 1}, {3, "Alan", 1}} {
		row := []mysql.ColumnValue{id.Val(r.id), name.Val(r.name)}
		if r.manager != 0 {
			row = append(row, managerID.Val(r.manager))
		}
		if _, err := db.Insert(tbl).Row(row...).Exec(ctx); err != nil {
			t.Fatalf("seed %s: %v", r.name, err)
		}
	}

	// The employee is the aliased instance; the manager is reached
	// through the un-aliased handles. Both ends have to come from the
	// handle that names the instance meant, which is the whole of what
	// the alias buys.
	emp := tbl.As("e")
	var rows []struct {
		Staff   string `drop:"staff"`
		Manager string `drop:"manager"`
	}
	err := db.Select(emp.Col("name").As("staff"), name.As("manager")).
		From(emp).
		Join(tbl, mysql.Eq(emp.Col(managerID.Name()), id)).
		OrderBy(emp.Col("name").Asc()).
		All(ctx, &rows)
	if err != nil {
		t.Fatalf("self join: %v", err)
	}
	want := map[string]string{"Alan": "Ada", "Grace": "Ada"}
	if len(rows) != len(want) {
		t.Fatalf("self join returned %d rows, want %d: %+v", len(rows), len(want), rows)
	}
	for _, r := range rows {
		if want[r.Staff] != r.Manager {
			t.Errorf("%s reports to %q, want %q", r.Staff, r.Manager, want[r.Staff])
		}
	}
}

// A foreign key whose target belongs to no table has no table name to
// render, and the REFERENCES clause the server sees is a syntax error.
// drops refuses the declaration instead of emitting it.
func TestMySQLForeignKeyTargetMustBeRegistered(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()

	parent := mysql.NewTable(integration.UniqueName(t, "parent"))
	dropMySQL(t, db, parent)
	parentID := mysql.Add(parent, mysql.BigInt("id").PrimaryKey())
	execMySQL(t, db, mysql.CreateTable(parent))

	child := mysql.NewTable(integration.UniqueName(t, "child"))
	dropMySQL(t, db, child)
	mysql.Add(child, mysql.BigSerial("id").PrimaryKey())
	childParent := mysql.Add(child, mysql.BigInt("parentId").NotNull().References(parentID))
	execMySQL(t, db, mysql.CreateTable(child))

	if _, err := db.Insert(child).Row(childParent.Val(404)).Exec(ctx); err == nil {
		t.Error("the foreign key is not being enforced; the rest of this test proves nothing")
	}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("References accepted a target that belongs to no table")
		}
		// Any panic would satisfy a bare recover() != nil — a nil map
		// write, a bad index — so name the one this test is about.
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panicked with %T(%v), want the References diagnostic", r, r)
		}
		for _, want := range []string{"danglingId", "belongs to no table"} {
			if !strings.Contains(msg, want) {
				t.Errorf("panic %q does not mention %q", msg, want)
			}
		}
	}()
	mysql.Add(mysql.NewTable(integration.UniqueName(t, "orphan")),
		mysql.BigInt("danglingId").References(mysql.BigInt("id")))
}

// isDuplicateKey reports MySQL error 1062, the duplicate-key
// collision. The number is the driver's, so it reads the same on
// MariaDB and on MySQL where the message text does not.
func isDuplicateKey(err error) bool {
	var me *mysqldriver.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

// OnDuplicateKeyUpdate with an empty assignment list renders no clause
// at all, so the collision still reaches the caller as 1062 and the
// row already in the table keeps its values. The list is the caller's:
// one that came out empty by accident must not be read as consent to
// discard the incoming row. OnDuplicateKeyUpdateAll degrades the other
// way, and TestMySQLUpsertAllWhenEveryColumnIsAKey pins that.
func TestMySQLOnDuplicateKeyUpdateWithNoAssignmentsStillRaises(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl := mysql.NewTable(integration.UniqueName(t, "settings"))
	dropMySQL(t, db, tbl)

	key := mysql.Add(tbl, mysql.BigInt("k").PrimaryKey())
	val := mysql.Add(tbl, mysql.Varchar("v", 32).NotNull())
	execMySQL(t, db, mysql.CreateTable(tbl))

	insert := func(v string) error {
		_, err := db.Insert(tbl).
			Row(key.Val(int64(1)), val.Val(v)).
			OnDuplicateKeyUpdate().
			Exec(ctx)
		return err
	}
	if err := insert("first"); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	err := insert("second")
	if err == nil {
		t.Fatal("the duplicate was swallowed; an empty assignment list must not mean INSERT IGNORE")
	}
	if !isDuplicateKey(err) {
		t.Fatalf("second insert failed with %v, want error 1062", err)
	}

	var got string
	if err := db.Select(val).From(tbl).One(ctx, &got); err != nil {
		t.Fatal(err)
	}
	if got != "first" {
		t.Errorf("stored value = %q, want the row the collision protected", got)
	}
}

// OnDuplicateKeyUpdateAll on a table whose every column is part of the
// key has nothing to assign. Dropping the clause turns the upsert back
// into a plain INSERT, which raises 1062 on the second call.
func TestMySQLUpsertAllWhenEveryColumnIsAKey(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl := mysql.NewTable(integration.UniqueName(t, "members"))
	dropMySQL(t, db, tbl)

	orgID := mysql.Add(tbl, mysql.BigInt("orgId").PrimaryKey())
	userID := mysql.Add(tbl, mysql.BigInt("userId").PrimaryKey())
	execMySQL(t, db, mysql.CreateTable(tbl))

	upsert := func() error {
		_, err := db.Insert(tbl).
			Row(orgID.Val(int64(1)), userID.Val(int64(2))).
			OnDuplicateKeyUpdateAll().
			Exec(ctx)
		return err
	}
	if err := upsert(); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := upsert(); err != nil {
		t.Fatalf("second upsert on the same key: %v", err)
	}

	var n int64
	if err := db.Select(mysql.CountAll()).From(tbl).One(ctx, &n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d rows after upserting the same key twice, want 1", n)
	}
}

// The refusal in ErrAliasedDeleteBounded rests on a claim about the
// server, so ask the server. The statement the builder would have sent
// is rendered here and executed raw: an aliased DELETE has to use the
// multi-table form, and that form takes no ORDER BY and no LIMIT. Once
// that is established, the builder refusing to send it is the right
// answer rather than a guess.
func TestMySQLAliasedDeleteCannotBeBounded(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl := mysql.NewTable(integration.UniqueName(t, "queue"))
	dropMySQL(t, db, tbl)

	id := mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	execMySQL(t, db, mysql.CreateTable(tbl))
	for i := int64(1); i <= 3; i++ {
		if _, err := db.Insert(tbl).Row(id.Val(i)).Exec(ctx); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	u := tbl.As("u")
	bounded := db.Delete(u).Where(mysql.Lt(u.Col("id"), 100)).OrderBy(u.Col("id").Asc()).Limit(1)

	sqlText, args := bounded.ToSQL()
	if !strings.Contains(sqlText, "DELETE `u` FROM") {
		t.Fatalf("not the multi-table form, so this test proves nothing: %s", sqlText)
	}
	_, err := db.Exec(ctx, sqlText, args...)
	if err == nil {
		t.Fatalf("the server accepted %q; the refusal is now wrong and should be removed", sqlText)
	}
	var me *mysqldriver.MySQLError
	if !errors.As(err, &me) || me.Number != 1064 {
		t.Fatalf("server answered %v, want the 1064 the refusal is built on", err)
	}

	// So Exec refuses rather than sending it.
	if _, err := bounded.Exec(ctx); !errors.Is(err, mysql.ErrAliasedDeleteBounded) {
		t.Errorf("Exec = %v, want ErrAliasedDeleteBounded", err)
	}

	// Nothing was deleted by either attempt.
	var n int64
	if err := db.Select(mysql.CountAll()).From(tbl).One(ctx, &n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("%d rows left, want all 3 untouched", n)
	}

	// The un-aliased handle is the documented way to batch, and it works.
	if _, err := db.Delete(tbl).Where(mysql.Lt(id, 100)).OrderBy(id.Asc()).Limit(1).Exec(ctx); err != nil {
		t.Fatalf("un-aliased batched DELETE: %v", err)
	}
	if err := db.Select(mysql.CountAll()).From(tbl).One(ctx, &n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("%d rows left after batching one away, want 2", n)
	}
}

// Constraint names are derived from the table and the column, and both
// can approach MySQL's 64-byte identifier limit on their own. An
// over-long derivation is error 1059 and takes the whole CREATE TABLE
// with it.
func TestMySQLDerivedConstraintNamesFitTheIdentifierLimit(t *testing.T) {
	db := openMySQL(t)

	parent := mysql.NewTable(integration.UniqueName(t, "p"))
	dropMySQL(t, db, parent)
	parentID := mysql.Add(parent, mysql.BigInt("id").PrimaryKey())
	execMySQL(t, db, mysql.CreateTable(parent))

	tbl := mysql.NewTable(integration.UniqueName(t, "a_table_name_that_eats_most_of_the_identifier_budget"))
	dropMySQL(t, db, tbl)
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(tbl, mysql.Varchar("a_reasonably_long_column_name", 64).Unique())
	mysql.Add(tbl, mysql.BigInt("another_long_column_name_here").References(parentID))
	execMySQL(t, db, mysql.CreateTable(tbl))
}

// Schema text is interpolated into a single-quoted literal rather than
// bound, so its escaping has to hold under NO_BACKSLASH_ESCAPES, where
// \' is two characters and not an escape at all.
func TestMySQLSchemaTextSurvivesNoBackslashEscapes(t *testing.T) {
	db, sqlDB := openMySQLPinnedConn(t)
	ctx := context.Background()
	if _, err := sqlDB.ExecContext(ctx,
		"SET SESSION sql_mode = CONCAT(@@sql_mode, ',NO_BACKSLASH_ESCAPES')"); err != nil {
		t.Fatalf("set sql_mode: %v", err)
	}

	const comment = "Ada's table"
	tbl := mysql.NewTable(integration.UniqueName(t, "quoted")).Comment(comment)
	dropMySQL(t, db, tbl)
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(tbl, mysql.Enum("state", "it's draft", "live"))
	execMySQL(t, db, mysql.CreateTable(tbl))

	var got []struct {
		Comment string `drop:"TABLE_COMMENT"`
	}
	rows, err := db.Query(ctx,
		"SELECT TABLE_COMMENT FROM information_schema.TABLES"+
			" WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?", tbl.Name())
	if err != nil {
		t.Fatalf("information_schema: %v", err)
	}
	if err := drops.ScanAll(rows, &got); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(got) != 1 || got[0].Comment != comment {
		t.Errorf("the comment came back as %+v, want %q", got, comment)
	}
}

// openMySQLPinnedConn returns a DB backed by a pool of exactly one
// connection, so a session setting made through it survives into the
// statements that follow. The pool is this test's own: leaving a
// modified sql_mode on a shared connection would poison whichever test
// drew it next.
func openMySQLPinnedConn(t *testing.T) (*mysql.DB, *sql.DB) {
	t.Helper()
	dsn := integration.DSN(t, integration.EnvMySQL)
	sqlDB, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := sqlDB.PingContext(context.Background()); err != nil {
		t.Fatalf("ping %s: %v", dsn, err)
	}
	return mysql.New(stdlib.New(sqlDB)), sqlDB
}

// A shared read lock has to be spelled the way both servers accept:
// MariaDB has never taken FOR SHARE, MySQL still takes the older form.
func TestMySQLSharedLockIsPortable(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl := mysql.NewTable(integration.UniqueName(t, "locked"))
	dropMySQL(t, db, tbl)

	id := mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	execMySQL(t, db, mysql.CreateTable(tbl))
	if _, err := db.Insert(tbl).Row(id.Val(int64(1))).Exec(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := db.InTx(ctx, func(tx *mysql.DB) error {
		var rows []struct{ ID int64 }
		return tx.Select(id).From(tbl).ForShare().All(ctx, &rows)
	})
	if err != nil {
		t.Fatalf("shared lock: %v", err)
	}
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

// ----------------------------------------------------------------------
// Schema migrations
//
// Everything below needs a server rather than a rendered string: what
// information_schema reports back, whether a constraint is enforced,
// and — the one that has no analogue in the PostgreSQL suite — what a
// failed migration leaves behind.
// ----------------------------------------------------------------------

// migName derives an identifier short enough for MySQL's 64-byte limit.
// A test name plus a prefix routinely exceeds it, and UniqueName trims
// from the front — which would leave two indexes in the same test with
// the same trimmed name — so the prefix goes back on the front of the
// tail it keeps.
func migName(t *testing.T, prefix string) string {
	full := integration.UniqueName(t, prefix)
	if len(full) <= 40 {
		return full
	}
	return prefix + "_" + full[len(full)-30:]
}

// migrationSchema returns a two-table schema exercising the things the
// snapshot has to survive a round trip through information_schema:
// an AUTO_INCREMENT key, a unique column, a default, a JSON column
// (LONGTEXT on MariaDB), an ON UPDATE clause, a comment, a prefixed
// index over TEXT, a CHECK constraint and a foreign key.
func migrationSchema(t *testing.T) (*mysql.Schema, *mysql.Table, *mysql.Table) {
	t.Helper()
	users := mysql.NewTable(integration.UniqueName(t, "mig_users"))
	userID := mysql.Add(users, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(users, mysql.Varchar("email", 190).NotNull().Unique())
	mysql.Add(users, mysql.Integer("age").Default("0").Comment("years"))
	mysql.Add(users, mysql.JSON("profile"))
	mysql.Add(users, mysql.Timestamp("updated", false).
		Default("CURRENT_TIMESTAMP(6)").OnUpdateExpr("CURRENT_TIMESTAMP(6)"))
	users.AddCheck(migName(t, "ckage"), "age >= 0")

	posts := mysql.NewTable(integration.UniqueName(t, "mig_posts"))
	mysql.Add(posts, mysql.BigSerial("id").PrimaryKey())
	author := mysql.Add(posts, mysql.BigInt("author").References(userID, mysql.OnDelete("CASCADE")))
	body := mysql.Add(posts, mysql.Text("body"))
	posts.AddIndex(mysql.NewIndex(migName(t, "ixbody"), posts, body).Prefix(body, 32))
	posts.AddIndex(mysql.NewIndex(migName(t, "ixauthor"), posts, author))

	return mysql.NewSchema(users, posts), users, posts
}

// dropMigrationTables removes the tables in FK order — the child first,
// since InnoDB refuses to drop a table something still references.
func dropMigrationTables(t *testing.T, db *mysql.DB, child, parent *mysql.Table) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = db.ExecExpr(ctx, mysql.DropTableIfExists(child))
		_, _ = db.ExecExpr(ctx, mysql.DropTableIfExists(parent))
	})
}

// mysqlScalar reads one string out of the catalogue. The migration
// tests ask the server what it built rather than what it was asked to
// build, and information_schema is not a table the schema layer
// declares.
func mysqlScalar(t *testing.T, db *mysql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.Query(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		t.Fatalf("%s: no row", query)
	}
	var out string
	if err := rows.Scan(&out); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return out
}

// A push that has just run must find nothing left to do. Everything the
// schema can declare has to survive the round trip through
// information_schema, or the second push emits work that is not work.
func TestMySQLPushIsIdempotent(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	schema, users, posts := migrationSchema(t)
	dropMigrationTables(t, db, posts, users)

	first, err := mysql.Push(ctx, db, schema)
	if err != nil {
		t.Fatalf("first push: %v (applied %d of %d, failed on %q)",
			err, first.AppliedCount, len(first.Statements), first.Failed)
	}
	if !first.Applied || len(first.Statements) == 0 {
		t.Fatalf("first push did nothing: %+v", first)
	}

	second, err := mysql.Push(ctx, db, schema)
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if len(second.Statements) != 0 {
		t.Fatalf("the second push of an unchanged schema is not empty:\n%s",
			strings.Join(second.Statements, "\n"))
	}
	for _, n := range second.Notices {
		// The index InnoDB creates behind a foreign key is the classic
		// false positive here: it is named after the constraint and
		// belongs to it, not to the schema.
		t.Errorf("unexpected notice: %s", n)
	}
}

// The same proof through the generator: the migration file it writes
// has to produce exactly the schema the pusher would, or the two
// disagree about what "migrated" means.
func TestMySQLGeneratedMigrationLeavesNothingToPush(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	schema, users, posts := migrationSchema(t)
	dropMigrationTables(t, db, posts, users)

	server, err := mysql.ServerVersion(ctx, db)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	res, err := mysql.GenerateMigration(mysql.GenerateOptions{
		Schema: schema,
		Dir:    "migrations",
		Name:   "init",
		Server: server,
		FS:     fstest.MapFS{},
		Write:  func(string, []byte) error { return nil },
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, stmt := range strings.Split(res.SQL, "--> statement-breakpoint") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := db.Exec(ctx, strings.TrimSuffix(stmt, ";")); err != nil {
			t.Fatalf("%v rejected a generated statement: %v\n%s", server, err, stmt)
		}
	}

	after, err := mysql.Push(ctx, db, schema)
	if err != nil {
		t.Fatalf("push after the generated migration: %v", err)
	}
	if len(after.Statements) != 0 {
		t.Fatalf("the generated migration and the pusher disagree:\n%s",
			strings.Join(after.Statements, "\n"))
	}
}

// What a hand-altered table gets is what the doc says: within a table
// the schema declares, the schema is the truth — a column it does not
// name is dropped and a changed default is put back — but an index
// nobody declared is left alone and reported.
func TestMySQLPushAgainstAHandAlteredTable(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	schema, users, posts := migrationSchema(t)
	dropMigrationTables(t, db, posts, users)

	if _, err := mysql.Push(ctx, db, schema); err != nil {
		t.Fatalf("initial push: %v", err)
	}
	hand := []string{
		"ALTER TABLE `" + users.Name() + "` ADD COLUMN `nickname` varchar(20) NULL",
		"ALTER TABLE `" + users.Name() + "` ALTER COLUMN `age` SET DEFAULT 42",
		"CREATE INDEX `by_hand_idx` ON `" + users.Name() + "` (`age`)",
		"DROP INDEX `" + migName(t, "ixbody") + "` ON `" + posts.Name() + "`",
	}
	for _, s := range hand {
		if _, err := db.Exec(ctx, s); err != nil {
			t.Fatalf("hand-altering the schema: %v\n%s", err, s)
		}
	}

	res, err := mysql.Push(ctx, db, schema)
	if err != nil {
		t.Fatalf("push over the hand-altered schema: %v", err)
	}
	joined := strings.Join(res.Statements, "\n")
	for _, want := range []string{
		"DROP COLUMN `nickname`",
		"ALTER COLUMN `age` SET DEFAULT 0",
		"CREATE INDEX `" + migName(t, "ixbody") + "` ON `" + posts.Name() + "` (`body`(32));",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "by_hand_idx") {
		t.Errorf("an index the schema never declared was dropped:\n%s", joined)
	}
	var notice *mysql.SchemaNotice
	for i, n := range res.Notices {
		if n.Rule == "unmanaged-index" && n.Object == "by_hand_idx" {
			notice = &res.Notices[i]
		}
	}
	if notice == nil {
		t.Fatalf("the withheld index drop was not reported: %+v", res.Notices)
	}
	if !strings.Contains(notice.SQL, "DROP INDEX `by_hand_idx`") {
		t.Errorf("the notice does not carry the statement to run by hand: %+v", notice)
	}

	// And the third push settles again.
	settled, err := mysql.Push(ctx, db, schema)
	if err != nil {
		t.Fatalf("settling push: %v", err)
	}
	if len(settled.Statements) != 0 {
		t.Fatalf("the schema did not settle:\n%s", strings.Join(settled.Statements, "\n"))
	}
	// The by-hand index is still there, untouched.
	rows, err := db.Query(ctx,
		"SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND INDEX_NAME = 'by_hand_idx'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var n int64
	if !rows.Next() {
		t.Fatal("COUNT(*) returned no row")
	}
	if err := rows.Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Errorf("the by-hand index was dropped after all")
	}
}

// The contract MySQL forces and PostgreSQL does not: a migration that
// fails part-way stays part-way applied, and Push has to say how far it
// got rather than pretending nothing happened.
func TestMySQLPushReportsHowFarItGot(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()

	name := integration.UniqueName(t, "halfway")
	before := mysql.NewTable(name)
	mysql.Add(before, mysql.BigSerial("id").PrimaryKey())
	beforeEmail := mysql.Add(before, mysql.Varchar("email", 190).NotNull())
	dropMySQL(t, db, before)

	if _, err := mysql.Push(ctx, db, mysql.NewSchema(before)); err != nil {
		t.Fatalf("initial push: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := db.Insert(before).Row(beforeEmail.Val("duplicate@example.com")).Exec(ctx); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// Two changes: a new column, which succeeds, and a unique index over
	// a column that already holds duplicates, which cannot.
	after := mysql.NewTable(name)
	mysql.Add(after, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(after, mysql.Varchar("email", 190).NotNull().Unique())
	mysql.Add(after, mysql.Integer("added"))

	res, err := mysql.Push(ctx, db, mysql.NewSchema(after))
	if err == nil {
		t.Fatalf("a unique index over duplicate rows was accepted: %+v", res)
	}
	if res == nil {
		t.Fatal("Push returned no result alongside the error; there is no way to learn how far it got")
	}
	if res.Applied {
		t.Errorf("Applied is true on a failed push")
	}
	if res.AppliedCount != 1 {
		t.Errorf("AppliedCount = %d, want 1 (the ADD COLUMN)", res.AppliedCount)
	}
	if !strings.Contains(res.Failed, "CREATE UNIQUE INDEX") {
		t.Errorf("Failed = %q, want the unique index", res.Failed)
	}
	if !strings.Contains(err.Error(), "push stopped after 1 of") {
		t.Errorf("the error does not say how far it got: %v", err)
	}

	// The half that succeeded is still there. This is the whole point:
	// there was no transaction to take it back.
	snap, ierr := mysql.Introspect(ctx, db)
	if ierr != nil {
		t.Fatalf("introspect: %v", ierr)
	}
	if _, ok := snap.Tables[name].Columns["added"]; !ok {
		t.Errorf("the column added before the failure was rolled back; MySQL cannot do that, so something else is wrong")
	}
}

// The premise behind every "half-applied" sentence in the package
// documentation, asserted rather than assumed.
func TestMySQLDDLIgnoresTheTransactionAroundIt(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl := mysql.NewTable(integration.UniqueName(t, "no_tx_ddl"))
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	dropMySQL(t, db, tbl)

	sentinel := errors.New("roll it back")
	err := db.InTx(ctx, func(tx *mysql.DB) error {
		if _, err := tx.ExecExpr(ctx, mysql.CreateTable(tbl)); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("InTx: %v", err)
	}

	snap, err := mysql.Introspect(ctx, db)
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	if _, ok := snap.Tables[tbl.Name()]; !ok {
		t.Fatalf("the rolled-back CREATE TABLE was undone — this server has transactional DDL, and drops/mysql's documented contract is written for one that does not")
	}
}

// A CHECK constraint reads back in the server's own spelling, never the
// Go program's. Push respells the declared side before diffing; without
// that the constraint is dropped and re-added on every push.
func TestMySQLCheckConstraintRoundTripsAndIsEnforced(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	server, err := mysql.ServerVersion(ctx, db)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if !server.SupportsCheckConstraints() {
		t.Skipf("%s parses CHECK constraints and discards them", server)
	}

	tbl := mysql.NewTable(integration.UniqueName(t, "checked"))
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	age := mysql.Add(tbl, mysql.Integer("age"))
	tbl.AddCheck(migName(t, "ckage"), "age >= 0")
	dropMySQL(t, db, tbl)

	schema := mysql.NewSchema(tbl)
	if _, err := mysql.Push(ctx, db, schema); err != nil {
		t.Fatalf("push: %v", err)
	}

	// The catalogue holds the server's rendering, not "age >= 0".
	snap, err := mysql.Introspect(ctx, db)
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	got := snap.Tables[tbl.Name()].CheckConstraints[migName(t, "ckage")]
	if got == nil {
		t.Fatalf("the check was not read back: %+v", snap.Tables[tbl.Name()].CheckConstraints)
	}
	if !strings.Contains(got.Value, "`age`") {
		t.Errorf("expected the server's own spelling with a quoted identifier, got %q", got.Value)
	}

	// Which is why the second push is still empty.
	second, err := mysql.Push(ctx, db, schema)
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if len(second.Statements) != 0 {
		t.Fatalf("the check was not recognised as unchanged:\n%s", strings.Join(second.Statements, "\n"))
	}
	// And no probe table was left behind. The probe is the one thing
	// here that creates a table of its own, and without a transaction
	// to unwind it the drop has to be explicit.
	after, err := mysql.Introspect(ctx, db)
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	for name := range after.Tables {
		if strings.HasPrefix(name, "_drops_probe_") {
			t.Errorf("probe table %q was not cleaned up", name)
		}
	}

	if _, err := db.Insert(tbl).Row(age.Val(-1)).Exec(ctx); err == nil {
		t.Errorf("%s accepted a row violating the CHECK constraint", server)
	}

	// Tightening it under the same name is a real change: MySQL cannot
	// alter a CHECK in place, so it is dropped and re-added.
	tightened := mysql.NewTable(tbl.Name())
	mysql.Add(tightened, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(tightened, mysql.Integer("age"))
	tightened.AddCheck(migName(t, "ckage"), "age >= 18")
	res, err := mysql.Push(ctx, db, mysql.NewSchema(tightened))
	if err != nil {
		t.Fatalf("tightening push: %v", err)
	}
	if len(res.Statements) != 2 {
		t.Fatalf("tightening a check = %v, want a drop and an add", res.Statements)
	}
}

// Introspection has to read back exactly what the schema declared, or
// Diff reads "not looked at" as "absent" and pushes work for ever.
func TestMySQLIntrospectReadsBackTheDeclaredShape(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	server, err := mysql.ServerVersion(ctx, db)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	schema, users, posts := migrationSchema(t)
	dropMigrationTables(t, db, posts, users)
	if _, err := mysql.Push(ctx, db, schema); err != nil {
		t.Fatalf("push: %v", err)
	}

	snap, err := mysql.Introspect(ctx, db)
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	u := snap.Tables[users.Name()]
	if u == nil {
		t.Fatalf("users table missing")
	}
	if u.PrimaryKey == nil || u.PrimaryKey.Name != "PRIMARY" || len(u.PrimaryKey.Columns) != 1 {
		t.Fatalf("primary key = %+v; MySQL renames every key to PRIMARY", u.PrimaryKey)
	}
	if id := u.Columns["id"]; id == nil || !id.AutoIncrement || id.Type != "bigint" {
		t.Fatalf("id = %+v (the display width MariaDB reports has to be normalised away)", id)
	}
	if age := u.Columns["age"]; age == nil || age.Default == nil || *age.Default != "0" || age.Comment != "years" {
		t.Fatalf("age = %+v", age)
	}
	if upd := u.Columns["updated"]; upd == nil || upd.OnUpdate != "CURRENT_TIMESTAMP(6)" {
		t.Fatalf("updated = %+v; the ON UPDATE clause lives in information_schema's EXTRA", upd)
	}
	// MariaDB has no JSON type: it is LONGTEXT plus a generated
	// json_valid() CHECK, which must not be read back as a constraint
	// the schema forgot to declare.
	profile := u.Columns["profile"]
	if server.MariaDB {
		if profile.Type != "longtext" {
			t.Errorf("MariaDB reports a JSON column as %q, expected longtext", profile.Type)
		}
		for name, c := range u.CheckConstraints {
			if strings.Contains(c.Value, "json_valid") {
				t.Errorf("the generated constraint %q was read back as a declared check", name)
			}
		}
	} else if profile.Type != "json" {
		t.Errorf("MySQL reports a JSON column as %q", profile.Type)
	}

	p := snap.Tables[posts.Name()]
	fk := p.ForeignKeys["fk_"+posts.Name()+"_author"]
	if fk == nil {
		t.Fatalf("foreign keys = %+v", p.ForeignKeys)
	}
	if fk.TableTo != users.Name() || fk.OnDelete != "cascade" {
		t.Errorf("fk = %+v", fk)
	}
	// The two servers disagree about what an unspecified ON UPDATE is
	// called — MariaDB says RESTRICT, MySQL says NO ACTION — and they
	// mean the same thing.
	if fk.OnUpdate != "no action" {
		t.Errorf("ON UPDATE = %q, want the normalised %q", fk.OnUpdate, "no action")
	}
	// The index InnoDB creates behind the foreign key has the
	// constraint's name and cannot be dropped separately, so it must not
	// appear as an index of its own.
	if _, ok := p.Indexes["fk_"+posts.Name()+"_author"]; ok {
		t.Errorf("the foreign key's own index was reported as a secondary index: %+v", p.Indexes)
	}
	body := p.Indexes[migName(t, "ixbody")]
	if body == nil || len(body.Prefixes) != 1 || body.Prefixes[0] != 32 {
		t.Errorf("prefixed index = %+v; SUB_PART is what makes a TEXT column indexable", body)
	}
}

// A DryRun is the safety valve for the statements that cannot be taken
// back: preview them, run the analyser over them, then decide.
func TestMySQLDryRunFeedsTheSafetyAnalyser(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	name := integration.UniqueName(t, "dryrun")
	before := mysql.NewTable(name)
	mysql.Add(before, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(before, mysql.Varchar("doomed", 20))
	dropMySQL(t, db, before)

	if _, err := mysql.Push(ctx, db, mysql.NewSchema(before)); err != nil {
		t.Fatalf("initial push: %v", err)
	}

	after := mysql.NewTable(name)
	mysql.Add(after, mysql.BigSerial("id").PrimaryKey())
	res, err := mysql.Push(ctx, db, mysql.NewSchema(after), mysql.PushOptions{DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if res.Applied || len(res.Statements) != 1 {
		t.Fatalf("dry run = %+v", res)
	}
	var found bool
	for _, w := range mysql.AnalyzeStatements(res.Statements) {
		if w.Rule == "drop-column" && w.Severity == mysql.SeverityError {
			found = true
		}
	}
	if !found {
		t.Fatalf("the analyser did not flag the column drop: %v", res.Statements)
	}

	// And the dry run changed nothing.
	snap, err := mysql.Introspect(ctx, db)
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	if _, ok := snap.Tables[name].Columns["doomed"]; !ok {
		t.Fatalf("a dry run dropped the column")
	}
}

// An ENUM keeps its members inside its type, so the migration layer's
// type normalisation runs over the column's values as well as over its
// spelling. Lower-casing them leaves both sides of the diff agreeing —
// each was folded the same way — while the table the migration built
// stores something the schema never described. Only the server can
// tell the two apart, which is why this is here and not in mysql/.
func TestMySQLEnumMembersSurviveTheMigration(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	name := integration.UniqueName(t, "enum_case")
	tbl := mysql.NewTable(name)
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	status := mysql.Add(tbl, mysql.Enum("status", "Draft", "Published").
		NotNull().Default("'Draft'"))
	dropMySQL(t, db, tbl)

	schema := mysql.NewSchema(tbl)
	if _, err := mysql.Push(ctx, db, schema); err != nil {
		t.Fatalf("push: %v", err)
	}

	// The column the server actually built, in its own words.
	colType := mysqlScalar(t, db, `SELECT COLUMN_TYPE FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = 'status'`, name)
	if !strings.Contains(colType, "'Draft'") {
		t.Errorf("the migration built %s; the schema said ENUM('Draft', 'Published')", colType)
	}

	// And the value round-trips as written rather than as folded.
	if _, err := db.Insert(tbl).Row(status.Val("Draft")).Exec(ctx); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var got string
	if err := db.Select(status).From(tbl).One(ctx, &got); err != nil {
		t.Fatalf("select: %v", err)
	}
	if got != "Draft" {
		t.Errorf("stored %q, want %q — the enum member was rewritten by the migration", got, "Draft")
	}

	// The default is one of those members, so a folded type takes the
	// default with it and the push never settles.
	second, err := mysql.Push(ctx, db, schema)
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if len(second.Statements) != 0 {
		t.Fatalf("the enum column did not settle:\n%s", strings.Join(second.Statements, "\n"))
	}
}

// Two tables going at once, where the one being referenced sorts first.
// InnoDB refuses to drop a referenced table and MySQL has no DROP TABLE
// … CASCADE to lean on — it parses CASCADE and ignores it — so the diff
// has to clear the reference itself. Whether it did is a question for
// the server, not for the rendered text.
func TestMySQLDroppingTwoTablesAtOnceSurvivesTheReference(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()

	// The parent sorts before the child, which is the order the diff
	// emits its drops in.
	parentName := "a_" + integration.UniqueName(t, "parent")
	childName := "z_" + integration.UniqueName(t, "child")
	parent := mysql.NewTable(parentName)
	parentID := mysql.Add(parent, mysql.BigSerial("id").PrimaryKey())
	child := mysql.NewTable(childName)
	mysql.Add(child, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(child, mysql.BigInt("pid").References(parentID))
	dropMigrationTables(t, db, child, parent)

	schema := mysql.NewSchema(parent, child)
	if _, err := mysql.Push(ctx, db, schema); err != nil {
		t.Fatalf("push: %v", err)
	}

	// Push never drops a table it was not handed — see ownedBy — so the
	// teardown is a Diff to nothing, which is what a generated
	// migration would carry.
	prev := mysql.BuildSnapshot(schema)
	for _, stmt := range mysql.Diff(prev, mysql.EmptySnapshot()) {
		if _, err := db.Exec(ctx, strings.TrimSuffix(stmt, ";")); err != nil {
			t.Fatalf("the server refused a teardown statement: %v\n%s", err, stmt)
		}
	}

	snap, err := mysql.Introspect(ctx, db)
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	for _, n := range []string{parentName, childName} {
		if _, ok := snap.Tables[n]; ok {
			t.Errorf("%q survived the teardown", n)
		}
	}
}

// InnoDB takes USING HASH on a CREATE INDEX, builds a B-tree and says
// nothing. Comparing the declared method against the one the catalogue
// reports would then differ for ever, and recreating the index would
// produce the same B-tree again: an index rebuild on every push. The
// request is reported instead.
func TestMySQLDeclaredHashIndexDoesNotChurn(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	name := integration.UniqueName(t, "hash_idx")
	tbl := mysql.NewTable(name)
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	n := mysql.Add(tbl, mysql.Integer("n"))
	idxName := migName(t, "ixhash")
	tbl.AddIndex(mysql.NewIndex(idxName, tbl, n).Using("HASH"))
	dropMySQL(t, db, tbl)

	schema := mysql.NewSchema(tbl)
	if _, err := mysql.Push(ctx, db, schema); err != nil {
		t.Fatalf("push: %v", err)
	}

	// The engine's answer, not the schema's request.
	indexType := mysqlScalar(t, db, `SELECT INDEX_TYPE FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND INDEX_NAME = ? LIMIT 1`, name, idxName)
	if strings.EqualFold(indexType, "HASH") {
		t.Skipf("this engine honoured USING HASH (INDEX_TYPE = %s); the churn this test guards against cannot happen", indexType)
	}

	second, err := mysql.Push(ctx, db, schema)
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if len(second.Statements) != 0 {
		t.Fatalf("the index is rebuilt on every push, and rebuilding it changes nothing:\n%s",
			strings.Join(second.Statements, "\n"))
	}
	var reported bool
	for _, notice := range second.Notices {
		if notice.Rule == "index-method-ignored" && notice.Object == idxName {
			reported = true
		}
	}
	if !reported {
		t.Errorf("the discarded USING HASH was neither acted on nor reported: %+v", second.Notices)
	}
}

// The idempotency claim is only as wide as the schema it is made over.
// Every type constructor that renders something the catalogue reports
// back differently from the way it was written — a display width, a
// resolved alias, a quoted default, a member list — gets one column
// here, and the second push has to be empty.
func TestMySQLPushSettlesOverEveryDeclarableType(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	name := integration.UniqueName(t, "wide")
	tbl := mysql.NewTable(name)
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(tbl, mysql.Boolean("flag").Default("FALSE"))        // tinyint(1), and TRUE/FALSE store as 1/0
	mysql.Add(tbl, mysql.Integer("n").NotNull().Default("0"))     // int(11) on MariaDB, int on MySQL 8.4
	mysql.Add(tbl, mysql.UnsignedBigInt("u"))                     // bigint(20) unsigned
	mysql.Add(tbl, mysql.Numeric("price", 10, 2))                 // decimal(10,2)
	mysql.Add(tbl, mysql.Numeric("bare", 0, 0))                   // DECIMAL is decimal(10,0)
	mysql.Add(tbl, mysql.DoublePrecision("d"))                    // DOUBLE PRECISION resolves to double
	mysql.Add(tbl, mysql.Real("f"))                               // float
	mysql.Add(tbl, mysql.Char("code", 3))                         // char(3)
	mysql.Add(tbl, mysql.Varchar("title", 40).Default("'UnSet'")) // MySQL 8 strips the quotes, MariaDB does not
	mysql.Add(tbl, mysql.Text("body"))                            // text
	mysql.Add(tbl, mysql.Blob("payload"))                         // blob
	mysql.Add(tbl, mysql.Enum("kind", "Alpha", "Beta").
		Default("'Alpha'")) // members are values, and the default is one of them
	mysql.Add(tbl, mysql.JSON("doc")) // longtext + json_valid on MariaDB
	mysql.Add(tbl, mysql.Date("day")) // date
	mysql.Add(tbl, mysql.UUID("uid")) // char(36)
	mysql.Add(tbl, mysql.Timestamp("at", false).
		Default("CURRENT_TIMESTAMP(6)").OnUpdateExpr("CURRENT_TIMESTAMP(6)")) // datetime(6), clock in EXTRA
	dropMySQL(t, db, tbl)

	schema := mysql.NewSchema(tbl)
	first, err := mysql.Push(ctx, db, schema)
	if err != nil {
		t.Fatalf("push: %v (applied %d of %d, failed on %q)",
			err, first.AppliedCount, len(first.Statements), first.Failed)
	}
	second, err := mysql.Push(ctx, db, schema)
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if len(second.Statements) != 0 {
		t.Fatalf("a type does not survive the round trip through information_schema:\n%s",
			strings.Join(second.Statements, "\n"))
	}
	for _, n := range second.Notices {
		t.Errorf("unexpected notice: %s", n)
	}
}

// Diff renders every statement unqualified, so a push naming a second
// database reads that one and writes this one: the CREATE TABLE is
// re-emitted on every run and fails on the table it made here the first
// time. Push refuses instead. The database it will accept is the one
// the server says the connection is on, which only the server knows.
func TestMySQLPushRefusesADatabaseTheConnectionIsNotOn(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	name := integration.UniqueName(t, "dbopt")
	tbl := mysql.NewTable(name)
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	dropMySQL(t, db, tbl)
	schema := mysql.NewSchema(tbl)

	if _, err := mysql.Push(ctx, db, schema, mysql.PushOptions{
		Database: "drops_a_database_no_connection_is_on",
	}); !errors.Is(err, mysql.ErrForeignDatabase) {
		t.Fatalf("push into a foreign database = %v, want ErrForeignDatabase", err)
	}
	// Nothing was created on the way to that refusal.
	snap, err := mysql.Introspect(ctx, db)
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	if _, ok := snap.Tables[name]; ok {
		t.Fatalf("the refused push created %q anyway", name)
	}

	// Naming the connection's own database is the case the check must
	// not break, and only the server can say what that name is.
	session := mysqlScalar(t, db, "SELECT DATABASE()")
	if _, err := mysql.Push(ctx, db, schema, mysql.PushOptions{Database: session}); err != nil {
		t.Fatalf("push naming the connection's own database %q: %v", session, err)
	}
	settled, err := mysql.Push(ctx, db, schema, mysql.PushOptions{Database: session})
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if len(settled.Statements) != 0 {
		t.Fatalf("the push did not settle:\n%s", strings.Join(settled.Statements, "\n"))
	}
}

// dropCheckSQL picks between DROP CONSTRAINT and DROP CHECK off a
// version number, and the unit test for it compares the two strings.
// Which one a server accepts is not a string question: MariaDB has
// never taken DROP CHECK, MySQL took only it between 8.0.16 and
// 8.0.18, and both take DROP CONSTRAINT from 8.0.19. This asks.
func TestMySQLDropCheckSpellingIsTheOneTheServerTakes(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	server, err := mysql.ServerVersion(ctx, db)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if !server.SupportsCheckConstraints() {
		t.Skipf("%s parses CHECK constraints and discards them", server)
	}

	name := integration.UniqueName(t, "ckdrop")
	withCheck := mysql.NewTable(name)
	mysql.Add(withCheck, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(withCheck, mysql.Integer("age"))
	withCheck.AddCheck(migName(t, "ckdrop"), "age >= 0")
	dropMySQL(t, db, withCheck)
	if _, err := mysql.Push(ctx, db, mysql.NewSchema(withCheck)); err != nil {
		t.Fatalf("push: %v", err)
	}

	// The spelling drops chose for this server, run against it.
	without := mysql.NewTable(name)
	mysql.Add(without, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(without, mysql.Integer("age"))
	stmts := mysql.Diff(
		mysql.BuildSnapshot(mysql.NewSchema(withCheck)),
		mysql.BuildSnapshot(mysql.NewSchema(without)),
		mysql.DiffOptions{Server: server})
	if len(stmts) != 1 {
		t.Fatalf("dropping a check = %v", stmts)
	}
	if _, err := db.Exec(ctx, strings.TrimSuffix(stmts[0], ";")); err != nil {
		t.Fatalf("%s refused the spelling drops picked for it: %v\n%s", server, err, stmts[0])
	}

	// And the spelling it did not pick. MariaDB rejects DROP CHECK
	// outright, which is the whole reason for the gate; MySQL 8.0.19+
	// accepts both, so there is nothing to assert there.
	if !server.MariaDB {
		t.Skipf("%s accepts both spellings from 8.0.19; only MariaDB rejects DROP CHECK", server)
	}
	if !strings.Contains(stmts[0], "DROP CONSTRAINT") {
		t.Errorf("MariaDB has no DROP CHECK, so the statement must use DROP CONSTRAINT: %s", stmts[0])
	}
	if _, err := db.Exec(ctx, "ALTER TABLE `"+name+"` DROP CHECK `"+migName(t, "ckdrop")+"`"); err == nil {
		t.Errorf("%s accepted DROP CHECK after all; the version gate is guessing", server)
	}
}

// DiffOptions.Safe guards CREATE TABLE and DROP TABLE and nothing else,
// and the sharp edge is what it leaves unguarded: re-running a
// migration that adds a column still fails. Both halves are the
// server's answer, not the statement's shape.
func TestMySQLSafeGuardsOnlyTheStatementsThatCanBeRerun(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	name := integration.UniqueName(t, "safe")
	before := mysql.NewTable(name)
	mysql.Add(before, mysql.BigSerial("id").PrimaryKey())
	after := mysql.NewTable(name)
	mysql.Add(after, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(after, mysql.Integer("n"))
	dropMySQL(t, db, after)

	safe := mysql.DiffOptions{Safe: true}
	create := mysql.Diff(nil, mysql.BuildSnapshot(mysql.NewSchema(before)), safe)
	add := mysql.Diff(
		mysql.BuildSnapshot(mysql.NewSchema(before)),
		mysql.BuildSnapshot(mysql.NewSchema(after)), safe)
	if len(create) != 1 || len(add) != 1 {
		t.Fatalf("create = %v, add = %v", create, add)
	}

	run := func(stmt string) error {
		_, err := db.Exec(ctx, strings.TrimSuffix(stmt, ";"))
		return err
	}
	if err := run(create[0]); err != nil {
		t.Fatalf("guarded CREATE TABLE: %v", err)
	}
	if err := run(create[0]); err != nil {
		t.Errorf("a guarded CREATE TABLE has to be re-runnable: %v", err)
	}
	if err := run(add[0]); err != nil {
		t.Fatalf("ADD COLUMN: %v", err)
	}
	// Unguarded on purpose: MySQL has no IF NOT EXISTS here, so drops
	// emits none and Push recomputes the diff instead of re-running.
	if err := run(add[0]); err == nil {
		t.Errorf("%s let the same ADD COLUMN run twice; Safe is documented as unable to guard it", name)
	}

	server, err := mysql.ServerVersion(ctx, db)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if !server.MariaDB {
		t.Skipf("%s has no ADD COLUMN IF NOT EXISTS; the divergence being pinned here is MariaDB's", server)
	}
	// And the extension drops declines to use, so the reason it
	// declines is a fact rather than a recollection.
	if _, err := db.Exec(ctx, "ALTER TABLE `"+name+"` ADD COLUMN IF NOT EXISTS `n` int NULL"); err != nil {
		t.Errorf("MariaDB is supposed to accept ADD COLUMN IF NOT EXISTS: %v", err)
	}
}

// MODIFY COLUMN restates the column in full, and whatever the
// restatement leaves out is gone — which is why the snapshot has to
// carry a column's COMMENT and ON UPDATE even though nothing else
// reads them. The unit test for this compares the rendered string; the
// thing it is guarding is what the server does with it.
func TestMySQLModifyColumnDropsWhatItDoesNotRestate(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	name := integration.UniqueName(t, "restate")
	before := mysql.NewTable(name)
	mysql.Add(before, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(before, mysql.Timestamp("updated", false).
		NotNull().
		Default("CURRENT_TIMESTAMP(6)").
		OnUpdateExpr("CURRENT_TIMESTAMP(6)").
		Comment("last write"))
	dropMySQL(t, db, before)
	if _, err := mysql.Push(ctx, db, mysql.NewSchema(before)); err != nil {
		t.Fatalf("push: %v", err)
	}

	readBack := func() (comment, extra string) {
		t.Helper()
		return mysqlScalar(t, db, `SELECT COLUMN_COMMENT FROM information_schema.COLUMNS
				WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = 'updated'`, name),
			mysqlScalar(t, db, `SELECT EXTRA FROM information_schema.COLUMNS
				WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = 'updated'`, name)
	}
	if c, e := readBack(); c != "last write" || !strings.Contains(strings.ToLower(e), "on update") {
		t.Fatalf("the push did not create the column it described: comment=%q extra=%q", c, e)
	}

	// A restatement that leaves them out is a restatement that removes
	// them. This is the failure the snapshot fields exist to prevent.
	if _, err := db.Exec(ctx, "ALTER TABLE `"+name+
		"` MODIFY COLUMN `updated` datetime(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)"); err != nil {
		t.Fatalf("modify: %v", err)
	}
	if c, e := readBack(); c != "" || strings.Contains(strings.ToLower(e), "on update") {
		t.Fatalf("the server kept attributes the MODIFY left out, so nothing in the snapshot would need to carry them: comment=%q extra=%q", c, e)
	}

	// And the push puts them back, because its own MODIFY carries
	// them. Moving DATETIME(6) to TIMESTAMP(6) is what forces a MODIFY
	// rather than an ALTER COLUMN … SET DEFAULT.
	after := mysql.NewTable(name)
	mysql.Add(after, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(after, mysql.Timestamp("updated", true).
		NotNull().
		Default("CURRENT_TIMESTAMP(6)").
		OnUpdateExpr("CURRENT_TIMESTAMP(6)").
		Comment("last write"))
	if _, err := mysql.Push(ctx, db, mysql.NewSchema(after)); err != nil {
		t.Fatalf("push after the hand-made restatement: %v", err)
	}
	if c, e := readBack(); c != "last write" || !strings.Contains(strings.ToLower(e), "on update") {
		t.Errorf("the push's MODIFY dropped the column's comment or ON UPDATE: comment=%q extra=%q", c, e)
	}
	settled, err := mysql.Push(ctx, db, mysql.NewSchema(after))
	if err != nil {
		t.Fatalf("settling push: %v", err)
	}
	if len(settled.Statements) != 0 {
		t.Fatalf("the restated column did not settle:\n%s", strings.Join(settled.Statements, "\n"))
	}
}

// A schema can be created two ways — CreateTable renders it directly,
// Push renders it from a snapshot — and the two have to agree on every
// name and every type or a table made by one and evolved by the other
// grows duplicates: a second unique index beside the first, a column
// restated on every push. The names are derived in different files
// (ddl.go and snapshot.go) from the same rule, which is exactly the
// arrangement that drifts.
func TestMySQLCreateTableAndPushAgreeOnTheSameSchema(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	parent := mysql.NewTable(integration.UniqueName(t, "agree_parent"))
	parentID := mysql.Add(parent, mysql.BigSerial("id").PrimaryKey())

	child := mysql.NewTable(integration.UniqueName(t, "agree_child"))
	mysql.Add(child, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(child, mysql.Varchar("email", 190).NotNull().Unique())
	mysql.Add(child, mysql.Enum("kind", "Alpha", "Beta").Default("'Alpha'"))
	mysql.Add(child, mysql.Boolean("flag").Default("TRUE"))
	mysql.Add(child, mysql.Numeric("price", 10, 2))
	mysql.Add(child, mysql.BigInt("pid").References(parentID, mysql.OnDelete("CASCADE")))
	dropMigrationTables(t, db, child, parent)

	// Built by the DDL layer, not by the migration layer.
	execMySQL(t, db, mysql.CreateTable(parent))
	execMySQL(t, db, mysql.CreateTable(child))

	res, err := mysql.Push(ctx, db, mysql.NewSchema(parent, child))
	if err != nil {
		t.Fatalf("push over a CreateTable-built schema: %v", err)
	}
	if len(res.Statements) != 0 {
		t.Fatalf("the two layers disagree about the same schema:\n%s", strings.Join(res.Statements, "\n"))
	}
	for _, n := range res.Notices {
		t.Errorf("unexpected notice: %s", n)
	}
	// One unique index on email, not two under two derived names.
	if n := mysqlScalar(t, db, `SELECT COUNT(DISTINCT INDEX_NAME) FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = 'email'`, child.Name()); n != "1" {
		t.Errorf("email carries %s indexes, want 1", n)
	}
}

// Push reports what it declines to do rather than doing it quietly.
// Two of those decisions can only be provoked with a server: a table
// whose ENGINE the schema disagrees with, and a CHECK expression the
// server will not parse.
func TestMySQLPushReportsTheChangesItDeclinesToMake(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	name := integration.UniqueName(t, "declined")
	tbl := mysql.NewTable(name).Engine("InnoDB")
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(tbl, mysql.Integer("age"))
	dropMySQL(t, db, tbl)

	// A table the schema wants on InnoDB and the database has on
	// something else. Changing it copies every row, so Push says so
	// and stops.
	if _, err := db.Exec(ctx, "CREATE TABLE `"+name+
		"` (`id` bigint NOT NULL AUTO_INCREMENT, `age` int NULL, PRIMARY KEY (`id`)) ENGINE=MyISAM"); err != nil {
		t.Skipf("this server will not make a MyISAM table: %v", err)
	}
	res, err := mysql.Push(ctx, db, mysql.NewSchema(tbl))
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(res.Statements) != 0 {
		t.Fatalf("Push rewrote the table to change its engine:\n%s", strings.Join(res.Statements, "\n"))
	}
	var options *mysql.SchemaNotice
	for i, n := range res.Notices {
		if n.Rule == "table-options" && n.Table == name {
			options = &res.Notices[i]
		}
	}
	if options == nil {
		t.Fatalf("the engine mismatch was neither changed nor reported: %+v", res.Notices)
	}
	if !strings.Contains(options.SQL, "ENGINE=InnoDB") {
		t.Errorf("the notice does not carry the statement to run by hand: %+v", options)
	}
	if got := mysqlScalar(t, db, `SELECT ENGINE FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, name); !strings.EqualFold(got, "MyISAM") {
		t.Errorf("the engine changed to %q behind the notice", got)
	}
}

// An expression the server refuses to parse must not be treated as a
// change. Push probes the declared spelling before it diffs, and a
// probe that fails has to be told apart from a probe that could not
// run — the SQLSTATE the driver carries is the only thing separating
// "this expression is nonsense" from "the disk is full", and drops
// reads it off whatever error type the driver returned by reflection,
// which nothing else here exercises against a real one.
func TestMySQLARefusedCheckExpressionIsReportedNotChurned(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	server, err := mysql.ServerVersion(ctx, db)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if !server.SupportsCheckConstraints() {
		t.Skipf("%s parses CHECK constraints and discards them", server)
	}

	name := integration.UniqueName(t, "badexpr")
	ckName := migName(t, "ckbad")
	good := mysql.NewTable(name)
	mysql.Add(good, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(good, mysql.Integer("age"))
	good.AddCheck(ckName, "age >= 0")
	dropMySQL(t, db, good)
	if _, err := mysql.Push(ctx, db, mysql.NewSchema(good)); err != nil {
		t.Fatalf("push: %v", err)
	}

	// The same constraint name, now declared with something no server
	// can parse.
	bad := mysql.NewTable(name)
	mysql.Add(bad, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(bad, mysql.Integer("age"))
	bad.AddCheck(ckName, "drops_no_such_function(age) > 0")
	res, err := mysql.Push(ctx, db, mysql.NewSchema(bad))
	if err != nil {
		t.Fatalf("push over an unparseable expression: %v", err)
	}
	if len(res.Statements) != 0 {
		t.Fatalf("Push acted on an expression the server would not parse:\n%s", strings.Join(res.Statements, "\n"))
	}
	var reported bool
	for _, n := range res.Notices {
		if n.Rule == "check-not-normalised" && n.Object == ckName {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("the unparseable expression was skipped without a word: %+v", res.Notices)
	}
	// The constraint the database has is untouched, and the probe table
	// is gone.
	snap, err := mysql.Introspect(ctx, db)
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	if _, ok := snap.Tables[name].CheckConstraints[ckName]; !ok {
		t.Errorf("the live constraint was dropped: %+v", snap.Tables[name].CheckConstraints)
	}
	for tn := range snap.Tables {
		if strings.HasPrefix(tn, "_drops_probe_") {
			t.Errorf("probe table %q survived a failed probe", tn)
		}
	}
}

// ----------------------------------------------------------------------
// Outbox, event store, idempotency
//
// Everything below needs the server rather than the SQL: whether SKIP
// LOCKED parses, whether a named lock excludes a second session,
// whether REPEATABLE READ hides a committed append from a reader that
// has already taken its snapshot. None of it can be settled by looking
// at a rendered statement.
// ----------------------------------------------------------------------

// mysqlServerVersion returns VERSION() and whether it is MariaDB.
func mysqlServerVersion(t *testing.T, db *mysql.DB) (major, minor int, mariadb bool) {
	t.Helper()
	var v string
	if err := db.Select(drops.Raw("VERSION()")).One(context.Background(), &v); err != nil {
		t.Fatalf("VERSION(): %v", err)
	}
	mariadb = strings.Contains(strings.ToLower(v), "mariadb")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		t.Fatalf("unparsable version %q", v)
	}
	major, _ = strconv.Atoi(parts[0])
	minor, _ = strconv.Atoi(parts[1])
	return major, minor, mariadb
}

// mysqlHasSkipLocked reports whether the server is new enough for FOR
// UPDATE SKIP LOCKED: MySQL 8.0 and MariaDB 10.6 introduced it, and
// MySQL 5.7 has no equivalent.
func mysqlHasSkipLocked(t *testing.T, db *mysql.DB) bool {
	major, minor, mariadb := mysqlServerVersion(t, db)
	if mariadb {
		return major > 10 || (major == 10 && minor >= 6)
	}
	return major >= 8
}

// mysqlIsolationLevel reads the session's isolation level, which the
// two servers spell differently: MySQL 8.0 removed @@tx_isolation and
// MariaDB 10.11 has not added @@transaction_isolation, so a portable
// read has to try both.
func mysqlIsolationLevel(t *testing.T, db *mysql.DB) string {
	t.Helper()
	ctx := context.Background()
	var level string
	if err := db.Select(drops.Raw("@@transaction_isolation")).One(ctx, &level); err == nil {
		return level
	}
	if err := db.Select(drops.Raw("@@tx_isolation")).One(ctx, &level); err != nil {
		t.Fatalf("neither @@transaction_isolation nor @@tx_isolation is readable: %v", err)
	}
	return level
}

// mysqlOutboxFixture creates an outbox table with its indexes and
// returns an Outbox bound to it.
func mysqlOutboxFixture(t *testing.T, db *mysql.DB) (*mysql.Outbox, *mysql.Table) {
	t.Helper()
	tbl := mysql.NewOutboxTable(integration.UniqueName(t, "outbox"))
	dropMySQL(t, db, tbl)
	for _, stmt := range mysql.OutboxDDL(tbl) {
		execMySQL(t, db, stmt)
	}
	return mysql.NewOutbox(db, tbl.Name()), tbl
}

func mysqlCountRows(t *testing.T, db *mysql.DB, tbl *mysql.Table) int64 {
	t.Helper()
	var n int64
	if err := db.Select(mysql.CountAll()).From(tbl).One(context.Background(), &n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// The whole point of the pattern: the event and the business write
// commit together, and a drain after the commit sees it.
func TestMySQLOutboxEmitDrainPublish(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	ob, tbl := mysqlOutboxFixture(t, db)

	var emitted int64
	err := db.InTx(ctx, func(tx *mysql.DB) error {
		id, err := ob.EmitWithID(tx, ctx, "user.created", map[string]any{"id": 7}, mysql.EmitOptions{
			AggregateType: "user",
			AggregateID:   "7",
			Headers:       map[string]string{"traceparent": "00-abc"},
		})
		emitted = id
		return err
	})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if emitted == 0 {
		t.Fatal("MySQL has no RETURNING, so LastInsertId is the only way back to the id — and it came back 0")
	}

	events, err := ob.Drain(ctx, 10)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("drained %d events, want 1", len(events))
	}
	e := events[0]
	if e.ID != emitted {
		t.Errorf("drained id %d, but the emit reported %d", e.ID, emitted)
	}
	if e.Kind != "user.created" || e.AggregateType != "user" || e.AggregateID != "7" {
		t.Errorf("event = %+v", e)
	}
	if e.Headers["traceparent"] != "00-abc" {
		t.Errorf("headers = %v", e.Headers)
	}
	if string(e.Payload) != `{"id":7}` {
		t.Errorf("payload = %s", e.Payload)
	}
	if time.Since(e.CreatedAt) > time.Minute || time.Since(e.CreatedAt) < -time.Minute {
		t.Errorf("createdAt = %v, which is not close to now — a time zone is being applied somewhere", e.CreatedAt)
	}

	if err := ob.MarkPublished(ctx, e.ID); err != nil {
		t.Fatalf("markPublished: %v", err)
	}
	again, err := ob.Drain(ctx, 10)
	if err != nil {
		t.Fatalf("second drain: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("a published event was drained again: %+v", again)
	}
	if n := mysqlCountRows(t, db, tbl); n != 1 {
		t.Errorf("publishing must not delete the row, %d rows left", n)
	}
}

// A rolled-back business write takes its event with it. This is the
// property that makes the outbox worth having over publishing inline.
func TestMySQLOutboxEmitRollsBackWithItsTransaction(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	ob, _ := mysqlOutboxFixture(t, db)

	boom := errors.New("business rule said no")
	err := db.InTx(ctx, func(tx *mysql.DB) error {
		if err := ob.Emit(tx, ctx, "user.created", nil); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	events, err := ob.Drain(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Errorf("a rolled-back emit left %d events behind", len(events))
	}
}

// What the live server actually does with SKIP LOCKED, and whether
// ProbeLocking agrees with it.
func TestMySQLOutboxProbeLockingMatchesTheServer(t *testing.T) {
	db := openMySQL(t)
	ob, _ := mysqlOutboxFixture(t, db)

	mode, err := ob.ProbeLocking(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	want := mysql.LockForUpdate
	if mysqlHasSkipLocked(t, db) {
		want = mysql.LockSkipLocked
	}
	if mode != want {
		major, minor, mariadb := mysqlServerVersion(t, db)
		t.Errorf("probe chose %v on version %d.%d (mariadb=%v), want %v", mode, major, minor, mariadb, want)
	}
}

// SKIP LOCKED is what lets a stalled consumer not block the others:
// the second worker must step over the row the first is holding rather
// than wait for it or take it twice.
func TestMySQLOutboxSkipLockedStepsOverAHeldRow(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	if !mysqlHasSkipLocked(t, db) {
		t.Skip("server predates FOR UPDATE SKIP LOCKED (MySQL 8.0 / MariaDB 10.6)")
	}
	ob, _ := mysqlOutboxFixture(t, db)

	for i := 0; i < 2; i++ {
		if err := db.InTx(ctx, func(tx *mysql.DB) error {
			return ob.Emit(tx, ctx, "e", map[string]int{"i": i})
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	err := db.InTx(ctx, func(tx *mysql.DB) error {
		// This drain runs inside the transaction, so its row lock
		// survives until commit — which is what the second worker
		// has to step over.
		held, err := mysql.NewOutbox(tx, ob.Table()).Drain(ctx, 1)
		if err != nil {
			return err
		}
		if len(held) != 1 {
			t.Fatalf("first worker drained %d rows", len(held))
		}
		other, err := ob.Drain(ctx, 1)
		if err != nil {
			return err
		}
		if len(other) != 1 {
			t.Fatalf("second worker drained %d rows; it should have skipped to the next one", len(other))
		}
		if other[0].ID == held[0].ID {
			t.Errorf("both workers took event %d — SKIP LOCKED did not exclude", held[0].ID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
}

// The fallback for a server without SKIP LOCKED has to be the one that
// stalls, not the one that duplicates. Under LockForUpdate the second
// drain waits on the first worker's lock; with the wait bounded to a
// second it surfaces as a lock-wait timeout, which is the honest
// answer — never the same row handed out twice.
func TestMySQLOutboxBlockingFallbackWaitsRatherThanDuplicates(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	ob, _ := mysqlOutboxFixture(t, db)
	ob.WithLocking(mysql.LockForUpdate)

	if err := db.InTx(ctx, func(tx *mysql.DB) error {
		return ob.Emit(tx, ctx, "e", nil)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := db.InTx(ctx, func(tx *mysql.DB) error {
		held, err := mysql.NewOutbox(tx, ob.Table()).WithLocking(mysql.LockForUpdate).Drain(ctx, 1)
		if err != nil || len(held) != 1 {
			t.Fatalf("first worker: %d rows, %v", len(held), err)
		}
		return db.InTx(ctx, func(tx2 *mysql.DB) error {
			if _, err := tx2.Exec(ctx, "SET SESSION innodb_lock_wait_timeout = 1"); err != nil {
				return err
			}
			second, err := mysql.NewOutbox(tx2, ob.Table()).WithLocking(mysql.LockForUpdate).Drain(ctx, 1)
			if err == nil {
				t.Errorf("the second drain returned %d rows instead of waiting", len(second))
				return nil
			}
			var me *mysqldriver.MySQLError
			if !errors.As(err, &me) || me.Number != 1205 {
				t.Errorf("err = %v, want error 1205 (lock wait timeout)", err)
			}
			return nil
		})
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
}

// The per-aggregate lock is MySQL's GET_LOCK, which is session-scoped
// rather than transaction-scoped. Two sessions must not hold it at
// once, and the second must give up immediately rather than queue.
func TestMySQLOutboxAggregateLockExcludesASecondSession(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	ob, _ := mysqlOutboxFixture(t, db)

	for i := 0; i < 2; i++ {
		if err := db.InTx(ctx, func(tx *mysql.DB) error {
			return ob.EmitWith(tx, ctx, "e", map[string]int{"i": i},
				mysql.EmitOptions{AggregateType: "cart", AggregateID: "9"})
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	held := make(chan struct{})
	release := make(chan struct{})
	var inner int32
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := ob.DrainAggregate(ctx, "cart", "9", 10, func(tx *mysql.DB, events []mysql.OutboxEvent) error {
			close(held)
			<-release
			return nil
		})
		if err != nil {
			t.Errorf("holder: %v", err)
		}
	}()

	<-held
	err := ob.DrainAggregate(ctx, "cart", "9", 10, func(*mysql.DB, []mysql.OutboxEvent) error {
		atomic.AddInt32(&inner, 1)
		return nil
	})
	close(release)
	wg.Wait()
	if err != nil {
		t.Fatalf("a lock another session holds is a skip, not an error: %v", err)
	}
	if atomic.LoadInt32(&inner) != 0 {
		t.Error("two sessions processed one aggregate at the same time")
	}

	// And once the holder is done the lock is free again — which it
	// would not be if releasing had been left to the commit.
	ran := false
	if err := ob.DrainAggregate(ctx, "cart", "9", 10, func(*mysql.DB, []mysql.OutboxEvent) error {
		ran = true
		return nil
	}); err != nil {
		t.Fatalf("third drain: %v", err)
	}
	if !ran {
		t.Error("the named lock outlived its transaction — it must be released explicitly")
	}
}

// Cleanup keeps the newest row so the AUTO_INCREMENT counter has a
// floor. An outbox emptied to zero rows can hand out an id that has
// already been delivered, and drops/mirror versions a replicated row
// by that id.
func TestMySQLOutboxCleanupLeavesAnIDFloor(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	ob, tbl := mysqlOutboxFixture(t, db)

	var ids []int64
	for i := 0; i < 3; i++ {
		if err := db.InTx(ctx, func(tx *mysql.DB) error {
			id, err := ob.EmitWithID(tx, ctx, "e", nil, mysql.EmitOptions{})
			ids = append(ids, id)
			return err
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := ob.MarkPublished(ctx, ids...); err != nil {
		t.Fatalf("markPublished: %v", err)
	}

	n, err := ob.Cleanup(ctx, 0)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if n != 2 {
		t.Errorf("cleanup removed %d rows, want 2 of 3", n)
	}
	if left := mysqlCountRows(t, db, tbl); left != 1 {
		t.Fatalf("%d rows left, want the newest one", left)
	}
	var kept int64
	if err := db.Select(drops.Raw("MAX(`id`)")).From(tbl).One(ctx, &kept); err != nil {
		t.Fatal(err)
	}
	if kept != ids[2] {
		t.Errorf("kept id %d, want the newest %d", kept, ids[2])
	}

	// The next id still climbs past the floor.
	var next int64
	if err := db.InTx(ctx, func(tx *mysql.DB) error {
		id, err := ob.EmitWithID(tx, ctx, "e", nil, mysql.EmitOptions{})
		next = id
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if next <= kept {
		t.Errorf("next id %d did not advance past the retained %d", next, kept)
	}
}

// Timestamps are written from Go and never compared against the
// server's clock, so a session in another time zone changes nothing.
// A DATETIME carries no zone: were the drain predicate written against
// NOW(), this test would silently drain nothing.
func TestMySQLOutboxIgnoresTheSessionTimeZone(t *testing.T) {
	db, _ := openMySQLPinnedConn(t)
	ctx := context.Background()
	ob, _ := mysqlOutboxFixture(t, db)

	if _, err := db.Exec(ctx, "SET SESSION time_zone = '+05:00'"); err != nil {
		t.Fatalf("set time_zone: %v", err)
	}
	if err := db.InTx(ctx, func(tx *mysql.DB) error {
		return ob.Emit(tx, ctx, "e", nil)
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	events, err := ob.Drain(ctx, 10)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("drained %d events under a shifted session zone, want 1", len(events))
	}
	if d := time.Since(events[0].CreatedAt); d > time.Minute || d < -time.Minute {
		t.Errorf("createdAt is %v away from now — the session zone reached the value", d)
	}
}

// LastInsertId reports the FIRST id of a multi-row INSERT, which is
// why Emit writes one row per statement and there is no batch form:
// with innodb_autoinc_lock_mode=2 (MySQL 8's default) the block is not
// even guaranteed contiguous, so the other ids cannot be inferred.
func TestMySQLLastInsertIDOfABatchIsTheFirstRow(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl := mysql.NewTable(integration.UniqueName(t, "batch"))
	dropMySQL(t, db, tbl)
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	n := mysql.Add(tbl, mysql.Integer("n").NotNull())
	execMySQL(t, db, mysql.CreateTable(tbl))

	res, err := db.Insert(tbl).Row(n.Val(1)).Row(n.Val(2)).Row(n.Val(3)).Exec(ctx)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	first, ok := mysql.LastInsertID(res)
	if !ok {
		t.Fatal("the driver reported no LastInsertId")
	}
	var lo, hi int64
	if err := db.Select(drops.Raw("MIN(`id`)")).From(tbl).One(ctx, &lo); err != nil {
		t.Fatal(err)
	}
	if err := db.Select(drops.Raw("MAX(`id`)")).From(tbl).One(ctx, &hi); err != nil {
		t.Fatal(err)
	}
	if first != lo {
		t.Errorf("LastInsertId = %d, but the batch starts at %d", first, lo)
	}
	if first == hi {
		t.Errorf("a three-row insert reported the last id %d; the rest of the batch would be unreachable", hi)
	}
}

// --- event store --------------------------------------------------------

func mysqlEventStoreFixture(t *testing.T, db *mysql.DB) (*mysql.EventStore, *mysql.Table) {
	t.Helper()
	tbl := mysql.NewEventStoreTable(integration.UniqueName(t, "events"))
	dropMySQL(t, db, tbl)
	execMySQL(t, db, mysql.CreateTable(tbl))
	return mysql.NewEventStore(db, tbl.Name()), tbl
}

func TestMySQLEventStoreAppendAndReplay(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	store, _ := mysqlEventStoreFixture(t, db)

	err := db.InTx(ctx, func(tx *mysql.DB) error {
		return store.Append(tx, ctx, "match", "abc", -1,
			mysql.EventInput{Type: "matchStarted", Payload: map[string]int{"seed": 1}},
			mysql.EventInput{Type: "playerJoined", Payload: map[string]string{"who": "ada"},
				Headers: map[string]string{"traceparent": "00-x"}},
		)
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	events, err := store.Load(ctx, "match", "abc", -1)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("loaded %d events", len(events))
	}
	if events[0].Version != 0 || events[1].Version != 1 {
		t.Errorf("versions = %d, %d", events[0].Version, events[1].Version)
	}
	if events[1].Headers["traceparent"] != "00-x" {
		t.Errorf("headers = %v", events[1].Headers)
	}
	if events[0].Offset >= events[1].Offset {
		t.Errorf("the append offsets are not increasing: %d, %d", events[0].Offset, events[1].Offset)
	}

	// Resuming from the last version applied returns nothing.
	rest, err := store.Load(ctx, "match", "abc", events[1].Version)
	if err != nil || len(rest) != 0 {
		t.Errorf("resume returned %d events, %v", len(rest), err)
	}

	v, err := store.LatestVersion(ctx, "match", "abc")
	if err != nil || v != 1 {
		t.Errorf("LatestVersion = %d, %v", v, err)
	}
	if v, err := store.LatestVersion(ctx, "match", "never"); err != nil || v != -1 {
		t.Errorf("an empty stream should be -1, got %d, %v", v, err)
	}

	streamed, err := store.Stream(ctx, 0, 10)
	if err != nil || len(streamed) != 2 {
		t.Errorf("Stream returned %d events, %v", len(streamed), err)
	}
}

// The concurrency contract, pinned against two real transactions.
//
// MySQL defaults to REPEATABLE READ, so the loser's snapshot is taken
// before the winner commits and never advances: its LatestVersion says
// the stream is empty long after it is not. The unique key is what
// catches the collision, because a write is checked against the live
// index rather than the snapshot — and that is the whole reason this
// port cannot lean on a re-read the way a READ COMMITTED one could.
func TestMySQLEventStoreConflictSurvivesRepeatableRead(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	store, tbl := mysqlEventStoreFixture(t, db)

	if level := mysqlIsolationLevel(t, db); !strings.EqualFold(strings.ReplaceAll(level, "-", " "), "REPEATABLE READ") {
		t.Skipf("this test is about REPEATABLE READ; the session is %s", level)
	}

	loser, loserTx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = loserTx.Rollback(ctx) }()

	// Reading here is what fixes the loser's snapshot.
	loserStore := mysql.NewEventStore(loser, tbl.Name())
	before, err := loserStore.LatestVersion(ctx, "match", "abc")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if before != -1 {
		t.Fatalf("the stream should be empty, got version %d", before)
	}

	// A second transaction appends version 0 and commits.
	if err := db.InTx(ctx, func(tx *mysql.DB) error {
		return store.Append(tx, ctx, "match", "abc", -1, mysql.EventInput{Type: "won"})
	}); err != nil {
		t.Fatalf("winner: %v", err)
	}

	// The loser still sees an empty stream...
	if v, err := loserStore.LatestVersion(ctx, "match", "abc"); err != nil || v != -1 {
		t.Errorf("the snapshot moved: version = %d, %v — is this server really REPEATABLE READ?", v, err)
	}
	// ...and yet its append is refused, because the INSERT is checked
	// against the index rather than the snapshot.
	err = loserStore.Append(loser, ctx, "match", "abc", -1, mysql.EventInput{Type: "lost"})
	if !errors.Is(err, mysql.ErrConcurrencyConflict) {
		t.Fatalf("err = %v, want ErrConcurrencyConflict", err)
	}
	// The trap this port exists to document: re-reading the head
	// inside the failed transaction produces the same expected
	// version, so a retry loop that does not open a new transaction
	// spins forever.
	if v, err := loserStore.LatestVersion(ctx, "match", "abc"); err != nil || v != -1 {
		t.Errorf("after the conflict the snapshot answers %d, %v", v, err)
	}
	if v, err := store.LatestVersion(ctx, "match", "abc"); err != nil || v != 0 {
		t.Errorf("outside the transaction the head is %d, %v; want 0", v, err)
	}
}

// The other shape of the same race: the winner has not committed yet,
// so the loser's INSERT waits on its lock and collects the conflict
// when the winner lands.
func TestMySQLEventStoreSecondWriterWaitsThenConflicts(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	store, tbl := mysqlEventStoreFixture(t, db)

	winner, winnerTx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := mysql.NewEventStore(winner, tbl.Name()).
		Append(winner, ctx, "match", "abc", -1, mysql.EventInput{Type: "won"}); err != nil {
		_ = winnerTx.Rollback(ctx)
		t.Fatalf("winner append: %v", err)
	}

	result := make(chan error, 1)
	go func() {
		result <- db.InTx(ctx, func(tx *mysql.DB) error {
			return store.Append(tx, ctx, "match", "abc", -1, mysql.EventInput{Type: "lost"})
		})
	}()

	// The loser is now blocked on the winner's row lock. Committing
	// releases it and turns the wait into a duplicate-key error.
	select {
	case err := <-result:
		_ = winnerTx.Rollback(ctx)
		t.Fatalf("the second writer did not wait; it returned %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := winnerTx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, mysql.ErrConcurrencyConflict) {
			t.Fatalf("err = %v, want ErrConcurrencyConflict", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the second writer never unblocked")
	}
}

// Stream keys compare case-sensitively, which they would not under the
// server's default collation — two aggregates whose ids differ only in
// case would be one stream, and their versions would collide.
func TestMySQLEventStoreStreamKeysAreCaseSensitive(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	store, _ := mysqlEventStoreFixture(t, db)

	for _, id := range []string{"abc", "ABC"} {
		if err := db.InTx(ctx, func(tx *mysql.DB) error {
			return store.Append(tx, ctx, "match", id, -1, mysql.EventInput{Type: "started"})
		}); err != nil {
			t.Fatalf("append %q: %v", id, err)
		}
	}
	for _, id := range []string{"abc", "ABC"} {
		events, err := store.Load(ctx, "match", id, -1)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 || events[0].AggregateID != id {
			t.Errorf("stream %q loaded %d events: %+v", id, len(events), events)
		}
	}
}

func TestMySQLSnapshotUpsertReplacesInPlace(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	store, _ := mysqlEventStoreFixture(t, db)
	snaps := mysql.NewSnapshotTable(integration.UniqueName(t, "snaps"))
	dropMySQL(t, db, snaps)
	execMySQL(t, db, mysql.CreateTable(snaps))

	for _, v := range []int64{3, 9} {
		err := store.SaveSnapshot(ctx, snaps.Name(), mysql.AggregateSnapshot{
			AggregateType: "match", AggregateID: "abc", Version: v,
			State: json.RawMessage(`{"score":` + strconv.FormatInt(v, 10) + `}`),
		})
		if err != nil {
			t.Fatalf("save %d: %v", v, err)
		}
	}
	if n := mysqlCountRows(t, db, snaps); n != 1 {
		t.Errorf("%d snapshot rows, want the second to have replaced the first", n)
	}
	got, ok, err := store.LoadSnapshot(ctx, snaps.Name(), "match", "abc")
	if err != nil || !ok {
		t.Fatalf("load: ok=%v, err=%v", ok, err)
	}
	if got.Version != 9 || string(got.State) != `{"score":9}` {
		t.Errorf("snapshot = %+v", got)
	}
	if _, ok, err := store.LoadSnapshot(ctx, snaps.Name(), "match", "missing"); err != nil || ok {
		t.Errorf("a missing snapshot should report ok=false, got ok=%v err=%v", ok, err)
	}
}

// --- idempotency --------------------------------------------------------

func mysqlIdempotencyFixture(t *testing.T, db *mysql.DB, ttl time.Duration) (*mysql.IdempotencyStore, *mysql.Table) {
	t.Helper()
	tbl := mysql.NewIdempotencyTable(integration.UniqueName(t, "idem"))
	dropMySQL(t, db, tbl)
	execMySQL(t, db, mysql.CreateTable(tbl))
	return mysql.NewIdempotencyStore(db, tbl.Name(), ttl), tbl
}

// Two callers racing on one key: the closure runs once and both see
// the same answer. The loser's wait is on the row lock, and a locking
// read is what lets it observe the winner's commit under REPEATABLE
// READ, where a plain SELECT would still be looking at its snapshot.
func TestMySQLIdempotencySerialisesConcurrentCallers(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	store, _ := mysqlIdempotencyFixture(t, db, time.Hour)

	var runs int32
	start := make(chan struct{})
	results := make([][]byte, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = store.Run(ctx, "req-1", func(*mysql.DB) ([]byte, error) {
				atomic.AddInt32(&runs, 1)
				time.Sleep(100 * time.Millisecond)
				return []byte(`{"paymentId":1}`), nil
			})
		}(i)
	}
	close(start)
	wg.Wait()

	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if string(results[i]) != `{"paymentId":1}` {
			t.Errorf("caller %d got %s", i, results[i])
		}
	}
	if n := atomic.LoadInt32(&runs); n != 1 {
		t.Errorf("the closure ran %d times, want once", n)
	}
}

// A failed closure leaves no claim, so the next attempt with the same
// key really does retry.
func TestMySQLIdempotencyRetriesAfterAFailure(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	store, _ := mysqlIdempotencyFixture(t, db, time.Hour)

	boom := errors.New("declined")
	if _, err := store.Run(ctx, "req-2", func(*mysql.DB) ([]byte, error) {
		return nil, boom
	}); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	ran := false
	got, err := store.Run(ctx, "req-2", func(*mysql.DB) ([]byte, error) {
		ran = true
		return []byte(`ok`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Error("the retry did not re-run the closure")
	}
	if string(got) != "ok" {
		t.Errorf("result = %s", got)
	}
}

// Under the server's default collation "A1" and "a1" are one key, and
// one client would be handed another's response.
func TestMySQLIdempotencyKeysAreCaseSensitive(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	store, _ := mysqlIdempotencyFixture(t, db, time.Hour)

	for _, key := range []string{"a1", "A1"} {
		got, err := store.Run(ctx, key, func(*mysql.DB) ([]byte, error) {
			return []byte(key), nil
		})
		if err != nil {
			t.Fatalf("%q: %v", key, err)
		}
		if string(got) != key {
			t.Fatalf("key %q was served the response for %q", key, got)
		}
	}
}

// A response past 64KB would be truncated in a plain BLOB — silently,
// under a permissive sql_mode.
func TestMySQLIdempotencyStoresALargeResponse(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	store, _ := mysqlIdempotencyFixture(t, db, time.Hour)

	big := strings.Repeat("x", 200*1024)
	if _, err := store.Run(ctx, "big", func(*mysql.DB) ([]byte, error) {
		return []byte(big), nil
	}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	got, err := store.Run(ctx, "big", func(*mysql.DB) ([]byte, error) {
		t.Error("the second call should have been served from the table")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(got) != len(big) {
		t.Errorf("the cached response came back %d bytes, want %d", len(got), len(big))
	}
}

func TestMySQLIdempotencyCleanupReclaimsExpiredKeys(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	store, tbl := mysqlIdempotencyFixture(t, db, -time.Hour)

	if _, err := store.Run(ctx, "stale", func(*mysql.DB) ([]byte, error) {
		return []byte("v"), nil
	}); err != nil {
		t.Fatal(err)
	}
	// A negative ttl is clamped to the 24h default, so the row is not
	// expired yet and Cleanup must leave it alone.
	if n, err := store.Cleanup(ctx); err != nil || n != 0 {
		t.Errorf("cleanup removed %d rows, %v", n, err)
	}
	if n := mysqlCountRows(t, db, tbl); n != 1 {
		t.Fatalf("%d rows", n)
	}

	// Age it past its expiry and it goes.
	if _, err := db.Exec(ctx, "UPDATE "+mysql.Dialect.QuoteIdent(tbl.Name())+" SET `expiresAt` = ?", time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n, err := store.Cleanup(ctx); err != nil || n != 1 {
		t.Errorf("cleanup removed %d rows, %v", n, err)
	}
}

// --- the version contract drops/mirror depends on -----------------------

// drops/mirror versions a mirrored row by the outbox event id, which
// works only if ids agree with commit order per key. They do here for
// the same reason they do on PostgreSQL: an emitter that mutates a row
// and emits in one transaction makes the next writer of that row wait
// on the row lock, so the second writer cannot take an id ahead of the
// first.
func TestMySQLOutboxIDsFollowCommitOrderPerKey(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	ob, _ := mysqlOutboxFixture(t, db)

	rows := mysql.NewTable(integration.UniqueName(t, "docs"))
	dropMySQL(t, db, rows)
	docID := mysql.Add(rows, mysql.BigInt("id").PrimaryKey())
	title := mysql.Add(rows, mysql.Varchar("title", 64).NotNull())
	execMySQL(t, db, mysql.CreateTable(rows))
	if _, err := db.Insert(rows).Row(docID.Val(int64(1)), title.Val("first")).Exec(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}

	first, firstTx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Update(rows).Set(title.Val("one")).Where(docID.Eq(int64(1))).Exec(ctx); err != nil {
		_ = firstTx.Rollback(ctx)
		t.Fatalf("first update: %v", err)
	}

	secondID := make(chan int64, 1)
	secondErr := make(chan error, 1)
	go func() {
		var id int64
		err := db.InTx(ctx, func(tx *mysql.DB) error {
			// Blocks on the row lock the first transaction holds.
			if _, err := tx.Update(rows).Set(title.Val("two")).Where(docID.Eq(int64(1))).Exec(ctx); err != nil {
				return err
			}
			var e error
			id, e = ob.EmitWithID(tx, ctx, "doc.updated", map[string]string{"title": "two"},
				mysql.EmitOptions{AggregateType: "doc", AggregateID: "1"})
			return e
		})
		secondID <- id
		secondErr <- err
	}()

	// Give the second writer time to reach the lock, then emit and
	// commit — so the first writer's id is taken second in wall-clock
	// terms if the lock did not order them.
	time.Sleep(200 * time.Millisecond)
	firstEventID, err := ob.EmitWithID(first, ctx, "doc.updated", map[string]string{"title": "one"},
		mysql.EmitOptions{AggregateType: "doc", AggregateID: "1"})
	if err != nil {
		_ = firstTx.Rollback(ctx)
		t.Fatalf("first emit: %v", err)
	}
	if err := firstTx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := <-secondErr; err != nil {
		t.Fatalf("second writer: %v", err)
	}
	secondEventID := <-secondID
	if secondEventID <= firstEventID {
		t.Errorf("the second committer took id %d, at or below the first's %d — per-key order would be inverted in a mirror",
			secondEventID, firstEventID)
	}
}

// The worker end to end against the server: per-aggregate ordering
// takes the named lock, delivers in id order inside it, and marks the
// rows published in the same transaction. Events with no aggregate
// still flow, through the unordered pass the same tick makes.
func TestMySQLOutboxWorkerDeliversInAggregateOrder(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	ob, _ := mysqlOutboxFixture(t, db)

	for i := 0; i < 3; i++ {
		if err := db.InTx(ctx, func(tx *mysql.DB) error {
			return ob.EmitWith(tx, ctx, "cart.updated", map[string]int{"i": i},
				mysql.EmitOptions{AggregateType: "cart", AggregateID: "9"})
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := db.InTx(ctx, func(tx *mysql.DB) error {
		return ob.Emit(tx, ctx, "unaggregated", nil)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var delivered []int64
	worker := mysql.NewOutboxWorker(ob).
		WithOrdering(mysql.OrderingPerAggregate).
		OnEvent(func(_ context.Context, e mysql.OutboxEvent) error {
			delivered = append(delivered, e.ID)
			return nil
		})
	if err := worker.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(delivered) != 4 {
		t.Fatalf("delivered %d events, want 4: %v", len(delivered), delivered)
	}
	for i := 1; i < len(delivered); i++ {
		if delivered[i] <= delivered[i-1] {
			t.Errorf("out of order: %v", delivered)
			break
		}
	}
	rest, err := ob.Drain(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 0 {
		t.Errorf("%d events were not marked published", len(rest))
	}
}

// A failing handler reschedules its row instead of losing it, and the
// attempt count survives on the row rather than in the worker.
func TestMySQLOutboxWorkerRetriesAFailedEvent(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	ob, _ := mysqlOutboxFixture(t, db)

	if err := db.InTx(ctx, func(tx *mysql.DB) error {
		return ob.Emit(tx, ctx, "flaky", nil)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	attempts := 0
	worker := mysql.NewOutboxWorker(ob).
		WithBackoff(func(int) time.Duration { return 0 }).
		OnEvent(func(_ context.Context, e mysql.OutboxEvent) error {
			attempts++
			if attempts == 1 {
				if e.Attempts != 0 {
					t.Errorf("first delivery already had %d attempts", e.Attempts)
				}
				return errors.New("broker down")
			}
			if e.Attempts != 1 {
				t.Errorf("the retry saw %d attempts, want 1 — the count must live on the row", e.Attempts)
			}
			return nil
		})
	if err := worker.Tick(ctx); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	if err := worker.Tick(ctx); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("the handler ran %d times, want 2", attempts)
	}
	rest, err := ob.Drain(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 0 {
		t.Errorf("the event was not published on the retry: %+v", rest)
	}
}

// The safety analyser's online-DDL classification is a claim about what
// the server will do, so the server is asked. Appending ALGORITHM= to a
// statement makes both servers commit: they refuse the statement
// outright rather than silently falling back to the slower plan, which
// is exactly what makes the suggestion in the copy-table warning worth
// following.
func TestMySQLServerAgreesWithTheDDLClassification(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	server, err := mysql.ServerVersion(ctx, db)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	tbl := mysql.NewTable(integration.UniqueName(t, "algo"))
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(tbl, mysql.Integer("n"))
	dropMySQL(t, db, tbl)
	execMySQL(t, db, mysql.CreateTable(tbl))

	name := tbl.Name()
	// Classified INPLACE: an index build, which runs without blocking
	// writes.
	inplace := "ALTER TABLE `" + name + "` ADD INDEX `idx_n` (`n`)"
	if got := mysql.ClassifyDDL(inplace); got != mysql.AlgorithmInplace {
		t.Fatalf("ClassifyDDL(index build) = %v", got)
	}
	if _, err := db.Exec(ctx, inplace+", ALGORITHM=INPLACE, LOCK=NONE"); err != nil {
		t.Errorf("%s refused an index build the classifier calls INPLACE: %v", server, err)
	}

	// Classified COPY: a column type change, which has no in-place
	// implementation on either server.
	copying := "ALTER TABLE `" + name + "` MODIFY COLUMN `n` bigint NULL"
	if got := mysql.ClassifyDDL(copying); got != mysql.AlgorithmCopy {
		t.Fatalf("ClassifyDDL(type change) = %v", got)
	}
	if _, err := db.Exec(ctx, copying+", ALGORITHM=INPLACE, LOCK=NONE"); err == nil {
		t.Errorf("%s accepted a column type change as INPLACE; the classifier calls it a table copy", server)
	}
	// And it succeeds when the server is allowed to copy.
	if _, err := db.Exec(ctx, copying); err != nil {
		t.Errorf("the same statement without an algorithm: %v", err)
	}

	// Classified INSTANT: adding a column, on a server new enough to
	// have the algorithm at all.
	instantly := server.MariaDB && server.AtLeast(10, 3, 2) ||
		!server.MariaDB && server.AtLeast(8, 0, 12)
	if !instantly {
		t.Skipf("%s predates ALGORITHM=INSTANT", server)
	}
	adding := "ALTER TABLE `" + name + "` ADD COLUMN `m` int NULL"
	if got := mysql.ClassifyDDL(adding); got != mysql.AlgorithmInstant {
		t.Fatalf("ClassifyDDL(add column) = %v", got)
	}
	if _, err := db.Exec(ctx, adding+", ALGORITHM=INSTANT"); err != nil {
		t.Errorf("%s refused an INSTANT column add: %v", server, err)
	}

	// Also classified INPLACE: dropping a column, which rewrites every
	// row but does not copy the table.
	dropping := "ALTER TABLE `" + name + "` DROP COLUMN `m`"
	if got := mysql.ClassifyDDL(dropping); got != mysql.AlgorithmInplace {
		t.Fatalf("ClassifyDDL(drop column) = %v", got)
	}
	if _, err := db.Exec(ctx, dropping+", ALGORITHM=INPLACE, LOCK=NONE"); err != nil {
		t.Errorf("%s refused an in-place column drop: %v", server, err)
	}

	// Classified COPY, and the one that reads like metadata and is not:
	// adding a CHECK has no in-place implementation on either server.
	// MariaDB 10.11 answers error 1845 and MySQL documents the rebuild.
	if server.SupportsCheckConstraints() {
		checking := "ALTER TABLE `" + name + "` ADD CONSTRAINT `ck_algo_n` CHECK (`n` >= 0)"
		if got := mysql.ClassifyDDL(checking); got != mysql.AlgorithmCopy {
			t.Fatalf("ClassifyDDL(add check) = %v", got)
		}
		if _, err := db.Exec(ctx, checking+", ALGORITHM=INPLACE, LOCK=NONE"); err == nil {
			t.Errorf("%s added a CHECK constraint in place; the classifier calls it a table copy", server)
		}
		if _, err := db.Exec(ctx, checking); err != nil {
			t.Errorf("the same statement without an algorithm: %v", err)
		}
	}

	// The one verdict that depends on a server setting rather than on
	// the statement. ADD FOREIGN KEY is classified INPLACE, which it is
	// only while foreign_key_checks is off; under the default the
	// server refuses INPLACE and copies. ClassifyDDL is documented as a
	// lower bound, so this pins the caveat rather than the verdict.
	parent := mysql.NewTable(integration.UniqueName(t, "algo_parent"))
	mysql.Add(parent, mysql.BigSerial("id").PrimaryKey())
	dropMySQL(t, db, parent)
	execMySQL(t, db, mysql.CreateTable(parent))
	if _, err := db.Exec(ctx, "ALTER TABLE `"+name+"` ADD COLUMN `pid` bigint NULL"); err != nil {
		t.Fatalf("adding the child column: %v", err)
	}
	linking := "ALTER TABLE `" + name + "` ADD CONSTRAINT `fk_algo_pid` FOREIGN KEY (`pid`) REFERENCES `" +
		parent.Name() + "` (`id`)"
	if got := mysql.ClassifyDDL(linking); got != mysql.AlgorithmInplace {
		t.Fatalf("ClassifyDDL(add foreign key) = %v", got)
	}
	if _, err := db.Exec(ctx, linking+", ALGORITHM=INPLACE, LOCK=NONE"); err == nil {
		t.Logf("%s added a foreign key in place under the default foreign_key_checks", server)
	} else if !strings.Contains(err.Error(), "foreign_key_checks") {
		t.Errorf("%s refused the in-place foreign key for a reason other than foreign_key_checks: %v", server, err)
	}
}

// A user-level lock belongs to the session, and database/sql returns
// the session to the pool rather than closing it — so a release
// skipped because the caller's context was cancelled would strand the
// lock on an idle connection and starve that aggregate for good. The
// release runs detached for exactly this case.
func TestMySQLOutboxAggregateLockSurvivesACancelledContext(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	ob, tbl := mysqlOutboxFixture(t, db)

	if err := db.InTx(ctx, func(tx *mysql.DB) error {
		return ob.EmitWith(tx, ctx, "e", nil, mysql.EmitOptions{AggregateType: "cart", AggregateID: "9"})
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cancelCtx, cancel := context.WithCancel(ctx)
	err := ob.DrainAggregate(cancelCtx, "cart", "9", 10, func(*mysql.DB, []mysql.OutboxEvent) error {
		cancel()
		return nil
	})
	if err == nil {
		t.Fatal("the commit should have failed on the cancelled context")
	}

	// A different pool means a different session, which is the only
	// one that can tell whether the lock was really given back.
	other := openMySQL(t)
	ran := false
	if err := mysql.NewOutbox(other, tbl.Name()).
		DrainAggregate(ctx, "cart", "9", 10, func(*mysql.DB, []mysql.OutboxEvent) error {
			ran = true
			return nil
		}); err != nil {
		t.Fatalf("second session: %v", err)
	}
	if !ran {
		t.Error("the lock was stranded on the cancelled caller's pooled connection")
	}
}

// ----------------------------------------------------------------------
// Expression library: CTEs, subqueries, JSON, strings, math, date/time,
// window functions, Entity.Patch.
//
// Every case here asserts the value the server *returned*. A rendered
// string cannot tell LOG from LOG10, LENGTH from CHAR_LENGTH, or
// PostgreSQL's strpos argument order from MySQL's; a returned value
// can, and those three are exactly where a port goes wrong.
// ----------------------------------------------------------------------

// mysqlText renders whatever the driver handed back as a comparable
// string. parseTime=true means a DATETIME arrives as a time.Time, so
// the time formatting is not incidental.
func mysqlText(v any) string {
	switch x := v.(type) {
	case nil:
		return "<nil>"
	case []byte:
		return string(x)
	case time.Time:
		return x.Format("2006-01-02 15:04:05.999999")
	default:
		return fmt.Sprint(x)
	}
}

// mysqlValue runs "SELECT <e>" and returns the single scalar it
// produced, already stringified. A statement the server rejects fails
// the test with the SQL that was sent — the only useful diagnostic
// when a helper renders something no server accepts.
func mysqlValue(t *testing.T, db *mysql.DB, e drops.Expression) string {
	t.Helper()
	rows, err := db.Select(e).Rows(context.Background())
	if err != nil {
		text, args := drops.StringWithDialect(mysql.Dialect, e)
		t.Fatalf("MySQL rejected the expression: %v\n%s\nargs: %v", err, text, args)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("expression returned no row")
	}
	var v any
	if err := rows.Scan(&v); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return mysqlText(v)
}

// mysqlValueErr is mysqlValue for the cases that are meant to fail on
// one server and not the other.
func mysqlValueErr(t *testing.T, db *mysql.DB, e drops.Expression) (string, error) {
	t.Helper()
	rows, err := db.Select(e).Rows(context.Background())
	if err != nil {
		return "", err
	}
	defer rows.Close()
	if !rows.Next() {
		return "", rows.Err()
	}
	var v any
	if err := rows.Scan(&v); err != nil {
		return "", err
	}
	return mysqlText(v), rows.Err()
}

type exprCase struct {
	name string
	expr drops.Expression
	want string
}

func runExprCases(t *testing.T, db *mysql.DB, cases []exprCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mysqlValue(t, db, tc.expr); got != tc.want {
				text, args := drops.StringWithDialect(mysql.Dialect, tc.expr)
				t.Errorf("value = %q, want %q\n%s\nargs: %v", got, tc.want, text, args)
			}
		})
	}
}

func TestMySQLStringFunctionValues(t *testing.T) {
	db := openMySQL(t)
	runExprCases(t, db, []exprCase{
		// CONCAT propagates NULL where PostgreSQL's concat() would
		// have skipped it and answered "ab". This is the difference
		// that reaches production silently, so it is pinned.
		{"concat", mysql.Concat("a", "b"), "ab"},
		{"concat with a null argument is null", mysql.Concat("a", nil, "b"), "<nil>"},
		{"concat_ws skips nulls", mysql.ConcatWS("-", "a", nil, "b"), "a-b"},
		{"concat_ws with a null separator is null", mysql.ConcatWS(nil, "a", "b"), "<nil>"},

		// Length is characters, OctetLength is bytes. "é" is two bytes
		// in utf8mb4 and one character.
		{"Length counts characters", mysql.Length("héllo"), "5"},
		{"OctetLength counts bytes", mysql.OctetLength("héllo"), "6"},
		{"CharLength agrees with Length", mysql.CharLength("héllo"), "5"},

		// StrPos keeps PostgreSQL's (haystack, needle) order over a
		// function whose own order is the reverse.
		{"StrPos takes the haystack first", mysql.StrPos("hello", "ll"), "3"},
		{"Locate takes the needle first", mysql.Locate("ll", "hello"), "3"},
		{"Locate from an offset", mysql.Locate("l", "hello", 4), "4"},
		{"Position", mysql.Position("ll", "hello"), "3"},

		{"Substring is 1-based", mysql.Substring("hello", 2, 3), "ell"},
		{"Substring with no count", mysql.Substring("hello", 2, nil), "ello"},
		{"Left", mysql.Left("hello", 2), "he"},
		{"Right", mysql.Right("hello", 2), "lo"},

		{"Trim", mysql.Trim("  x  "), "x"},
		{"LTrim", mysql.LTrim("  x"), "x"},
		{"RTrim", mysql.RTrim("x  "), "x"},
		{"TrimChars BOTH", mysql.TrimChars("BOTH", "x", "xaxx"), "a"},
		{"TrimChars LEADING", mysql.TrimChars("LEADING", "x", "xax"), "ax"},
		{"TrimChars TRAILING", mysql.TrimChars("TRAILING", "x", "xax"), "xa"},

		{"Replace", mysql.Replace("hello", "l", "L"), "heLLo"},
		{"LPad", mysql.LPad("a", 3, "-"), "--a"},
		{"RPad", mysql.RPad("a", 3, "-"), "a--"},
		{"Repeat", mysql.Repeat("ab", 2), "abab"},
		{"Reverse", mysql.Reverse("abc"), "cba"},
		{"SubstringIndex", mysql.SubstringIndex("a.b.c", ".", 2), "a.b"},

		// The three-argument REGEXP_REPLACE is the portable one; the
		// case flag rides in the pattern instead of a fourth argument
		// MariaDB rejects.
		{"RegexpReplace", mysql.RegexpReplace("hello", "l+", "L"), "heLo"},
		{"RegexpReplace with an inline flag", mysql.RegexpReplace("Hello", "(?i)h", "X"), "Xello"},
		{"RegexpSubstr", mysql.RegexpSubstr("hello", "l+"), "ll"},
		{"RegexpInstr", mysql.RegexpInstr("hello", "l+"), "3"},
		{"RegexpLike", mysql.RegexpLike("hello", "^h"), "1"},

		{"Md5", mysql.Md5("a"), "0cc175b9c0f1b6a831c399e269772661"},
		{"Hex", mysql.Hex("ab"), "6162"},
		{"Unhex", mysql.Unhex("6162"), "ab"},
		{"ToBase64", mysql.ToBase64("ab"), "YWI="},
		{"FromBase64", mysql.FromBase64("YWI="), "ab"},

		{"Lower", mysql.Lower("AÉ"), "aé"},
		{"Upper", mysql.Upper("aé"), "AÉ"},
		{"Coalesce", mysql.Coalesce(nil, "a"), "a"},
		{"NullIf", mysql.NullIf(1, 1), "<nil>"},
		{"IfNull", mysql.IfNull(nil, "a"), "a"},
	})
}

// REGEXP_SUBSTR disagrees about the no-match case: MySQL 8 answers
// NULL, MariaDB answers the empty string. Both are "no match", and a
// caller testing for one of them on the wrong server gets the opposite
// answer from a query that ran fine.
func TestMySQLRegexpSubstrMissesDifferently(t *testing.T) {
	db := openMySQL(t)
	_, _, mariadb := mysqlServerVersion(t, db)
	got := mysqlValue(t, db, mysql.RegexpSubstr("hello", "z+"))
	want := "<nil>"
	if mariadb {
		want = ""
	}
	if got != want {
		t.Errorf("no-match REGEXP_SUBSTR = %q, want %q (mariadb=%v)", got, want, mariadb)
	}
}

// Collate is the only way to make a comparison's case sensitivity a
// property of the query rather than of the schema, which is the whole
// reason drops has no ILike for MySQL.
func TestMySQLCollateForcesCaseSensitivity(t *testing.T) {
	db := openMySQL(t)
	insensitive := mysql.Eq(drops.Raw("'A'"), drops.Raw("'a'"))
	if got := mysqlValue(t, db, insensitive); got != "1" {
		t.Errorf("the default collation should compare case-insensitively, got %s", got)
	}
	sensitive := mysql.Eq(mysql.Collate(drops.Raw("'A'"), "utf8mb4_bin"), drops.Raw("'a'"))
	if got := mysqlValue(t, db, sensitive); got != "0" {
		t.Errorf("COLLATE utf8mb4_bin should compare case-sensitively, got %s", got)
	}
}

func TestMySQLMathFunctionValues(t *testing.T) {
	db := openMySQL(t)
	runExprCases(t, db, []exprCase{
		// The headline divergence: MySQL's LOG is the natural log and
		// PostgreSQL's log() is base 10. mysql.Log renders LOG10, so
		// log(10) is 1 on both dialects, and mysql.Ln is the other one.
		{"Log is base 10", mysql.Log(10), "1"},
		{"Log10", mysql.Log10(1000), "3"},
		{"Ln is natural", mysql.Ln(drops.Raw("EXP(1)")), "1"},
		{"Log2", mysql.Log2(8), "3"},
		{"LogBase reads base first", mysql.LogBase(2, 8), "3"},

		// The second: integer division. MySQL's / never truncates.
		{"Div does not truncate", mysql.Div(5, 2), "2.5000"},
		{"IntDiv truncates", mysql.IntDiv(5, 2), "2"},
		{"IntDiv truncates toward zero", mysql.IntDiv(-5, 2), "-2"},

		{"Abs", mysql.Abs(-3), "3"},
		{"Ceil", mysql.Ceil(1.2), "2"},
		{"Floor", mysql.Floor(1.8), "1"},
		{"Sign", mysql.Sign(-3), "-1"},
		{"Sqrt", mysql.Sqrt(9), "3"},
		{"Power", mysql.Power(2, 10), "1024"},
		{"Mod", mysql.Mod(5, 2), "1"},
		{"Round half away from zero", mysql.Round(drops.Raw("2.5")), "3"},
		{"Round negative half away from zero", mysql.Round(drops.Raw("-2.5")), "-3"},
		{"Round to digits", mysql.Round(drops.Raw("2.345"), 2), "2.35"},
		{"Truncate cuts rather than rounds", mysql.Truncate(drops.Raw("2.349"), 2), "2.34"},

		{"Greatest", mysql.Greatest(1, 5, 3), "5"},
		{"Least", mysql.Least(1, 5, 3), "1"},
		// Documented at the helper, and the reason SetIfGreater warns
		// about nullable columns.
		{"Greatest with a null is null", mysql.Greatest(1, nil), "<nil>"},

		{"Plus", mysql.Plus(1, 2), "3"},
		{"Minus", mysql.Minus(3, 1), "2"},
		{"Mul", mysql.Mul(3, 4), "12"},
		{"Cast to signed", mysql.Cast("12", "SIGNED"), "12"},
	})

	// Random has no fixed value; that it lands in [0, 1) is the claim.
	if got := mysqlValue(t, db, mysql.Lt(mysql.Random(), 1)); got != "1" {
		t.Errorf("Random() should be under 1, got comparison %s", got)
	}
}

func TestMySQLDateTimeFunctionValues(t *testing.T) {
	db := openMySQL(t)
	const ts = "2024-03-15 13:45:56.123456"
	runExprCases(t, db, []exprCase{
		// date_trunc does not exist on either server; these are the
		// rebuilt forms, and "week" truncating to Monday is what makes
		// them match PostgreSQL rather than MySQL's Sunday-based WEEK().
		{"DateTrunc second", mysql.DateTrunc("second", ts), "2024-03-15 13:45:56"},
		{"DateTrunc minute", mysql.DateTrunc("minute", ts), "2024-03-15 13:45:00"},
		{"DateTrunc hour", mysql.DateTrunc("hour", ts), "2024-03-15 13:00:00"},
		{"DateTrunc day", mysql.DateTrunc("day", ts), "2024-03-15 00:00:00"},
		{"DateTrunc week is Monday", mysql.DateTrunc("week", ts), "2024-03-11 00:00:00"},
		{"DateTrunc month", mysql.DateTrunc("month", ts), "2024-03-01 00:00:00"},
		{"DateTrunc quarter", mysql.DateTrunc("quarter", ts), "2024-01-01 00:00:00"},
		{"DateTrunc quarter, Q2", mysql.DateTrunc("quarter", "2024-05-15 00:00:00"), "2024-04-01 00:00:00"},
		{"DateTrunc year", mysql.DateTrunc("year", ts), "2024-01-01 00:00:00"},

		{"Extract YEAR", mysql.Extract("YEAR", ts), "2024"},
		{"Extract QUARTER", mysql.Extract("QUARTER", ts), "1"},
		{"Extract MICROSECOND", mysql.Extract("MICROSECOND", ts), "123456"},

		// MySQL numbers its days differently from PostgreSQL, which is
		// why these are separate helpers rather than an Extract unit.
		{"DayOfWeek numbers Sunday 1", mysql.DayOfWeek("2024-03-17"), "1"},
		{"Weekday numbers Monday 0", mysql.Weekday("2024-03-11"), "0"},
		{"DayOfYear", mysql.DayOfYear("2024-03-15"), "75"},

		{"MakeTime", mysql.MakeTime(1, 2, 3), "01:02:03"},
		// Not MySQL's MAKEDATE(year, dayofyear), which would answer
		// 2024-01-03 for these arguments.
		{"MakeDate keeps y/m/d", mysql.MakeDate(2024, 3, 15), "2024-03-15 00:00:00"},
		{"MakeTimestamp", mysql.MakeTimestamp(2024, 3, 15, 13, 45, 56), "2024-03-15 13:45:56"},

		{"DateFormat", mysql.DateFormat(ts, "%Y/%m/%d %H:%i:%s"), "2024/03/15 13:45:56"},
		{"StrToDate", mysql.StrToDate("15/03/2024", "%d/%m/%Y"), "2024-03-15 00:00:00"},

		{"TimestampDiff counts whole units", mysql.TimestampDiff("DAY", "2024-03-15", "2024-03-20"), "5"},
		{"TimestampDiff truncates", mysql.TimestampDiff("DAY", "2024-03-15 00:00:00", "2024-03-15 23:00:00"), "0"},

		{"LastDay", mysql.LastDay("2024-02-05"), "2024-02-29 00:00:00"},
		{"Quarter", mysql.Quarter("2024-05-01"), "2"},
		{"UnixTimestamp", mysql.UnixTimestamp("1970-01-01 00:00:00"), "0"},
		{"FromUnixTime", mysql.FromUnixTime(0), "1970-01-01 00:00:00"},
	})
}

func TestMySQLIntervalArithmetic(t *testing.T) {
	db := openMySQL(t)
	runExprCases(t, db, []exprCase{
		// An interval is syntax, not a value: it is only legal as an
		// operand of + / - or inside DATE_ADD / DATE_SUB.
		{"plus an interval", mysql.Plus("2020-01-01 00:00:00", mysql.Day(3)), "2020-01-04 00:00:00"},
		{"minus an interval", mysql.Minus("2020-01-04 00:00:00", mysql.Day(3)), "2020-01-01 00:00:00"},
		{"DateAdd", mysql.DateAdd("2020-01-01 00:00:00", mysql.Hour(6)), "2020-01-01 06:00:00"},
		{"DateSub", mysql.DateSub("2020-01-01 06:00:00", mysql.Minute(30)), "2020-01-01 05:30:00"},
		{"Interval with an explicit unit", mysql.DateAdd("2020-01-01 00:00:00", mysql.Interval(2, "quarter")), "2020-07-01 00:00:00"},
		// Month arithmetic clamps rather than overflowing, same as
		// PostgreSQL.
		{"month arithmetic clamps", mysql.DateAdd("2024-01-31 00:00:00", mysql.Month(1)), "2024-02-29 00:00:00"},
	})

	// SELECT INTERVAL 1 DAY on its own is a syntax error on both
	// servers. Asserting that keeps the doc honest.
	if _, err := mysqlValueErr(t, db, mysql.Day(1)); err == nil {
		t.Error("a bare INTERVAL should not be a valid expression")
	}
}

// CONVERT_TZ answers NULL rather than raising when a named zone cannot
// be resolved, because the mysql.time_zone_name table is empty unless
// an administrator loaded it. Numeric offsets never need it.
func TestMySQLConvertTZNeedsTheTimeZoneTables(t *testing.T) {
	db := openMySQL(t)
	if got := mysqlValue(t, db, mysql.ConvertTZ("2024-03-15 12:00:00", "+00:00", "+02:00")); got != "2024-03-15 14:00:00" {
		t.Errorf("numeric offsets = %s, want 2024-03-15 14:00:00", got)
	}

	loaded := false
	rows, err := db.Query(context.Background(), "SELECT 1 FROM mysql.time_zone_name LIMIT 1")
	if err == nil {
		loaded = rows.Next()
		rows.Close()
	}
	got := mysqlValue(t, db, mysql.ConvertTZ("2024-03-15 12:00:00", "UTC", "Europe/Rome"))
	if loaded {
		if got == "<nil>" {
			t.Error("the time-zone tables are loaded, so a named zone should resolve")
		}
		return
	}
	if got != "<nil>" {
		t.Errorf("without the time-zone tables a named zone must answer NULL, got %s", got)
	}
}

// ----------------------------------------------------------------------
// Pagination and typed errors. A keyset walk is a sequence rather than
// a statement — every claim about it is a claim about what the server
// does between two queries — and an error number can only be pinned by
// causing it.
// ----------------------------------------------------------------------

// walkRow is the row type the pagination tests page over.
type walkRow struct {
	ID    int64  `drop:"id"`
	Sort  int64  `drop:"sortKey"`
	Label string `drop:"label"`
}

// mysqlWalkFixture builds a table of n rows with a covering index on
// (sortKey, id) — the shape a keyset walk is supposed to exploit.
func mysqlWalkFixture(t *testing.T, db *mysql.DB, n int) (*mysql.Table, *mysql.Col[int64], *mysql.Col[int64], *mysql.Entity[walkRow], string) {
	t.Helper()
	ctx := context.Background()
	tbl := mysql.NewTable(integration.UniqueName(t, "walk"))
	dropMySQL(t, db, tbl)

	id := mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	sortKey := mysql.Add(tbl, mysql.BigInt("sortKey").NotNull())
	mysql.Add(tbl, mysql.Varchar("label", 64).NotNull())
	execMySQL(t, db, mysql.CreateTable(tbl))

	idxName := integration.UniqueName(t, "ix")
	execMySQL(t, db, mysql.NewIndex(idxName, tbl, sortKey, id))

	ent := mysql.NewEntity[walkRow](tbl)
	rows := make([]walkRow, 0, n)
	for i := 0; i < n; i++ {
		// Deliberately few distinct sort keys, so every page boundary
		// lands inside a group of ties and the tiebreaker has to carry
		// the walk.
		rows = append(rows, walkRow{Sort: int64(i % 17), Label: fmt.Sprintf("row%04d", i)})
	}
	for start := 0; start < len(rows); start += 100 {
		end := start + 100
		if end > len(rows) {
			end = len(rows)
		}
		if _, err := ent.CreateMany(db, ctx, rows[start:end]); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return tbl, id, sortKey, ent, idxName
}

// The whole promise of a keyset walk: a few hundred rows, paged over a
// non-unique sort key, and every row visited exactly once.
func TestMySQLKeysetWalkVisitsEveryRowExactlyOnce(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	const total = 400
	_, id, sortKey, ent, _ := mysqlWalkFixture(t, db, total)

	for _, dir := range []struct {
		name  string
		order []mysql.OrderingColumn
	}{
		{"ascending", []mysql.OrderingColumn{mysql.Asc(sortKey), mysql.Asc(id)}},
		{"descending", []mysql.OrderingColumn{mysql.Desc(sortKey), mysql.Desc(id)}},
	} {
		t.Run(dir.name, func(t *testing.T) {
			seen := map[int64]int{}
			cursor := ""
			pages := 0
			for {
				page, err := ent.Page(db).OrderBy(dir.order...).Limit(37).After(cursor).All(ctx)
				if err != nil {
					t.Fatalf("page %d: %v", pages, err)
				}
				for _, r := range page.Items {
					seen[r.ID]++
				}
				pages++
				if !page.HasMore {
					break
				}
				cursor = page.NextCursor
				if pages > 100 {
					t.Fatal("the walk did not terminate")
				}
			}
			if len(seen) != total {
				t.Errorf("visited %d distinct rows of %d", len(seen), total)
			}
			for rowID, count := range seen {
				if count != 1 {
					t.Errorf("row %d visited %d times", rowID, count)
				}
			}
			// 400 rows in pages of 37 is 11 pages: proof the walk paged
			// rather than fetching everything at once.
			if pages != 11 {
				t.Errorf("pages = %d, want 11", pages)
			}
		})
	}
}

// A walk is a sequence of independent queries, so rows move underneath
// it. What a keyset cursor guarantees — and OFFSET does not — is that
// no row already visited comes back, and no row that stayed put is
// missed, however much lands behind or ahead of the cursor.
func TestMySQLKeysetWalkSurvivesWritesUnderneath(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	const total = 300
	_, id, sortKey, ent, _ := mysqlWalkFixture(t, db, total)

	// The order the walk will follow, read once at the start, so the
	// test can reach ahead of the cursor and delete a row the walk has
	// not seen yet.
	ordered, err := ent.Query(db).OrderBy(sortKey.Asc(), id.Asc()).All(ctx)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if len(ordered) != total {
		t.Fatalf("seeded %d rows, not %d", len(ordered), total)
	}

	deleted := map[int64]bool{}
	seen := map[int64]int{}
	cursor := ""
	for page := 0; page < 50; page++ {
		p, err := ent.Page(db).OrderBy(mysql.Asc(sortKey), mysql.Asc(id)).Limit(25).After(cursor).All(ctx)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, r := range p.Items {
			seen[r.ID]++
		}
		if !p.HasMore {
			break
		}
		cursor = p.NextCursor

		// A row appended at the end of the sort order, which the walk
		// should still reach...
		if _, err := ent.CreateMany(db, ctx, []walkRow{
			{Sort: 999, Label: fmt.Sprintf("added%d", page)},
		}); err != nil {
			t.Fatalf("insert under the walk: %v", err)
		}
		// ...and a row ten places ahead of the cursor, which it must
		// not, because it will be gone before the walk arrives.
		if ahead := len(seen) + 10; ahead < len(ordered) {
			victim := ordered[ahead].ID
			if !deleted[victim] {
				if _, err := ent.Delete(db, ctx, victim); err != nil {
					t.Fatalf("delete under the walk: %v", err)
				}
				deleted[victim] = true
			}
		}
	}

	for rowID, count := range seen {
		if count != 1 {
			t.Errorf("row %d visited %d times — a keyset cursor must never repeat a row", rowID, count)
		}
	}
	for _, r := range ordered {
		switch {
		case deleted[r.ID] && seen[r.ID] > 0:
			t.Errorf("row %d was deleted ahead of the walk and still turned up", r.ID)
		case !deleted[r.ID] && seen[r.ID] == 0:
			t.Errorf("row %d stayed put for the whole walk and was skipped", r.ID)
		}
	}
	if len(deleted) == 0 {
		t.Fatal("the test deleted nothing, so it proved nothing")
	}
	t.Logf("%d rows deleted and %d inserted underneath a %d-row walk, and every surviving row was"+
		" visited exactly once", len(deleted), len(seen)-(total-len(deleted)), total)
}

// mysqlExplain runs EXPLAIN and returns the first row as a map, so a
// test can read "type" and "key" without depending on the column order,
// which differs between MySQL and MariaDB.
func mysqlExplain(t *testing.T, db *mysql.DB, query string, args ...any) map[string]string {
	t.Helper()
	rows, err := db.Query(context.Background(), "EXPLAIN "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN: %v\n%s", err, query)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("EXPLAIN columns: %v", err)
	}
	if !rows.Next() {
		t.Fatalf("EXPLAIN returned no rows for %s", query)
	}
	cells := make([]sql.NullString, len(cols))
	dest := make([]any, len(cols))
	for i := range cells {
		dest[i] = &cells[i]
	}
	if err := rows.Scan(dest...); err != nil {
		t.Fatalf("EXPLAIN scan: %v", err)
	}
	out := make(map[string]string, len(cols))
	for i, c := range cols {
		out[c] = cells[i].String
	}
	return out
}

// The guard drops emits has to be sargable, or keyset pagination is
// just OFFSET with extra steps. This is the test behind the claim in
// cursor.go's package comment: the expanded disjunction plans as a
// range on both servers, and the compact row-constructor spelling does
// not on MariaDB.
func TestMySQLKeysetGuardUsesTheIndex(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl, id, sortKey, _, idxName := mysqlWalkFixture(t, db, 1000)
	if _, err := db.Exec(ctx, "ANALYZE TABLE `"+tbl.Name()+"`"); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	spec := mysql.NewCursorSpec(
		mysql.OrderKey{Col: sortKey},
		mysql.OrderKey{Col: id},
	)
	cur, err := mysql.EncodeCursor(spec, int64(9), int64(500))
	if err != nil {
		t.Fatal(err)
	}
	query, args := db.Select(id).From(tbl).OrderByCursor(spec).AfterCursor(spec, cur).Limit(20).ToSQL()

	plan := mysqlExplain(t, db, query, args...)
	if plan["type"] != "range" {
		t.Errorf("the keyset guard planned as %q, not a range — the walk would scan\n%s\nplan: %v",
			plan["type"], query, plan)
	}
	if plan["key"] != idxName {
		t.Errorf("the guard used index %q, want %q", plan["key"], idxName)
	}

	// The compact spelling, for comparison. MySQL's optimiser has
	// turned a row constructor into a range since 5.7.3; MariaDB's
	// does not, which is why drops never emits this form.
	rowForm := "SELECT `id` FROM `" + tbl.Name() + "` WHERE (`sortKey`, `id`) > (?, ?) ORDER BY `sortKey`, `id` LIMIT 20"
	rowPlan := mysqlExplain(t, db, rowForm, int64(9), int64(500))
	_, _, mariadb := mysqlServerVersion(t, db)
	if mariadb {
		if rowPlan["type"] == "range" {
			t.Errorf("MariaDB planned the row constructor as a range after all: %v —"+
				" the comment in cursor.go needs updating", rowPlan)
		}
		t.Logf("MariaDB plans the row-constructor guard as %q (key %q); drops emits the expansion instead",
			rowPlan["type"], rowPlan["key"])
	} else {
		t.Logf("MySQL plans the row-constructor guard as %q (key %q)", rowPlan["type"], rowPlan["key"])
	}

	// The plan is a label; the claim is about work. A sargable guard
	// descends the index to the cursor and reads a page's worth of
	// entries, so the rows it steps over must not grow with the table.
	// Measuring it settles the question on a server whose optimiser
	// names its plans differently, and it is the number cursor.go's
	// package comment quotes.
	pinned, _ := openMySQLPinnedConn(t)
	guardReads := mysqlIndexStepsFor(t, pinned, query, args...)
	if guardReads > 200 {
		t.Errorf("the keyset guard stepped over %d index entries for a 20-row page of a 1000-row table;"+
			" it is scanning, not seeking\n%s", guardReads, query)
	}
	rowReads := mysqlIndexStepsFor(t, pinned, rowForm, int64(9), int64(500))
	t.Logf("index entries read: %d for the expansion drops emits, %d for the row constructor",
		guardReads, rowReads)
	if mariadb && rowReads <= guardReads*4 {
		t.Errorf("the row constructor read %d entries against the expansion's %d — on MariaDB it is"+
			" supposed to be far worse, and cursor.go's measurement says so", rowReads, guardReads)
	}
}

// mysqlIndexStepsFor runs query on a pinned connection and returns how
// many index entries the server stepped over to answer it, read from
// the session's Handler_read_next counter.
//
// The counter is per session, so the connection has to be the pinned
// one; the delta is taken rather than FLUSH STATUS because that needs
// RELOAD, which a test user has no business holding.
func mysqlIndexStepsFor(t *testing.T, db *mysql.DB, query string, args ...any) int64 {
	t.Helper()
	ctx := context.Background()
	before := mysqlSessionCounter(t, db, "Handler_read_next")
	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		t.Fatalf("measure: %v\n%s", err, query)
	}
	for rows.Next() {
		// Drained on purpose: the counter only moves as rows are read.
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		t.Fatalf("measure: %v", err)
	}
	_ = rows.Close()
	return mysqlSessionCounter(t, db, "Handler_read_next") - before
}

// mysqlSessionCounter reads one SHOW SESSION STATUS value. The
// statement is spelled out rather than read from information_schema
// because MySQL 8 moved SESSION_STATUS to performance_schema and
// MariaDB did not.
func mysqlSessionCounter(t *testing.T, db *mysql.DB, name string) int64 {
	t.Helper()
	rows, err := db.Query(context.Background(), "SHOW SESSION STATUS LIKE '"+name+"'")
	if err != nil {
		t.Fatalf("SHOW SESSION STATUS %s: %v", name, err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatalf("SHOW SESSION STATUS returned no row for %s", name)
	}
	var variable, value string
	if err := rows.Scan(&variable, &value); err != nil {
		t.Fatalf("scan %s: %v", name, err)
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		t.Fatalf("%s = %q: %v", name, value, err)
	}
	return n
}

// mysqlCollationTiesCase reports whether the server's default collation
// makes two strings differing only in case compare equal — which both
// servers' defaults do, MariaDB through utf8mb4_general_ci and MySQL 8
// through utf8mb4_0900_ai_ci.
func mysqlCollationTiesCase(t *testing.T, db *mysql.DB) bool {
	t.Helper()
	var equal int64
	if err := db.Select(drops.Raw("'alice' = 'Alice'")).One(context.Background(), &equal); err != nil {
		t.Fatalf("collation probe: %v", err)
	}
	return equal == 1
}

// The collation hazard, on the server rather than in a comment: under a
// case-insensitive collation a strict > steps over every casing of the
// value at once, so a walk keyed on a text column alone loses rows. The
// tiebreaker is what makes the same walk correct.
func TestMySQLCaseInsensitiveSortKeyNeedsATiebreaker(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	if !mysqlCollationTiesCase(t, db) {
		t.Skip("this server's default collation is case-sensitive; the hazard cannot arise")
	}

	tbl := mysql.NewTable(integration.UniqueName(t, "people"))
	dropMySQL(t, db, tbl)
	id := mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	name := mysql.Add(tbl, mysql.Varchar("name", 64).NotNull())
	execMySQL(t, db, mysql.CreateTable(tbl))
	execMySQL(t, db, mysql.NewIndex(integration.UniqueName(t, "ixn"), tbl, name, id))

	ent := mysql.NewEntity[personRow](tbl)
	var rows []personRow
	for i := 0; i < 10; i++ {
		base := fmt.Sprintf("person%02d", i)
		rows = append(rows,
			personRow{Name: strings.ToLower(base)},
			personRow{Name: strings.ToUpper(base)},
			personRow{Name: strings.ToUpper(base[:1]) + base[1:]},
		)
	}
	if _, err := ent.CreateMany(db, ctx, rows); err != nil {
		t.Fatalf("seed: %v", err)
	}
	total := len(rows)

	// The unsafe walk: name alone, with the tiebreaker check waived.
	unsafe := mysql.NewCursorSpec(mysql.OrderKey{Col: name}).AllowNonUniqueKey()
	seen := map[int64]int{}
	var cursor mysql.Cursor
	for page := 0; page < 50; page++ {
		q := db.Select(id, name).From(tbl).OrderByCursor(unsafe).Limit(4)
		if cursor != "" {
			q = q.AfterCursor(unsafe, cursor)
		}
		var got []personRow
		if err := q.All(ctx, &got); err != nil {
			t.Fatalf("unsafe walk: %v", err)
		}
		if len(got) == 0 {
			break
		}
		for _, r := range got {
			seen[r.ID]++
		}
		next, err := mysql.EncodeCursor(unsafe, got[len(got)-1].Name)
		if err != nil {
			t.Fatal(err)
		}
		cursor = next
	}
	if len(seen) >= total {
		t.Errorf("the unsafe walk visited %d of %d rows; the collation hazard did not materialise,"+
			" so the tiebreaker check is guarding against nothing on this server", len(seen), total)
	} else {
		t.Logf("keyed on the text column alone, the walk visited %d of %d rows — %d were skipped by a"+
			" case-insensitive tie", len(seen), total, total-len(seen))
	}

	// The same walk with the primary key appended visits every row.
	safe := map[int64]int{}
	cur := ""
	for page := 0; page < 50; page++ {
		p, err := ent.Page(db).OrderBy(mysql.Asc(name), mysql.Asc(id)).Limit(4).After(cur).All(ctx)
		if err != nil {
			t.Fatalf("safe walk: %v", err)
		}
		for _, r := range p.Items {
			safe[r.ID]++
		}
		if !p.HasMore {
			break
		}
		cur = p.NextCursor
	}
	if len(safe) != total {
		t.Errorf("the tiebroken walk visited %d of %d rows", len(safe), total)
	}
	for rowID, count := range safe {
		if count != 1 {
			t.Errorf("row %d visited %d times", rowID, count)
		}
	}
}

// personRow is the row type of the collation walk.
type personRow struct {
	ID   int64  `drop:"id"`
	Name string `drop:"name"`
}

// nullableRow carries a NULL-able sort key.
type nullableRow struct {
	ID   int64   `drop:"id"`
	Nick *string `drop:"nick"`
}

// Neither server takes NULLS FIRST / NULLS LAST, and both sort NULL as
// the smallest value. The guard has to encode that placement itself, so
// a walk over a nullable key is only correct if the server agrees with
// what keysetStrict assumes.
func TestMySQLNullableSortKeyWalksEveryRow(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()

	// First, the assumption the guard is built on.
	if _, err := db.Exec(ctx, "SELECT 1 ORDER BY 1 NULLS LAST"); err == nil {
		t.Error("this server accepts NULLS LAST; OrderKey could carry a Nulls field after all")
	}

	tbl := mysql.NewTable(integration.UniqueName(t, "nicks"))
	dropMySQL(t, db, tbl)
	id := mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	nick := mysql.Add(tbl, mysql.Varchar("nick", 32))
	execMySQL(t, db, mysql.CreateTable(tbl))
	execMySQL(t, db, mysql.NewIndex(integration.UniqueName(t, "ixk"), tbl, nick, id))

	ent := mysql.NewEntity[nullableRow](tbl)
	var rows []nullableRow
	for i := 0; i < 60; i++ {
		if i%3 == 0 {
			rows = append(rows, nullableRow{})
			continue
		}
		v := fmt.Sprintf("nick%02d", i%7)
		rows = append(rows, nullableRow{Nick: &v})
	}
	if _, err := ent.CreateMany(db, ctx, rows); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The server's placement, read back rather than assumed.
	var firstIsNull int64
	if err := db.Select(drops.Raw("`nick` IS NULL")).From(tbl).
		OrderBy(nick.Asc(), id.Asc()).Limit(1).One(ctx, &firstIsNull); err != nil {
		t.Fatalf("placement probe: %v", err)
	}
	if firstIsNull != 1 {
		t.Error("ascending order does not start with the NULLs — the guard assumes it does")
	}

	for _, dir := range []struct {
		name  string
		order []mysql.OrderingColumn
	}{
		{"ascending", []mysql.OrderingColumn{mysql.Asc(nick), mysql.Asc(id)}},
		{"descending", []mysql.OrderingColumn{mysql.Desc(nick), mysql.Desc(id)}},
	} {
		t.Run(dir.name, func(t *testing.T) {
			seen := map[int64]int{}
			cursor := ""
			for page := 0; page < 50; page++ {
				p, err := ent.Page(db).OrderBy(dir.order...).Limit(7).After(cursor).All(ctx)
				if err != nil {
					t.Fatalf("page %d: %v", page, err)
				}
				for _, r := range p.Items {
					seen[r.ID]++
				}
				if !p.HasMore {
					break
				}
				cursor = p.NextCursor
			}
			if len(seen) != len(rows) {
				t.Errorf("visited %d of %d rows across the NULL boundary", len(seen), len(rows))
			}
			for rowID, count := range seen {
				if count != 1 {
					t.Errorf("row %d visited %d times", rowID, count)
				}
			}
		})
	}
}

// A cursor is a token a client holds, and the client is where the next
// request comes from. Round-tripping it through a string — which is
// what a query parameter does — must not change where the walk resumes.
func TestMySQLCursorSurvivesTheRoundTripToTheClient(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	_, id, sortKey, ent, _ := mysqlWalkFixture(t, db, 40)

	first, err := ent.Page(db).OrderBy(mysql.Asc(sortKey), mysql.Asc(id)).Limit(10).All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.NextCursor == "" {
		t.Fatal("no cursor")
	}
	// URL-safe base64 and nothing else, so the token survives a query
	// string without escaping — which is where it spends its life.
	for i := 0; i < len(first.NextCursor); i++ {
		c := first.NextCursor[i]
		safe := c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_'
		if !safe {
			t.Fatalf("cursor %q contains %q, which a query string would escape", first.NextCursor, c)
		}
	}

	second, err := ent.Page(db).OrderBy(mysql.Asc(sortKey), mysql.Asc(id)).Limit(10).After(first.NextCursor).All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) == 0 {
		t.Fatal("the second page is empty")
	}
	for _, r := range second.Items {
		for _, seen := range first.Items {
			if r.ID == seen.ID {
				t.Errorf("row %d appeared on both pages", r.ID)
			}
		}
	}
}

// ----------------------------------------------------------------------
// Typed errors. Each one is pinned by causing it on the server: a
// number copied out of a manual is a number that can be wrong.
// ----------------------------------------------------------------------

func TestMySQLErrorsAreClassifiedByCausingThem(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()

	parent := mysql.NewTable(integration.UniqueName(t, "par"))
	dropMySQL(t, db, parent)
	parentID := mysql.Add(parent, mysql.BigInt("id").PrimaryKey())
	email := mysql.Add(parent, mysql.Varchar("email", 128).NotNull().Unique())
	execMySQL(t, db, mysql.CreateTable(parent))

	child := mysql.NewTable(integration.UniqueName(t, "chi"))
	dropMySQL(t, db, child)
	childID := mysql.Add(child, mysql.BigInt("id").PrimaryKey())
	ref := mysql.Add(child, mysql.BigInt("parentId").NotNull().References(parentID))
	execMySQL(t, db, mysql.CreateTable(child))

	seed := func() {
		if _, err := db.Insert(parent).
			Row(parentID.Val(int64(1)), email.Val("a@example.com")).
			Exec(ctx); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seed()

	t.Run("unique", func(t *testing.T) {
		_, err := db.Insert(parent).Row(parentID.Val(int64(2)), email.Val("a@example.com")).Exec(ctx)
		requireSentinel(t, err, mysql.ErrUniqueViolation, 1062)
		var se *mysql.ServerError
		if errors.As(err, &se) && se.Constraint == "" {
			t.Errorf("the server named the index in %q and it was not read out", err)
		}
	})

	t.Run("primary key is also a unique violation", func(t *testing.T) {
		_, err := db.Insert(parent).Row(parentID.Val(int64(1)), email.Val("b@example.com")).Exec(ctx)
		requireSentinel(t, err, mysql.ErrUniqueViolation, 1062)
	})

	t.Run("foreign key, missing parent", func(t *testing.T) {
		_, err := db.Insert(child).Row(childID.Val(int64(1)), ref.Val(int64(999))).Exec(ctx)
		requireSentinel(t, err, mysql.ErrForeignKeyViolation, 1452)
	})

	t.Run("foreign key, parent still referenced", func(t *testing.T) {
		if _, err := db.Insert(child).Row(childID.Val(int64(2)), ref.Val(int64(1))).Exec(ctx); err != nil {
			t.Fatalf("seed child: %v", err)
		}
		_, err := db.Delete(parent).Where(parentID.Eq(1)).Exec(ctx)
		requireSentinel(t, err, mysql.ErrForeignKeyViolation, 1451)
	})

	t.Run("not null", func(t *testing.T) {
		_, err := db.Exec(ctx, "INSERT INTO `"+parent.Name()+"` (`id`, `email`) VALUES (?, NULL)", int64(3))
		requireSentinel(t, err, mysql.ErrNotNullViolation, 1048)
	})

	t.Run("undefined table", func(t *testing.T) {
		var n int64
		err := db.Select(drops.Raw("1")).From(mysql.NewTable(integration.UniqueName(t, "nope"))).One(ctx, &n)
		requireSentinel(t, err, mysql.ErrUndefinedTable, 1146)
	})

	t.Run("undefined column", func(t *testing.T) {
		var n int64
		err := db.Select(drops.Raw("`nosuchcolumn`")).From(parent).One(ctx, &n)
		requireSentinel(t, err, mysql.ErrUndefinedColumn, 1054)
	})
}

// requireSentinel asserts both halves of the mapping: the sentinel a
// caller branches on, and the number it was derived from.
func requireSentinel(t *testing.T, err error, sentinel error, number uint16) {
	t.Helper()
	if err == nil {
		t.Fatalf("the statement succeeded; expected %v", sentinel)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("errors.Is(%v, %v) = false", err, sentinel)
	}
	var se *mysql.ServerError
	if !errors.As(err, &se) {
		t.Fatalf("%v is not a *mysql.ServerError", err)
	}
	if se.Number != number {
		t.Errorf("number = %d, want %d (%v)", se.Number, number, err)
	}
	// The driver's own error must still be reachable, so code that
	// already type-asserts on it keeps working.
	var me *mysqldriver.MySQLError
	if !errors.As(err, &me) {
		t.Errorf("the driver's *MySQLError was lost from the chain: %v", err)
	} else if me.Number != number {
		t.Errorf("driver number = %d, want %d", me.Number, number)
	}
}

// The CHECK constraint is the one classified failure whose number the
// two servers disagree about: 3819 on MySQL 8, 4025 on MariaDB. Both
// have to reach the same sentinel.
func TestMySQLCheckViolationIsClassifiedOnBothServers(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	server, err := mysql.ServerVersion(ctx, db)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if !server.SupportsCheckConstraints() {
		t.Skipf("%s parses CHECK constraints and discards them", server)
	}

	tbl := mysql.NewTable(integration.UniqueName(t, "checked"))
	dropMySQL(t, db, tbl)
	id := mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	n := mysql.Add(tbl, mysql.Integer("n").NotNull())
	// CreateTable does not render CHECK constraints — they belong to
	// the migration layer — so this one is declared by hand.
	if _, err := db.Exec(ctx, "CREATE TABLE `"+tbl.Name()+"` (`id` BIGINT NOT NULL,"+
		" `n` INT NOT NULL, PRIMARY KEY (`id`), CONSTRAINT `ckpos` CHECK (`n` > 0))"); err != nil {
		t.Fatalf("create: %v", err)
	}

	_, err = db.Insert(tbl).Row(id.Val(int64(1)), n.Val(int32(0))).Exec(ctx)
	if err == nil {
		t.Fatal("the CHECK was not enforced")
	}
	if !errors.Is(err, mysql.ErrCheckViolation) {
		t.Fatalf("errors.Is(%v, ErrCheckViolation) = false", err)
	}
	var se *mysql.ServerError
	if !errors.As(err, &se) {
		t.Fatalf("not a *ServerError: %v", err)
	}
	if se.Number != 3819 && se.Number != 4025 {
		t.Errorf("number = %d, want 3819 (MySQL) or 4025 (MariaDB)", se.Number)
	}
	t.Logf("%s reports a failed CHECK as error %d, constraint %q", server, se.Number, se.Constraint)
}

// mysqlLockRow is the row the contention tests fight over.
type mysqlLockRow struct {
	ID int64 `drop:"id"`
	N  int64 `drop:"n"`
}

// mysqlContentionFixture creates a two-row table and returns it.
func mysqlContentionFixture(t *testing.T, db *mysql.DB) (*mysql.Table, *mysql.Col[int64], *mysql.Col[int64]) {
	t.Helper()
	tbl := mysql.NewTable(integration.UniqueName(t, "locks"))
	dropMySQL(t, db, tbl)
	id := mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	n := mysql.Add(tbl, mysql.BigInt("n").NotNull())
	execMySQL(t, db, mysql.CreateTable(tbl))
	for _, v := range []int64{1, 2} {
		if _, err := db.Insert(tbl).Row(id.Val(v), n.Val(0)).Exec(context.Background()); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return tbl, id, n
}

// 1213 is the number the retry policy is built around, so it is worth
// causing rather than assuming: two transactions taking the same two
// rows in opposite orders, and InnoDB picking one of them as the
// victim.
func TestMySQLDeadlockIsClassified(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl, id, n := mysqlContentionFixture(t, db)

	a, _ := openMySQLPinnedConn(t)
	b, _ := openMySQLPinnedConn(t)
	txA, rawA, err := a.Begin(ctx)
	if err != nil {
		t.Fatalf("begin A: %v", err)
	}
	defer func() { _ = rawA.Rollback(ctx) }()
	txB, rawB, err := b.Begin(ctx)
	if err != nil {
		t.Fatalf("begin B: %v", err)
	}
	defer func() { _ = rawB.Rollback(ctx) }()

	bump := func(tx *mysql.DB, row int64) error {
		_, err := tx.Update(tbl).Set(n.Expr(drops.Raw("`n` + 1"))).Where(id.Eq(row)).Exec(ctx)
		return err
	}
	if err := bump(txA, 1); err != nil {
		t.Fatalf("A takes row 1: %v", err)
	}
	if err := bump(txB, 2); err != nil {
		t.Fatalf("B takes row 2: %v", err)
	}

	errs := make(chan error, 2)
	go func() { errs <- bump(txA, 2) }()
	go func() { errs <- bump(txB, 1) }()

	deadlocks := 0
	for i := 0; i < 2; i++ {
		err := <-errs
		if err == nil {
			continue
		}
		if !errors.Is(err, mysql.ErrDeadlock) {
			t.Fatalf("crossing update failed with %v, want a classified deadlock", err)
		}
		var se *mysql.ServerError
		if errors.As(err, &se) && se.Number != 1213 {
			t.Errorf("number = %d, want 1213", se.Number)
		}
		deadlocks++
	}
	if deadlocks != 1 {
		t.Errorf("%d of the two transactions were rolled back as victims, want exactly 1", deadlocks)
	}
}

// And the point of classifying it: InTx under a retry policy re-runs
// the victim's work instead of handing the deadlock to the caller.
func TestMySQLDeadlockIsRetriedToSuccess(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl, id, n := mysqlContentionFixture(t, db)

	a, _ := openMySQLPinnedConn(t)
	b, _ := openMySQLPinnedConn(t)
	policy := mysql.DefaultRetryPolicy()

	// Both transactions cross only on their first attempt: the barrier
	// makes the deadlock certain, and its absence on the retry makes
	// the retry able to finish.
	var barrier sync.WaitGroup
	barrier.Add(2)
	var attempts [2]int64

	run := func(db *mysql.DB, which int, first, second int64) error {
		return db.WithRetry(policy).InTx(ctx, func(tx *mysql.DB) error {
			attempt := atomic.AddInt64(&attempts[which], 1)
			bump := func(row int64) error {
				_, err := tx.Update(tbl).Set(n.Expr(drops.Raw("`n` + 1"))).Where(id.Eq(row)).Exec(ctx)
				return err
			}
			if err := bump(first); err != nil {
				return err
			}
			if attempt == 1 {
				barrier.Done()
				barrier.Wait()
			}
			return bump(second)
		})
	}

	results := make(chan error, 2)
	go func() { results <- run(a, 0, 1, 2) }()
	go func() { results <- run(b, 1, 2, 1) }()
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Errorf("a retried transaction still failed: %v", err)
		}
	}

	total := atomic.LoadInt64(&attempts[0]) + atomic.LoadInt64(&attempts[1])
	if total < 3 {
		t.Errorf("%d attempts in total: no deadlock materialised, so nothing was retried", total)
	}
	// Both transactions committed, so both rows were bumped twice.
	var sum int64
	if err := db.Select(mysql.Sum(n)).From(tbl).One(ctx, &sum); err != nil {
		t.Fatal(err)
	}
	if sum != 4 {
		t.Errorf("sum(n) = %d, want 4 — every increment must have landed exactly once", sum)
	}
}

// 1205 is the other retryable number, and the one whose semantics
// differ: the statement is rolled back, the transaction is not.
func TestMySQLLockWaitTimeoutIsClassified(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl, id, n := mysqlContentionFixture(t, db)

	holder, _ := openMySQLPinnedConn(t)
	waiter, _ := openMySQLPinnedConn(t)
	if _, err := waiter.Exec(ctx, "SET SESSION innodb_lock_wait_timeout = 1"); err != nil {
		t.Fatalf("set timeout: %v", err)
	}

	txHold, rawHold, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer func() { _ = rawHold.Rollback(ctx) }()
	if _, err := txHold.Update(tbl).Set(n.Val(1)).Where(id.Eq(1)).Exec(ctx); err != nil {
		t.Fatalf("holder takes the row: %v", err)
	}

	txWait, rawWait, err := waiter.Begin(ctx)
	if err != nil {
		t.Fatalf("begin waiter: %v", err)
	}
	defer func() { _ = rawWait.Rollback(ctx) }()
	start := time.Now()
	_, err = txWait.Update(tbl).Set(n.Val(2)).Where(id.Eq(1)).Exec(ctx)
	if err == nil {
		t.Fatal("the waiter got the row while the holder still had it")
	}
	if !errors.Is(err, mysql.ErrLockWaitTimeout) {
		t.Fatalf("waited %v and failed with %v, want a classified lock wait timeout", time.Since(start), err)
	}
	var se *mysql.ServerError
	if errors.As(err, &se) && se.Number != 1205 {
		t.Errorf("number = %d, want 1205", se.Number)
	}

	// The transaction is still open — this is the difference from a
	// deadlock, and the reason the retry is at the transaction level.
	var alive int64
	if err := txWait.Select(drops.Raw("1")).One(ctx, &alive); err != nil {
		t.Errorf("the waiter's transaction did not survive the timeout: %v", err)
	}
}

// jsonFixture creates a table with one JSON document in it.
func jsonFixture(t *testing.T, db *mysql.DB) (*mysql.Table, *mysql.Col[int64], *mysql.Col[json.RawMessage]) {
	t.Helper()
	tbl := mysql.NewTable(integration.UniqueName(t, "jsondoc"))
	id := mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	doc := mysql.Add(tbl, mysql.JSON("doc"))
	dropMySQL(t, db, tbl)
	execMySQL(t, db, mysql.CreateTable(tbl))
	ctx := context.Background()
	if _, err := db.Insert(tbl).Row(
		doc.Val(json.RawMessage(`{"a": 1, "b": {"c": "x"}, "d": [10, 20], "e": null}`)),
	).Exec(ctx); err != nil {
		t.Fatalf("insert: %v", err)
	}
	return tbl, id, doc
}

// jsonValue runs "SELECT <e> FROM <fixture>" for the single row.
func jsonValue(t *testing.T, db *mysql.DB, tbl *mysql.Table, e drops.Expression) string {
	t.Helper()
	rows, err := db.Select(e).From(tbl).Rows(context.Background())
	if err != nil {
		text, args := drops.StringWithDialect(mysql.Dialect, e)
		t.Fatalf("MySQL rejected the expression: %v\n%s\nargs: %v", err, text, args)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no row")
	}
	var v any
	if err := rows.Scan(&v); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return mysqlText(v)
}

func TestMySQLJSONAccessorValues(t *testing.T) {
	db := openMySQL(t)
	tbl, _, doc := jsonFixture(t, db)

	cases := []struct {
		name string
		expr drops.Expression
		want string
	}{
		// JSON_EXTRACT rather than ->, because MariaDB has no -> at
		// all — not even against a JSON column.
		{"JSONGet returns a JSON value", mysql.JSONGet(doc, "a"), "1"},
		{"JSONGet of a string stays quoted", mysql.JSONGet(mysql.JSONGet(doc, "b"), "c"), `"x"`},
		{"JSONGetText unquotes", mysql.JSONGetText(mysql.JSONGet(doc, "b"), "c"), "x"},
		{"JSONGet by array index", mysql.JSONGet(mysql.JSONGet(doc, "d"), 1), "20"},
		{"JSONGetPath walks", mysql.JSONGetPath(doc, "b", "c"), `"x"`},
		{"JSONGetPathText walks and unquotes", mysql.JSONGetPathText(doc, "b", "c"), "x"},
		{"JSONGetPath into an array", mysql.JSONGetPath(doc, "d", 0), "10"},
		{"a missing path is SQL NULL", mysql.JSONGetText(doc, "zz"), "<nil>"},
		{"JSONExtract with a literal path", mysql.JSONExtract(doc, "$.a"), "1"},
		{"JSONValue extracts and unquotes", mysql.JSONValue(doc, "$.b.c"), "x"},

		{"JSONContains", mysql.JSONContains(doc, `{"a": 1}`), "1"},
		{"JSONContains is false for a mismatch", mysql.JSONContains(doc, `{"a": 2}`), "0"},
		{"JSONContains at a path", mysql.JSONContains(doc, `1`, "$.a"), "1"},
		{"JSONContainedIn swaps the arguments", mysql.JSONContainedIn(`{"a": 1}`, doc), "1"},
		{"JSONHasKey", mysql.JSONHasKey(doc, "a"), "1"},
		{"JSONHasKey is false for a missing key", mysql.JSONHasKey(doc, "zz"), "0"},
		{"JSONHasAnyKey", mysql.JSONHasAnyKey(doc, "zz", "a"), "1"},
		{"JSONHasAllKeys", mysql.JSONHasAllKeys(doc, "a", "b"), "1"},
		{"JSONHasAllKeys is false when one is missing", mysql.JSONHasAllKeys(doc, "a", "zz"), "0"},

		{"JSONLength counts object keys", mysql.JSONArrayLength(doc), "4"},
		{"JSONLength scoped to a path", mysql.JSONLength(doc, "$.d"), "2"},
		{"JSONDepth", mysql.JSONDepth(doc), "3"},
		{"JSONKeys", mysql.JSONKeys(doc), `["a", "b", "d", "e"]`},
		{"JSONValid", mysql.JSONValid(doc), "1"},
		{"JSONSearch returns a path", mysql.JSONSearch(doc, "one", "x"), `"$.b.c"`},

		{"JSONSet creates a missing path", mysql.JSONGetText(mysql.JSONSet(doc, "$.new", "v"), "new"), "v"},
		{"JSONReplace does not create", mysql.JSONGetText(mysql.JSONReplace(doc, "$.new", "v"), "new"), "<nil>"},
		{"JSONReplace replaces", mysql.JSONGetText(mysql.JSONReplace(doc, "$.a", "v"), "a"), "v"},
		{"JSONInsert does not overwrite", mysql.JSONGetText(mysql.JSONInsert(doc, "$.a", 9), "a"), "1"},
		{"JSONRemove", mysql.JSONHasKey(mysql.JSONRemove(doc, "$.a"), "a"), "0"},

		{"JSONMergePatch overwrites", mysql.JSONGetText(mysql.JSONMergePatch(doc, `{"a": 2}`), "a"), "2"},
		{"JSONMergePreserve makes an array", mysql.JSONGet(mysql.JSONMergePreserve(doc, `{"a": 2}`), "a"), "[1, 2]"},
		// The caveat in the package doc: || would set the key to null,
		// JSON_MERGE_PATCH deletes it.
		{"JSONMergePatch deletes on a null value", mysql.JSONHasKey(mysql.JSONMergePatch(doc, `{"a": null}`), "a"), "0"},

		{"JSONBuildObject", mysql.JSONBuildObject("k", "v"), `{"k": "v"}`},
		{"JSONBuildArray", mysql.JSONBuildArray(1, "x"), `[1, "x"]`},
		{"JSONQuote", mysql.JSONQuote("x"), `"x"`},
		{"JSONUnquote", mysql.JSONUnquote(`"x"`), "x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := jsonValue(t, db, tbl, tc.expr); got != tc.want {
				text, args := drops.StringWithDialect(mysql.Dialect, tc.expr)
				t.Errorf("value = %q, want %q\n%s\nargs: %v", got, tc.want, text, args)
			}
		})
	}
}

// JSON_TYPE's vocabulary is not json_typeof's: upper case, and it
// splits pg's "number" into INTEGER and DOUBLE. Code that switches on
// the answer has to be rewritten, not just repointed, which is worth
// having pinned rather than described.
func TestMySQLJSONTypeVocabularyIsNotPostgreSQLs(t *testing.T) {
	db := openMySQL(t)
	tbl, _, doc := jsonFixture(t, db)
	want := map[string]string{
		"a": "INTEGER",
		"b": "OBJECT",
		"d": "ARRAY",
		"e": "NULL",
	}
	for key, typ := range want {
		if got := jsonValue(t, db, tbl, mysql.JSONType(mysql.JSONGet(doc, key))); got != typ {
			t.Errorf("JSON_TYPE of %q = %s, want %s", key, got, typ)
		}
	}
	if got := jsonValue(t, db, tbl, mysql.JSONType(mysql.JSONGet(mysql.JSONGet(doc, "b"), "c"))); got != "STRING" {
		t.Errorf("JSON_TYPE of a string = %s, want STRING", got)
	}
	// The one that catches people: a float is DOUBLE, not INTEGER, so
	// a switch that only handles INTEGER falls through.
	if got := mysqlValue(t, db, mysql.JSONType(mysql.JSONExtract(`{"n": 1.5}`, "$.n"))); got != "DOUBLE" {
		t.Errorf("JSON_TYPE of 1.5 = %s, want DOUBLE", got)
	}
}

// JSON_QUERY is MariaDB's function, not MySQL's. MySQL took JSON_VALUE
// and JSON_TABLE from the same standard clause and never implemented
// JSON_QUERY, so this is the one helper in json.go that does not work
// on both servers — which is why JSONGet renders JSON_EXTRACT and not
// this. If it ever starts answering on MySQL, JSONQuery's doc is the
// thing that needs correcting.
func TestMySQLJSONQueryIsMariaDBOnly(t *testing.T) {
	db := openMySQL(t)
	tbl, _, doc := jsonFixture(t, db)
	_, _, mariadb := mysqlServerVersion(t, db)

	e := mysql.JSONQuery(doc, "$.d")
	rows, err := db.Select(e).From(tbl).Rows(context.Background())
	if err == nil {
		defer rows.Close()
	}
	if !mariadb {
		if err == nil {
			t.Fatal("MySQL answered JSON_QUERY; the helper's doc says it cannot, and the doc is what is wrong")
		}
		if !strings.Contains(err.Error(), "1305") && !strings.Contains(strings.ToLower(err.Error()), "does not exist") {
			t.Fatalf("expected \"no such function\" from MySQL, got %v", err)
		}
		// The portable spelling has to answer the same thing.
		if got := jsonValue(t, db, tbl, mysql.JSONExtract(doc, "$.d")); got != "[10, 20]" && got != "[10,20]" {
			t.Errorf("JSONExtract = %s, want the array JSON_QUERY would have returned", got)
		}
		return
	}
	if err != nil {
		t.Fatalf("MariaDB has JSON_QUERY: %v", err)
	}
	if !rows.Next() {
		t.Fatal("no row")
	}
	var v any
	if err := rows.Scan(&v); err != nil {
		t.Fatal(err)
	}
	if got := mysqlText(v); got != "[10, 20]" && got != "[10,20]" {
		t.Errorf("JSONQuery = %s, want the array at $.d", got)
	}
}

// A JSON path built by JSONPath addresses the key the caller named,
// even when the key contains the characters a path language uses.
func TestMySQLJSONPathQuotingReachesAwkwardKeys(t *testing.T) {
	db := openMySQL(t)
	tbl := mysql.NewTable(integration.UniqueName(t, "jsonkeys"))
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	doc := mysql.Add(tbl, mysql.JSON("doc"))
	dropMySQL(t, db, tbl)
	execMySQL(t, db, mysql.CreateTable(tbl))
	ctx := context.Background()
	if _, err := db.Insert(tbl).Row(
		doc.Val(json.RawMessage(`{"first name": "ada", "a.b": "dotted", "x": {"y": "nested"}}`)),
	).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	cases := map[string]drops.Expression{
		"ada":    mysql.JSONGetText(doc, "first name"),
		"dotted": mysql.JSONGetText(doc, "a.b"),
		"nested": mysql.JSONGetPathText(doc, "x", "y"),
	}
	for want, e := range cases {
		if got := jsonValue(t, db, tbl, e); got != want {
			t.Errorf("value = %q, want %q", got, want)
		}
	}
}

func TestMySQLJSONAggregatesAndTable(t *testing.T) {
	db := openMySQL(t)
	tbl := mysql.NewTable(integration.UniqueName(t, "jsonagg"))
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	name := mysql.Add(tbl, mysql.Varchar("name", 64))
	score := mysql.Add(tbl, mysql.Integer("score"))
	dropMySQL(t, db, tbl)
	execMySQL(t, db, mysql.CreateTable(tbl))
	ctx := context.Background()
	if _, err := db.Insert(tbl).Row(name.Val("a"), score.Val(1)).Row(name.Val("b"), score.Val(2)).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	if got := jsonValue(t, db, tbl, mysql.JSONAgg(score)); got != "[1,2]" && got != "[1, 2]" {
		t.Errorf("JSONAgg = %s, want [1,2]", got)
	}
	if got := jsonValue(t, db, tbl, mysql.JSONObjectAgg(name, score)); got != `{"a": 1, "b": 2}` && got != `{"a":1, "b":2}` {
		t.Errorf("JSONObjectAgg = %s", got)
	}

	// JSON_TABLE turns a document into rows — the thing MySQL has and
	// nothing else in the dialect does. MySQL 8.0+ / MariaDB 10.6+.
	info, err := mysql.ServerVersion(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if info.MariaDB && !info.AtLeast(10, 6, 0) {
		t.Skip("JSON_TABLE needs MariaDB 10.6+")
	}
	jt := mysql.JSONTable(`[{"a": 1}, {"a": 2}]`, "$[*]", "jt", mysql.JSONCol("a", "INT", "$.a"))
	sum := mysql.Sum(drops.Raw("`jt`.`a`"))
	rows, err := db.Select(sum).FromExpr(jt).Rows(ctx)
	if err != nil {
		text, args := drops.StringWithDialect(mysql.Dialect, jt)
		t.Fatalf("JSON_TABLE: %v\n%s\nargs: %v", err, text, args)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no row")
	}
	var v any
	if err := rows.Scan(&v); err != nil {
		t.Fatal(err)
	}
	if mysqlText(v) != "3" {
		t.Errorf("JSON_TABLE sum = %s, want 3", mysqlText(v))
	}
}

// scoresFixture is the small ranked table the CTE and window tests
// share: two teams, three players, distinct scores.
func scoresFixture(t *testing.T, db *mysql.DB) (*mysql.Table, *mysql.Col[string], *mysql.Col[string], *mysql.Col[int64]) {
	t.Helper()
	tbl := mysql.NewTable(integration.UniqueName(t, "scores"))
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	team := mysql.Add(tbl, mysql.Varchar("team", 32).NotNull())
	player := mysql.Add(tbl, mysql.Varchar("player", 32).NotNull())
	score := mysql.Add(tbl, mysql.BigInt("score").NotNull())
	dropMySQL(t, db, tbl)
	execMySQL(t, db, mysql.CreateTable(tbl))
	ctx := context.Background()
	if _, err := db.Insert(tbl).
		Row(team.Val("red"), player.Val("ada"), score.Val(10)).
		Row(team.Val("red"), player.Val("bob"), score.Val(30)).
		Row(team.Val("blue"), player.Val("cy"), score.Val(20)).
		Exec(ctx); err != nil {
		t.Fatal(err)
	}
	return tbl, team, player, score
}

func requireCTEs(t *testing.T, db *mysql.DB) mysql.ServerInfo {
	t.Helper()
	info, err := mysql.ServerVersion(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if !info.SupportsCTE() {
		t.Skipf("WITH needs MySQL 8.0 or MariaDB 10.2; this is %s", info.Version)
	}
	return info
}

func TestMySQLCTEFeedsTheOuterQuery(t *testing.T) {
	db := openMySQL(t)
	requireCTEs(t, db)
	tbl, team, _, score := scoresFixture(t, db)
	ctx := context.Background()

	// WITH totals AS (SELECT team, SUM(score) FROM scores GROUP BY team)
	// SELECT SUM(total) FROM totals WHERE total > 20
	inner := db.Select(team, mysql.As(mysql.Sum(score), "total")).From(tbl).GroupBy(team)
	totals := mysql.CTEDef("totals", inner)

	rows, err := db.Select(mysql.Sum(totals.Col("total"))).
		With(totals).
		FromExpr(totals.Ref()).
		Where(mysql.Gt(totals.Col("total"), 20)).
		Rows(ctx)
	if err != nil {
		t.Fatalf("CTE query: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no row")
	}
	var v any
	if err := rows.Scan(&v); err != nil {
		t.Fatal(err)
	}
	if mysqlText(v) != "40" {
		t.Errorf("sum of totals over 20 = %s, want 40 (red is 40, blue is 20)", mysqlText(v))
	}
}

// A WITH clause binds its arguments before the SELECT that follows it.
// MySQL placeholders are positional, so if the WITH rendered after the
// body the values would land in each other's slots — a bug that a
// rendered-SQL test cannot see, because the SQL is identical either
// way and only the argument order changes.
func TestMySQLCTEArgumentsBindInRenderOrder(t *testing.T) {
	db := openMySQL(t)
	requireCTEs(t, db)
	tbl, team, player, score := scoresFixture(t, db)
	ctx := context.Background()

	inner := db.Select(player, score).From(tbl).Where(team.Eq("red"))
	reds := mysql.CTEDef("reds", inner)
	var got []struct {
		Player string `drop:"player"`
	}
	err := db.Select(reds.Col("player")).
		With(reds).
		FromExpr(reds.Ref()).
		Where(mysql.Gt(reds.Col("score"), 20)).
		All(ctx, &got)
	if err != nil {
		t.Fatalf("CTE query: %v", err)
	}
	if len(got) != 1 || got[0].Player != "bob" {
		t.Errorf("rows = %v, want just bob", got)
	}
}

func TestMySQLRecursiveCTECounts(t *testing.T) {
	db := openMySQL(t)
	requireCTEs(t, db)
	ctx := context.Background()

	// WITH RECURSIVE t (n) AS (SELECT 1 UNION ALL SELECT n+1 FROM t WHERE n < 5)
	cte := mysql.CTEDef("t",
		drops.Raw("SELECT 1 UNION ALL SELECT `n` + 1 FROM `t` WHERE `n` < 5"), "n")
	rows, err := db.Select(mysql.Max(cte.Col("n")), mysql.CountAll()).
		WithRecursive(cte).
		FromExpr(cte.Ref()).
		Rows(ctx)
	if err != nil {
		t.Fatalf("recursive CTE: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no row")
	}
	var maxN, count any
	if err := rows.Scan(&maxN, &count); err != nil {
		t.Fatal(err)
	}
	if mysqlText(maxN) != "5" || mysqlText(count) != "5" {
		t.Errorf("max = %s, count = %s; want 5 and 5", mysqlText(maxN), mysqlText(count))
	}
}

// The divergence that matters most in this whole file. Both servers cap
// a recursive CTE at 1000 iterations by default, under different
// variable names — and when the cap is hit MySQL raises error 3636
// while MariaDB stops and returns a short answer with no error at all.
// So the same query fails loudly on one server and lies quietly on the
// other.
func TestMySQLRecursionCapDiffersBetweenTheServers(t *testing.T) {
	db := openMySQL(t)
	info := requireCTEs(t, db)
	ctx := context.Background()

	deep := mysql.CTEDef("t",
		drops.Raw("SELECT 1 UNION ALL SELECT `n` + 1 FROM `t` WHERE `n` < 2000"), "n")
	sel := db.Select(mysql.Max(deep.Col("n"))).WithRecursive(deep).FromExpr(deep.Ref())

	rows, err := sel.Rows(ctx)
	if info.MariaDB {
		if err != nil {
			t.Fatalf("MariaDB is documented to truncate silently, not to fail: %v", err)
		}
		defer rows.Close()
		if !rows.Next() {
			t.Fatal("no row")
		}
		var v any
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		if mysqlText(v) == "2000" {
			t.Fatal("the recursion was not capped; this server's max_recursive_iterations is not the default")
		}
		if mysqlText(v) != "1001" {
			t.Logf("MariaDB truncated at %s rather than 1001; the cap is set, the number is the server's", mysqlText(v))
		}
	} else {
		if err == nil {
			rows.Close()
			t.Fatal("MySQL is documented to raise error 3636 past cte_max_recursion_depth")
		}
		if !strings.Contains(err.Error(), "3636") && !strings.Contains(strings.ToLower(err.Error()), "recursi") {
			t.Fatalf("unexpected error past the recursion cap: %v", err)
		}
	}

	// Raising the limit makes the same query answer in full on either
	// server. It is a session setting, so it goes on the same pinned
	// connection the query runs on.
	pinned, sqlDB := openMySQLPinnedConn(t)
	_ = sqlDB
	if err := mysql.SetRecursionLimit(ctx, pinned, 5000); err != nil {
		t.Fatalf("SetRecursionLimit: %v", err)
	}
	rows2, err := pinned.Select(mysql.Max(deep.Col("n"))).WithRecursive(deep).FromExpr(deep.Ref()).Rows(ctx)
	if err != nil {
		t.Fatalf("after raising the limit: %v", err)
	}
	defer rows2.Close()
	if !rows2.Next() {
		t.Fatal("no row")
	}
	var v any
	if err := rows2.Scan(&v); err != nil {
		t.Fatal(err)
	}
	if mysqlText(v) != "2000" {
		t.Errorf("with the limit raised, max = %s, want 2000", mysqlText(v))
	}
}

func TestMySQLSetRecursionLimitRejectsZero(t *testing.T) {
	db := openMySQL(t)
	// Zero means "unlimited" to MySQL and nothing to MariaDB, so drops
	// refuses rather than doing different things on each.
	if err := mysql.SetRecursionLimit(context.Background(), db, 0); err == nil {
		t.Error("expected a refusal")
	}
}

func TestMySQLWindowFunctionValues(t *testing.T) {
	db := openMySQL(t)
	info, err := mysql.ServerVersion(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if !info.SupportsWindowFunctions() {
		t.Skipf("window functions need MySQL 8.0 or MariaDB 10.2; this is %s", info.Version)
	}
	tbl, team, player, score := scoresFixture(t, db)
	ctx := context.Background()

	byTeam := mysql.WindowSpec().PartitionBy(team).OrderBy(score.Desc())
	var got []struct {
		Player string `drop:"player"`
		Rank   int64  `drop:"rn"`
	}
	err = db.Select(player, mysql.As(mysql.Over(mysql.RowNumber(), byTeam), "rn")).
		From(tbl).OrderBy(player.Asc()).All(ctx, &got)
	if err != nil {
		t.Fatalf("window query: %v", err)
	}
	want := map[string]int64{"ada": 2, "bob": 1, "cy": 1}
	if len(got) != 3 {
		t.Fatalf("rows = %v", got)
	}
	for _, r := range got {
		if want[r.Player] != r.Rank {
			t.Errorf("%s rank = %d, want %d", r.Player, r.Rank, want[r.Player])
		}
	}

	// A running total needs the frame; the default one already ends at
	// the current row, which is what makes this the sum so far.
	running := mysql.WindowSpec().OrderBy(score.Asc()).
		Frame("ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW")
	var totals []struct {
		Player string `drop:"player"`
		Total  int64  `drop:"total"`
	}
	if err := db.Select(player, mysql.As(mysql.Over(mysql.Sum(score), running), "total")).
		From(tbl).OrderBy(score.Asc()).All(ctx, &totals); err != nil {
		t.Fatalf("frame query: %v", err)
	}
	wantTotals := []int64{10, 30, 60}
	for i, r := range totals {
		if r.Total != wantTotals[i] {
			t.Errorf("running total at %d = %d, want %d", i, r.Total, wantTotals[i])
		}
	}

	// Lag with two arguments is portable; Rank, DenseRank and Ntile
	// all are too.
	for _, e := range []drops.Expression{
		mysql.Over(mysql.Rank(), byTeam),
		mysql.Over(mysql.DenseRank(), byTeam),
		mysql.Over(mysql.PercentRank(), byTeam),
		mysql.Over(mysql.CumeDist(), byTeam),
		mysql.Over(mysql.Ntile(2), byTeam),
		mysql.Over(mysql.Lag(score, 1), byTeam),
		mysql.Over(mysql.Lead(score, 1), byTeam),
		mysql.Over(mysql.FirstValue(score), byTeam),
		mysql.Over(mysql.LastValue(score), byTeam),
		mysql.Over(mysql.NthValue(score, 1), byTeam),
	} {
		rows, err := db.Select(e).From(tbl).Rows(ctx)
		if err != nil {
			text, args := drops.StringWithDialect(mysql.Dialect, e)
			t.Errorf("rejected: %v\n%s\nargs: %v", err, text, args)
			continue
		}
		rows.Close()
	}
}

// LAG(expr, offset, default) is MySQL 8 only — MariaDB answers a
// syntax error at the third argument. The two-argument form wrapped in
// IFNULL is what works on both, which is what Lag's doc says to do.
func TestMySQLLagDefaultArgumentIsNotPortable(t *testing.T) {
	db := openMySQL(t)
	info, err := mysql.ServerVersion(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if !info.SupportsWindowFunctions() {
		t.Skip("window functions unsupported")
	}
	tbl, _, _, score := scoresFixture(t, db)
	ctx := context.Background()
	win := mysql.WindowSpec().OrderBy(score.Asc())

	lagRows, err := db.Select(mysql.Over(mysql.Lag(score, 1, 0), win)).From(tbl).Rows(ctx)
	if err == nil {
		lagRows.Close()
	}
	if info.MariaDB {
		if err == nil {
			t.Error("MariaDB is documented to reject the three-argument LAG")
		}
	} else if err != nil {
		t.Errorf("MySQL takes the three-argument LAG: %v", err)
	}

	// The portable spelling, on either server.
	var first []struct {
		V int64 `drop:"v"`
	}
	if err := db.Select(mysql.As(mysql.IfNull(mysql.Over(mysql.Lag(score, 1), win), 0), "v")).
		From(tbl).OrderBy(score.Asc()).All(ctx, &first); err != nil {
		t.Fatalf("IFNULL around a two-argument LAG: %v", err)
	}
	if len(first) == 0 || first[0].V != 0 {
		t.Errorf("first row = %v, want the IFNULL default of 0", first)
	}
}

func TestMySQLSubqueryHelpersAgainstTheServer(t *testing.T) {
	db := openMySQL(t)
	tbl, team, player, score := scoresFixture(t, db)
	ctx := context.Background()

	reds := db.Select(score).From(tbl).Where(team.Eq("red"))

	count := func(pred drops.Expression) int64 {
		t.Helper()
		var out []struct {
			N int64 `drop:"n"`
		}
		if err := db.Select(mysql.As(mysql.CountAll(), "n")).From(tbl).Where(pred).All(ctx, &out); err != nil {
			text, args := drops.StringWithDialect(mysql.Dialect, pred)
			t.Fatalf("rejected: %v\n%s\nargs: %v", err, text, args)
		}
		return out[0].N
	}

	if n := count(mysql.InSub(score, reds)); n != 2 {
		t.Errorf("IN (subquery) matched %d, want 2", n)
	}
	if n := count(mysql.NotInSub(score, reds)); n != 1 {
		t.Errorf("NOT IN (subquery) matched %d, want 1", n)
	}
	if n := count(mysql.AnySub(score, reds)); n != 2 {
		t.Errorf("= ANY matched %d, want 2", n)
	}
	// "> ANY" is "greater than the smallest": the reds are 10 and 30,
	// so 20 and 30 both qualify.
	if n := count(mysql.CmpAny(score, ">", reds)); n != 2 {
		t.Errorf("> ANY matched %d, want 2", n)
	}
	if n := count(mysql.CmpAll(score, ">=", reds)); n != 1 {
		t.Errorf(">= ALL matched %d, want 1", n)
	}

	// A correlated EXISTS: players on a team that has someone scoring
	// over 25.
	inner := db.Select(drops.Raw("1")).From(tbl.As("peer")).
		Where(mysql.Eq(drops.Raw("`peer`.`team`"), team), mysql.Gt(drops.Raw("`peer`.`score`"), 25))
	if n := count(mysql.Exists(inner)); n != 2 {
		t.Errorf("EXISTS matched %d, want 2", n)
	}
	if n := count(mysql.NotExists(inner)); n != 1 {
		t.Errorf("NOT EXISTS matched %d, want 1", n)
	}

	// Scalar subquery in the projection.
	var out []struct {
		Player string `drop:"player"`
		Top    int64  `drop:"top"`
	}
	top := db.Select(mysql.Max(score)).From(tbl)
	if err := db.Select(player, mysql.As(mysql.Subquery(top), "top")).From(tbl).All(ctx, &out); err != nil {
		t.Fatalf("scalar subquery: %v", err)
	}
	for _, r := range out {
		if r.Top != 30 {
			t.Errorf("%s saw top = %d, want 30", r.Player, r.Top)
		}
	}
}

func TestMySQLConditionalAggregatesStandInForFilter(t *testing.T) {
	db := openMySQL(t)
	tbl, _, _, score := scoresFixture(t, db)
	ctx := context.Background()

	var out []struct {
		Big   int64 `drop:"big"`
		Total int64 `drop:"total"`
	}
	err := db.Select(
		mysql.As(mysql.CountIf(mysql.Gt(score, 15)), "big"),
		mysql.As(mysql.SumIf(mysql.Gt(score, 15), score), "total"),
	).From(tbl).All(ctx, &out)
	if err != nil {
		t.Fatalf("conditional aggregates: %v", err)
	}
	if out[0].Big != 2 || out[0].Total != 50 {
		t.Errorf("big = %d, total = %d; want 2 and 50", out[0].Big, out[0].Total)
	}

	// The FILTER clause pg writes is a syntax error on both servers,
	// which is why CountIf exists at all.
	if _, err := db.Query(ctx, "SELECT COUNT(*) FILTER (WHERE 1 = 1)"); err == nil {
		t.Error("FILTER (WHERE ...) is not supposed to parse on MySQL or MariaDB")
	}
}

// A group with no matching row sums to NULL rather than 0, exactly as
// PostgreSQL's FILTER does — the reason SumIf has no ELSE branch.
func TestMySQLSumIfIsNullWhenNothingMatches(t *testing.T) {
	db := openMySQL(t)
	tbl, _, _, score := scoresFixture(t, db)
	if got := jsonValue(t, db, tbl, mysql.SumIf(mysql.Gt(score, 1000), score)); got != "<nil>" {
		t.Errorf("SumIf over no rows = %s, want NULL", got)
	}
	if got := jsonValue(t, db, tbl, mysql.CountIf(mysql.Gt(score, 1000))); got != "0" {
		t.Errorf("CountIf over no rows = %s, want 0", got)
	}
}

func TestMySQLGroupConcatOrdersAndSeparates(t *testing.T) {
	db := openMySQL(t)
	tbl, _, player, _ := scoresFixture(t, db)
	// GROUP_CONCAT with no ORDER BY leaves the order to the plan, so
	// the claim is about the parts and the separator, not the string.
	// (Asserting only its length would pass for any three characters
	// and two pipes, separator included.)
	got := jsonValue(t, db, tbl, mysql.GroupConcat(player, "|"))
	parts := strings.Split(got, "|")
	sort.Strings(parts)
	if strings.Join(parts, ",") != "ada,bob,cy" {
		t.Errorf("GroupConcat = %q, want the three players separated by |", got)
	}
	got = jsonValue(t, db, tbl,
		mysql.GroupConcatDistinct(player, []drops.Expression{player.Asc()}, "|"))
	if got != "ada|bob|cy" {
		t.Errorf("GroupConcatDistinct = %q, want ada|bob|cy", got)
	}
}

func TestMySQLExistenceChecksReadTheCatalogue(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()

	// The database name is whatever the DSN connected to, not a
	// constant: the helpers take one because MySQL's "schema" is a
	// database, and the test has to ask the server which one that is
	// rather than assume the compose file's.
	var here []struct {
		DB string `drop:"db"`
	}
	if err := db.Select(mysql.As(drops.Raw("DATABASE()"), "db")).All(ctx, &here); err != nil {
		t.Fatal(err)
	}
	current := here[0].DB

	tbl := mysql.NewTable(integration.UniqueName(t, "exists"))
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	email := mysql.Add(tbl, mysql.Varchar("email", 191).NotNull().Unique())
	idxName := integration.UniqueName(t, "byemail")
	idx := mysql.NewIndex(idxName, tbl, email)
	dropMySQL(t, db, tbl)

	// Absent before the DDL, present after: the point of the helpers
	// is that they answer about the live server, so both halves need
	// the server to answer them.
	if ok, err := mysql.TableExists(ctx, db, "", tbl.Name()); err != nil || ok {
		t.Fatalf("TableExists before create = %v, %v; want false", ok, err)
	}
	execMySQL(t, db, mysql.CreateTable(tbl))

	cases := []struct {
		name string
		fn   func() (bool, error)
		want bool
	}{
		{"the connection's own database", func() (bool, error) { return mysql.DatabaseExists(ctx, db, current) }, true},
		{"a database that is not there", func() (bool, error) { return mysql.DatabaseExists(ctx, db, "no_such_db") }, false},
		{"the table, default database", func() (bool, error) { return mysql.TableExists(ctx, db, "", tbl.Name()) }, true},
		{"the table, named database", func() (bool, error) { return mysql.TableExists(ctx, db, current, tbl.Name()) }, true},
		{"a table that is not there", func() (bool, error) { return mysql.TableExists(ctx, db, "", "no_such_table") }, false},
		{"a column", func() (bool, error) { return mysql.ColumnExists(ctx, db, "", tbl.Name(), "email") }, true},
		{"a column that is not there", func() (bool, error) { return mysql.ColumnExists(ctx, db, "", tbl.Name(), "nope") }, false},
		// MySQL names the primary key's constraint PRIMARY, always.
		{"the primary key constraint", func() (bool, error) { return mysql.ConstraintExists(ctx, db, "", tbl.Name(), "PRIMARY") }, true},
		{"the unique constraint", func() (bool, error) {
			return mysql.ConstraintExists(ctx, db, "", tbl.Name(), "uq_"+tbl.Name()+"_email")
		}, true},
		{"the primary key as an index", func() (bool, error) { return mysql.IndexExists(ctx, db, "", tbl.Name(), "PRIMARY") }, true},
		{"an index that is not there", func() (bool, error) { return mysql.IndexExists(ctx, db, "", tbl.Name(), idxName) }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.fn()
			if err != nil {
				t.Fatalf("error: %v", err)
			}
			if got != tc.want {
				t.Errorf("= %v, want %v", got, tc.want)
			}
		})
	}

	// And once the index is created it is visible — a plain secondary
	// index being an index and not a constraint, which is the split
	// the two helpers exist to express.
	execMySQL(t, db, idx)
	if ok, err := mysql.IndexExists(ctx, db, "", tbl.Name(), idxName); err != nil || !ok {
		t.Errorf("IndexExists after create = %v, %v; want true", ok, err)
	}
	if ok, err := mysql.ConstraintExists(ctx, db, "", tbl.Name(), idxName); err != nil || ok {
		t.Errorf("a secondary index is not a constraint: %v, %v", ok, err)
	}
}

type patchCounter struct {
	ID    int64  `drop:"id"`
	Likes int64  `drop:"likes"`
	Peak  int64  `drop:"peak"`
	Name  string `drop:"name"`
}

func patchFixture(t *testing.T, db *mysql.DB) (*mysql.Entity[patchCounter], *mysql.Table, *mysql.Col[int64], *mysql.Col[int64], *mysql.Col[string]) {
	t.Helper()
	tbl := mysql.NewTable(integration.UniqueName(t, "patch"))
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	likes := mysql.Add(tbl, mysql.BigInt("likes").NotNull().Default("0"))
	peak := mysql.Add(tbl, mysql.BigInt("peak").NotNull().Default("0"))
	name := mysql.Add(tbl, mysql.Varchar("name", 64).NotNull().Default("''"))
	dropMySQL(t, db, tbl)
	execMySQL(t, db, mysql.CreateTable(tbl))
	ent := mysql.NewEntity[patchCounter](tbl)
	if _, err := db.Insert(tbl).Row(name.Val("row")).Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
	return ent, tbl, likes, peak, name
}

// The whole reason Patch exists: a hundred concurrent increments have
// to land as a hundred, which a read-modify-write through the
// application would not manage.
func TestMySQLPatchIncrementsWithoutLosingUpdates(t *testing.T) {
	db := openMySQL(t)
	ent, _, likes, _, _ := patchFixture(t, db)
	ctx := context.Background()

	const workers, each = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if _, err := ent.Patch(db, ctx, int64(1), mysql.Inc(likes, int64(1))); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("patch: %v", err)
	}
	row, err := ent.Get(db, ctx, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	if row.Likes != workers*each {
		t.Errorf("likes = %d, want %d — increments were lost", row.Likes, workers*each)
	}
}

func TestMySQLPatchOperationsAgainstTheServer(t *testing.T) {
	db := openMySQL(t)
	ent, _, likes, peak, name := patchFixture(t, db)
	ctx := context.Background()

	if _, err := ent.Patch(db, ctx, int64(1),
		mysql.Inc(likes, int64(5)),
		mysql.SetIfGreater(peak, int64(10)),
		mysql.Set(name, "first"),
	); err != nil {
		t.Fatal(err)
	}
	row, err := ent.Get(db, ctx, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	if row.Likes != 5 || row.Peak != 10 || row.Name != "first" {
		t.Fatalf("after the first patch: %+v", row)
	}

	// A high-watermark only rises.
	if _, err := ent.Patch(db, ctx, int64(1), mysql.SetIfGreater(peak, int64(3))); err != nil {
		t.Fatal(err)
	}
	if row, _ = ent.Get(db, ctx, int64(1)); row.Peak != 10 {
		t.Errorf("SetIfGreater lowered the value to %d", row.Peak)
	}
	if _, err := ent.Patch(db, ctx, int64(1), mysql.SetIfGreater(peak, int64(42))); err != nil {
		t.Fatal(err)
	}
	if row, _ = ent.Get(db, ctx, int64(1)); row.Peak != 42 {
		t.Errorf("SetIfGreater did not raise the value: %d", row.Peak)
	}

	// A low-watermark only falls.
	if _, err := ent.Patch(db, ctx, int64(1), mysql.SetIfLess(peak, int64(100))); err != nil {
		t.Fatal(err)
	}
	if row, _ = ent.Get(db, ctx, int64(1)); row.Peak != 42 {
		t.Errorf("SetIfLess raised the value to %d", row.Peak)
	}

	if _, err := ent.Patch(db, ctx, int64(1), mysql.Dec(likes, int64(2))); err != nil {
		t.Fatal(err)
	}
	if row, _ = ent.Get(db, ctx, int64(1)); row.Likes != 3 {
		t.Errorf("Dec left likes at %d, want 3", row.Likes)
	}

	// SetExpr reaches anything the named ops do not.
	if _, err := ent.Patch(db, ctx, int64(1),
		mysql.SetExpr(name, mysql.Concat(name, "-suffix"))); err != nil {
		t.Fatal(err)
	}
	if row, _ = ent.Get(db, ctx, int64(1)); row.Name != "first-suffix" {
		t.Errorf("SetExpr = %q", row.Name)
	}

	// A key that matches nothing reports zero rows rather than an
	// error — the reason Patch hands back the Result.
	res, err := ent.Patch(db, ctx, int64(999), mysql.Inc(likes, int64(1)))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 0 {
		t.Errorf("rows affected for a missing row = %d, %v; want 0", n, err)
	}
}

// SetIfChanged must compare with MySQL's null-safe <=>. A plain <>
// against a NULL column evaluates to NULL, the CASE falls through to
// its ELSE, and the assignment silently never happens — so the NULL
// column is the case that proves the operator.
func TestMySQLSetIfChangedReachesANullColumn(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl := mysql.NewTable(integration.UniqueName(t, "ifchanged"))
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	label := mysql.Add(tbl, mysql.Varchar("label", 64))
	dropMySQL(t, db, tbl)
	execMySQL(t, db, mysql.CreateTable(tbl))

	type row struct {
		ID    int64          `drop:"id"`
		Label sql.NullString `drop:"label"`
	}
	ent := mysql.NewEntity[row](tbl)
	if _, err := db.Insert(tbl).Row(label.Val("")).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Update(tbl).Set(mysql.Bind(label, nil)).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	res, err := ent.Patch(db, ctx, int64(1), mysql.SetIfChanged(label, "set"))
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Errorf("rows affected = %d, want 1", n)
	}
	got, err := ent.Get(db, ctx, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Label.Valid || got.Label.String != "set" {
		t.Fatalf("label = %+v; a NULL column was not updated, which is what a plain <> would do", got.Label)
	}

	// Assigning the same value again changes nothing, and MySQL
	// reports zero rows affected for a row it did not have to write.
	res, err = ent.Patch(db, ctx, int64(1), mysql.SetIfChanged(label, "set"))
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 0 {
		t.Errorf("rows affected for an unchanged value = %d, want 0", n)
	}
}

// Inc on an UNSIGNED column raises rather than clamping, whatever the
// sql_mode — the arithmetic fails before a value reaches the column,
// so strict mode never enters into it. Dec's doc turns on this, and it
// is a claim about what the server does with a statement rather than
// about the statement, so it is made here.
func TestMySQLDecOnAnUnsignedColumnRaisesRatherThanClamping(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()

	type row struct {
		ID    int64  `drop:"id"`
		Seats uint64 `drop:"seats"`
	}
	tbl := mysql.NewTable(integration.UniqueName(t, "unsigned"))
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	seats := mysql.Add(tbl, mysql.Custom[uint64]("seats", "BIGINT UNSIGNED").NotNull().Default("0"))
	dropMySQL(t, db, tbl)
	execMySQL(t, db, mysql.CreateTable(tbl))
	ent := mysql.NewEntity[row](tbl)
	if _, err := db.Insert(tbl).Row(seats.Val(1)).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	// Under the default strict mode, and then with strict mode off on
	// the same connection: the answer has to be the same both times,
	// which is the part the doc used to get wrong.
	pinned, sqlDB := openMySQLPinnedConn(t)
	_ = sqlDB
	for _, mode := range []string{"", "''"} {
		name := "strict"
		if mode != "" {
			name = "non-strict"
			if _, err := pinned.Exec(ctx, "SET SESSION sql_mode = "+mode); err != nil {
				t.Fatal(err)
			}
		}
		t.Run(name, func(t *testing.T) {
			_, err := ent.Patch(pinned, ctx, int64(1), mysql.Dec(seats, uint64(5)))
			if err == nil {
				t.Fatal("an unsigned counter taken below zero is documented to raise, not to clamp")
			}
			if !strings.Contains(err.Error(), "1690") && !strings.Contains(err.Error(), "out of range") {
				t.Fatalf("want the out-of-range error, got %v", err)
			}
			var got []struct {
				N uint64 `drop:"seats"`
			}
			if err := pinned.Select(seats).From(tbl).All(ctx, &got); err != nil {
				t.Fatal(err)
			}
			if got[0].N != 1 {
				t.Errorf("seats = %d; the failed statement stored something", got[0].N)
			}
		})
	}
}

// JSON_CONTAINS with a candidate that is not JSON is the divergence
// that is worst to discover in production: MySQL fails the statement,
// MariaDB answers NULL, so the same predicate is a loud error on one
// server and a silently unmatched row on the other.
func TestMySQLJSONContainsRejectsNonJSONDifferently(t *testing.T) {
	db := openMySQL(t)
	tbl, _, doc := jsonFixture(t, db)
	_, _, mariadb := mysqlServerVersion(t, db)

	e := mysql.JSONContains(doc, "notjson")
	rows, err := db.Select(e).From(tbl).Rows(context.Background())
	if err == nil {
		defer rows.Close()
	}
	if mariadb {
		if err != nil {
			t.Fatalf("MariaDB is documented to answer NULL rather than to fail: %v", err)
		}
		if !rows.Next() {
			t.Fatal("no row")
		}
		var v any
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		if got := mysqlText(v); got != "<nil>" {
			t.Errorf("JSON_CONTAINS with a non-JSON candidate = %s, want NULL", got)
		}
	} else {
		if err == nil {
			t.Fatal("MySQL is documented to raise error 3141 for a candidate that is not JSON")
		}
		if !strings.Contains(err.Error(), "3141") && !strings.Contains(strings.ToLower(err.Error()), "invalid json") {
			t.Fatalf("want the invalid-JSON error, got %v", err)
		}
	}

	// The quoted form is what the doc tells callers to pass, and it
	// answers the same on both.
	if got := jsonValue(t, db, tbl, mysql.JSONContains(mysql.JSONGet(doc, "b"), `{"c": "x"}`)); got != "1" {
		t.Errorf("a properly quoted candidate = %s, want 1", got)
	}
}

// The half of SetIfChanged that the NULL test does not reach: <=> is
// null-safe but not collation-blind. PostgreSQL's IS DISTINCT FROM
// compares text under a deterministic collation, so 'ada' and 'ADA'
// are distinct and the row is written. MySQL compares under the
// column's collation, which is case- and accent-insensitive by
// default, so the same patch writes nothing at all — and a plain
// assignment on the same column would have written it.
//
// This is a claim about the server's collation, not about the SQL, so
// it can only be made by patching a row and reading it back.
func TestMySQLSetIfChangedFollowsTheColumnCollation(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()

	type row struct {
		ID    int64  `drop:"id"`
		Label string `drop:"label"`
	}
	build := func(name, collation string) (*mysql.Entity[row], *mysql.Col[string]) {
		t.Helper()
		tbl := mysql.NewTable(integration.UniqueName(t, name))
		if collation != "" {
			tbl.Charset("utf8mb4").Collate(collation)
		}
		mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
		label := mysql.Add(tbl, mysql.Varchar("label", 64).NotNull())
		dropMySQL(t, db, tbl)
		execMySQL(t, db, mysql.CreateTable(tbl))
		if _, err := db.Insert(tbl).Row(label.Val("ada")).Exec(ctx); err != nil {
			t.Fatal(err)
		}
		return mysql.NewEntity[row](tbl), label
	}

	// The server's default collation, whatever the two servers make it
	// — utf8mb4_general_ci on MariaDB 10.11, utf8mb4_0900_ai_ci on
	// MySQL 8.4. Both are case- and accent-insensitive, which is the
	// only property this asserts.
	ci, ciLabel := build("ifchci", "")
	if got := mysqlValue(t, db, mysql.Eq(drops.Raw("'ada'"), drops.Raw("'ADA'"))); got != "1" {
		t.Skipf("this server's default collation is case-sensitive (%q); the divergence needs a case-insensitive one", got)
	}
	res, err := ci.Patch(db, ctx, int64(1), mysql.SetIfChanged(ciLabel, "ADA"))
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 0 {
		t.Errorf("rows affected = %d; under a case-insensitive collation <=> calls this unchanged", n)
	}
	got, err := ci.Get(db, ctx, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	if got.Label != "ada" {
		t.Errorf("label = %q, want the original %q — SetIfChanged is documented not to write a case-only change", got.Label, "ada")
	}
	// A plain assignment does write it, which is what makes the
	// difference SetIfChanged's own and not the server's.
	if _, err := ci.Patch(db, ctx, int64(1), mysql.Set(ciLabel, "ADA")); err != nil {
		t.Fatal(err)
	}
	if got, _ = ci.Get(db, ctx, int64(1)); got.Label != "ADA" {
		t.Errorf("a plain Set left the label at %q", got.Label)
	}

	// Under a binary collation the operator is exact, which is the
	// workaround SetIfChanged's doc points at.
	bin, binLabel := build("ifchbin", "utf8mb4_bin")
	if _, err := bin.Patch(db, ctx, int64(1), mysql.SetIfChanged(binLabel, "ADA")); err != nil {
		t.Fatal(err)
	}
	if got, _ = bin.Get(db, ctx, int64(1)); got.Label != "ADA" {
		t.Errorf("under utf8mb4_bin the label = %q, want ADA", got.Label)
	}
}

// ----------------------------------------------------------------------
// Outbox: the schema, against the migration layer and the catalogue
// ----------------------------------------------------------------------

// An application that manages its schema with [mysql.Push] never calls
// OutboxDDL, so the drain indexes have to be on the table itself — and
// the push has to settle, because a table that MODIFYs a column on
// every deploy rebuilds the outbox on every deploy. Both are questions
// for the catalogue: what the declared type looks like coming back out
// of information_schema is the whole of it, and a rendered CREATE
// TABLE cannot say.
func TestMySQLOutboxTableSurvivesThePushLoop(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl := mysql.NewOutboxTable(integration.UniqueName(t, "pushbox"))
	dropMySQL(t, db, tbl)

	schema := mysql.NewSchema(tbl)
	first, err := mysql.Push(ctx, db, schema)
	if err != nil {
		t.Fatalf("push: %v (applied %d of %d, failed on %q)",
			err, first.AppliedCount, len(first.Statements), first.Failed)
	}

	// The two indexes the drain and the per-aggregate drain run on.
	rows, err := db.Query(ctx, `SELECT INDEX_NAME, COLUMN_NAME FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? ORDER BY INDEX_NAME, SEQ_IN_INDEX`, tbl.Name())
	if err != nil {
		t.Fatal(err)
	}
	built := map[string][]string{}
	for rows.Next() {
		var idx, col string
		if err := rows.Scan(&idx, &col); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		built[idx] = append(built[idx], col)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var drain, agg bool
	for name, cols := range built {
		joined := strings.Join(cols, ",")
		if strings.Contains(name, "drain") && joined == "publishedAt,id" {
			drain = true
		}
		if strings.Contains(name, "aggregate") && joined == "publishedAt,aggregateType,aggregateID,id" {
			agg = true
		}
	}
	if !drain || !agg {
		t.Errorf("Push built %v; the outbox needs both drain indexes or its hot query is a full scan", built)
	}

	// And the collation survives the round trip, so the push settles.
	second, err := mysql.Push(ctx, db, schema)
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if len(second.Statements) != 0 {
		t.Errorf("the outbox does not settle — every deploy rewrites it:\n%s",
			strings.Join(second.Statements, "\n"))
	}
	for _, n := range second.Notices {
		t.Errorf("unexpected notice: %s", n)
	}

	// The collation is the point of declaring one at all.
	coll := mysqlScalar(t, db, `SELECT COLLATION_NAME FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = 'aggregateID'`, tbl.Name())
	if coll != "utf8mb4_bin" {
		t.Errorf("aggregateID came back as %s; a pushed outbox folds case", coll)
	}
}

// The collation is declared so two aggregates differing only in case
// stay two aggregates. Only the server can say whether they do.
func TestMySQLOutboxAggregateKeysAreCaseSensitive(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	ob, _ := mysqlOutboxFixture(t, db)

	for _, id := range []string{"Cart-7", "cart-7"} {
		if err := db.InTx(ctx, func(tx *mysql.DB) error {
			return ob.EmitWith(tx, ctx, "e", nil, mysql.EmitOptions{AggregateType: "cart", AggregateID: id})
		}); err != nil {
			t.Fatalf("emit %q: %v", id, err)
		}
	}

	aggs, err := ob.PendingAggregates(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(aggs) != 2 {
		t.Fatalf("PendingAggregates returned %+v; under a case-insensitive collation the two ids are one ordering scope", aggs)
	}
	var drained int
	if err := ob.DrainAggregate(ctx, "cart", "Cart-7", 10, func(_ *mysql.DB, evs []mysql.OutboxEvent) error {
		drained = len(evs)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if drained != 1 {
		t.Errorf("draining Cart-7 took %d events; it should not see cart-7's", drained)
	}
}

// The GET_LOCK name is folded to 64 characters by cutting bytes, so a
// non-ASCII aggregate id lands the cut inside a rune. MariaDB takes
// the mangled name without complaint, which is exactly why this has to
// be exercised rather than reasoned about: the fold must produce a
// name that is valid UTF-8 before the server's tolerance is what is
// keeping it working.
func TestMySQLOutboxAggregateLockHandlesAMultibyteID(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	ob, _ := mysqlOutboxFixture(t, db)

	// Long enough that "drops:<table>:<type>:<id>" overruns 64 bytes
	// several times over.
	id := strings.Repeat("世", 40)
	if err := db.InTx(ctx, func(tx *mysql.DB) error {
		return ob.EmitWith(tx, ctx, "e", nil, mysql.EmitOptions{AggregateType: "カート", AggregateID: id})
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	drained := 0
	if err := ob.DrainAggregate(ctx, "カート", id, 10, func(_ *mysql.DB, evs []mysql.OutboxEvent) error {
		drained = len(evs)
		return nil
	}); err != nil {
		t.Fatalf("DrainAggregate on a multibyte aggregate id: %v", err)
	}
	if drained != 1 {
		t.Errorf("drained %d events, want 1", drained)
	}

	// The lock is free again, which it would not be had the release
	// gone to a differently-mangled name.
	second := 0
	if err := ob.DrainAggregate(ctx, "カート", id, 10, func(_ *mysql.DB, evs []mysql.OutboxEvent) error {
		second = len(evs)
		return nil
	}); err != nil {
		t.Fatalf("second DrainAggregate: %v", err)
	}
	if second != 1 {
		t.Errorf("the second drain saw %d events; the lock was not released under the same name", second)
	}
}

// An index name is the table's name plus a suffix, and MySQL stops at
// 64 bytes. The unit test checks the name it renders; whether the
// whole DDL is legal — and whether the two indexes stayed distinct
// after the fold — is the server's answer.
func TestMySQLOutboxDDLSurvivesALongTableName(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	// 60 bytes: legal as a table name, illegal once a prefix and a
	// suffix are added.
	tbl := mysql.NewOutboxTable(strings.Repeat("o", 60))
	dropMySQL(t, db, tbl)
	for _, stmt := range mysql.OutboxDDL(tbl) {
		execMySQL(t, db, stmt)
	}

	n := mysqlScalar(t, db, `SELECT COUNT(DISTINCT INDEX_NAME) FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, tbl.Name())
	if n != "3" {
		t.Errorf("the table carries %s indexes (PRIMARY + 2 expected); the fold collapsed two names onto one", n)
	}

	// And it still works as an outbox.
	ob := mysql.NewOutbox(db, tbl.Name())
	if err := db.InTx(ctx, func(tx *mysql.DB) error { return ob.Emit(tx, ctx, "e", nil) }); err != nil {
		t.Fatalf("emit: %v", err)
	}
	events, err := ob.Drain(ctx, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("drain returned %d events, %v", len(events), err)
	}
}

// BeforeCursor is the half of the cursor API that only a unit test had
// ever exercised, and a rendered comparison operator is not evidence
// that a walk terminates. This runs one backwards over a nullable key,
// which is where the guard's hand-encoded NULL placement has to be
// right in the direction it was not written for: paging back through an
// ascending order means reaching the NULLs at the start, and paging
// back through a descending one means reaching them at the end.
func TestMySQLBackwardCursorWalksBackOverTheNullBoundary(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()

	tbl := mysql.NewTable(integration.UniqueName(t, "backnick"))
	dropMySQL(t, db, tbl)
	id := mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	nick := mysql.Add(tbl, mysql.Varchar("nick", 32))
	execMySQL(t, db, mysql.CreateTable(tbl))
	execMySQL(t, db, mysql.NewIndex(integration.UniqueName(t, "ixb"), tbl, nick, id))

	ent := mysql.NewEntity[nullableRow](tbl)
	var rows []nullableRow
	for i := 0; i < 60; i++ {
		if i%3 == 0 {
			rows = append(rows, nullableRow{})
			continue
		}
		v := fmt.Sprintf("nick%02d", i%7)
		rows = append(rows, nullableRow{Nick: &v})
	}
	if _, err := ent.CreateMany(db, ctx, rows); err != nil {
		t.Fatalf("seed: %v", err)
	}

	for _, desc := range []bool{false, true} {
		name := "ascending"
		if desc {
			name = "descending"
		}
		t.Run(name, func(t *testing.T) {
			spec := mysql.NewCursorSpec(
				mysql.OrderKey{Col: nick, Desc: desc},
				mysql.OrderKey{Col: id, Desc: desc},
			)
			// Backward paging reads the order upside down: the page
			// adjacent to the cursor is the FIRST few rows of the
			// reversed ordering, not the first few of the forward one.
			reversed := mysql.NewCursorSpec(
				mysql.OrderKey{Col: nick, Desc: !desc},
				mysql.OrderKey{Col: id, Desc: !desc},
			)

			// Start from the last row of the forward order, which is
			// the first of the reversed one.
			var tail []nullableRow
			if err := db.Select(id, nick).From(tbl).OrderByCursor(reversed).Limit(1).All(ctx, &tail); err != nil {
				t.Fatalf("tail: %v", err)
			}
			if len(tail) != 1 {
				t.Fatalf("the table is empty")
			}
			cursor, err := mysql.EncodeCursor(spec, tail[0].Nick, tail[0].ID)
			if err != nil {
				t.Fatal(err)
			}

			seen := map[int64]int{tail[0].ID: 1}
			nullsCrossed := 0
			for page := 0; page < 40; page++ {
				var got []nullableRow
				q := db.Select(id, nick).From(tbl).
					BeforeCursor(spec, cursor).
					OrderByCursor(reversed).
					Limit(7)
				if err := q.All(ctx, &got); err != nil {
					text, args := q.ToSQL()
					t.Fatalf("backward page %d: %v\n%s\nargs: %v", page, err, text, args)
				}
				if len(got) == 0 {
					break
				}
				for _, r := range got {
					seen[r.ID]++
					if r.Nick == nil {
						nullsCrossed++
					}
				}
				last := got[len(got)-1]
				if cursor, err = mysql.EncodeCursor(spec, last.Nick, last.ID); err != nil {
					t.Fatal(err)
				}
			}

			if len(seen) != len(rows) {
				t.Errorf("the backward walk visited %d of %d rows", len(seen), len(rows))
			}
			for rowID, count := range seen {
				if count != 1 {
					t.Errorf("row %d visited %d times walking backwards", rowID, count)
				}
			}
			// 20 of the 60 rows carry a NULL nick; a walk that stopped
			// at the boundary instead of crossing it would still visit
			// some of them, so the count is what makes the assertion
			// mean something.
			if nullsCrossed < 19 {
				t.Errorf("the backward walk reached %d NULL-keyed rows, want the 20 that exist"+
					" (less the one it started on)", nullsCrossed)
			}
		})
	}
}

// ----------------------------------------------------------------------
// Aliasing: an aliased handle means the same column and renders
// differently. Every test below crosses two handles of one table
// inside a statement drops itself composes, which is the shape none of
// the tests above reach.
// ----------------------------------------------------------------------

type aliasRow struct {
	ID   int64  `drop:"id"`
	Name string `drop:"name"`
	Age  int32  `drop:"age"`
}

// The flagship. An INSERT aligns every row after the first against the
// column list the first one fixed, and a value bound through an alias
// that fails to match falls to the DEFAULT fill: the statement is
// well-formed, the server accepts it, and the row is wrong. This is
// the one failure in the package no server can report.
func TestMySQLInsertRowBoundThroughAnAliasKeepsItsValues(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl := mysql.NewTable(integration.UniqueName(t, "alias_ins"))
	dropMySQL(t, db, tbl)

	id := mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	name := mysql.Add(tbl, mysql.Varchar("name", 64).NotNull().Default("'anon'"))
	age := mysql.Add(tbl, mysql.Integer("age").NotNull().Default("0"))
	execMySQL(t, db, mysql.CreateTable(tbl))

	u := tbl.As("u")
	// The key is bound through the declared handle so it aligns
	// either way. That is what keeps the failure silent: with the
	// other two unmatched the row still satisfies every NOT NULL from
	// its DEFAULTs, so the server has nothing to object to and writes
	// ("anon", 0).
	ins := db.Insert(tbl).
		Row(id.Val(1), name.Val("declared"), age.Val(41)).
		Row(id.Val(2),
			mysql.Bind(u.Col("name"), "aliased"),
			mysql.Bind(u.Col("age"), int32(42)))
	text, args := ins.ToSQL()
	// Naming the render as well as the rows is what tells the two
	// failure modes apart: without it a wrong row reads as a scan bug.
	if strings.Contains(text, "DEFAULT") {
		t.Errorf("the aliased row lost its bindings to the DEFAULT fill:\n%s\nargs: %v", text, args)
	}
	if _, err := ins.Exec(ctx); err != nil {
		t.Fatalf("insert: %v\n%s\nargs: %v", err, text, args)
	}

	var got []aliasRow
	if err := db.Select(id, name, age).From(tbl).OrderBy(id.Asc()).All(ctx, &got); err != nil {
		t.Fatal(err)
	}
	want := []aliasRow{{1, "declared", 41}, {2, "aliased", 42}}
	if len(got) != len(want) {
		t.Fatalf("%d rows, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("row %d = %+v, want %+v", i, got[i], w)
		}
	}
}

// The same seam on a NOT NULL column with no DEFAULT, where the server
// does object. Both branches come from one statement shape, so a
// schema change is all that separates the silent corruption above from
// this hard failure — which is why the silent one had to be found by
// reading rows and not by watching for errors.
func TestMySQLInsertThroughAnAliasOnAColumnWithNoDefault(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl := mysql.NewTable(integration.UniqueName(t, "alias_nd"))
	dropMySQL(t, db, tbl)

	id := mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	name := mysql.Add(tbl, mysql.Varchar("name", 64).NotNull())
	execMySQL(t, db, mysql.CreateTable(tbl))

	u := tbl.As("u")
	if _, err := db.Insert(tbl).
		Row(id.Val(1), name.Val("declared")).
		Row(mysql.Bind(u.Col("id"), int64(2)), mysql.Bind(u.Col("name"), "aliased")).
		Exec(ctx); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var got []struct {
		Name string `drop:"name"`
	}
	if err := db.Select(name).From(tbl).OrderBy(id.Asc()).All(ctx, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].Name != "aliased" {
		t.Errorf("rows = %+v, want the aliased row's own name", got)
	}
}

// A row that genuinely omits a column still defaults, so the fix
// cannot be "bind everything".
func TestMySQLInsertShortRowThroughAnAliasStillDefaults(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl := mysql.NewTable(integration.UniqueName(t, "alias_short"))
	dropMySQL(t, db, tbl)

	id := mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	name := mysql.Add(tbl, mysql.Varchar("name", 64).NotNull().Default("'anon'"))
	age := mysql.Add(tbl, mysql.Integer("age").NotNull().Default("7"))
	execMySQL(t, db, mysql.CreateTable(tbl))

	u := tbl.As("u")
	if _, err := db.Insert(tbl).
		Row(id.Val(1), name.Val("declared"), age.Val(41)).
		Row(mysql.Bind(u.Col("id"), int64(2)), mysql.Bind(u.Col("age"), int32(42))).
		Exec(ctx); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var got []aliasRow
	if err := db.Select(id, name, age).From(tbl).OrderBy(id.Asc()).All(ctx, &got); err != nil {
		t.Fatal(err)
	}
	want := []aliasRow{{1, "declared", 41}, {2, "anon", 42}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("rows = %+v, want %+v", got, want)
	}
}

// Asc and Desc are free functions, so a page's ordering column is
// whichever handle the caller held. Rendered against a FROM that names
// the other one, MariaDB answers 1054 — in both directions, since once
// a FROM carries an alias the base name stops being a legal qualifier
// and without one the alias never was.
func TestMySQLPageOrderByCrossesHandles(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl := mysql.NewTable(integration.UniqueName(t, "alias_page"))
	dropMySQL(t, db, tbl)

	id := mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	name := mysql.Add(tbl, mysql.Varchar("name", 64).NotNull())
	age := mysql.Add(tbl, mysql.Integer("age").NotNull())
	execMySQL(t, db, mysql.CreateTable(tbl))

	for i := 1; i <= 9; i++ {
		if _, err := db.Insert(tbl).
			Row(id.Val(int64(i)), name.Val(fmt.Sprintf("n%02d", i)), age.Val(int32(i))).
			Exec(ctx); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	u := tbl.As("u")
	// walk pages the entity to exhaustion and returns the ids in
	// visit order, so the cursor guard is exercised as well as the
	// ORDER BY — both render the ordering column.
	walk := func(t *testing.T, page func(cursor string) (*mysql.Page[aliasRow], error)) []int64 {
		t.Helper()
		var ids []int64
		cursor := ""
		for i := 0; i < 10; i++ {
			p, err := page(cursor)
			if err != nil {
				t.Fatalf("page %d: %v", i, err)
			}
			for _, r := range p.Items {
				ids = append(ids, r.ID)
			}
			if !p.HasMore {
				break
			}
			cursor = p.NextCursor
		}
		return ids
	}
	want := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9}

	t.Run("entity on the table, ordered by the alias's handle", func(t *testing.T) {
		ent := mysql.NewEntity[aliasRow](tbl)
		got := walk(t, func(c string) (*mysql.Page[aliasRow], error) {
			return ent.Page(db).OrderBy(mysql.Asc(u.Col("id"))).Limit(4).After(c).All(ctx)
		})
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("ids = %v, want %v", got, want)
		}
	})

	t.Run("entity on the alias, ordered by the declared handle", func(t *testing.T) {
		ent := mysql.NewEntity[aliasRow](u)
		got := walk(t, func(c string) (*mysql.Page[aliasRow], error) {
			return ent.Page(db).OrderBy(mysql.Asc(id)).Limit(4).After(c).All(ctx)
		})
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("ids = %v, want %v", got, want)
		}
	})

	t.Run("a cursor stamped under one handle spends under the other", func(t *testing.T) {
		ent := mysql.NewEntity[aliasRow](tbl)
		first, err := ent.Page(db).OrderBy(mysql.Asc(id)).Limit(4).All(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !first.HasMore {
			t.Fatal("the first page should not have exhausted the table")
		}
		// Same walk, alias handle, resuming on the declared handle's
		// token: the fingerprint is name-based, so tokens already
		// issued stay valid across the fix.
		next, err := ent.Page(db).OrderBy(mysql.Asc(u.Col("id"))).Limit(4).After(first.NextCursor).All(ctx)
		if err != nil {
			t.Fatalf("resuming under the other handle: %v", err)
		}
		if len(next.Items) == 0 || next.Items[0].ID != 5 {
			t.Errorf("second page starts at %+v, want id 5", next.Items)
		}
	})
}

// A CursorSpec handed straight to SelectBuilder has the same hazard,
// and the FROM may arrive after the spec does.
func TestMySQLCursorSpecCrossesHandles(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl := mysql.NewTable(integration.UniqueName(t, "alias_spec"))
	dropMySQL(t, db, tbl)

	id := mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	execMySQL(t, db, mysql.CreateTable(tbl))
	for i := 1; i <= 5; i++ {
		if _, err := db.Insert(tbl).Row(id.Val(int64(i))).Exec(ctx); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	u := tbl.As("u")
	type row struct {
		ID int64 `drop:"id"`
	}

	aliasSpec := mysql.NewCursorSpec(mysql.OrderKey{Col: u.Col("id")})
	declaredSpec := mysql.NewCursorSpec(mysql.OrderKey{Col: id})

	cur, err := mysql.EncodeCursor(aliasSpec, int64(2))
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		q    func() *mysql.SelectBuilder
	}{
		{"alias spec over the declared FROM", func() *mysql.SelectBuilder {
			return db.Select(id).From(tbl).OrderByCursor(aliasSpec).AfterCursor(aliasSpec, cur)
		}},
		{"declared spec over the aliased FROM", func() *mysql.SelectBuilder {
			c, _ := mysql.EncodeCursor(declaredSpec, int64(2))
			return db.Select(u.Col("id")).From(u).OrderByCursor(declaredSpec).AfterCursor(declaredSpec, c)
		}},
		{"FROM arriving after the spec", func() *mysql.SelectBuilder {
			return db.Select(id).OrderByCursor(aliasSpec).AfterCursor(aliasSpec, cur).From(tbl)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []row
			q := tc.q()
			if err := q.All(ctx, &got); err != nil {
				text, args := q.ToSQL()
				t.Fatalf("%v\n%s\nargs: %v", err, text, args)
			}
			if len(got) != 3 || got[0].ID != 3 {
				t.Errorf("rows = %+v, want ids 3..5", got)
			}
		})
	}
}

// A Patch names its column on the right of the assignment, qualified.
// An entity on an alias patched with the package-level handles — the
// natural spelling, since (*Table).As hands out untyped columns and
// Inc wants a typed one — therefore named a relation the UPDATE did
// not, and MariaDB answered 1054 in the SET.
func TestMySQLPatchCrossesHandles(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl := mysql.NewTable(integration.UniqueName(t, "alias_patch"))
	dropMySQL(t, db, tbl)

	id := mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	name := mysql.Add(tbl, mysql.Varchar("name", 64).NotNull())
	age := mysql.Add(tbl, mysql.Integer("age").NotNull())
	execMySQL(t, db, mysql.CreateTable(tbl))
	if _, err := db.Insert(tbl).Row(id.Val(1), name.Val("ada"), age.Val(10)).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	u := tbl.As("u")
	ent := mysql.NewEntity[aliasRow](u)
	if _, err := ent.Patch(db, ctx, int64(1),
		mysql.Inc(age, 5),
		mysql.SetIfGreater(age, 12),
		// A change of case alone is not a change under MariaDB's
		// default collation — see SetIfChanged — so the new value
		// differs by more than that.
		mysql.SetIfChanged(name, "grace"),
	); err != nil {
		t.Fatalf("patch through the entity's alias: %v", err)
	}

	var got []aliasRow
	if err := db.Select(id, name, age).From(tbl).All(ctx, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Age != 15 || got[0].Name != "grace" {
		t.Errorf("row = %+v, want age 15 and name grace", got)
	}

	// And the other direction: a plain UPDATE on the table whose
	// operation was built from the alias's handle.
	aliasAge := &mysql.Col[int32]{Column: u.Col("age")}
	if _, err := db.Update(tbl).Set(mysql.Inc(aliasAge, 1)).
		Where(mysql.Eq(id, int64(1))).Exec(ctx); err != nil {
		t.Fatalf("update through the alias's handle: %v", err)
	}
	got = nil
	if err := db.Select(id, name, age).From(tbl).All(ctx, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Age != 16 {
		t.Errorf("row = %+v, want age 16", got)
	}
}

// The whole entity CRUD path through an alias, so the identity fix is
// checked against the server rather than against a rendered string.
func TestMySQLEntityCRUDThroughAnAlias(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl := mysql.NewTable(integration.UniqueName(t, "alias_crud"))
	dropMySQL(t, db, tbl)

	id := mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(tbl, mysql.Varchar("name", 64).NotNull())
	mysql.Add(tbl, mysql.Integer("age").NotNull())
	execMySQL(t, db, mysql.CreateTable(tbl))

	ent := mysql.NewEntity[aliasRow](tbl.As("u"))
	r := aliasRow{Name: "ada", Age: 36}
	if err := ent.Create(db, ctx, &r); err != nil {
		t.Fatalf("create: %v", err)
	}
	if r.ID == 0 {
		t.Fatal("the generated key was not written back")
	}
	r.Age = 37
	if err := ent.Update(db, ctx, &r); err != nil {
		t.Fatalf("update: %v", err)
	}
	back, err := ent.Get(db, ctx, r.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if back.Age != 37 || back.Name != "ada" {
		t.Errorf("row = %+v", back)
	}
	if _, err := ent.Delete(db, ctx, r.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var left []aliasRow
	if err := db.Select(id).From(tbl).All(ctx, &left); err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("%d rows left", len(left))
	}
}

// An index declared against either handle of one table has to reach
// the server the same way — CREATE INDEX takes no AS clause, so the
// statement names the table either way.
func TestMySQLIndexDeclaredThroughAnAliasHandle(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl := mysql.NewTable(integration.UniqueName(t, "alias_idx"))
	dropMySQL(t, db, tbl)

	mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	mysql.Add(tbl, mysql.Varchar("name", 64).NotNull())
	execMySQL(t, db, mysql.CreateTable(tbl))

	u := tbl.As("u")
	idx := mysql.NewIndex("idx_"+tbl.Name(), u, u.Col("name"))
	tbl.AddIndex(idx)
	execMySQL(t, db, idx)

	snap, err := mysql.Introspect(ctx, db)
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	ts := snap.Tables[tbl.Name()]
	if ts == nil {
		t.Fatalf("table %q is not in the snapshot", tbl.Name())
	}
	if _, ok := ts.Indexes["idx_"+tbl.Name()]; !ok {
		t.Errorf("the index declared through the alias is not on the server: %+v", ts.Indexes)
	}
}

// An upsert's assignment list has the same shape as a Patch's, and
// one difference: INSERT INTO is written by writeName and carries no
// AS clause, so the declared table is the only legal qualifier even
// when the insert itself was built through an alias. MariaDB reports
// the mismatch against the SELECT rather than the SET, which is its
// own reason to pin it here.
func TestMySQLUpsertAssignmentCrossesHandles(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl := mysql.NewTable(integration.UniqueName(t, "alias_odku"))
	dropMySQL(t, db, tbl)

	id := mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	age := mysql.Add(tbl, mysql.Integer("age").NotNull())
	execMySQL(t, db, mysql.CreateTable(tbl))
	if _, err := db.Insert(tbl).Row(id.Val(1), age.Val(10)).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	u := tbl.As("u")
	aliasAge := &mysql.Col[int32]{Column: u.Col("age")}

	if _, err := db.Insert(tbl).Row(id.Val(1), age.Val(10)).
		OnDuplicateKeyUpdate(mysql.Inc(aliasAge, 1)).Exec(ctx); err != nil {
		t.Fatalf("upsert with an op from the alias's handle: %v", err)
	}
	// The insert itself on the alias, ops from the alias too: still
	// the declared name, because the INTO clause never aliases.
	if _, err := db.Insert(u).
		Row(mysql.Bind(u.Col("id"), int64(1)), mysql.Bind(u.Col("age"), int32(10))).
		OnDuplicateKeyUpdate(mysql.Inc(aliasAge, 1)).Exec(ctx); err != nil {
		t.Fatalf("upsert through the alias: %v", err)
	}

	var got []struct {
		Age int32 `drop:"age"`
	}
	if err := db.Select(age).From(tbl).All(ctx, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Age != 12 {
		t.Errorf("rows = %+v, want a single row at age 12", got)
	}
}

// A comma join names two instances of one table just as a JOIN does,
// and drops re-emits a FromExpr source without reading it — so it
// cannot see the second one. Restating the spec's key against the FROM
// table then orders and guards on the wrong instance, and MariaDB has
// nothing to object to: both relations exist, so the statement is
// valid SQL over the wrong rows. This is the shape that has to be read
// off the server rather than off the render.
func TestMySQLCursorSpecOverACommaSelfJoin(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl := mysql.NewTable(integration.UniqueName(t, "alias_comma"))
	dropMySQL(t, db, tbl)

	id := mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	execMySQL(t, db, mysql.CreateTable(tbl))
	for i := 1; i <= 3; i++ {
		if _, err := db.Insert(tbl).Row(id.Val(int64(i))).Exec(ctx); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	u := tbl.As("u")
	spec := mysql.NewCursorSpec(mysql.OrderKey{Col: u.Col("id")})
	cur, err := mysql.EncodeCursor(spec, int64(1))
	if err != nil {
		t.Fatal(err)
	}

	q := db.Select(u.Col("id")).From(tbl).FromExpr(u).
		OrderByCursor(spec).AfterCursor(spec, cur)
	var got []struct {
		ID int64 `drop:"id"`
	}
	if err := q.All(ctx, &got); err != nil {
		text, args := q.ToSQL()
		t.Fatalf("%v\n%s\nargs: %v", err, text, args)
	}
	ids := make([]int64, len(got))
	for i, r := range got {
		ids[i] = r.ID
	}
	// The cross join pairs every row with every row. Guarding and
	// ordering on `u` leaves each of u's two surviving ids three
	// times, in order; doing either on the un-aliased instance leaves
	// all three of u's ids twice, and interleaved.
	want := []int64{2, 2, 2, 3, 3, 3}
	if fmt.Sprint(ids) != fmt.Sprint(want) {
		text, args := q.ToSQL()
		t.Errorf("u.id = %v, want %v — the walk ran on the other instance\n%s\nargs: %v",
			ids, want, text, args)
	}
}
