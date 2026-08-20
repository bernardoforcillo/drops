package pg_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// gateRan reports whether any statement the analyser would have graded
// reached the driver. The point of a gate is that the answer is no.
func gateRan(fd *fakeDriver, fragment string) bool {
	for _, q := range fd.queries {
		if strings.Contains(q, fragment) {
			return true
		}
	}
	return false
}

func TestMigratorSafetyGateWithholdsWhatTheAnalyserGrades(t *testing.T) {
	const dropTable = `DROP TABLE "users";`
	const dropColumn = `ALTER TABLE "users" DROP COLUMN "nickname";`
	cases := []struct {
		name     string
		gate     bool
		min      pg.SafetySeverity
		sql      string
		wantRule string // empty means the migration is expected to run
	}{
		{
			name: "no gate, no opinion",
			sql:  dropTable,
		},
		{
			name:     "error gate stops a DROP TABLE",
			gate:     true,
			min:      pg.SeverityError,
			sql:      dropTable,
			wantRule: "drop-table",
		},
		{
			name: "error gate lets a warn-level change through",
			gate: true,
			min:  pg.SeverityError,
			sql:  dropColumn,
		},
		{
			name:     "warn gate stops the same change",
			gate:     true,
			min:      pg.SeverityWarn,
			sql:      dropColumn,
			wantRule: "drop-column",
		},
		{
			name: "a safe migration passes any gate",
			gate: true,
			min:  pg.SeverityInfo,
			sql:  `ALTER TABLE "users" ADD COLUMN "nickname" text;`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, fd := migratorFakeDB()
			m := pg.NewMigrator(db)
			if tc.gate {
				m.WithSafetyGate(tc.min)
			}
			m.AddSQL("0001", "change", tc.sql, "")

			err := m.Up(context.Background())
			if tc.wantRule == "" {
				if err != nil {
					t.Fatalf("Up: %v", err)
				}
				if !gateRan(fd, tc.sql) {
					t.Errorf("the migration never ran: %v", fd.queries)
				}
				return
			}
			if !errors.Is(err, pg.ErrUnsafeMigration) {
				t.Fatalf("err = %v, want ErrUnsafeMigration", err)
			}
			var unsafe *pg.UnsafeMigrationError
			if !errors.As(err, &unsafe) {
				t.Fatalf("err = %v, want an *pg.UnsafeMigrationError", err)
			}
			if unsafe.Migration != "0001_change" {
				t.Errorf("Migration = %v, want 0001_change", unsafe.Migration)
			}
			if len(unsafe.Warnings) == 0 || unsafe.Warnings[0].Rule != tc.wantRule {
				t.Errorf("Warnings = %+v, want one %v", unsafe.Warnings, tc.wantRule)
			}
			if gateRan(fd, tc.sql) {
				t.Errorf("the withheld statement ran anyway: %v", fd.queries)
			}
		})
	}
}

func TestMigratorSafetyGateChecksTheWholeRunFirst(t *testing.T) {
	db, fd := migratorFakeDB()
	m := pg.NewMigrator(db).WithSafetyGate(pg.SeverityError)
	m.AddSQL("0001", "safe", `ALTER TABLE "users" ADD COLUMN "nickname" text;`, "")
	m.AddSQL("0002", "unsafe", `DROP TABLE "users";`, "")

	if err := m.Up(context.Background()); !errors.Is(err, pg.ErrUnsafeMigration) {
		t.Fatalf("err = %v, want ErrUnsafeMigration", err)
	}
	if gateRan(fd, "ADD COLUMN") {
		t.Error("0001 was applied before 0002 was refused; the run is checked before any of it is applied")
	}
}

func TestMigratorSafetyGateIgnoresAMigrationWithNoSQLToRead(t *testing.T) {
	db, _ := migratorFakeDB()
	ran := false
	m := pg.NewMigrator(db).WithSafetyGate(pg.SeverityInfo)
	m.Add(pg.Migration{Version: "0001", Name: "programmatic", Up: func(context.Context, *pg.DB) error {
		ran = true
		return nil
	}})

	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if !ran {
		t.Error("a migration registered as a Go function was refused; there is no text to analyse")
	}
}

func TestMigratorSafetyGateReadsTheDownDirectionToo(t *testing.T) {
	db, fd := migratorFakeDB("0001")
	m := pg.NewMigrator(db).WithSafetyGate(pg.SeverityError)
	m.AddSQL("0001", "change", `ALTER TABLE "users" ADD COLUMN "nickname" text;`, `DROP TABLE "users";`)

	if err := m.Down(context.Background()); !errors.Is(err, pg.ErrUnsafeMigration) {
		t.Fatalf("err = %v, want ErrUnsafeMigration", err)
	}
	if gateRan(fd, "DROP TABLE") {
		t.Errorf("the withheld rollback ran anyway: %v", fd.queries)
	}
}

func TestDrizzleMigratorSafetyGateWithholdsTheFile(t *testing.T) {
	fsys := fstest.MapFS{
		"drizzle/meta/_journal.json": {Data: []byte(`{
			"version": "7", "dialect": "postgresql",
			"entries": [
				{"idx": 0, "version": "7", "when": 1, "tag": "0000_safe",   "breakpoints": true},
				{"idx": 1, "version": "7", "when": 2, "tag": "0001_unsafe", "breakpoints": true}
			]
		}`)},
		"drizzle/0000_safe.sql":   {Data: []byte(`ALTER TABLE "users" ADD COLUMN "nickname" text;`)},
		"drizzle/0001_unsafe.sql": {Data: []byte(`DROP TABLE "users";`)},
	}
	db, fd, applied := drizzleFakeDB()

	err := pg.NewDrizzleMigrator(db, fsys, "drizzle").
		WithSafetyGate(pg.SeverityError).
		Up(context.Background())
	if !errors.Is(err, pg.ErrUnsafeMigration) {
		t.Fatalf("err = %v, want ErrUnsafeMigration", err)
	}
	var unsafe *pg.UnsafeMigrationError
	if !errors.As(err, &unsafe) || unsafe.Migration != "0001_unsafe" {
		t.Fatalf("err = %v, want it to name 0001_unsafe", err)
	}
	if len(applied) != 0 {
		t.Errorf("applied = %v, want nothing recorded", applied)
	}
	if gateRan(fd, "ADD COLUMN") {
		t.Error("the safe file ahead of the unsafe one was applied; the run is checked first")
	}
}

// treeFakeDB drives TreeMigrator with an empty node table, so every
// registered migration is pending.
func treeFakeDB(applied ...string) (*pg.DB, *fakeDriver) {
	fd := &fakeDriver{
		handler: func(sql string, _ []any) (drops.Rows, error) {
			if strings.Contains(sql, "SELECT id, checksum, applied_at") {
				data := make([][]any, 0, len(applied))
				for _, id := range applied {
					data = append(data, []any{id, "", time.Time{}})
				}
				return &fakeRows{cols: []string{"id", "checksum", "applied_at"}, data: data}, nil
			}
			return &fakeRows{}, nil
		},
	}
	return pg.New(fd), fd
}

func TestTreeMigratorSafetyGateWithholdsTheNode(t *testing.T) {
	t.Run("applying", func(t *testing.T) {
		db, fd := treeFakeDB()
		m := pg.NewTreeMigrator(db).WithSafetyGate(pg.SeverityError)
		m.AddSQL("n1", "drop users", "main", nil, `DROP TABLE "users";`, "")

		err := m.Up(context.Background())
		if !errors.Is(err, pg.ErrUnsafeMigration) {
			t.Fatalf("err = %v, want ErrUnsafeMigration", err)
		}
		if gateRan(fd, "DROP TABLE") {
			t.Errorf("the withheld statement ran anyway: %v", fd.queries)
		}
	})
	t.Run("rolling back", func(t *testing.T) {
		db, fd := treeFakeDB("n1")
		m := pg.NewTreeMigrator(db).WithSafetyGate(pg.SeverityError)
		m.AddSQL("n1", "add nickname", "main", nil,
			`ALTER TABLE "users" ADD COLUMN "nickname" text;`, `DROP TABLE "users";`)

		err := m.Down(context.Background())
		if !errors.Is(err, pg.ErrUnsafeMigration) {
			t.Fatalf("err = %v, want ErrUnsafeMigration", err)
		}
		if gateRan(fd, "DROP TABLE") {
			t.Errorf("the withheld rollback ran anyway: %v", fd.queries)
		}
	})
}
