package integration_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/sqlite"
)

type tkMember struct {
	OrgID  int64  `drop:"orgId"`
	UserID int64  `drop:"userId"`
	Role   string `drop:"role"`
}

func tkMembers() *sqlite.Table {
	t := sqlite.NewTable("tk_members")
	org := sqlite.Add(t, sqlite.Integer("orgId"))
	user := sqlite.Add(t, sqlite.Integer("userId"))
	sqlite.Add(t, sqlite.Text("role").NotNull())
	return t.PrimaryKey(org, user)
}

// A key declared on the table has to survive the whole round trip:
// NewEntity has to see it, the DDL the same declaration renders has to
// be a table the engine agrees is keyed that way, and the CRUD the
// Entity issues has to address both columns on the live database.
func TestTableLevelKeyRoundTripsOnLiveSQLite(t *testing.T) {
	db := openSQLite(t)
	tbl := tkMembers()
	exec(t, db, sqlite.CreateTable(tbl))

	ent := sqlite.NewEntity[tkMember](tbl)
	ctx := context.Background()

	for _, r := range []tkMember{
		{OrgID: 1, UserID: 1, Role: "owner"},
		{OrgID: 1, UserID: 2, Role: "member"},
		{OrgID: 2, UserID: 1, Role: "member"},
	} {
		if err := ent.Create(db, ctx, &r); err != nil {
			t.Fatalf("Create %+v: %v", r, err)
		}
	}

	// Get addresses the whole key: (1,2) is not (2,1).
	got, err := ent.Get(db, ctx, int64(1), int64(2))
	if err != nil {
		t.Fatalf("Get(1,2): %v", err)
	}
	if got.Role != "member" || got.OrgID != 1 || got.UserID != 2 {
		t.Errorf("Get(1,2) = %+v", got)
	}

	// Update matches on the whole key too, so the sibling rows that
	// share half of it are left alone.
	got.Role = "admin"
	if err := ent.Update(db, ctx, &got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	other, err := ent.Get(db, ctx, int64(2), int64(1))
	if err != nil {
		t.Fatalf("Get(2,1): %v", err)
	}
	if other.Role != "member" {
		t.Errorf("Update touched the row keyed (2,1): role = %q", other.Role)
	}

	if _, err := ent.Delete(db, ctx, int64(1), int64(2)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := ent.Get(db, ctx, int64(1), int64(2)); !errors.Is(err, sqlite.ErrNoRows) {
		t.Errorf("Get after Delete: err = %v, want ErrNoRows", err)
	}
	if _, err := ent.Get(db, ctx, int64(1), int64(1)); err != nil {
		t.Errorf("Delete removed the row keyed (1,1): %v", err)
	}
}

// The engine has to agree the key is a key. SQLite's PRIMARY KEY does
// not enforce NOT NULL on its own, so this also covers the NOT NULL
// the declaration renders for the key's members.
func TestTableLevelKeyIsEnforcedByTheEngine(t *testing.T) {
	db := openSQLite(t)
	tbl := tkMembers()
	exec(t, db, sqlite.CreateTable(tbl))
	ctx := context.Background()
	ent := sqlite.NewEntity[tkMember](tbl)

	first := tkMember{OrgID: 7, UserID: 7, Role: "owner"}
	if err := ent.Create(db, ctx, &first); err != nil {
		t.Fatalf("Create: %v", err)
	}
	dup := tkMember{OrgID: 7, UserID: 7, Role: "member"}
	if err := ent.Create(db, ctx, &dup); err == nil {
		t.Fatal("the engine accepted a duplicate key")
	}

	if _, err := db.Exec(ctx, `INSERT INTO "tk_members" ("orgId", "userId", "role") VALUES (NULL, 1, 'x')`); err == nil {
		t.Error("the engine accepted NULL in a key column")
	} else if !strings.Contains(strings.ToLower(err.Error()), "not null") {
		t.Errorf("NULL in a key column was rejected, but not for being NULL: %v", err)
	}
}
