package sqlite_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/sqlite"
)

// --- PII --------------------------------------------------------------

func TestPIIRedaction(t *testing.T) {
	drv := &entDriver{}
	var hookArgs []any
	db := sqlite.New(drv).WithHook(func(_ context.Context, e drops.QueryEvent) {
		if e.Kind == "exec" {
			hookArgs = e.Args
		}
	})
	_, err := db.Exec(context.Background(), `UPDATE users SET pw = ? WHERE id = ?`,
		sqlite.PII("s3cret"), int64(1))
	if err != nil {
		t.Fatal(err)
	}
	// Driver receives the plain value.
	if drv.args[0][0] != "s3cret" {
		t.Errorf("driver should get plain value, got %v", drv.args[0][0])
	}
	// Hook formatting redacts it.
	if got := fmt.Sprintf("%v", hookArgs[0]); got != "<redacted>" {
		t.Errorf("hook arg not redacted: %q", got)
	}
	if !sqlite.IsPII(hookArgs[0]) {
		t.Error("hook arg should carry the PII marker")
	}
}

type piiUser struct {
	ID    int64  `drop:"id"`
	Email string `drop:"email"`
}

func TestPIIColumnBinding(t *testing.T) {
	tbl := sqlite.NewTable("users")
	sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(tbl, sqlite.Text("email").NotNull().AsPII())
	ent := sqlite.NewEntity[piiUser](tbl)

	drv := &entDriver{}
	var hookArgs []any
	db := sqlite.New(drv).WithHook(func(_ context.Context, e drops.QueryEvent) {
		if e.Kind == "exec" {
			hookArgs = e.Args
		}
	})
	if err := ent.Create(db, context.Background(), &piiUser{ID: 1, Email: "a@b.com"}); err != nil {
		t.Fatal(err)
	}
	// The email arg is redacted in the hook but plain to the driver.
	joined := fmt.Sprintf("%v", hookArgs)
	if !strings.Contains(joined, "<redacted>") {
		t.Errorf("email not redacted in hook: %v", hookArgs)
	}
	plain := fmt.Sprintf("%v", drv.args[0])
	if !strings.Contains(plain, "a@b.com") {
		t.Errorf("driver should see plain email: %v", drv.args[0])
	}
}

// --- AutoTable --------------------------------------------------------

type autoRow struct {
	ID        int64           `drop:"id,primaryKey,autoIncrement"`
	Name      string          `drop:"name,notNull,unique"`
	Age       int32           `drop:"age"`
	Score     float64         `drop:"score,default=0"`
	Active    bool            `drop:"active"`
	Meta      json.RawMessage `drop:"meta"`
	Blob      []byte          `drop:"blob"`
	CreatedAt time.Time       `drop:"createdAt,notNull"`
	Balance   sqlite.Money    `drop:"balance"`
	Ignored   string          `drop:"-"`
	Untagged  string
}

func TestAutoTableDDL(t *testing.T) {
	tbl := sqlite.AutoTable[autoRow]("things")
	sql, _ := sqlite.ToSQL(sqlite.CreateTable(tbl))
	// Every column states its nullability, and the field's type is
	// the statement: autoRow has no pointer field, so every column
	// is NOT NULL. A struct that meant otherwise says so with a
	// pointer.
	for _, want := range []string{
		`"id" INTEGER PRIMARY KEY AUTOINCREMENT`,
		`"name" TEXT NOT NULL UNIQUE`,
		`"age" INTEGER NOT NULL`,
		`"score" REAL NOT NULL DEFAULT 0`,
		`"active" BOOLEAN NOT NULL`,
		`"meta" TEXT NOT NULL`,
		`"blob" BLOB NOT NULL`,
		`"createdAt" DATETIME NOT NULL`,
		`"balance" INTEGER NOT NULL`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing %q in:\n%s", want, sql)
		}
	}
	// Skipped and untagged fields must not appear.
	if strings.Contains(sql, "Ignored") || strings.Contains(sql, "Untagged") {
		t.Errorf("skipped field leaked:\n%s", sql)
	}
}

func TestNewAutoEntity(t *testing.T) {
	ent := sqlite.NewAutoEntity[autoRow]("things")
	if ent.Table().Name() != "things" {
		t.Errorf("table name: %s", ent.Table().Name())
	}
	if ent.PK().Name() != "id" {
		t.Errorf("PK: %s", ent.PK().Name())
	}
}

// --- Drift ------------------------------------------------------------

func TestDetectDrift(t *testing.T) {
	tbl := sqlite.NewTable("t")
	sqlite.Add(tbl, sqlite.Integer("id").PrimaryKey())
	repo := sqlite.BuildSnapshot(sqlite.NewSchema(tbl))

	// Live is empty → production is behind the repo.
	rep := sqlite.DetectDrift(repo, sqlite.EmptySnapshot())
	if rep.InSync || !rep.HasPendingMigrations() {
		t.Errorf("expected pending migrations, got %+v", rep)
	}

	// Identical snapshots → in sync.
	same := sqlite.DetectDrift(repo, repo)
	if !same.InSync || same.HasPendingMigrations() || same.HasUnauthorizedChanges() {
		t.Errorf("expected in-sync, got %+v", same)
	}
}
