package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/mysql"
	"github.com/bernardoforcillo/drops/pg"
)

// drops.SQL exists to reach an operator the DSL does not model without
// giving up parameters on the way out. Both halves of that claim are
// server behaviour: the text has to parse where it lands, and the value
// beside it has to arrive as a bound parameter rather than as text.
func TestPGSQLFragmentParsesAndBinds(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()

	tbl := pg.NewTable(integration.UniqueName(t, "frag"))
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	email := pg.Add(tbl, pg.Text("email").NotNull())
	dropPG(t, db, tbl)
	execPG(t, db, pg.CreateTable(tbl))

	if _, err := db.Insert(tbl).Row(email.Val("Ada@Example.COM")).Exec(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The example from the doc comment, run by the server.
	in := "ADA@example.com"
	pred := drops.SQL("lower(", email, ") = ", drops.Arg(strings.ToLower(in)))

	var got string
	if err := db.Select(email).From(tbl).Where(pred).One(ctx, &got); err != nil {
		text, args := drops.String(pred)
		t.Fatalf("PostgreSQL rejected the fragment: %v\n%s\nargs: %v", err, text, args)
	}
	if got != "Ada@Example.COM" {
		t.Fatalf("email = %q, want the seeded row", got)
	}

	// A value that looks like SQL is still only a value: it comes back
	// as no rows, not as a broken statement or a dropped table.
	hostile := "' OR 1=1 --"
	var any string
	err := db.Select(email).From(tbl).
		Where(drops.SQL(email, " = ", drops.Arg(hostile))).One(ctx, &any)
	if err == nil {
		t.Fatalf("a bound value matched a row: %q", any)
	}
	var rows int64
	if err := db.Select(drops.Raw("count(*)")).From(tbl).One(ctx, &rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("rows = %d, want the table intact", rows)
	}
}

// The same fragment against MySQL, where the placeholder is "?" and the
// identifier quoting is backticks: the parts are held as given and
// rendered by whichever dialect the builder carries.
func TestMySQLSQLFragmentParsesAndBinds(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()

	tbl := mysql.NewTable(integration.UniqueName(t, "frag"))
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	email := mysql.Add(tbl, mysql.Varchar("email", 191).NotNull())
	dropMySQL(t, db, tbl)
	execMySQL(t, db, mysql.CreateTable(tbl))

	if _, err := db.Insert(tbl).Row(email.Val("Ada@Example.COM")).Exec(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}

	pred := drops.SQL("lower(", email, ") = ", drops.Arg("ada@example.com"))
	var got string
	if err := db.Select(email).From(tbl).Where(pred).One(ctx, &got); err != nil {
		text, args := drops.StringWithDialect(mysql.Dialect, pred)
		t.Fatalf("MySQL rejected the fragment: %v\n%s\nargs: %v", err, text, args)
	}
	if got != "Ada@Example.COM" {
		t.Fatalf("email = %q, want the seeded row", got)
	}
}
