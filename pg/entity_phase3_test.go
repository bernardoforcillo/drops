package pg_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// ----------------------------------------------------------------------
// Validation
// ----------------------------------------------------------------------

func TestValidatorBlocksCreate(t *testing.T) {
	_, ent := entUsersSchema()
	errBad := errors.New("invalid email")
	ent.Validate(func(u *entUser) error {
		if !strings.Contains(u.Email, "@") {
			return errBad
		}
		return nil
	})

	fd := &fakeDriver{}
	db := pg.New(fd)
	u := entUser{Name: "Alice", Email: "no-at-sign"}
	if err := ent.Create(db, context.Background(), &u); !errors.Is(err, errBad) {
		t.Errorf("expected validator error, got %v", err)
	}
	if len(fd.queries) != 0 {
		t.Errorf("validator must abort before any SQL: %v", fd.queries)
	}
}

func TestValidatorBlocksUpdate(t *testing.T) {
	_, ent := entUsersSchema()
	errBad := errors.New("name too short")
	ent.Validate(func(u *entUser) error {
		if len(u.Name) < 2 {
			return errBad
		}
		return nil
	})

	fd := &fakeDriver{}
	db := pg.New(fd)
	u := entUser{ID: 1, Name: "A", Email: "a@x"}
	if err := ent.Update(db, context.Background(), &u); !errors.Is(err, errBad) {
		t.Errorf("expected validator error, got %v", err)
	}
	if len(fd.queries) != 0 {
		t.Errorf("validator must abort before any SQL: %v", fd.queries)
	}
}

func TestValidatorsRunInOrderFirstFailWins(t *testing.T) {
	_, ent := entUsersSchema()
	err1 := errors.New("first")
	err2 := errors.New("second")
	calls := []string{}
	ent.
		Validate(func(u *entUser) error {
			calls = append(calls, "1")
			if u.Email == "" {
				return err1
			}
			return nil
		}).
		Validate(func(u *entUser) error {
			calls = append(calls, "2")
			return err2
		})

	db := pg.New(&fakeDriver{})
	u := entUser{Name: "Alice", Email: ""}
	err := ent.Create(db, context.Background(), &u)
	if !errors.Is(err, err1) {
		t.Errorf("first validator should win, got %v", err)
	}
	if len(calls) != 1 || calls[0] != "1" {
		t.Errorf("subsequent validators must not run after a failure: %v", calls)
	}
}

// ----------------------------------------------------------------------
// Optimistic locking
// ----------------------------------------------------------------------

type versionedUser struct {
	ID      int64  `db:"id"`
	Name    string `db:"name"`
	Version int32  `db:"version"`
}

func versionedSchema() (*pg.Table, *pg.Entity[versionedUser]) {
	tbl := pg.NewTable("v_users")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	pg.Add(tbl, pg.Text("name").NotNull())
	pg.Add(tbl, pg.Integer("version").NotNull().Default("0").OptimisticLock())
	return tbl, pg.NewEntity[versionedUser](tbl)
}

// TestOptimisticUpdateAppendsVersionGuard verifies the generated SQL
// includes the AND version = current guard and the version + 1 bump.
func TestOptimisticUpdateAppendsVersionGuard(t *testing.T) {
	_, ent := versionedSchema()
	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		// RETURNING produces the post-bump version.
		return &fakeRows{
			cols: []string{"id", "name", "version"},
			data: [][]any{{int64(1), "Alice", int32(8)}},
		}, nil
	}}
	db := pg.New(fd)
	u := versionedUser{ID: 1, Name: "Alice", Version: 7}
	if err := ent.Update(db, context.Background(), &u); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if u.Version != 8 {
		t.Errorf("RETURNING should refresh version: %+v", u)
	}
	sql := fd.queries[0]
	if !strings.Contains(sql, `"version" = "version" + 1`) {
		t.Errorf("SET should bump version: %s", sql)
	}
	if !strings.Contains(sql, `("v_users"."version" = $`) {
		t.Errorf("WHERE should guard by version: %s", sql)
	}
}

// TestOptimisticUpdateReturnsStaleObject verifies that ErrNoRows on a
// versioned table becomes ErrStaleObject.
func TestOptimisticUpdateReturnsStaleObject(t *testing.T) {
	_, ent := versionedSchema()
	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		// No rows returned ⇒ version mismatch.
		return &fakeRows{cols: []string{"id", "name", "version"}}, nil
	}}
	db := pg.New(fd)
	u := versionedUser{ID: 1, Name: "Alice", Version: 7}
	err := ent.Update(db, context.Background(), &u)
	if !errors.Is(err, pg.ErrStaleObject) {
		t.Errorf("expected ErrStaleObject, got: %v", err)
	}
}

// TestOptimisticVersionColumnNotInSetByCaller verifies the version
// column never leaks the caller's value into the SET list — even if
// the user accidentally bumps it themselves.
func TestOptimisticVersionColumnNotInSetByCaller(t *testing.T) {
	_, ent := versionedSchema()
	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"id", "name", "version"},
			data: [][]any{{int64(1), "Alice", int32(8)}},
		}, nil
	}}
	db := pg.New(fd)
	u := versionedUser{ID: 1, Name: "Alice", Version: 7}
	if err := ent.Update(db, context.Background(), &u); err != nil {
		t.Fatalf("Update: %v", err)
	}
	sql := fd.queries[0]
	setPart := sql[:strings.Index(sql, " WHERE")]
	// Make sure the SET clause uses the self-bump expression, not a
	// "version = $N" parameter binding from the caller's value.
	if strings.Contains(setPart, `"version" = $`) {
		t.Errorf("caller's version value must not be bound: %s", sql)
	}
}

// TestNewEntityPanicsOnMultipleVersionCols verifies the validator.
func TestNewEntityPanicsOnMultipleVersionCols(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for multiple OptimisticLock columns")
		}
	}()
	type doc struct {
		ID int64 `db:"id"`
		A  int32 `db:"a"`
		B  int32 `db:"b"`
	}
	tbl := pg.NewTable("docs")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	pg.Add(tbl, pg.Integer("a").NotNull().Default("0").OptimisticLock())
	pg.Add(tbl, pg.Integer("b").NotNull().Default("0").OptimisticLock())
	pg.NewEntity[doc](tbl)
}
