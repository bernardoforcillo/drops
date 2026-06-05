package pg_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// scanCallCount counts invocations of the fast scanner to verify it
// (a) is actually used by Get / Query.One / Query.All and (b) is
// bypassed when eager-loaded relations are queued.
var fastScanCalls int

func fastScanUser(rows pg.Scanner, u *entUser) error {
	fastScanCalls++
	return rows.Scan(&u.ID, &u.Name, &u.Email)
}

func TestEntityFastScanUsedByGet(t *testing.T) {
	fastScanCalls = 0
	_, ent := entUsersSchema()
	ent.SetFastScan(fastScanUser)

	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"id", "name", "email"},
			data: [][]any{{int64(7), "Alice", "a@x"}},
		}, nil
	}}
	db := pg.New(fd)
	u, err := ent.Get(db, context.Background(), int64(7))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if fastScanCalls != 1 {
		t.Errorf("fast scan must be invoked exactly once, got %d", fastScanCalls)
	}
	if u.Name != "Alice" {
		t.Errorf("scan target: %+v", u)
	}
}

func TestEntityFastScanUsedByQueryAll(t *testing.T) {
	fastScanCalls = 0
	_, ent := entUsersSchema()
	ent.SetFastScan(fastScanUser)

	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"id", "name", "email"},
			data: [][]any{
				{int64(1), "A", "a@x"},
				{int64(2), "B", "b@x"},
				{int64(3), "C", "c@x"},
			},
		}, nil
	}}
	db := pg.New(fd)
	users, err := ent.Query(db).All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(users) != 3 {
		t.Fatalf("expected 3 users, got %d", len(users))
	}
	if fastScanCalls != 3 {
		t.Errorf("fast scan must be invoked per row (3), got %d", fastScanCalls)
	}
}

func TestEntityFastScanReturnsErrNoRowsOnEmpty(t *testing.T) {
	fastScanCalls = 0
	_, ent := entUsersSchema()
	ent.SetFastScan(fastScanUser)

	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{cols: []string{"id", "name", "email"}}, nil
	}}
	db := pg.New(fd)
	_, err := ent.Get(db, context.Background(), int64(99))
	if !errors.Is(err, pg.ErrNoRows) {
		t.Errorf("expected ErrNoRows, got %v", err)
	}
	if fastScanCalls != 0 {
		t.Errorf("fast scan must NOT be invoked on empty result, got %d", fastScanCalls)
	}
}

// TestEntityFastScanFallsBackForRelations verifies that Query.All
// with .With(...) bypasses the fast path so the relation loader can
// use its reflection-based assignment.
func TestEntityFastScanFallsBackForRelations(t *testing.T) {
	type post struct {
		ID     int64 `db:"id"`
		UserID int64 `db:"user_id"`
	}
	type userWithPosts struct {
		ID    int64  `db:"id"`
		Name  string `db:"name"`
		Email string `db:"email"`
		Posts []post `db_rel:"posts"`
	}

	users := pg.NewTable("users")
	uid := pg.Add(users, pg.BigSerial("id").PrimaryKey())
	pg.Add(users, pg.Text("name").NotNull())
	pg.Add(users, pg.Text("email").NotNull())
	posts := pg.NewTable("posts")
	pg.Add(posts, pg.BigSerial("id").PrimaryKey())
	pUID := pg.Add(posts, pg.BigInt("user_id").NotNull())
	pg.NewRelations(users).HasMany("posts", posts, uid, pUID)

	ent := pg.NewEntity[userWithPosts](users)
	called := 0
	ent.SetFastScan(func(rows pg.Scanner, u *userWithPosts) error {
		called++
		return rows.Scan(&u.ID, &u.Name, &u.Email)
	})

	var fd *fakeDriver
	fd = &fakeDriver{handler: func(sql string, _ []any) (drops.Rows, error) {
		// First query: users; second: posts.
		switch {
		case len(fd.queries) == 1:
			return &fakeRows{
				cols: []string{"id", "name", "email"},
				data: [][]any{{int64(1), "Alice", "a@x"}},
			}, nil
		default:
			return &fakeRows{
				cols: []string{"id", "user_id"},
				data: [][]any{{int64(10), int64(1)}},
			}, nil
		}
	}}
	db := pg.New(fd)
	users2, err := ent.Query(db).With("posts").All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if called != 0 {
		t.Errorf("fast scan must be bypassed when relations are eager-loaded, got %d calls", called)
	}
	if len(users2) != 1 || len(users2[0].Posts) != 1 {
		t.Errorf("relation loader did not populate posts: %+v", users2)
	}
}

func TestHasFastScanReportsCorrectly(t *testing.T) {
	_, ent := entUsersSchema()
	if ent.HasFastScan() {
		t.Error("HasFastScan should be false initially")
	}
	ent.SetFastScan(fastScanUser)
	if !ent.HasFastScan() {
		t.Error("HasFastScan should be true after SetFastScan")
	}
}
