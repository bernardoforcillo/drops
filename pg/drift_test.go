package pg_test

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/pg"
)

func TestDriftReportInSyncWhenSchemasMatch(t *testing.T) {
	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	pg.Add(users, pg.Text("name").NotNull())

	a := pg.BuildSnapshot(pg.NewSchema(users))

	usersB := pg.NewTable("users")
	pg.Add(usersB, pg.BigSerial("id").PrimaryKey())
	pg.Add(usersB, pg.Text("name").NotNull())
	b := pg.BuildSnapshot(pg.NewSchema(usersB))

	report := pg.DetectDrift(a, b)
	if !report.InSync {
		t.Errorf("expected InSync, got pending=%v unauthorized=%v",
			report.PendingMigrations, report.UnauthorizedChanges)
	}
}

func TestDriftReportPendingWhenRepoIsAhead(t *testing.T) {
	// Repo declares a new column the live schema doesn't have.
	live := pg.NewTable("users")
	pg.Add(live, pg.BigSerial("id").PrimaryKey())
	repoTbl := pg.NewTable("users")
	pg.Add(repoTbl, pg.BigSerial("id").PrimaryKey())
	pg.Add(repoTbl, pg.Text("email").NotNull())

	report := pg.DetectDrift(
		pg.BuildSnapshot(pg.NewSchema(repoTbl)),
		pg.BuildSnapshot(pg.NewSchema(live)),
	)

	if !report.HasPendingMigrations() {
		t.Errorf("expected pending migrations, got none")
	}
	if report.InSync {
		t.Error("InSync should be false when repo is ahead")
	}
	joined := strings.Join(report.PendingMigrations, "\n")
	if !strings.Contains(joined, "ADD COLUMN") {
		t.Errorf("expected ADD COLUMN in pending: %s", joined)
	}
}

func TestDriftReportUnauthorizedWhenLiveDiverges(t *testing.T) {
	// Live has a column the repo doesn't — someone applied manual DDL.
	live := pg.NewTable("users")
	pg.Add(live, pg.BigSerial("id").PrimaryKey())
	pg.Add(live, pg.Text("hotfixColumn"))

	repoTbl := pg.NewTable("users")
	pg.Add(repoTbl, pg.BigSerial("id").PrimaryKey())

	report := pg.DetectDrift(
		pg.BuildSnapshot(pg.NewSchema(repoTbl)),
		pg.BuildSnapshot(pg.NewSchema(live)),
	)

	if !report.HasUnauthorizedChanges() {
		t.Errorf("expected unauthorized changes, got none")
	}
	joined := strings.Join(report.UnauthorizedChanges, "\n")
	if !strings.Contains(joined, "hotfixColumn") {
		t.Errorf("expected hotfixColumn in unauthorized: %s", joined)
	}
}

func TestDriftReportHandlesNilSnapshots(t *testing.T) {
	// Both nil → trivially in sync.
	if r := pg.DetectDrift(nil, nil); !r.InSync {
		t.Errorf("both nil should be InSync, got %+v", r)
	}

	// Repo non-empty vs live nil → everything is pending.
	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	repo := pg.BuildSnapshot(pg.NewSchema(users))

	r := pg.DetectDrift(repo, nil)
	if !r.HasPendingMigrations() {
		t.Error("non-empty repo vs nil live should have pending migrations")
	}
}
