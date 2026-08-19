package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

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

// A relation reached through an alias must rebind its near side to that
// alias. The failure is not a syntax error the server would catch: the
// un-rebound predicate is still valid SQL, it just compares one
// instance of the table to itself and quietly returns the wrong rows.
func TestMySQLRelationThroughAnAliasJoinsTheRightInstance(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl := mysql.NewTable(integration.UniqueName(t, "staff"))
	dropMySQL(t, db, tbl)

	id := mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	name := mysql.Add(tbl, mysql.Varchar("name", 64).NotNull())
	managerID := mysql.Add(tbl, mysql.BigInt("managerId"))
	mysql.NewRelations(tbl).BelongsTo("manager", tbl, managerID, id)
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
	// through the un-aliased handles, which is what the relation's far
	// side still names.
	emp := tbl.As("e")
	rel := emp.Rel("manager")
	var rows []struct {
		Staff   string `drop:"staff"`
		Manager string `drop:"manager"`
	}
	err := db.Select(emp.Col("name").As("staff"), name.As("manager")).
		From(emp).
		Join(tbl, mysql.Eq(rel.ChildKey, rel.ParentKey)).
		OrderBy(emp.Col("name").Asc()).
		All(ctx, &rows)
	if err != nil {
		t.Fatalf("relation join: %v", err)
	}
	want := map[string]string{"Alan": "Ada", "Grace": "Ada"}
	if len(rows) != len(want) {
		t.Fatalf("relation join returned %d rows, want %d: %+v", len(rows), len(want), rows)
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
