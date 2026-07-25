package pg_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// migratorFakeDB returns a fake driver whose applied-versions SELECT
// reports the supplied versions as already applied. Pass none to drive
// a fresh Up.
func migratorFakeDB(appliedVersions ...string) (*pg.DB, *fakeDriver) {
	fd := &fakeDriver{
		handler: func(sql string, _ []any) (drops.Rows, error) {
			if strings.Contains(sql, "SELECT version") {
				data := make([][]any, 0, len(appliedVersions))
				for _, v := range appliedVersions {
					data = append(data, []any{v, time.Time{}})
				}
				return &fakeRows{cols: []string{"version", "appliedAt"}, data: data}, nil
			}
			return &fakeRows{}, nil
		},
	}
	return pg.New(fd), fd
}

func TestMigratorHooksFireAroundUpInOrder(t *testing.T) {
	db, fd := migratorFakeDB()

	var log []string
	m := pg.NewMigrator(db)
	m.BeforeEach(func(_ context.Context, _ *pg.DB, mig pg.Migration, dir pg.MigrationDirection) error {
		log = append(log, "before:"+mig.Version+":"+dir.String())
		return nil
	})
	m.AfterEach(func(ctx context.Context, tx *pg.DB, mig pg.Migration, dir pg.MigrationDirection) error {
		log = append(log, "after:"+mig.Version+":"+dir.String())
		// Data migration keyed to a specific step, running in the same
		// tx as the schema change that just landed.
		if dir == pg.DirectionUp && mig.Version == "0002" {
			_, err := tx.Exec(ctx, `UPDATE users SET full_name = first_name || ' ' || last_name`)
			return err
		}
		return nil
	})
	m.Add(pg.Migration{Version: "0001", Name: "a", Up: func(_ context.Context, _ *pg.DB) error {
		log = append(log, "up:0001")
		return nil
	}})
	m.Add(pg.Migration{Version: "0002", Name: "b", Up: func(_ context.Context, _ *pg.DB) error {
		log = append(log, "up:0002")
		return nil
	}})

	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}

	want := []string{
		"before:0001:up", "up:0001", "after:0001:up",
		"before:0002:up", "up:0002", "after:0002:up",
	}
	if strings.Join(log, ",") != strings.Join(want, ",") {
		t.Errorf("hook order:\n  got:  %v\n  want: %v", log, want)
	}

	// The AfterEach data migration must have executed against the DB.
	var sawBackfill bool
	for _, q := range fd.queries {
		if strings.Contains(q, "SET full_name = first_name") {
			sawBackfill = true
		}
	}
	if !sawBackfill {
		t.Errorf("expected the after-hook backfill UPDATE to run; queries: %v", fd.queries)
	}
}

func TestMigratorHookErrorAbortsMigration(t *testing.T) {
	db, fd := migratorFakeDB()

	boom := errors.New("backfill failed")
	m := pg.NewMigrator(db)
	m.AfterEach(func(_ context.Context, _ *pg.DB, mig pg.Migration, _ pg.MigrationDirection) error {
		if mig.Version == "0002" {
			return boom
		}
		return nil
	})
	m.Add(pg.Migration{Version: "0001", Name: "a", Up: func(context.Context, *pg.DB) error { return nil }})
	m.Add(pg.Migration{Version: "0002", Name: "b", Up: func(context.Context, *pg.DB) error { return nil }})

	err := m.Up(context.Background())
	if err == nil {
		t.Fatal("expected Up to fail when an after-hook errors")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error should wrap the hook error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "after-hook") {
		t.Errorf("error should mention after-hook, got: %v", err)
	}

	// 0002's history row must never be inserted (its tx rolled back);
	// 0001's must, since it completed before the failure.
	var ins0001, ins0002 bool
	for i, q := range fd.queries {
		if strings.Contains(q, "INSERT INTO") && strings.Contains(q, "$1") {
			switch fd.args[i][0] {
			case "0001":
				ins0001 = true
			case "0002":
				ins0002 = true
			}
		}
	}
	if !ins0001 {
		t.Error("0001 should have been recorded as applied")
	}
	if ins0002 {
		t.Error("0002 must not be recorded when its after-hook failed")
	}
}

func TestMigratorHooksFireOnDownWithDirection(t *testing.T) {
	db, _ := migratorFakeDB("0001")

	var log []string
	m := pg.NewMigrator(db)
	m.BeforeEach(func(_ context.Context, _ *pg.DB, mig pg.Migration, dir pg.MigrationDirection) error {
		log = append(log, "before:"+mig.Version+":"+dir.String())
		return nil
	})
	m.AfterEach(func(_ context.Context, _ *pg.DB, mig pg.Migration, dir pg.MigrationDirection) error {
		log = append(log, "after:"+mig.Version+":"+dir.String())
		return nil
	})
	m.Add(pg.Migration{
		Version: "0001", Name: "a",
		Up:   func(context.Context, *pg.DB) error { return nil },
		Down: func(context.Context, *pg.DB) error { log = append(log, "down:0001"); return nil },
	})

	if err := m.Down(context.Background()); err != nil {
		t.Fatalf("Down: %v", err)
	}

	want := []string{"before:0001:down", "down:0001", "after:0001:down"}
	if strings.Join(log, ",") != strings.Join(want, ",") {
		t.Errorf("down hook order:\n  got:  %v\n  want: %v", log, want)
	}
}

func TestDrizzleMigratorHooksRunDataMigration(t *testing.T) {
	db, fd, _ := drizzleFakeDB()

	var tags []string
	m := pg.NewDrizzleMigrator(db, drizzleFS(), "drizzle")
	m.BeforeEach(func(_ context.Context, _ *pg.DB, e pg.DrizzleEntry) error {
		tags = append(tags, "before:"+e.Tag)
		return nil
	})
	m.AfterEach(func(ctx context.Context, tx *pg.DB, e pg.DrizzleEntry) error {
		tags = append(tags, "after:"+e.Tag)
		if e.Tag == "0001_more" {
			_, err := tx.Exec(ctx, `UPDATE users SET age = 0 WHERE age IS NULL`)
			return err
		}
		return nil
	})

	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}

	// Every file gets both hooks, in file order, before before after.
	want := []string{
		"before:0000_init", "after:0000_init",
		"before:0001_more", "after:0001_more",
		"before:0002_solo", "after:0002_solo",
	}
	if strings.Join(tags, ",") != strings.Join(want, ",") {
		t.Errorf("drizzle hook order:\n  got:  %v\n  want: %v", tags, want)
	}

	var sawBackfill bool
	for _, q := range fd.queries {
		if strings.Contains(q, "SET age = 0 WHERE age IS NULL") {
			sawBackfill = true
		}
	}
	if !sawBackfill {
		t.Errorf("expected the after-hook backfill UPDATE to run; queries: %v", fd.queries)
	}
}
