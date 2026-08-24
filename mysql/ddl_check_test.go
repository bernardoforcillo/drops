package mysql_test

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/mysql"
)

func checkedTable() *mysql.Table {
	tbl := mysql.NewTable("accounts")
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(tbl, mysql.Integer("age").NotNull())
	mysql.Add(tbl, mysql.Integer("balance").NotNull())
	tbl.AddCheck("chk_age", "age >= 0")
	tbl.AddCheck("chk_balance", "balance >= 0")
	return tbl
}

// CreateTable and the migration path describe the same table, so a
// CHECK the declaration carries has to reach both. Dropping it here
// left a table created by CreateTable unconstrained while the same
// schema pushed through a migration was not.
func TestCreateTableRendersChecks(t *testing.T) {
	got, _ := sqlOf(mysql.CreateTable(checkedTable()))
	for _, want := range []string{
		"CONSTRAINT `chk_age` CHECK (age >= 0)",
		"CONSTRAINT `chk_balance` CHECK (balance >= 0)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("CreateTable is missing %s:\n%s", want, got)
		}
	}
}

// The constraints are emitted in name order, so two renderings of one
// declaration are the same string — a map's iteration order is not.
func TestCreateTableChecksAreOrdered(t *testing.T) {
	first, _ := sqlOf(mysql.CreateTable(checkedTable()))
	for i := 0; i < 20; i++ {
		if got, _ := sqlOf(mysql.CreateTable(checkedTable())); got != first {
			t.Fatalf("CreateTable is not deterministic:\n%s\n---\n%s", first, got)
		}
	}
	if strings.Index(first, "chk_age") > strings.Index(first, "chk_balance") {
		t.Errorf("checks are not in name order:\n%s", first)
	}
}

// The CHECK clauses follow the keys, which is where MySQL's own
// SHOW CREATE TABLE puts them, and none of them may leak into the
// table options after the closing parenthesis.
func TestCreateTableChecksSitInsideTheBody(t *testing.T) {
	tbl := checkedTable()
	tbl.Engine("InnoDB")
	got, _ := sqlOf(mysql.CreateTable(tbl))
	body := got[:strings.LastIndex(got, "\n)")]
	if !strings.Contains(body, "chk_age") || !strings.Contains(body, "chk_balance") {
		t.Errorf("checks are outside the table body:\n%s", got)
	}
	if strings.Index(got, "PRIMARY KEY") > strings.Index(got, "chk_age") {
		t.Errorf("checks should follow the key:\n%s", got)
	}
}

func TestCreateTableWithoutChecksIsUnchanged(t *testing.T) {
	tbl := mysql.NewTable("plain")
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	got, _ := sqlOf(mysql.CreateTable(tbl))
	if strings.Contains(got, "CHECK") {
		t.Errorf("a table with no checks grew one:\n%s", got)
	}
}
