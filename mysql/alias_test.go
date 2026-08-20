package mysql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/mysql"
)

// Aliasing is a query-scope rename. A handle taken off (*Table).As has
// to render with the alias and mean the same column everywhere else,
// so these tests split along that seam: the rendering is checked
// where the alias must show up, and the identity is checked where it
// must not.

// aliasTable is a table with a defaulted column and a counter, which
// is what the alignment and patch tests need.
func aliasTable() (*mysql.Table, *mysql.Col[int64], *mysql.Col[string], *mysql.Col[int32]) {
	t := mysql.NewTable("users")
	id := mysql.Add(t, mysql.BigSerial("id").PrimaryKey())
	name := mysql.Add(t, mysql.Varchar("name", 255).NotNull().Default("anon"))
	age := mysql.Add(t, mysql.Integer("age").NotNull().Default("0"))
	return t, id, name, age
}

// ----------------------------------------------------------------------
// INSERT alignment — the silent one.
// ----------------------------------------------------------------------

// The column list is fixed by the first Row, and every later row is
// aligned to it. Matching the two by pointer makes an alias handle a
// stranger to the column it names, so the row is filled with DEFAULT
// and the values the caller bound are dropped — well-formed SQL that
// no server can object to.
func TestAliasInsertRowBoundThroughAliasKeepsItsValues(t *testing.T) {
	tbl, id, name, _ := aliasTable()
	u := tbl.As("u")
	db := mysql.New(&fakeDriver{})
	got, args := db.Insert(tbl).
		Row(id.Val(1), name.Val("declared")).
		Row(mysql.Bind(u.Col("id"), int64(2)), mysql.Bind(u.Col("name"), "aliased")).
		ToSQL()
	if strings.Contains(got, "DEFAULT") {
		t.Errorf("the aliased row lost its bindings to the DEFAULT fill: %s", got)
	}
	if len(args) != 4 {
		t.Errorf("args = %v, want all four bound values", args)
	}
}

// The same seam in the other direction: the alias fixes the column
// list and the declared handles have to align to it.
func TestAliasInsertColumnListFixedByAliasedRow(t *testing.T) {
	tbl, id, name, _ := aliasTable()
	u := tbl.As("u")
	db := mysql.New(&fakeDriver{})
	got, args := db.Insert(tbl).
		Row(mysql.Bind(u.Col("id"), int64(1)), mysql.Bind(u.Col("name"), "aliased")).
		Row(id.Val(2), name.Val("declared")).
		ToSQL()
	if strings.Contains(got, "DEFAULT") {
		t.Errorf("the declared row lost its bindings to the DEFAULT fill: %s", got)
	}
	if len(args) != 4 {
		t.Errorf("args = %v, want all four bound values", args)
	}
}

// An alias of an alias has to chain back to the declared column, or
// the root becomes a stranger two hops out.
func TestAliasOfAliasInsertKeepsItsValues(t *testing.T) {
	tbl, id, name, _ := aliasTable()
	u2 := tbl.As("u").As("u2")
	db := mysql.New(&fakeDriver{})
	got, args := db.Insert(tbl).
		Row(id.Val(1), name.Val("declared")).
		Row(mysql.Bind(u2.Col("id"), int64(2)), mysql.Bind(u2.Col("name"), "aliased")).
		ToSQL()
	if strings.Contains(got, "DEFAULT") {
		t.Errorf("the twice-aliased row lost its bindings: %s", got)
	}
	if len(args) != 4 {
		t.Errorf("args = %v", args)
	}
}

// A column the row genuinely omits still defaults — the fix must not
// turn every short row into a bound one.
func TestAliasInsertStillDefaultsAGenuinelyMissingColumn(t *testing.T) {
	tbl, id, name, age := aliasTable()
	u := tbl.As("u")
	db := mysql.New(&fakeDriver{})
	got, _ := db.Insert(tbl).
		Row(id.Val(1), name.Val("a"), age.Val(41)).
		Row(mysql.Bind(u.Col("id"), int64(2)), mysql.Bind(u.Col("age"), int32(42))).
		ToSQL()
	if !strings.Contains(got, "(?, DEFAULT, ?)") {
		t.Errorf("the unbound column should still default: %s", got)
	}
}

// Two tables that happen to share a column name are not the same
// column, so identity is the declared column and never the name.
func TestInsertDoesNotAlignAStrangerTableSColumn(t *testing.T) {
	tbl, id, name, _ := aliasTable()
	_, otherID, _, _ := aliasTable()
	db := mysql.New(&fakeDriver{})
	got, _ := db.Insert(tbl).
		Row(id.Val(1), name.Val("a")).
		Row(mysql.Bind(otherID, int64(2))).
		ToSQL()
	if !strings.Contains(got, "(DEFAULT, DEFAULT)") {
		t.Errorf("a column of another table must not align: %s", got)
	}
}

// ----------------------------------------------------------------------
// Patch / UPDATE — the assignment's right-hand side is rendered.
// ----------------------------------------------------------------------

// Patch renders the counter's own column into the SET, qualified. An
// entity on an alias patched with the declared handles therefore names
// a relation the UPDATE does not, and MySQL answers 1054.
func TestAliasPatchOnAliasedEntityQualifiesWithTheAlias(t *testing.T) {
	tbl, _, _, age := aliasTable()
	u := tbl.As("u")
	ent := mysql.NewEntity[aliasUser](u)
	db := mysql.New(&fakeDriver{})
	if _, err := ent.Patch(db, context.Background(), int64(1), mysql.Inc(age, 1)); err != nil {
		t.Fatalf("patch: %v", err)
	}
	got := db.Driver().(*fakeDriver).queries[0]
	if !strings.Contains(got, "SET `age` = `u`.`age` + ?") {
		t.Errorf("the assignment must qualify with the relation the UPDATE names: %s", got)
	}
}

// And the reverse: an entity on the base table patched through an
// alias handle.
func TestAliasPatchThroughAliasHandleQualifiesWithTheTable(t *testing.T) {
	tbl, _, _, _ := aliasTable()
	u := tbl.As("u")
	aliasAge := &mysql.Col[int32]{Column: u.Col("age")}
	db := mysql.New(&fakeDriver{})
	got, _ := db.Update(tbl).Set(mysql.Inc(aliasAge, 1)).ToSQL()
	if !strings.Contains(got, "SET `age` = `users`.`age` + ?") {
		t.Errorf("the assignment must qualify with the relation the UPDATE names: %s", got)
	}
}

// SetIfGreater and SetIfChanged render the column too — twice, in the
// CASE — so both have to move with the statement.
func TestAliasPatchRebindsEveryOperationShape(t *testing.T) {
	tbl, _, name, age := aliasTable()
	u := tbl.As("u")
	ent := mysql.NewEntity[aliasUser](u)
	db := mysql.New(&fakeDriver{})
	if _, err := ent.Patch(db, context.Background(), int64(1),
		mysql.SetIfGreater(age, 9),
		mysql.SetIfChanged(name, "x"),
	); err != nil {
		t.Fatalf("patch: %v", err)
	}
	got := db.Driver().(*fakeDriver).queries[0]
	if strings.Contains(got, "`users`.") {
		t.Errorf("no assignment may name the un-aliased table: %s", got)
	}
	if !strings.Contains(got, "GREATEST(`u`.`age`, ?)") {
		t.Errorf("GREATEST kept the wrong handle: %s", got)
	}
	if strings.Count(got, "`u`.`name`") != 2 {
		t.Errorf("the CASE renders the column twice and both must move: %s", got)
	}
}

// A column of another table in a SET is not a handle to restate — it
// is a caller's cross-table reference, and rewriting it would change
// what the statement means.
//
// The stranger table has to carry a different name for the assertion
// to mean anything: rebinding matches on the column name first, so two
// tables both called "users" render the same qualifier whether the
// rebind fired or declined, and the test would pass either way.
func TestUpdateSetLeavesAStrangerTableSColumnAlone(t *testing.T) {
	tbl, _, _, _ := aliasTable()
	other := mysql.NewTable("audits")
	otherAge := mysql.Add(other, mysql.Integer("age").NotNull())
	db := mysql.New(&fakeDriver{})
	got, _ := db.Update(tbl).Set(mysql.Inc(otherAge, 1)).ToSQL()
	if !strings.Contains(got, "`age` = `audits`.`age` + ?") {
		t.Errorf("a column of another table must keep its own qualifier: %s", got)
	}
}

// ----------------------------------------------------------------------
// Page — Asc/Desc are free functions, so the handle is the caller's.
// ----------------------------------------------------------------------

type aliasUser struct {
	ID   int64  `drop:"id"`
	Name string `drop:"name"`
	Age  int32  `drop:"age"`
}

func TestAliasPageOrderByAcceptsAnAliasHandle(t *testing.T) {
	tbl, _, _, _ := aliasTable()
	u := tbl.As("u")
	ent := mysql.NewEntity[aliasUser](tbl)
	drv := &pageDriver{}
	db := mysql.New(drv)
	if _, err := ent.Page(db).OrderBy(mysql.Asc(u.Col("id"))).All(context.Background()); err != nil {
		t.Fatalf("page: %v", err)
	}
	if strings.Contains(drv.queries[0], "`u`.") {
		t.Errorf("the ORDER BY names a relation the FROM does not: %s", drv.queries[0])
	}
}

func TestAliasPageOrderByOnAnAliasedEntityAcceptsTheDeclaredHandle(t *testing.T) {
	tbl, id, _, _ := aliasTable()
	u := tbl.As("u")
	ent := mysql.NewEntity[aliasUser](u)
	drv := &pageDriver{}
	db := mysql.New(drv)
	if _, err := ent.Page(db).OrderBy(mysql.Asc(id)).All(context.Background()); err != nil {
		t.Fatalf("page: %v", err)
	}
	if strings.Contains(drv.queries[0], "`users`.") {
		t.Errorf("the ORDER BY names the table the FROM aliased away: %s", drv.queries[0])
	}
	if !strings.Contains(drv.queries[0], "ORDER BY `u`.`id`") {
		t.Errorf("ORDER BY = %s", drv.queries[0])
	}
}

// The cursor guard renders the ordering columns as well, so a resumed
// page has the same hazard as the first one.
func TestAliasPageCursorGuardRebindsToo(t *testing.T) {
	tbl, _, _, _ := aliasTable()
	u := tbl.As("u")
	ent := mysql.NewEntity[aliasUser](tbl)
	cur, err := mysql.EncodeCursor(
		mysql.NewCursorSpec(mysql.OrderKey{Col: u.Col("id")}), int64(7))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	drv := &pageDriver{}
	db := mysql.New(drv)
	if _, err := ent.Page(db).
		OrderBy(mysql.Asc(u.Col("id"))).
		After(string(cur)).
		All(context.Background()); err != nil {
		t.Fatalf("page: %v", err)
	}
	if strings.Contains(drv.queries[0], "`u`.") {
		t.Errorf("the cursor guard names a relation the FROM does not: %s", drv.queries[0])
	}
}

// Restating the ordering column must not restate the cursor's
// fingerprint: a token handed out under one handle is spendable under
// the other, and tokens are already in the wild.
func TestAliasCursorFingerprintIsHandleBlind(t *testing.T) {
	tbl, id, _, _ := aliasTable()
	u := tbl.As("u")
	ent := mysql.NewEntity[aliasUser](tbl)
	cur, err := mysql.EncodeCursor(
		mysql.NewCursorSpec(mysql.OrderKey{Col: id}), int64(7))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	drv := &pageDriver{}
	db := mysql.New(drv)
	if _, err := ent.Page(db).
		OrderBy(mysql.Asc(u.Col("id"))).
		After(string(cur)).
		All(context.Background()); err != nil {
		t.Fatalf("a cursor stamped under the declared handle must spend under the alias: %v", err)
	}
}

// ----------------------------------------------------------------------
// CursorSpec driven straight through SelectBuilder.
// ----------------------------------------------------------------------

func TestSelectOrderByCursorRestatesAgainstTheFromTable(t *testing.T) {
	tbl, id, _, _ := aliasTable()
	u := tbl.As("u")
	db := mysql.New(&fakeDriver{})

	spec := mysql.NewCursorSpec(mysql.OrderKey{Col: u.Col("id")})
	got, _ := db.Select(id).From(tbl).OrderByCursor(spec).ToSQL()
	if strings.Contains(got, "`u`.") {
		t.Errorf("ORDER BY names a relation the FROM does not: %s", got)
	}

	spec = mysql.NewCursorSpec(mysql.OrderKey{Col: id})
	got, _ = db.Select(u.Col("id")).From(u).OrderByCursor(spec).ToSQL()
	if strings.Contains(got, "`users`.") {
		t.Errorf("ORDER BY names the table the FROM aliased away: %s", got)
	}
}

// The keyset guard is built from the same keys.
func TestSelectAfterCursorRestatesAgainstTheFromTable(t *testing.T) {
	tbl, id, _, _ := aliasTable()
	u := tbl.As("u")
	spec := mysql.NewCursorSpec(mysql.OrderKey{Col: u.Col("id")})
	cur, err := mysql.EncodeCursor(spec, int64(7))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	db := mysql.New(&fakeDriver{})
	got, _ := db.Select(id).From(tbl).OrderByCursor(spec).AfterCursor(spec, cur).ToSQL()
	if strings.Contains(got, "`u`.") {
		t.Errorf("the keyset guard names a relation the FROM does not: %s", got)
	}
}

// From may be called after the spec is, so the restatement cannot
// happen when OrderByCursor is called.
func TestSelectOrderByCursorRestatesWhenFromArrivesLater(t *testing.T) {
	tbl, id, _, _ := aliasTable()
	u := tbl.As("u")
	db := mysql.New(&fakeDriver{})
	spec := mysql.NewCursorSpec(mysql.OrderKey{Col: u.Col("id")})
	got, _ := db.Select(id).OrderByCursor(spec).From(tbl).ToSQL()
	if strings.Contains(got, "`u`.") {
		t.Errorf("ORDER BY names a relation the FROM does not: %s", got)
	}
}

// A self-join names two instances of one table on purpose, and the
// spec picks between them. Restating would erase the distinction the
// alias exists to draw, so a statement that names both is left alone.
func TestSelectOrderByCursorLeavesASelfJoinAlone(t *testing.T) {
	tbl, id, _, _ := aliasTable()
	u := tbl.As("u")
	db := mysql.New(&fakeDriver{})
	spec := mysql.NewCursorSpec(mysql.OrderKey{Col: u.Col("id")})
	got, _ := db.Select(id).From(tbl).
		Join(u, mysql.Eq(id, u.Col("id"))).
		OrderByCursor(spec).ToSQL()
	if !strings.Contains(got, "ORDER BY `u`.`id`") {
		t.Errorf("a self-join's ordering must stay on the instance it names: %s", got)
	}
}

// A second instance can also arrive through FromExpr — a comma join on
// the alias, or a derived table carrying its name. drops re-emits that
// source without reading it, so it cannot count the instances, and
// moving the key would pick the wrong one silently: both relations
// exist, so the statement is valid SQL over the wrong rows.
func TestSelectOrderByCursorLeavesAFromExprSelfJoinAlone(t *testing.T) {
	tbl, _, _, _ := aliasTable()
	u := tbl.As("u")
	db := mysql.New(&fakeDriver{})
	spec := mysql.NewCursorSpec(mysql.OrderKey{Col: u.Col("id")})
	cur, err := mysql.EncodeCursor(spec, int64(7))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, _ := db.Select(u.Col("id")).From(tbl).FromExpr(u).
		OrderByCursor(spec).AfterCursor(spec, cur).ToSQL()
	if !strings.Contains(got, "ORDER BY `u`.`id`") {
		t.Errorf("the ordering must stay on the instance the spec names: %s", got)
	}
	if !strings.Contains(got, "(`u`.`id` > ?)") {
		t.Errorf("the keyset guard must stay on the instance the spec names: %s", got)
	}
}

// ----------------------------------------------------------------------
// AddIndex — the one *Table identity in the package.
// ----------------------------------------------------------------------

func TestAddIndexAcceptsEitherHandleOfOneTable(t *testing.T) {
	tbl, _, name, _ := aliasTable()
	u := tbl.As("u")
	tbl.AddIndex(mysql.NewIndex("idx_name", u, u.Col("name")))
	u.AddIndex(mysql.NewIndex("idx_name2", tbl, name))
	if len(tbl.Indexes()) != 1 || len(u.Indexes()) != 1 {
		t.Errorf("indexes = %d on the table, %d on the alias", len(tbl.Indexes()), len(u.Indexes()))
	}
}

// An index really built against another table still has to be
// refused, and the message has to name the two handles apart — both
// Name()s answer the base name, so an alias printed as its table reads
// as "cannot be added to itself".
func TestAddIndexPanicNamesTheAlias(t *testing.T) {
	tbl, _, name, _ := aliasTable()
	other := mysql.NewTable("users")
	mysql.Add(other, mysql.BigSerial("id").PrimaryKey())
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("an index built against another table should panic")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, `alias "u" of table "users"`) {
			t.Errorf("the message does not tell the two handles apart: %s", msg)
		}
	}()
	other.As("u").AddIndex(mysql.NewIndex("idx_name", tbl, name))
}

// ----------------------------------------------------------------------
// As copies — an alias must not write through into its table.
// ----------------------------------------------------------------------

func TestAliasChecksAreNotSharedWithTheBaseTable(t *testing.T) {
	tbl, _, _, _ := aliasTable()
	tbl.AddCheck("before", "age >= 0")
	u := tbl.As("u")
	tbl.AddCheck("after", "age < 200")
	if _, ok := u.Checks()["after"]; ok {
		t.Error("a check added after As reached the alias; the copy is meant to be a snapshot")
	}
	u.AddCheck("onlyOnTheAlias", "age < 150")
	if _, ok := tbl.Checks()["onlyOnTheAlias"]; ok {
		t.Error("the alias wrote a check through into its table")
	}
	if _, ok := u.Checks()["before"]; !ok {
		t.Error("the alias lost the checks it was copied with")
	}
}

func TestAliasSlicesDoNotShareSpareCapacity(t *testing.T) {
	tbl, id, _, _ := aliasTable()
	// Three appends leave len 3, cap 4 — one slot of spare capacity
	// both aliases would otherwise append into.
	for i := 0; i < 3; i++ {
		tbl.DefaultFilter(mysql.Eq(id, int64(i)))
		tbl.AddIndex(mysql.NewIndex("idx"+string(rune('a'+i)), tbl, id))
	}
	a1, a2 := tbl.As("a1"), tbl.As("a2")
	a1.DefaultFilter(mysql.Eq(id, int64(111)))
	a2.DefaultFilter(mysql.Eq(id, int64(222)))
	a1.AddIndex(mysql.NewIndex("idx_a1", tbl, id))
	a2.AddIndex(mysql.NewIndex("idx_a2", tbl, id))

	db := mysql.New(&fakeDriver{})
	_, args := db.Select(id).From(a1).ToSQL()
	if len(args) != 4 || args[3] != int64(111) {
		t.Errorf("a1's own filter was overwritten through the shared array: %v", args)
	}
	if got := a1.Indexes(); len(got) != 4 {
		t.Errorf("a1 has %d indexes, want 4", len(got))
	} else if sql, _ := sqlOf(got[3]); !strings.Contains(sql, "idx_a1") {
		t.Errorf("a1's own index was overwritten through the shared array: %s", sql)
	}
	if len(tbl.Indexes()) != 3 {
		t.Errorf("an alias appended an index into its table: %d", len(tbl.Indexes()))
	}
	if _, baseArgs := db.Select(id).From(tbl).ToSQL(); len(baseArgs) != 3 {
		t.Errorf("an alias appended a filter into its table: %v", baseArgs)
	}
}

// ----------------------------------------------------------------------
// ON DUPLICATE KEY UPDATE — the assignment list the inventory missed.
// ----------------------------------------------------------------------

// An upsert assignment renders its column qualified, just as a Patch
// op does, and INSERT INTO never carries an alias: writeName is what
// renders the target. So the declared table is the only qualifier the
// statement can accept, whichever handle the insert itself was built
// from — an upsert on an alias with the alias's own handles fails too,
// which is what makes this different from the UPDATE case.
func TestUpsertAssignmentQualifiesWithTheDeclaredTable(t *testing.T) {
	tbl, id, _, age := aliasTable()
	u := tbl.As("u")
	aliasAge := &mysql.Col[int32]{Column: u.Col("age")}
	db := mysql.New(&fakeDriver{})

	got, _ := db.Insert(tbl).Row(id.Val(1), age.Val(10)).
		OnDuplicateKeyUpdate(mysql.Inc(aliasAge, 1)).ToSQL()
	if !strings.Contains(got, "`age` = `users`.`age` + ?") {
		t.Errorf("an op from the alias's handle must restate: %s", got)
	}

	got, _ = db.Insert(u).
		Row(mysql.Bind(u.Col("id"), int64(1)), mysql.Bind(u.Col("age"), int32(10))).
		OnDuplicateKeyUpdate(mysql.Inc(aliasAge, 1)).ToSQL()
	if !strings.Contains(got, "`age` = `users`.`age` + ?") {
		t.Errorf("an upsert on an alias still names the declared table: %s", got)
	}
}
