package pg_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/pg"
)

type counterRow struct {
	ID    int64  `drop:"id"`
	Likes int64  `drop:"likes"`
	Views int64  `drop:"views"`
	Score int64  `drop:"score"`
	Name  string `drop:"name"`
}

// patchSchema declares the columns explicitly so the test owns
// typed *Col[T] handles for Inc / SetIfGreater / etc.
func patchSchema(t *testing.T) (ent *pg.Entity[counterRow], likesCol, viewsCol, scoreCol *pg.Col[int64], nameCol *pg.Col[string]) {
	t.Helper()
	tbl := pg.NewTable("posts")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	likes := pg.Add(tbl, pg.BigInt("likes").NotNull().Default("0"))
	views := pg.Add(tbl, pg.BigInt("views").NotNull().Default("0"))
	score := pg.Add(tbl, pg.BigInt("score").NotNull().Default("0"))
	name := pg.Add(tbl, pg.Text("name").NotNull())
	return pg.NewEntity[counterRow](tbl), likes, views, score, name
}

func TestPatchInc(t *testing.T) {
	ent, likes, _, _, _ := patchSchema(t)
	fd := &fakeDriver{}
	db := pg.New(fd)
	if _, err := ent.Patch(db, context.Background(), int64(7), pg.Inc(likes, int64(1))); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	sql := fd.queries[0]
	if !strings.Contains(sql, `"likes" = "posts"."likes" + $1`) {
		t.Errorf("Inc must render col = col + arg: %s", sql)
	}
	if !strings.Contains(sql, `WHERE ("posts"."id" = $2)`) {
		t.Errorf("Patch must filter by PK: %s", sql)
	}
}

func TestPatchMultipleOps(t *testing.T) {
	ent, likes, views, score, _ := patchSchema(t)
	fd := &fakeDriver{}
	db := pg.New(fd)
	if _, err := ent.Patch(db, context.Background(), int64(7),
		pg.Inc(likes, int64(1)),
		pg.Inc(views, int64(100)),
		pg.SetIfGreater(score, int64(50)),
	); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	sql := fd.queries[0]
	for _, want := range []string{
		`"likes" = "posts"."likes" + $1`,
		`"views" = "posts"."views" + $2`,
		`"score" = GREATEST("posts"."score", $3)`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing fragment %q in:\n%s", want, sql)
		}
	}
}

func TestPatchSetIfLess(t *testing.T) {
	ent, _, _, score, _ := patchSchema(t)
	fd := &fakeDriver{}
	db := pg.New(fd)
	if _, err := ent.Patch(db, context.Background(), int64(7), pg.SetIfLess(score, int64(5))); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if !strings.Contains(fd.queries[0], "LEAST") {
		t.Errorf("SetIfLess must use LEAST: %s", fd.queries[0])
	}
}

func TestPatchSetIfChanged(t *testing.T) {
	ent, _, _, _, name := patchSchema(t)
	fd := &fakeDriver{}
	db := pg.New(fd)
	if _, err := ent.Patch(db, context.Background(), int64(7), pg.SetIfChanged(name, "Alice")); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	sql := fd.queries[0]
	if !strings.Contains(sql, "CASE WHEN") || !strings.Contains(sql, "IS DISTINCT FROM") {
		t.Errorf("SetIfChanged must use CASE WHEN ... IS DISTINCT FROM: %s", sql)
	}
}

func TestPatchSetAlias(t *testing.T) {
	ent, _, _, _, name := patchSchema(t)
	fd := &fakeDriver{}
	db := pg.New(fd)
	if _, err := ent.Patch(db, context.Background(), int64(7), pg.Set(name, "Bob")); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	sql := fd.queries[0]
	if !strings.Contains(sql, `"name" = $1`) {
		t.Errorf("Set must render col = arg: %s", sql)
	}
}

func TestPatchDec(t *testing.T) {
	ent, likes, _, _, _ := patchSchema(t)
	fd := &fakeDriver{}
	db := pg.New(fd)
	if _, err := ent.Patch(db, context.Background(), int64(7), pg.Dec(likes, int64(3))); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if !strings.Contains(fd.queries[0], `"likes" = "posts"."likes" - $1`) {
		t.Errorf("Dec must render a subtraction: %s", fd.queries[0])
	}
	if args := fd.args[0]; args[0] != int64(3) {
		t.Errorf("Dec bound %v, want the delta itself", args[0])
	}
}

// The delta's type is the column handle's, and PostgreSQL having no
// unsigned column type does not stop a handle from being one: Custom
// takes any Go type over any SQL type, and [number] admits the
// unsigned half. Negating the delta to reuse the addition wraps there
// — Dec(seats, uint32(5)) would bind 4294967291 — and the statement
// renders perfectly either way, so the bound value is where it shows.
func TestPatchDecOnAnUnsignedHandleSubtracts(t *testing.T) {
	tbl := pg.NewTable("seatmap")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	seats := pg.Add(tbl, pg.Custom[uint32]("seats", "bigint").NotNull().Default("0"))
	ent := pg.NewEntity[seatRow](tbl)
	fd := &fakeDriver{}
	db := pg.New(fd)
	if _, err := ent.Patch(db, context.Background(), int64(7), pg.Dec(seats, uint32(5))); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if got := fd.args[0][0]; got != uint32(5) {
		t.Errorf("Dec bound %v, want 5 — a negated unsigned delta wraps", got)
	}
	if !strings.Contains(fd.queries[0], `"seats" = "seatmap"."seats" - $1`) {
		t.Errorf("Dec must render a subtraction: %s", fd.queries[0])
	}
}

type seatRow struct {
	ID    int64  `drop:"id"`
	Seats uint32 `drop:"seats"`
}

func TestPatchEmptyOpsErrors(t *testing.T) {
	ent, _, _, _, _ := patchSchema(t)
	db := pg.New(&fakeDriver{})
	if _, err := ent.Patch(db, context.Background(), int64(1)); err == nil {
		t.Error("Patch with no ops should error")
	}
}
