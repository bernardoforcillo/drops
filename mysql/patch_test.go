package mysql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/mysql"
)

type counterRow struct {
	ID    int64  `db:"id"`
	Likes int64  `db:"likes"`
	Name  string `db:"name"`
}

func counterFixture() (*mysql.Table, *mysql.Col[int64], *mysql.Col[int64], *mysql.Col[string]) {
	t := mysql.NewTable("counters")
	id := mysql.Add(t, mysql.BigSerial("id").PrimaryKey())
	likes := mysql.Add(t, mysql.BigInt("likes").NotNull())
	name := mysql.Add(t, mysql.Varchar("name", 64).NotNull())
	return t, id, likes, name
}

func TestPatchRendersServerSideAssignments(t *testing.T) {
	tbl, _, likes, name := counterFixture()
	ent := mysql.NewEntity[counterRow](tbl)
	drv := &fakeDriver{}
	db := mysql.New(drv)

	if _, err := ent.Patch(db, context.Background(), int64(7),
		mysql.Inc(likes, 1),
		mysql.SetIfGreater(likes, int64(100)),
		mysql.Set(name, "x"),
	); err != nil {
		t.Fatal(err)
	}
	sql := drv.queries[len(drv.queries)-1]
	want := "UPDATE `counters` SET " +
		"`likes` = `counters`.`likes` + ?, " +
		"`likes` = GREATEST(`counters`.`likes`, ?), " +
		"`name` = ? WHERE (`counters`.`id` = ?)"
	if sql != want {
		t.Errorf("SQL = %s\nwant  %s", sql, want)
	}
	if len(drv.args[len(drv.args)-1]) != 4 || drv.args[len(drv.args)-1][3] != int64(7) {
		t.Errorf("args = %v; the key must bind last", drv.args[len(drv.args)-1])
	}
}

// Dec subtracts; it does not negate the delta and add it. Negating is
// the obvious shorthand and it is wrong for every unsigned type in the
// number constraint — -uint64(3) is 18446744073709551613, so the
// counter would climb by that instead of falling by three. The bind
// has to stay positive.
func TestDecSubtractsRatherThanAddingANegative(t *testing.T) {
	tbl, _, likes, _ := counterFixture()
	ent := mysql.NewEntity[counterRow](tbl)
	drv := &fakeDriver{}
	if _, err := ent.Patch(mysql.New(drv), context.Background(), int64(1), mysql.Dec(likes, int64(3))); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(drv.queries[len(drv.queries)-1], "`likes` = `counters`.`likes` - ?") {
		t.Errorf("SQL = %s", drv.queries[len(drv.queries)-1])
	}
	if drv.args[len(drv.args)-1][0] != int64(3) {
		t.Errorf("delta = %v, want 3", drv.args[len(drv.args)-1][0])
	}
}

// The same claim over an unsigned delta, which is where negating
// silently wraps instead of failing.
func TestDecOnAnUnsignedDeltaDoesNotWrap(t *testing.T) {
	tbl := mysql.NewTable("seats")
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	seats := mysql.Add(tbl, mysql.Custom[uint64]("seats", "BIGINT UNSIGNED").NotNull())
	sql, args := mysql.New(&fakeDriver{}).Update(tbl).Set(mysql.Dec(seats, uint64(3))).ToSQL()
	if sql != "UPDATE `seats` SET `seats` = `seats`.`seats` - ?" {
		t.Errorf("SQL = %s", sql)
	}
	if len(args) != 1 || args[0] != uint64(3) {
		t.Fatalf("args = %v, want [3]; a negated uint64 binds 2^64-3", args)
	}
}

// PostgreSQL writes IS DISTINCT FROM here; neither MySQL nor MariaDB
// has it, and a plain <> would go NULL against a NULL column and skip
// the assignment entirely.
func TestSetIfChangedUsesTheNullSafeOperator(t *testing.T) {
	tbl, _, _, name := counterFixture()
	ent := mysql.NewEntity[counterRow](tbl)
	drv := &fakeDriver{}
	if _, err := ent.Patch(mysql.New(drv), context.Background(), int64(1), mysql.SetIfChanged(name, "y")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(drv.queries[len(drv.queries)-1], "IS DISTINCT FROM") {
		t.Fatalf("IS DISTINCT FROM does not exist on either server: %s", drv.queries[len(drv.queries)-1])
	}
	if !strings.Contains(drv.queries[len(drv.queries)-1], "CASE WHEN NOT (`counters`.`name` <=> ?) THEN ? ELSE `counters`.`name` END") {
		t.Errorf("SQL = %s", drv.queries[len(drv.queries)-1])
	}
}

func TestPatchWithNoOperationsIsAnError(t *testing.T) {
	tbl, _, _, _ := counterFixture()
	ent := mysql.NewEntity[counterRow](tbl)
	if _, err := ent.Patch(mysql.New(&fakeDriver{}), context.Background(), int64(1)); err == nil {
		t.Fatal("expected an error")
	}
}

func TestPatchKeyChecksArity(t *testing.T) {
	tbl, _, likes, _ := counterFixture()
	ent := mysql.NewEntity[counterRow](tbl)
	_, err := ent.PatchKey(mysql.New(&fakeDriver{}), context.Background(), []any{1, 2}, mysql.Inc(likes, 1))
	if err == nil {
		t.Fatal("expected a key-arity error")
	}
}

// A PatchOp is a ColumnValue, so the same helper feeds a plain UPDATE.
func TestPatchOpsGoStraightIntoAnUpdate(t *testing.T) {
	tbl, _, likes, _ := counterFixture()
	sql, args := mysql.New(&fakeDriver{}).Update(tbl).Set(mysql.Inc(likes, 5)).ToSQL()
	if sql != "UPDATE `counters` SET `likes` = `counters`.`likes` + ?" {
		t.Errorf("SQL = %s", sql)
	}
	if len(args) != 1 || args[0] != int64(5) {
		t.Errorf("args = %v", args)
	}
}
