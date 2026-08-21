package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/mysql"
)

// A CHECK the declaration carries has to reach the table CreateTable
// builds, and the server has to enforce it. Rendering it is only half
// the claim; the other half is that MariaDB agrees.
func TestMySQLCreateTableEnforcesChecks(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()

	info, err := mysql.ServerVersion(ctx, db)
	if err != nil {
		t.Fatalf("ServerVersion: %v", err)
	}
	if !info.SupportsCheckConstraints() {
		t.Skipf("server %s parses CHECK and discards it", info.Version)
	}

	tbl := mysql.NewTable(integration.UniqueName(t, "checked"))
	dropMySQL(t, db, tbl)
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(tbl, mysql.Integer("age").NotNull())
	tbl.AddCheck("chk_age_nonneg", "age >= 0")

	execMySQL(t, db, mysql.CreateTable(tbl))

	if _, err := db.Exec(ctx, "INSERT INTO `"+tbl.Name()+"` (`age`) VALUES (7)"); err != nil {
		t.Fatalf("a conforming row was rejected: %v", err)
	}
	err = nil
	if _, err = db.Exec(ctx, "INSERT INTO `"+tbl.Name()+"` (`age`) VALUES (-1)"); err == nil {
		t.Fatal("the server accepted a row the CHECK forbids, so CreateTable did not create it")
	}
	if !strings.Contains(err.Error(), "chk_age_nonneg") {
		t.Errorf("the row was rejected, but not by the declared constraint: %v", err)
	}
}

// The two paths that build a table from one declaration have to build
// the same table: what CreateTable creates is what Push, handed the
// same schema, finds nothing left to do about.
func TestMySQLCreateTableAndPushAgreeOnChecks(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()

	info, err := mysql.ServerVersion(ctx, db)
	if err != nil {
		t.Fatalf("ServerVersion: %v", err)
	}
	if !info.SupportsCheckConstraints() {
		t.Skipf("server %s parses CHECK and discards it", info.Version)
	}

	tbl := mysql.NewTable(integration.UniqueName(t, "agree"))
	dropMySQL(t, db, tbl)
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(tbl, mysql.Integer("age").NotNull())
	tbl.AddCheck("chk_age_agree", "age >= 0")

	execMySQL(t, db, mysql.CreateTable(tbl))

	schema := mysql.NewSchema(tbl)
	res, err := mysql.Push(ctx, db, schema, mysql.PushOptions{DryRun: true})
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(res.Statements) != 0 {
		t.Errorf("the two layers disagree about the same schema:\n%s", strings.Join(res.Statements, "\n"))
	}
	for _, n := range res.Notices {
		t.Errorf("unexpected notice: %s", n)
	}
}
