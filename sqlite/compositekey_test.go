package sqlite_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/sqlite"
)

type ckMembership struct {
	OrgID  int64 `drop:"orgId"`
	UserID int64 `drop:"userId"`
	Role   string
}

func ckTable() *sqlite.Table {
	t := sqlite.NewTable("memberships")
	sqlite.Add(t, sqlite.Integer("orgId").PrimaryKey())
	sqlite.Add(t, sqlite.Integer("userId").PrimaryKey())
	sqlite.Add(t, sqlite.Text("role").NotNull())
	return t
}

func TestCompositeKeyBuildsAndReportsColumns(t *testing.T) {
	ent := sqlite.NewEntity[ckMembership](ckTable())
	if got := len(ent.PKs()); got != 2 {
		t.Fatalf("PKs = %d, want 2", got)
	}
	if ent.PK() != nil {
		t.Errorf("PK() should be nil for a composite key")
	}
}

func TestCompositeGetAddressesBothColumns(t *testing.T) {
	ent := sqlite.NewEntity[ckMembership](ckTable())
	d := &entDriver{}
	_, _ = ent.Get(sqlite.New(d), context.Background(), int64(1), int64(2))
	if len(d.queries) == 0 {
		t.Fatal("no query issued")
	}
	sql := d.queries[0]
	if !strings.Contains(sql, `"orgId"`) || !strings.Contains(sql, `"userId"`) {
		t.Errorf("SELECT should address both key columns: %s", sql)
	}
}

func TestCompositeRejectsWrongArity(t *testing.T) {
	ent := sqlite.NewEntity[ckMembership](ckTable())
	db := sqlite.New(&entDriver{})
	if _, err := ent.Get(db, context.Background(), int64(1)); !errors.Is(err, sqlite.ErrKeyArity) {
		t.Errorf("Get: err = %v want ErrKeyArity", err)
	}
	if _, err := ent.Delete(db, context.Background(), int64(1)); !errors.Is(err, sqlite.ErrKeyArity) {
		t.Errorf("Delete: err = %v want ErrKeyArity", err)
	}
}

func TestCompositeUpdateExcludesKeyColumnsFromSet(t *testing.T) {
	ent := sqlite.NewEntity[ckMembership](ckTable())
	d := &entDriver{}
	m := ckMembership{OrgID: 1, UserID: 2, Role: "admin"}
	if err := ent.Update(sqlite.New(d), context.Background(), &m); err != nil {
		t.Fatalf("Update: %v", err)
	}
	sql := d.queries[0]
	setStart, whereStart := strings.Index(sql, "SET"), strings.Index(sql, "WHERE")
	if setStart < 0 || whereStart < setStart {
		t.Fatalf("unexpected UPDATE: %s", sql)
	}
	set := sql[setStart:whereStart]
	for _, key := range []string{`"orgId"`, `"userId"`} {
		if strings.Contains(set, key) {
			t.Errorf("key column %s must not be in SET: %s", key, set)
		}
	}
}

func TestCompositePatchKey(t *testing.T) {
	tbl := ckTable()
	role := sqlite.Add(tbl, sqlite.Text("note").NotNull())
	ent := sqlite.NewEntity[ckMembershipNote](tbl)
	d := &entDriver{}
	_, err := ent.PatchKey(sqlite.New(d), context.Background(),
		[]any{int64(1), int64(2)}, sqlite.Set(role, "x"))
	if err != nil {
		t.Fatalf("PatchKey: %v", err)
	}
	sql := d.queries[0]
	if !strings.Contains(sql, `"orgId"`) || !strings.Contains(sql, `"userId"`) {
		t.Errorf("PatchKey should address both key columns: %s", sql)
	}
}

type ckMembershipNote struct {
	OrgID  int64 `drop:"orgId"`
	UserID int64 `drop:"userId"`
	Role   string
	Note   string
}
