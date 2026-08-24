package sqlite_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/sqlite"
)

type tkMembership struct {
	OrgID  int64 `drop:"orgId"`
	UserID int64 `drop:"userId"`
	Role   string
}

// tkTable declares the same key as ckTable in compositekey_test.go, in
// the other spelling: on the table rather than on each column.
func tkTable() *sqlite.Table {
	t := sqlite.NewTable("memberships")
	org := sqlite.Add(t, sqlite.Integer("orgId"))
	user := sqlite.Add(t, sqlite.Integer("userId"))
	sqlite.Add(t, sqlite.Text("role").NotNull())
	return t.PrimaryKey(org, user)
}

func TestTableLevelKeyIsSeenByNewEntity(t *testing.T) {
	ent := sqlite.NewEntity[tkMembership](tkTable())
	if got := len(ent.PKs()); got != 2 {
		t.Fatalf("PKs = %d, want 2", got)
	}
	if ent.PK() != nil {
		t.Errorf("PK() should be nil for a composite key")
	}
	if a, b := ent.PKs()[0].Name(), ent.PKs()[1].Name(); a != "orgId" || b != "userId" {
		t.Errorf("PKs = %q, %q; want key order orgId, userId", a, b)
	}
}

func TestTableLevelKeyAddressesBothColumns(t *testing.T) {
	ent := sqlite.NewEntity[tkMembership](tkTable())
	d := &entDriver{}
	// The stub driver returns no rows; the SQL it was handed is the
	// point, not the result.
	_, _ = ent.Get(sqlite.New(d), context.Background(), int64(1), int64(2))
	if len(d.queries) == 0 {
		t.Fatal("no query issued")
	}
	sql := d.queries[0]
	if !strings.Contains(sql, `"orgId"`) || !strings.Contains(sql, `"userId"`) {
		t.Errorf("SELECT should address both key columns: %s", sql)
	}
}

// A single-column key declared on the table is still a single-column
// key, so PK() has to answer rather than staying nil.
func TestTableLevelSingleColumnKeyIsSingular(t *testing.T) {
	tbl := sqlite.NewTable("notes")
	id := sqlite.Add(tbl, sqlite.Integer("id"))
	sqlite.Add(tbl, sqlite.Text("body").NotNull())
	tbl.PrimaryKey(id)

	type note struct {
		ID   int64  `drop:"id"`
		Body string `drop:"body"`
	}
	ent := sqlite.NewEntity[note](tbl)
	if ent.PK() == nil {
		t.Fatal("PK() is nil for a single-column key declared on the table")
	}
	if ent.PK().Name() != "id" {
		t.Errorf("PK() = %q, want id", ent.PK().Name())
	}
}

// SQLite does not enforce NOT NULL for a table-level PRIMARY KEY on
// its own, so the declaration has to say it — as the column-level
// spelling already did.
func TestTableLevelKeyRendersNotNull(t *testing.T) {
	sql, _ := sqlite.ToSQL(sqlite.CreateTable(tkTable()))
	for _, want := range []string{`"orgId" INTEGER NOT NULL`, `"userId" INTEGER NOT NULL`} {
		if !strings.Contains(sql, want) {
			t.Errorf("CreateTable is missing %s:\n%s", want, sql)
		}
	}
	if !strings.Contains(sql, `PRIMARY KEY ("orgId", "userId")`) {
		t.Errorf("CreateTable is missing the table-level key:\n%s", sql)
	}
}
