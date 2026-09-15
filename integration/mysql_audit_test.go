package integration_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/mysql"
)

// The audit trail against a real server.
//
// This suite is why it is a real server: the audit INSERT was ported
// with double-quoted column names, which MySQL reads as string literals
// unless ANSI_QUOTES is set — a syntax error that fires only on an
// audited write, and so only on the write you least want to find it on.
// A fake driver accepts any string at all and would have shipped it.

type auAccount struct {
	ID   int64  `drop:"id"`
	Name string `drop:"name"`
}

func TestMySQLAuditRecordsEveryWrite(t *testing.T) {
	db := openMySQL(t)
	ctx := mysql.WithActor(context.Background(), "ada")

	accounts := mysql.NewTable(integration.UniqueName(t, "au_accounts"))
	mysql.Add(accounts, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(accounts, mysql.Varchar("name", 255).NotNull())
	dropMySQL(t, db, accounts)
	execMySQL(t, db, mysql.CreateTable(accounts))

	trail := mysql.NewAuditTable(integration.UniqueName(t, "au_trail"))
	dropMySQL(t, db, trail)
	execMySQL(t, db, mysql.CreateTable(trail))

	ent := mysql.WithAudit(mysql.NewEntity[auAccount](accounts),
		mysql.NewAuditLog(db, trail.Name()))

	row := auAccount{Name: "Ada"}
	if err := ent.Create(db, ctx, &row); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if row.ID == 0 {
		t.Fatal("Create did not read the generated key back, so the audit row cannot name it")
	}
	row.Name = "Ada Lovelace"
	if err := ent.Update(db, ctx, &row); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := ent.Delete(db, ctx, row.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	type entry struct {
		op, actor string
		pk        []byte
	}
	var got []entry
	rows, err := db.Query(ctx, "SELECT `op`, `actor`, `pk` FROM `"+trail.Name()+"` ORDER BY `id`")
	if err != nil {
		t.Fatalf("read the trail: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.op, &e.actor, &e.pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("the trail has %d rows, want create/update/delete: %+v", len(got), got)
	}
	for i, want := range []string{"create", "update", "delete"} {
		if got[i].op != want {
			t.Errorf("row %d op = %q, want %q", i, got[i].op, want)
		}
		if got[i].actor != "ada" {
			t.Errorf("row %d actor = %q, want the ctx actor", i, got[i].actor)
		}
	}
	// The key each row names is the key that was written, which for a
	// generated one is only knowable after the INSERT.
	for i, e := range got {
		var pk int64
		if err := json.Unmarshal(e.pk, &pk); err != nil {
			// A composite key is joined into a string; a single one is
			// the value. Either way it must not be null or zero.
			t.Fatalf("row %d has an unreadable pk %q: %v", i, e.pk, err)
		}
		if pk != row.ID {
			t.Errorf("row %d names key %d, want %d", i, pk, row.ID)
		}
	}
}

// The audit row and the write are one transaction: a write that fails
// leaves no trail entry claiming it happened.
func TestMySQLAuditRowRollsBackWithAFailedWrite(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()

	accounts := mysql.NewTable(integration.UniqueName(t, "au_rb"))
	mysql.Add(accounts, mysql.BigInt("id").PrimaryKey())
	mysql.Add(accounts, mysql.Varchar("name", 255).NotNull())
	dropMySQL(t, db, accounts)
	execMySQL(t, db, mysql.CreateTable(accounts))

	trail := mysql.NewAuditTable(integration.UniqueName(t, "au_rb_trail"))
	dropMySQL(t, db, trail)
	execMySQL(t, db, mysql.CreateTable(trail))

	ent := mysql.WithAudit(mysql.NewEntity[auAccount](accounts),
		mysql.NewAuditLog(db, trail.Name()))

	first := auAccount{ID: 1, Name: "Ada"}
	if err := ent.Create(db, ctx, &first); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The same key again: the primary key refuses it.
	clash := auAccount{ID: 1, Name: "Grace"}
	if err := ent.Create(db, ctx, &clash); err == nil {
		t.Fatal("the duplicate key was accepted")
	}

	var n int64
	rows, err := db.Query(ctx, "SELECT COUNT(*) FROM `"+trail.Name()+"`")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
	}
	if n != 1 {
		t.Errorf("the trail has %d rows, want 1 — the failed write left an entry behind", n)
	}
}
