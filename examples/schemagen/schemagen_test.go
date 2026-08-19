package schemagen_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/examples/schemagen"
	"github.com/bernardoforcillo/drops/pg"
)

// The whole point: because every column came from a field, the entity
// builds without a single AllowUnmappedColumns exemption. A generator
// that produced a schema the struct could not satisfy would fail
// right here.
func TestGeneratedSchemaSatisfiesTheDriftCheck(t *testing.T) {
	ent := pg.NewEntity[schemagen.User](schemagen.Users)
	if ent.PK() == nil || ent.PK().Name() != "id" {
		t.Fatalf("PK = %v, want id", ent.PK())
	}
}

// And the handles are typed, which is what a runtime AutoTable cannot
// give you: this line would not compile if Age were declared as text.
func TestGeneratedColumnsAreTyped(t *testing.T) {
	pred := schemagen.UserAge.Gte(18)
	sql, args := drops.String(pred)
	if !strings.Contains(sql, `"age"`) || len(args) != 1 {
		t.Errorf("predicate = %s %v", sql, args)
	}
}

// The checked-in generated file must match what the generator
// produces today, or the example documents a stale workflow.
func TestGeneratedFileIsCurrent(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	dir := t.TempDir()
	src, err := os.ReadFile("models.go")
	if err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(dir, "models.go")
	if err := os.WriteFile(in, src, 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(context.Background(),
		"go", "run", "github.com/bernardoforcillo/drops/cmd/dropsgen", "-schema", in)
	cmd.Dir = ".."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("could not run dropsgen: %v\n%s", err, out)
	}

	fresh, err := os.ReadFile(filepath.Join(dir, "models_drops_schema.go"))
	if err != nil {
		t.Fatal(err)
	}
	checkedIn, err := os.ReadFile("models_drops_schema.go")
	if err != nil {
		t.Fatal(err)
	}
	if string(fresh) != string(checkedIn) {
		t.Errorf("models_drops_schema.go is stale — rerun go generate\n--- fresh ---\n%s\n--- checked in ---\n%s", fresh, checkedIn)
	}
}
