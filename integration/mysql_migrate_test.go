package integration_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/mysql"
)

// The migrator against a real server, and mostly against the one thing
// MySQL does differently: it commits the open transaction when it meets
// DDL, so the transactional shape drops/pg and drops/sqlite rely on is
// not available. What replaces it is a history row written before the
// migration and marked applied after, so a failure leaves a state
// nobody can mistake for either outcome.
//
// These tests are the reason to believe that. The interrupted case in
// particular cannot be checked against a fake driver: it needs a server
// that really does commit the ALTER and really does leave the column
// behind.

func mysqlMigrator(t *testing.T, db *mysql.DB) *mysql.Migrator {
	t.Helper()
	table := integration.UniqueName(t, "mig")
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), "DROP TABLE IF EXISTS `"+table+"`")
	})
	return mysql.NewMigrator(db).WithTable(table)
}

func TestMySQLMigratorAppliesInOrderAndIsIdempotent(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl := integration.UniqueName(t, "mig_users")
	t.Cleanup(func() { _, _ = db.Exec(ctx, "DROP TABLE IF EXISTS `"+tbl+"`") })

	m := mysqlMigrator(t, db).
		AddSQL("0001", "create", fmt.Sprintf("CREATE TABLE `%s` (id BIGINT PRIMARY KEY)", tbl),
			fmt.Sprintf("DROP TABLE `%s`", tbl)).
		AddSQL("0002", "add_email", fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN email VARCHAR(255)", tbl),
			fmt.Sprintf("ALTER TABLE `%s` DROP COLUMN email", tbl))

	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if _, err := db.Exec(ctx, fmt.Sprintf("INSERT INTO `%s` (id, email) VALUES (1, 'a@b.c')", tbl)); err != nil {
		t.Fatalf("the migrations did not leave the table they describe: %v", err)
	}

	// A second Up is a no-op: every migration is recorded applied.
	if err := m.Up(ctx); err != nil {
		t.Fatalf("second Up: %v", err)
	}

	st, err := m.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(st) != 2 {
		t.Fatalf("Status returned %d rows, want 2", len(st))
	}
	for _, s := range st {
		if !s.Applied || s.Interrupted || s.AppliedAt.IsZero() {
			t.Errorf("%s_%s: applied=%v interrupted=%v appliedAt=%v",
				s.Version, s.Name, s.Applied, s.Interrupted, s.AppliedAt)
		}
	}

	// Down rolls back the most recent one and no more.
	if err := m.Down(ctx); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if _, err := db.Exec(ctx, fmt.Sprintf("SELECT email FROM `%s`", tbl)); err == nil {
		t.Error("Down did not drop the column its migration added")
	}
	if _, err := db.Exec(ctx, fmt.Sprintf("SELECT id FROM `%s`", tbl)); err != nil {
		t.Errorf("Down rolled back more than one migration: %v", err)
	}
}

// The case the whole design exists for. A migration whose DDL lands and
// whose body then fails leaves the schema changed — MySQL committed the
// ALTER when it met it — and the history row unfinished.
//
// The next Up must not guess. Retrying would fail on a column that
// already exists; skipping would call a half-applied migration done.
// It refuses, and says which one.
func TestMySQLMigratorRefusesAfterAnInterruptedMigration(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl := integration.UniqueName(t, "mig_broken")
	t.Cleanup(func() { _, _ = db.Exec(ctx, "DROP TABLE IF EXISTS `"+tbl+"`") })

	table := integration.UniqueName(t, "mig")
	t.Cleanup(func() { _, _ = db.Exec(ctx, "DROP TABLE IF EXISTS `"+table+"`") })

	fail := errors.New("the deploy was killed here")
	first := mysql.NewMigrator(db).WithTable(table).
		AddSQL("0001", "create", fmt.Sprintf("CREATE TABLE `%s` (id BIGINT PRIMARY KEY)", tbl), "").
		Add(mysql.Migration{
			Version: "0002", Name: "add_then_die",
			Up: func(ctx context.Context, db *mysql.DB) error {
				if _, err := db.Exec(ctx,
					fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN nickname VARCHAR(255)", tbl)); err != nil {
					return err
				}
				return fail
			},
		})

	if err := first.Up(ctx); !errors.Is(err, fail) {
		t.Fatalf("Up: %v, want the migration's own error", err)
	}
	// The DDL landed anyway. This is the fact the design is built on;
	// if MySQL ever stops doing it, this assertion is where we find out.
	if _, err := db.Exec(ctx, fmt.Sprintf("SELECT nickname FROM `%s`", tbl)); err != nil {
		t.Fatalf("MySQL rolled the DDL back after all — the premise of the two-phase record has changed: %v", err)
	}

	// A fresh migrator over the same history: it must refuse.
	second := mysql.NewMigrator(db).WithTable(table).
		AddSQL("0001", "create", "SELECT 1", "").
		AddSQL("0002", "add_then_die", "SELECT 1", "").
		AddSQL("0003", "later", "SELECT 1", "")
	err := second.Up(ctx)
	if !errors.Is(err, mysql.ErrMigrationInterrupted) {
		t.Fatalf("Up over an interrupted history: %v, want ErrMigrationInterrupted", err)
	}
	if !strings.Contains(err.Error(), "0002") {
		t.Errorf("the refusal does not name the migration:\n%v", err)
	}

	// Status shows it as neither applied nor absent.
	st, err := second.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	var found bool
	for _, s := range st {
		if s.Version != "0002" {
			continue
		}
		found = true
		if s.Applied || !s.Interrupted {
			t.Errorf("0002: applied=%v interrupted=%v, want applied=false interrupted=true", s.Applied, s.Interrupted)
		}
		if s.StartedAt.IsZero() {
			t.Error("0002 has no StartedAt, so nobody can tell when the deploy died")
		}
	}
	if !found {
		t.Error("Status did not report the interrupted migration at all")
	}

	// And the way out works: mark it applied by hand, and the run
	// continues from there.
	if _, err := db.Exec(ctx,
		fmt.Sprintf("UPDATE `%s` SET appliedAt = NOW() WHERE version = ?", table), "0002"); err != nil {
		t.Fatalf("marking it applied: %v", err)
	}
	if err := second.Up(ctx); err != nil {
		t.Fatalf("Up after the operator resolved it: %v", err)
	}
}

// The lock is a real lock: a second migrator that cannot get it gives
// up with ErrMigrationLocked rather than running the migrations twice.
func TestMySQLMigratorSerialisesTwoRuns(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()

	table := integration.UniqueName(t, "mig")
	t.Cleanup(func() { _, _ = db.Exec(ctx, "DROP TABLE IF EXISTS `"+table+"`") })

	holder := mysql.NewMigrator(db).WithTable(table)

	// Take the lock the migrator would take, on a connection of our
	// own, and hold it for the length of the attempt.
	other := openMySQL(t)
	lockHeld, _, err := other.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	rows, err := lockHeld.Query(ctx, "SELECT GET_LOCK(?, 5)", holder.LockKey())
	if err != nil {
		t.Fatalf("GET_LOCK: %v", err)
	}
	var got int64
	if rows.Next() {
		_ = rows.Scan(&got)
	}
	_ = rows.Close()
	if got != 1 {
		t.Fatalf("could not take the lock to hold it: %d", got)
	}
	defer func() {
		_, _ = lockHeld.Exec(ctx, "DO RELEASE_LOCK(?)", holder.LockKey())
	}()

	blocked := mysql.NewMigrator(db).WithTable(table).
		WithLockTimeout(-1). // do not wait
		AddSQL("0001", "noop", "SELECT 1", "")
	if err := blocked.Up(ctx); !errors.Is(err, mysql.ErrMigrationLocked) {
		t.Fatalf("Up while the lock is held: %v, want ErrMigrationLocked", err)
	}

	// WithoutLock is the documented escape, and it runs.
	free := mysql.NewMigrator(db).WithTable(table).
		WithoutLock().
		AddSQL("0001", "noop", "SELECT 1", "")
	if err := free.Up(ctx); err != nil {
		t.Fatalf("Up with the lock waived: %v", err)
	}
}
