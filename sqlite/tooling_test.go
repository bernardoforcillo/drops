package sqlite_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/sqlite"
)

func userFactory() *sqlite.Factory[entUser] {
	ent := sqlite.NewEntity[entUser](entSchema())
	return sqlite.NewFactory(ent, func(seq int) entUser {
		return entUser{ID: int64(seq), Name: fmt.Sprintf("u%d", seq)}
	})
}

func TestFactoryBuildAndCreate(t *testing.T) {
	f := userFactory()
	a := f.Build()
	b := f.Build()
	if a.ID != 1 || b.ID != 2 {
		t.Errorf("sequence: %d %d", a.ID, b.ID)
	}
	admin := f.With(func(u *entUser) { u.Name = "admin" })
	if admin.Build().Name != "admin" {
		t.Error("With mutate not applied")
	}
	f.Reset()
	if f.Sequence() != 0 {
		t.Error("reset failed")
	}

	drv := &entDriver{}
	db := sqlite.New(drv)
	if _, err := f.Create(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(drv.queries[0], `INSERT`) {
		t.Errorf("create SQL: %s", drv.queries[0])
	}
}

func TestFactoryCreateN(t *testing.T) {
	f := userFactory()
	drv := &entDriver{}
	db := sqlite.New(drv)
	rows, err := f.CreateN(context.Background(), db, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows: %d", len(rows))
	}
	// Single multi-row INSERT.
	if strings.Count(drv.queries[0], "VALUES") != 1 || strings.Count(drv.queries[0], "(?, ?)") != 3 {
		t.Errorf("batch insert SQL:\n%s", drv.queries[0])
	}
}

func TestSeeder(t *testing.T) {
	ent := sqlite.NewEntity[entUser](entSchema())
	drv := &entDriver{}
	db := sqlite.New(drv)
	s := sqlite.NewSeeder(db)
	sqlite.SeedAdd(s, ent, entUser{ID: 1, Name: "a"}, entUser{ID: 2, Name: "b"})
	if err := s.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(drv.queries, "\n")
	if !strings.Contains(joined, "INSERT") {
		t.Errorf("seed did not insert:\n%s", joined)
	}
}

type fakeTB struct {
	cleanups []func()
	fatal    bool
}

func (f *fakeTB) Helper()               {}
func (f *fakeTB) Errorf(string, ...any) {}
func (f *fakeTB) Fatalf(string, ...any) { f.fatal = true }
func (f *fakeTB) Logf(string, ...any)   {}
func (f *fakeTB) Cleanup(fn func())     { f.cleanups = append(f.cleanups, fn) }

func TestTestTx(t *testing.T) {
	db := sqlite.New(&entDriver{})
	tb := &fakeTB{}
	ran := false
	sqlite.TestTx(tb, db, context.Background(), func(tx *sqlite.DB) {
		ran = true
	})
	for _, c := range tb.cleanups {
		c()
	}
	if !ran || tb.fatal {
		t.Errorf("TestTx ran=%v fatal=%v", ran, tb.fatal)
	}
}

func TestN1Detector(t *testing.T) {
	drv := &entDriver{}
	db := sqlite.New(drv).WithHook(sqlite.N1Hook)
	ctx, finish := sqlite.WithN1Detector(context.Background())
	_, _ = db.Query(ctx, "SELECT * FROM t WHERE id = ?", 1)
	_, _ = db.Query(ctx, "SELECT * FROM t WHERE id = ?", 2)
	_, _ = db.Query(ctx, "SELECT * FROM other", nil)
	r := finish(2)
	if r.IsClean() {
		t.Fatal("expected an N+1 pattern")
	}
	if len(r.Patterns) != 1 || r.Patterns[0].Count != 2 {
		t.Errorf("patterns: %+v", r.Patterns)
	}
}

func TestCursorEncodeDecode(t *testing.T) {
	spec := sqlite.NewCursorSpec(sqlite.OrderKey{Col: entSchema().Col("id")})
	cur, err := sqlite.EncodeCursor(spec, int64(5))
	if err != nil {
		t.Fatal(err)
	}
	vals, err := cur.Decode()
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 1 || vals[0].(int64) != 5 {
		t.Errorf("decoded: %v", vals)
	}
}

func TestCursorSQL(t *testing.T) {
	users := entSchema()
	spec := sqlite.NewCursorSpec(sqlite.OrderKey{Col: users.Col("id")})
	cur, _ := sqlite.EncodeCursor(spec, int64(5))
	db := sqlite.New(&entDriver{})
	sql, args := db.Select().From(users).OrderByCursor(spec).AfterCursor(spec, cur).ToSQL()
	if !strings.Contains(sql, `ORDER BY "users"."id" ASC`) {
		t.Errorf("order by cursor:\n%s", sql)
	}
	if !strings.Contains(sql, `("users"."id" > ?)`) {
		t.Errorf("keyset where:\n%s", sql)
	}
	if len(args) != 1 || args[0].(int64) != 5 {
		t.Errorf("cursor arg: %v", args)
	}
}

func TestEnumColDDL(t *testing.T) {
	tasks := sqlite.NewTable("tasks")
	sqlite.Add(tasks, sqlite.Integer("id").PrimaryKey())
	sqlite.EnumCol(tasks, "priority", "low", "high")
	sql, _ := sqlite.ToSQL(sqlite.CreateTable(tasks))
	if !strings.Contains(sql, `CHECK ("priority" IN ('low', 'high'))`) {
		t.Errorf("enum CHECK absent:\n%s", sql)
	}
}

// silence unused import when drops is only referenced transitively.
var _ = drops.Raw("")
