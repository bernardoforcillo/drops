package pg_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// A join table — the shape Entity used to reject outright.
type membership struct {
	OrgID  int64 `drop:"orgId"`
	UserID int64 `drop:"userId"`
	Role   string
}

func membershipTable() (*pg.Table, *pg.Col[string]) {
	t := pg.NewTable("memberships")
	pg.Add(t, pg.BigInt("orgId").PrimaryKey())
	pg.Add(t, pg.BigInt("userId").PrimaryKey())
	role := pg.Add(t, pg.Text("role").NotNull())
	return t, role
}

func ckEntity(t *testing.T) *pg.Entity[membership] {
	t.Helper()
	tbl, _ := membershipTable()
	return pg.NewEntity[membership](tbl)
}

func TestCompositeKeyEntityBuilds(t *testing.T) {
	ent := ckEntity(t)
	pks := ent.PKs()
	if len(pks) != 2 {
		t.Fatalf("PKs = %d, want 2", len(pks))
	}
	if pks[0].Name() != "orgId" || pks[1].Name() != "userId" {
		t.Errorf("PKs = %s, %s; want declaration order orgId, userId", pks[0].Name(), pks[1].Name())
	}
	// PK() is the single-column accessor and has no answer here.
	if ent.PK() != nil {
		t.Errorf("PK() should be nil for a composite key, got %v", ent.PK())
	}
}

func TestSingleKeyEntityStillReportsPK(t *testing.T) {
	ent := pg.NewEntity[driftRow](driftTable(), pg.AllowUnmappedColumns("email"))
	if ent.PK() == nil || ent.PK().Name() != "id" {
		t.Errorf("PK() = %v, want the id column", ent.PK())
	}
	if len(ent.PKs()) != 1 {
		t.Errorf("PKs = %d, want 1", len(ent.PKs()))
	}
}

func TestCompositeGetAddressesBothColumns(t *testing.T) {
	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{cols: []string{"orgId", "userId", "role"}}, nil
	}}
	_, _ = ckEntity(t).Get(pg.New(fd), context.Background(), int64(1), int64(2))

	sql := fd.queries[0]
	for _, want := range []string{`"orgId"`, `"userId"`, " AND "} {
		if !strings.Contains(sql, want) {
			t.Errorf("SELECT missing %s: %s", want, sql)
		}
	}
	// The trailing arg is the LIMIT the One() path adds.
	args := fd.args[0]
	if len(args) < 2 || args[0] != int64(1) || args[1] != int64(2) {
		t.Errorf("args = %v, want both key values first, in order", args)
	}
}

// The dangerous case: addressing a two-column key with one value
// would match every row sharing that column. It must be an error, not
// a partial match.
func TestCompositeKeyRejectsWrongArity(t *testing.T) {
	ent := ckEntity(t)
	db := pg.New(&fakeDriver{})

	if _, err := ent.Get(db, context.Background(), int64(1)); !errors.Is(err, pg.ErrKeyArity) {
		t.Errorf("Get with one value: err = %v, want ErrKeyArity", err)
	}
	if _, err := ent.Delete(db, context.Background(), int64(1)); !errors.Is(err, pg.ErrKeyArity) {
		t.Errorf("Delete with one value: err = %v, want ErrKeyArity", err)
	}
	if _, err := ent.Get(db, context.Background(), int64(1), int64(2), int64(3)); !errors.Is(err, pg.ErrKeyArity) {
		t.Errorf("Get with three values: err = %v, want ErrKeyArity", err)
	}
	// And the message should say what was expected.
	_, err := ent.Get(db, context.Background(), int64(1))
	if !strings.Contains(err.Error(), "orgId") || !strings.Contains(err.Error(), "userId") {
		t.Errorf("message should name the key columns: %v", err)
	}
}

func TestCompositeDeleteAddressesBothColumns(t *testing.T) {
	fd := &fakeDriver{}
	if _, err := ckEntity(t).Delete(pg.New(fd), context.Background(), int64(7), int64(9)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	sql := fd.queries[0]
	if !strings.Contains(sql, `"orgId"`) || !strings.Contains(sql, `"userId"`) {
		t.Errorf("DELETE should address both key columns: %s", sql)
	}
}

func TestCompositeUpdateAddressesBothColumnsAndSetsNeither(t *testing.T) {
	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{cols: []string{"orgId", "userId", "role"},
			data: [][]any{{int64(1), int64(2), "admin"}}}, nil
	}}
	m := membership{OrgID: 1, UserID: 2, Role: "admin"}
	if err := ckEntity(t).Update(pg.New(fd), context.Background(), &m); err != nil {
		t.Fatalf("Update: %v", err)
	}
	sql := fd.queries[0]
	setStart, whereStart := strings.Index(sql, "SET"), strings.Index(sql, "WHERE")
	if setStart < 0 || whereStart < setStart {
		t.Fatalf("unexpected UPDATE shape: %s", sql)
	}
	set := sql[setStart:whereStart]
	for _, key := range []string{`"orgId"`, `"userId"`} {
		if strings.Contains(set, key) {
			t.Errorf("key column %s must not appear in SET: %s", key, set)
		}
	}
	if !strings.Contains(set, `"role"`) {
		t.Errorf("SET should carry the non-key column: %s", set)
	}
}

func TestCompositeUpdateRequiresAKey(t *testing.T) {
	m := membership{Role: "admin"} // both key fields zero
	err := ckEntity(t).Update(pg.New(&fakeDriver{}), context.Background(), &m)
	if !errors.Is(err, pg.ErrPKNotSet) {
		t.Errorf("err = %v want ErrPKNotSet", err)
	}
}

// Save routes on the key being unset, and a composite key is unset
// only when every column is.
func TestCompositeSaveRoutesOnTheWholeKey(t *testing.T) {
	rows := func(string, []any) (drops.Rows, error) {
		return &fakeRows{cols: []string{"orgId", "userId", "role"},
			data: [][]any{{int64(1), int64(2), "admin"}}}, nil
	}

	insert := &fakeDriver{handler: rows}
	empty := membership{Role: "admin"}
	if err := ckEntity(t).Save(pg.New(insert), context.Background(), &empty); err != nil {
		t.Fatalf("Save (empty key): %v", err)
	}
	if !strings.HasPrefix(insert.queries[0], "INSERT") {
		t.Errorf("an entirely unset key should insert: %s", insert.queries[0])
	}

	update := &fakeDriver{handler: rows}
	partial := membership{OrgID: 1, Role: "admin"} // one column set
	if err := ckEntity(t).Save(pg.New(update), context.Background(), &partial); err != nil {
		t.Fatalf("Save (partial key): %v", err)
	}
	if !strings.HasPrefix(update.queries[0], "UPDATE") {
		t.Errorf("a partially-set key should update, not insert: %s", update.queries[0])
	}
}

func TestCompositeUpsertConflictsOnBothColumns(t *testing.T) {
	fd := &fakeDriver{}
	_, err := ckEntity(t).UpsertMany(pg.New(fd), context.Background(), []membership{
		{OrgID: 1, UserID: 2, Role: "admin"},
	})
	if err != nil {
		t.Fatalf("UpsertMany: %v", err)
	}
	sql := fd.queries[0]
	i := strings.Index(sql, "ON CONFLICT")
	if i < 0 {
		t.Fatalf("no ON CONFLICT clause: %s", sql)
	}
	conflict := sql[i:]
	if !strings.Contains(conflict, `"orgId"`) || !strings.Contains(conflict, `"userId"`) {
		t.Errorf("ON CONFLICT should name both key columns: %s", conflict)
	}
}

func TestCompositePatchKey(t *testing.T) {
	fd := &fakeDriver{}
	_, role := membershipTable()
	_, err := ckEntity(t).PatchKey(pg.New(fd), context.Background(),
		[]any{int64(1), int64(2)},
		pg.Set(role, "admin"),
	)
	if err != nil {
		t.Fatalf("PatchKey: %v", err)
	}
	sql := fd.queries[0]
	if !strings.Contains(sql, `"orgId"`) || !strings.Contains(sql, `"userId"`) {
		t.Errorf("PatchKey should address both key columns: %s", sql)
	}
}

func TestPatchOnCompositeKeyRejectsSingleValue(t *testing.T) {
	_, role := membershipTable()
	_, err := ckEntity(t).Patch(pg.New(&fakeDriver{}), context.Background(), int64(1), pg.Set(role, "admin"))
	if !errors.Is(err, pg.ErrKeyArity) {
		t.Errorf("err = %v want ErrKeyArity", err)
	}
}

// A key column with no struct field is still a hard error: without it
// no row can be addressed at all.
func TestCompositeKeyNeedsEveryFieldBound(t *testing.T) {
	type partial struct {
		OrgID int64 `drop:"orgId"`
		Role  string
	}
	tbl, _ := membershipTable()
	msg := mustPanic(t, func() { pg.NewEntity[partial](tbl) })
	if !strings.Contains(msg, "userId") {
		t.Errorf("message should name the unbound key column: %s", msg)
	}
}
