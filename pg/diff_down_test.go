package pg_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bernardoforcillo/drops/pg"
)

// makeSchema returns a small two-table fixture used by the
// down-migration tests.
func makeSchema(t *testing.T) *pg.Schema {
	t.Helper()
	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	pg.Add(users, pg.Text("name").NotNull())

	posts := pg.NewTable("posts")
	pg.Add(posts, pg.BigSerial("id").PrimaryKey())
	uid := pg.Add(users, pg.BigSerial("id_dup").PrimaryKey()) // ensure import used
	_ = uid
	pg.Add(posts, pg.Text("title").NotNull())

	return pg.NewSchema(users, posts)
}

func TestDiffDownIsInverseOfDiff(t *testing.T) {
	prev := pg.EmptySnapshot()
	curSchema := makeSchema(t)
	cur := pg.BuildSnapshot(curSchema)

	up := pg.Diff(prev, cur)
	down := pg.DiffDown(prev, cur)

	if len(up) == 0 {
		t.Fatal("expected up statements")
	}
	if len(down) == 0 {
		t.Fatal("expected down statements")
	}
	// First up should be a CREATE TABLE, first down should be a DROP TABLE.
	if !strings.HasPrefix(up[0], "CREATE TABLE") {
		t.Errorf("up should start with CREATE TABLE, got: %s", up[0])
	}
	if !strings.HasPrefix(down[0], "DROP TABLE") {
		t.Errorf("down should start with DROP TABLE, got: %s", down[0])
	}
}

func TestDiffDownReversesAddColumn(t *testing.T) {
	prevTbl := pg.NewTable("users")
	pg.Add(prevTbl, pg.BigSerial("id").PrimaryKey())
	prev := pg.BuildSnapshot(pg.NewSchema(prevTbl))

	curTbl := pg.NewTable("users")
	pg.Add(curTbl, pg.BigSerial("id").PrimaryKey())
	pg.Add(curTbl, pg.Text("name").NotNull())
	cur := pg.BuildSnapshot(pg.NewSchema(curTbl))

	up := pg.Diff(prev, cur)
	down := pg.DiffDown(prev, cur)

	sawAdd := false
	for _, s := range up {
		if strings.Contains(s, "ADD COLUMN") {
			sawAdd = true
		}
	}
	sawDrop := false
	for _, s := range down {
		if strings.Contains(s, "DROP COLUMN") {
			sawDrop = true
		}
	}
	if !sawAdd {
		t.Errorf("up should contain ADD COLUMN: %v", up)
	}
	if !sawDrop {
		t.Errorf("down should contain DROP COLUMN: %v", down)
	}
}

func TestGenerateMigrationWithDownWritesPairedFile(t *testing.T) {
	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	pg.Add(users, pg.Text("email").NotNull().Unique())

	fsys := fstest.MapFS{}
	written := map[string][]byte{}

	res, err := pg.GenerateMigration(pg.GenerateOptions{
		Schema: pg.NewSchema(users),
		Dir:    "migrations",
		Name:   "init",
		FS:     fsys,
		Write: func(rel string, data []byte) error {
			written[rel] = data
			return nil
		},
		WithDown: true,
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	if res.NoOp {
		t.Fatal("expected diff")
	}
	if res.DownSQL == "" {
		t.Error("WithDown should populate DownSQL")
	}
	if _, ok := written["0000_init.down.sql"]; !ok {
		t.Errorf("expected 0000_init.down.sql to be written, got files: %v", written)
	}
	if _, ok := written["0000_init.sql"]; !ok {
		t.Errorf("expected 0000_init.sql to be written, got files: %v", written)
	}
}

func TestGenerateMigrationWithoutDownLeavesDownEmpty(t *testing.T) {
	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())

	fsys := fstest.MapFS{}
	written := map[string][]byte{}
	res, err := pg.GenerateMigration(pg.GenerateOptions{
		Schema: pg.NewSchema(users),
		Dir:    "migrations",
		Name:   "init",
		FS:     fsys,
		Write: func(rel string, data []byte) error {
			written[rel] = data
			return nil
		},
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	if res.DownSQL != "" {
		t.Errorf("DownSQL should be empty without WithDown, got %q", res.DownSQL)
	}
	if _, ok := written["0000_init.down.sql"]; ok {
		t.Error("down file should not be written without WithDown")
	}
}

func TestDiffDownOnEmptyDiffEmitsNothing(t *testing.T) {
	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	snap := pg.BuildSnapshot(pg.NewSchema(users))
	if got := pg.DiffDown(snap, snap); len(got) != 0 {
		t.Errorf("identical snapshots should produce empty down: %v", got)
	}
}

