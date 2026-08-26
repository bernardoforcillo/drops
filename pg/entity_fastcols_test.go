package pg_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// The fixture reproduces the failure P0-3 is about: the table has a
// column no field is bound to, sitting between the primary key and
// the first mapped column — what an ALTER TABLE ADD COLUMN leaves
// behind when the migration lands before the struct is regenerated.
//
// A positional scanner is decoding "the first value is ID, the second
// is Name". That statement is only true of a row whose columns this
// entity chose. Fed a "SELECT *", the row's second value is nickname,
// and every field after it holds its neighbour's value — with no
// error at any layer, because there is nothing in a positional scan
// to compare a name against.
type fastColUser struct {
	ID    int64  `drop:"id"`
	Name  string `drop:"name"`
	Email string `drop:"email"`
}

func scanFastColUser(rows pg.Scanner, u *fastColUser) error {
	return rows.Scan(&u.ID, &u.Name, &u.Email)
}

// fastColPhysical is the table's physical column order, nickname and
// all. fastColGenerated is what the generator emitted alongside the
// scanner.
var (
	fastColPhysical  = []string{"id", "nickname", "name", "email"}
	fastColGenerated = []string{"id", "name", "email"}
)

// fastColSchema builds the entity with the fast scanner wired up, and
// returns the primary-key handle Page needs for its ordering.
func fastColSchema(t *testing.T) (*pg.Entity[fastColUser], *pg.Col[int64]) {
	t.Helper()
	tbl := pg.NewTable("users")
	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	pg.Add(tbl, pg.Text("nickname"))
	pg.Add(tbl, pg.Text("name").NotNull())
	pg.Add(tbl, pg.Text("email").NotNull())
	ent := pg.NewEntity[fastColUser](tbl, pg.AllowUnmappedColumns("nickname"))
	return ent.SetFastScan(fastColGenerated, scanFastColUser), id
}

// fastColDriver answers a SELECT the way a server does: with the
// columns the statement named, in the order it named them, or with
// every column of the table when the statement named none. Handing
// back a fixed column set regardless of the SQL is what let this bug
// live — the test has to model the half of the contract that lives in
// the database.
func fastColDriver() *fakeDriver {
	row := map[string]any{
		"id":       int64(7),
		"nickname": "nick",
		"name":     "Alice",
		"email":    "alice@x",
	}
	return &fakeDriver{handler: func(sql string, _ []any) (drops.Rows, error) {
		cols := selectedCols(sql)
		vals := make([]any, len(cols))
		for i, c := range cols {
			vals[i] = row[c]
		}
		return &fakeRows{cols: cols, data: [][]any{vals}}, nil
	}}
}

// selectedCols extracts the column names a rendered SELECT projects,
// falling back to the table's physical order for "SELECT *".
func selectedCols(sql string) []string {
	head, _, _ := strings.Cut(sql, " FROM ")
	head = strings.TrimPrefix(head, "SELECT ")
	if strings.HasPrefix(head, "*") {
		return fastColPhysical
	}
	parts := strings.Split(head, ", ")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.Trim(p[strings.LastIndex(p, ".")+1:], `"`))
	}
	return out
}

// TestFastScanReadsTheColumnsItWasGeneratedFor is the regression: a
// column added to the middle of the table must not move a value into
// the neighbouring field. Every executor that takes the fast path has
// to render the generated list, so every one of them is exercised —
// the bug was per-path, and a fix that reaches Get and forgets Page
// is not a fix.
func TestFastScanReadsTheColumnsItWasGeneratedFor(t *testing.T) {
	cases := []struct {
		name string
		run  func(*pg.Entity[fastColUser], *pg.Col[int64], *pg.DB) (fastColUser, error)
	}{
		{"Get", func(e *pg.Entity[fastColUser], _ *pg.Col[int64], db *pg.DB) (fastColUser, error) {
			return e.Get(db, context.Background(), int64(7))
		}},
		{"Query.One", func(e *pg.Entity[fastColUser], _ *pg.Col[int64], db *pg.DB) (fastColUser, error) {
			return e.Query(db).One(context.Background())
		}},
		{"Query.All", func(e *pg.Entity[fastColUser], _ *pg.Col[int64], db *pg.DB) (fastColUser, error) {
			us, err := e.Query(db).All(context.Background())
			if err != nil || len(us) == 0 {
				return fastColUser{}, err
			}
			return us[0], nil
		}},
		{"Query.Stream", func(e *pg.Entity[fastColUser], _ *pg.Col[int64], db *pg.DB) (fastColUser, error) {
			var got fastColUser
			err := e.Query(db).Stream(context.Background(), func(u *fastColUser) error {
				got = *u
				return nil
			})
			return got, err
		}},
		{"Page.All", func(e *pg.Entity[fastColUser], id *pg.Col[int64], db *pg.DB) (fastColUser, error) {
			p, err := e.Page(db).OrderBy(pg.Asc(id)).All(context.Background())
			if err != nil || len(p.Items) == 0 {
				return fastColUser{}, err
			}
			return p.Items[0], nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ent, id := fastColSchema(t)
			fd := fastColDriver()
			got, err := tc.run(ent, id, pg.New(fd))
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			sql := fd.queries[0]
			if strings.Contains(sql, "SELECT *") {
				t.Errorf("fast path must not render SELECT *, got %s", sql)
			}
			want := `SELECT "users"."id", "users"."name", "users"."email" FROM`
			if !strings.HasPrefix(sql, want) {
				t.Errorf("projection: got %s, want prefix %s", sql, want)
			}
			if got.Name != "Alice" {
				t.Errorf("Name = %v, want %v (the row shifted by one column)", got.Name, "Alice")
			}
			if got.Email != "alice@x" {
				t.Errorf("Email = %v, want %v", got.Email, "alice@x")
			}
		})
	}
}

// TestFastScanRendersTheRegisteredOrder pins the other half: the
// order the statement lists the columns in is the generator's, not
// the table's declaration order. The two coincide in the fixture
// above, so a reversed list is what proves which one is in charge.
func TestFastScanRendersTheRegisteredOrder(t *testing.T) {
	tbl := pg.NewTable("users")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	pg.Add(tbl, pg.Text("name").NotNull())
	pg.Add(tbl, pg.Text("email").NotNull())
	ent := pg.NewEntity[fastColUser](tbl).SetFastScan(
		[]string{"email", "name", "id"},
		func(rows pg.Scanner, u *fastColUser) error {
			return rows.Scan(&u.Email, &u.Name, &u.ID)
		})

	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"email", "name", "id"},
			data: [][]any{{"alice@x", "Alice", int64(7)}},
		}, nil
	}}
	u, err := ent.Get(pg.New(fd), context.Background(), int64(7))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := `SELECT "users"."email", "users"."name", "users"."id" FROM`
	if !strings.HasPrefix(fd.queries[0], want) {
		t.Errorf("projection: got %s, want prefix %s", fd.queries[0], want)
	}
	if u.ID != 7 || u.Name != "Alice" {
		t.Errorf("scan: got %+v, want ID 7 / Alice", u)
	}
}

// TestFastScanFallbackKeepsSelectStar guards the reflection path: an
// eager-loaded relation cannot use the positional scanner, so the
// projection must not follow it there — the relation loader stitches
// by reading the columns the row reports.
func TestFastScanFallbackKeepsSelectStar(t *testing.T) {
	_, ent := entUsersSchema()
	ent.SetFastScan(entUserCols, fastScanUser)
	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{cols: []string{"id", "name", "email"}}, nil
	}}
	if _, err := ent.Query(pg.New(fd)).All(context.Background()); err != nil {
		t.Fatalf("All: %v", err)
	}
	if strings.Contains(fd.queries[0], "SELECT *") {
		t.Fatalf("fast path should project, got %s", fd.queries[0])
	}

	_, plain := entUsersSchema() // no fast scanner registered
	fd2 := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{cols: []string{"id", "name", "email"}}, nil
	}}
	if _, err := plain.Query(pg.New(fd2)).All(context.Background()); err != nil {
		t.Fatalf("All: %v", err)
	}
	if !strings.Contains(fd2.queries[0], "SELECT *") {
		t.Errorf("reflection path should stay on SELECT *, got %s", fd2.queries[0])
	}
}

// TestSetFastScanRejectsDivergedSchema covers the panic. A generated
// file naming a column the table does not have is a schema and a
// checkout that disagree; the alternative to failing at startup is
// scanning rows into the wrong fields for the life of the process.
func TestSetFastScanRejectsDivergedSchema(t *testing.T) {
	cases := []struct {
		name string
		cols []string
		want string
	}{
		{"unknown column", []string{"id", "nickname_typo", "email"}, "has no column"},
		{"no column list", nil, "needs the column list"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("SetFastScan(%v) must panic", tc.cols)
				}
				msg, _ := r.(string)
				if !strings.Contains(msg, tc.want) {
					t.Errorf("panic = %v, want it to mention %q", msg, tc.want)
				}
			}()
			_, ent := entUsersSchema()
			ent.SetFastScan(tc.cols, fastScanUser)
		})
	}
}

// TestFastScanProjectsOnTheCachedPaths covers the two executors that
// render the statement twice — once to key the cache, once to run it.
// Both renders have to be the projected one, or the cache is keyed on
// a statement nobody issues.
func TestFastScanProjectsOnTheCachedPaths(t *testing.T) {
	cases := []struct {
		name string
		run  func(*pg.Entity[fastColUser], *pg.DB) (fastColUser, error)
	}{
		{"Get", func(e *pg.Entity[fastColUser], db *pg.DB) (fastColUser, error) {
			return e.Get(db, context.Background(), int64(7))
		}},
		{"Query.All", func(e *pg.Entity[fastColUser], db *pg.DB) (fastColUser, error) {
			us, err := e.Query(db).All(context.Background())
			if err != nil || len(us) == 0 {
				return fastColUser{}, err
			}
			return us[0], nil
		}},
		{"Query.One", func(e *pg.Entity[fastColUser], db *pg.DB) (fastColUser, error) {
			return e.Query(db).One(context.Background())
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ent, _ := fastColSchema(t)
			ent.WithCache(newCountingCache(t), time.Minute)
			fd := fastColDriver()
			got, err := tc.run(ent, pg.New(fd))
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			want := `SELECT "users"."id", "users"."name", "users"."email" FROM`
			if !strings.HasPrefix(fd.queries[0], want) {
				t.Errorf("projection: got %s, want prefix %s", fd.queries[0], want)
			}
			if got.Name != "Alice" {
				t.Errorf("Name = %v, want %v (the row shifted by one column)", got.Name, "Alice")
			}
		})
	}
}
