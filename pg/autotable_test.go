package pg_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

type autoUser struct {
	ID        int64     `db:"id,pk,autoinc"`
	Email     string    `db:"email,notnull,unique"`
	Name      string    `db:"name,notnull"`
	Age       *int32    `db:"age"`
	CreatedAt time.Time `db:"created_at,notnull,default=now()"`
	Internal  string    // untagged — skipped
	Skip      string    `db:"-"`
}

func TestAutoTableInfersColumns(t *testing.T) {
	tbl := pg.AutoTable[autoUser]("users")
	got, _ := drops.String(pg.CreateTable(tbl))

	wantFragments := []string{
		`"id" bigserial PRIMARY KEY`,
		`"email" text NOT NULL UNIQUE`,
		`"name" text NOT NULL`,
		`"age" integer`,
		`"created_at" timestamptz NOT NULL DEFAULT now()`,
	}
	for _, w := range wantFragments {
		if !strings.Contains(got, w) {
			t.Errorf("missing fragment %q in DDL:\n%s", w, got)
		}
	}
	if strings.Contains(got, "internal") || strings.Contains(got, "Skip") {
		t.Errorf("untagged or db:\"-\" fields must be skipped:\n%s", got)
	}
}

func TestAutoTablePointerMakesNullable(t *testing.T) {
	tbl := pg.AutoTable[autoUser]("users")
	age := tbl.Col("age")
	if age == nil {
		t.Fatal("age column missing")
	}
	if age.IsNotNull() {
		t.Error("*int32 field without notnull should map to nullable column")
	}
}

func TestNewAutoEntityCRUD(t *testing.T) {
	ent := pg.NewAutoEntity[autoUser]("users")

	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"id", "email", "name", "age", "created_at"},
			data: [][]any{{int64(7), "a@x", "Alice", (*int32)(nil), time.Now()}},
		}, nil
	}}
	db := pg.New(fd)

	u, err := ent.Get(db, context.Background(), int64(7))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if u.Name != "Alice" {
		t.Errorf("Get target: %+v", u)
	}

	sql := fd.queries[0]
	if !strings.Contains(sql, `FROM "users"`) {
		t.Errorf("Get must select from users: %s", sql)
	}
	if !strings.Contains(sql, `WHERE ("users"."id" = $1)`) {
		t.Errorf("Get must filter by PK: %s", sql)
	}
}

func TestAutoTableUnknownTagOptionPanics(t *testing.T) {
	type bad struct {
		ID int64 `db:"id,wat"`
	}
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on unknown tag option")
		}
	}()
	pg.AutoTable[bad]("bads")
}

func TestAutoTableUnsupportedTypePanics(t *testing.T) {
	type bad struct {
		ID  int64       `db:"id,pk"`
		Bad chan string `db:"bad"`
	}
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on unsupported field type")
		}
	}()
	pg.AutoTable[bad]("bads")
}

func TestAutoTableVersionMarker(t *testing.T) {
	type doc struct {
		ID  int64 `db:"id,pk,autoinc"`
		V   int32 `db:"version,notnull,default=0,version"`
		Tag string `db:"tag"`
	}
	tbl := pg.AutoTable[doc]("docs")
	v := tbl.Col("version")
	if v == nil || !v.IsOptimisticVersion() {
		t.Errorf("version column missing or unmarked")
	}
	// NewEntity should pick it up.
	ent := pg.NewEntity[doc](tbl)
	if ent.PK() == nil {
		t.Error("entity PK missing")
	}
}

func TestAutoTableSchemaQualified(t *testing.T) {
	tbl := pg.AutoSchemaTable[autoUser]("app", "users")
	if tbl.Schema() != "app" {
		t.Errorf("schema: got %q, want app", tbl.Schema())
	}
}
