package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/mysql"
)

// DetectDrift against a live server: the two questions a deploy gate
// asks, and the difference between them.
//
// PendingMigrations is "production is behind the repo".
// UnauthorizedChanges is "somebody changed production by hand". They
// are two directions of the same Diff and a gate usually treats them
// very differently, which is why they are separate fields rather than
// one boolean.
func TestMySQLDetectDriftSeesBothDirections(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()

	name := integration.UniqueName(t, "drift_users")
	tbl := mysql.NewTable(name)
	mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	mysql.Add(tbl, mysql.Varchar("name", 255).NotNull())
	dropMySQL(t, db, tbl)
	execMySQL(t, db, mysql.CreateTable(tbl))

	repo := mysql.BuildSnapshot(mysql.NewSchema(tbl))
	live, err := onlyTable(t, db, name)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}

	// In sync: the repo declares what the server has.
	if r := mysql.DetectDrift(repo, live); !r.InSync {
		t.Fatalf("a freshly created table reads as drifted:\npending = %v\nunauthorised = %v",
			r.PendingMigrations, r.UnauthorizedChanges)
	}

	// Somebody adds a column by hand. That is an unauthorised change,
	// and it is NOT a pending migration — the repo is not behind.
	if _, err := db.Exec(ctx, "ALTER TABLE `"+name+"` ADD COLUMN nickname VARCHAR(255)"); err != nil {
		t.Fatalf("hand edit: %v", err)
	}
	live, err = onlyTable(t, db, name)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	r := mysql.DetectDrift(repo, live)
	if r.InSync {
		t.Fatal("a column added by hand did not register as drift")
	}
	if !r.HasPendingMigrations() {
		t.Error("the repo is behind by the DROP that would remove the hand-added column")
	}
	if !r.HasUnauthorizedChanges() {
		t.Error("the hand-added column is not reported as an unrecorded change")
	}
	if !strings.Contains(strings.Join(r.UnauthorizedChanges, "\n"), "nickname") {
		t.Errorf("the unauthorised change does not name the column: %v", r.UnauthorizedChanges)
	}

	// And once the repo declares it, the two agree again.
	mysql.Add(tbl, mysql.Varchar("nickname", 255).Nullable())
	repo = mysql.BuildSnapshot(mysql.NewSchema(tbl))
	if r := mysql.DetectDrift(repo, live); !r.InSync {
		t.Errorf("declaring the column did not settle the drift:\npending = %v\nunauthorised = %v",
			r.PendingMigrations, r.UnauthorizedChanges)
	}
}

// A nil snapshot is an empty one rather than a panic, because the
// fresh-database case is ordinary. What that case reveals is a trap in
// the field names, and it is pinned here rather than left to a deploy
// gate to discover.
//
// The two fields are two directions of ONE diff. Against an empty
// database, "pending" is the CREATE TABLE and "unauthorised" is the
// DROP TABLE that would bring the repo down to what the server has.
// The second is not a report that somebody edited production — it is
// arithmetic. A gate that gets paged on HasUnauthorizedChanges alone
// will be paged by every fresh database it ever meets; the signal it
// wants is the one where UnauthorizedChanges is non-empty and the
// tables on both sides exist.
func TestMySQLDetectDriftAgainstAnEmptyDatabaseIsNonEmptyBothWays(t *testing.T) {
	tbl := mysql.NewTable("drift_nil_users")
	mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	repo := mysql.BuildSnapshot(mysql.NewSchema(tbl))

	r := mysql.DetectDrift(repo, nil)
	if r.InSync {
		t.Error("a repo schema against an empty database reads as in sync")
	}
	if !r.HasPendingMigrations() {
		t.Error("the CREATE TABLE is not reported as pending")
	}
	if !strings.Contains(strings.Join(r.PendingMigrations, "\n"), "CREATE TABLE") {
		t.Errorf("pending does not carry the CREATE: %v", r.PendingMigrations)
	}
	// The other direction, which is the part that misleads.
	if !strings.Contains(strings.Join(r.UnauthorizedChanges, "\n"), "DROP TABLE") {
		t.Errorf("the reverse direction does not carry the DROP: %v", r.UnauthorizedChanges)
	}

	// Both empty against itself, which is the only shape InSync has.
	if r := mysql.DetectDrift(repo, repo); !r.InSync {
		t.Errorf("a snapshot drifted from itself: %+v", r)
	}
	if r := mysql.DetectDrift(nil, nil); !r.InSync {
		t.Errorf("two empty snapshots drifted: %+v", r)
	}
}

// onlyTable introspects the live database and keeps one table.
//
// mysql.Introspect reads the whole database — it has no per-table
// option — and this suite shares one with every other MySQL test, so an
// unfiltered snapshot would report every other test's tables as
// unauthorised changes. Narrowing here keeps the assertion about the
// table under test, which is the same thing Push does with ownedBy for
// the same reason.
func onlyTable(t *testing.T, db *mysql.DB, name string) (*mysql.Snapshot, error) {
	t.Helper()
	full, err := mysql.Introspect(context.Background(), db)
	if err != nil {
		return nil, err
	}
	out := mysql.EmptySnapshot()
	for key, tbl := range full.Tables {
		if tbl.Name == name {
			out.Tables[key] = tbl
		}
	}
	return out, nil
}
