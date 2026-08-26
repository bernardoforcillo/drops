package mysql_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/mysql"
)

type user struct {
	ID    int64
	Name  string
	Email string
}

func userEntity(t *testing.T) (*mysql.Entity[user], *mysql.Table) {
	t.Helper()
	tbl, _, _, _ := usersTable()
	return mysql.NewEntity[user](tbl), tbl
}

// MySQL has no RETURNING, so a generated key can only come back
// through the driver's LastInsertId.
func TestCreateReadsGeneratedKeyFromLastInsertID(t *testing.T) {
	ent, _ := userEntity(t)
	d := &fakeDriver{hasLast: true, lastID: 4242}
	u := user{Name: "ada", Email: "ada@example.com"}

	if err := ent.Create(mysql.New(d), context.Background(), &u); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if u.ID != 4242 {
		t.Errorf("ID = %d, want the driver's LastInsertId 4242", u.ID)
	}
	// The zero key must not be written into the INSERT — the server
	// generates it.
	if strings.Contains(d.queries[0], "`id`") {
		t.Errorf("an unset AUTO_INCREMENT key should be omitted: %s", d.queries[0])
	}
}

// A driver that exposes no LastInsertId must leave the field alone.
// The row is inserted either way, and writing 0 into the key would be
// worse than writing nothing.
func TestCreateToleratesDriverWithoutLastInsertID(t *testing.T) {
	ent, _ := userEntity(t)
	u := user{Name: "ada", Email: "ada@example.com"}
	if err := ent.Create(mysql.New(&fakeDriver{}), context.Background(), &u); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if u.ID != 0 {
		t.Errorf("ID = %d, want it left untouched", u.ID)
	}
}

// An id the caller set explicitly must survive.
func TestCreateKeepsAnExplicitKey(t *testing.T) {
	ent, _ := userEntity(t)
	d := &fakeDriver{hasLast: true, lastID: 999}
	u := user{ID: 7, Name: "ada", Email: "a@x"}
	if err := ent.Create(mysql.New(d), context.Background(), &u); err != nil {
		t.Fatal(err)
	}
	if u.ID != 7 {
		t.Errorf("ID = %d, want the caller's 7", u.ID)
	}
	if !strings.Contains(d.queries[0], "`id`") {
		t.Errorf("an explicit key should be inserted: %s", d.queries[0])
	}
}

func TestUpdateNeverReassignsTheKey(t *testing.T) {
	ent, _ := userEntity(t)
	d := &fakeDriver{}
	u := user{ID: 1, Name: "ada", Email: "a@x"}
	if err := ent.Update(mysql.New(d), context.Background(), &u); err != nil {
		t.Fatalf("Update: %v", err)
	}
	sql := d.queries[0]
	setStart, whereStart := strings.Index(sql, "SET"), strings.Index(sql, "WHERE")
	if setStart < 0 || whereStart < setStart {
		t.Fatalf("unexpected UPDATE: %s", sql)
	}
	if strings.Contains(sql[setStart:whereStart], "`id`") {
		t.Errorf("key column in SET: %s", sql)
	}
	if !strings.Contains(sql[whereStart:], "`id`") {
		t.Errorf("key column missing from WHERE: %s", sql)
	}
}

func TestUpdateRequiresAKey(t *testing.T) {
	ent, _ := userEntity(t)
	u := user{Name: "ada"}
	if err := ent.Update(mysql.New(&fakeDriver{}), context.Background(), &u); !errors.Is(err, mysql.ErrPKNotSet) {
		t.Errorf("err = %v want ErrPKNotSet", err)
	}
}

func TestSaveRoutesOnTheKey(t *testing.T) {
	ent, _ := userEntity(t)

	ins := &fakeDriver{}
	empty := user{Name: "ada", Email: "a@x"}
	if err := ent.Save(mysql.New(ins), context.Background(), &empty); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ins.queries[0], "INSERT") {
		t.Errorf("unset key should insert: %s", ins.queries[0])
	}

	upd := &fakeDriver{}
	set := user{ID: 3, Name: "ada", Email: "a@x"}
	if err := ent.Save(mysql.New(upd), context.Background(), &set); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(upd.queries[0], "UPDATE") {
		t.Errorf("set key should update: %s", upd.queries[0])
	}
}

func TestGetAddressesTheKey(t *testing.T) {
	ent, _ := userEntity(t)
	d := &fakeDriver{}
	_, _ = ent.Get(mysql.New(d), context.Background(), int64(7))
	if !strings.Contains(d.queries[0], "WHERE (`users`.`id` = ?)") {
		t.Errorf("got %s", d.queries[0])
	}
}

func TestGetRejectsWrongArity(t *testing.T) {
	ent, _ := userEntity(t)
	db := mysql.New(&fakeDriver{})
	if _, err := ent.Get(db, context.Background(), int64(1), int64(2)); !errors.Is(err, mysql.ErrKeyArity) {
		t.Errorf("err = %v want ErrKeyArity", err)
	}
	if _, err := ent.Delete(db, context.Background()); !errors.Is(err, mysql.ErrKeyArity) {
		t.Errorf("err = %v want ErrKeyArity", err)
	}
}

// --- composite keys ----------------------------------------------------

type membership struct {
	OrgID  int64 `drop:"orgId"`
	UserID int64 `drop:"userId"`
	Role   string
}

func membershipEntity(t *testing.T) *mysql.Entity[membership] {
	t.Helper()
	tbl := mysql.NewTable("memberships")
	mysql.Add(tbl, mysql.BigInt("orgId").PrimaryKey())
	mysql.Add(tbl, mysql.BigInt("userId").PrimaryKey())
	mysql.Add(tbl, mysql.Varchar("role", 32).NotNull())
	return mysql.NewEntity[membership](tbl)
}

func TestCompositeKeyAddressesEveryColumn(t *testing.T) {
	ent := membershipEntity(t)
	d := &fakeDriver{}
	_, _ = ent.Get(mysql.New(d), context.Background(), int64(1), int64(2))
	sql := d.queries[0]
	if !strings.Contains(sql, "`orgId`") || !strings.Contains(sql, "`userId`") {
		t.Errorf("both key columns should be addressed: %s", sql)
	}
}

func TestCompositeKeyRejectsPartialKey(t *testing.T) {
	ent := membershipEntity(t)
	_, err := ent.Get(mysql.New(&fakeDriver{}), context.Background(), int64(1))
	if !errors.Is(err, mysql.ErrKeyArity) {
		t.Errorf("err = %v want ErrKeyArity", err)
	}
	if !strings.Contains(err.Error(), "orgId") {
		t.Errorf("message should name the key columns: %v", err)
	}
}

// --- drift -------------------------------------------------------------

func TestEntityRejectsUnmappedColumn(t *testing.T) {
	tbl := mysql.NewTable("people")
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(tbl, mysql.Varchar("email", 255).NotNull())

	type row struct {
		ID    int64
		Emial string // the column is "email"
	}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected a panic for the unmapped column")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "email") || !strings.Contains(msg, "Emial") {
			t.Errorf("message should name the column and the suspect: %s", msg)
		}
	}()
	mysql.NewEntity[row](tbl)
}

func TestEntityAllowUnmappedColumns(t *testing.T) {
	tbl := mysql.NewTable("people")
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(tbl, mysql.Varchar("search", 255))

	type row struct{ ID int64 }
	if mysql.NewEntity[row](tbl, mysql.AllowUnmappedColumns("search")) == nil {
		t.Fatal("expected the entity to build")
	}
}
