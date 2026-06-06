package pg_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

func TestPIIArgRedactsAcrossVerbs(t *testing.T) {
	wrapped := pg.PII("alice@example.com")
	for _, verb := range []string{"%v", "%s", "%q", "%+v", "%#v"} {
		got := fmt.Sprintf(verb, wrapped)
		if strings.Contains(got, "alice") {
			t.Errorf("verb %s leaked PII value: %q", verb, got)
		}
		if !strings.Contains(got, "redacted") {
			t.Errorf("verb %s did not produce redaction marker, got: %q", verb, got)
		}
	}
}

// piiCapturingDriver captures the args it receives so the test
// can verify the driver got the plain value (not the marker).
type piiCapturingDriver struct {
	mu      sync.Mutex
	args    []any
	queries []string
}

func (d *piiCapturingDriver) Exec(_ context.Context, sql string, args ...any) (drops.Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.queries = append(d.queries, sql)
	d.args = append(d.args, args...)
	return nil, nil
}
func (d *piiCapturingDriver) Query(_ context.Context, sql string, args ...any) (drops.Rows, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.queries = append(d.queries, sql)
	d.args = append(d.args, args...)
	return &fakeRows{}, nil
}
func (d *piiCapturingDriver) Begin(_ context.Context) (drops.Tx, error) { return nil, nil }

func TestPIIUnwrappedBeforeDriver(t *testing.T) {
	drv := &piiCapturingDriver{}
	db := pg.New(drv)
	_, _ = db.Exec(context.Background(), "UPDATE users SET email = $1 WHERE id = $2",
		pg.PII("alice@example.com"), int64(7))
	if len(drv.args) != 2 {
		t.Fatalf("expected 2 args, got %d", len(drv.args))
	}
	if pg.IsPII(drv.args[0]) {
		t.Errorf("driver must receive unwrapped value, got marker")
	}
	if got, ok := drv.args[0].(string); !ok || got != "alice@example.com" {
		t.Errorf("driver arg[0]: %v", drv.args[0])
	}
}

func TestPIIHookSeesWrappedArgs(t *testing.T) {
	drv := &piiCapturingDriver{}
	var capturedFormatted string
	hook := func(_ context.Context, e drops.QueryEvent) {
		capturedFormatted = fmt.Sprintf("%v", e.Args)
	}
	db := pg.New(drv).WithHook(hook)
	_, _ = db.Exec(context.Background(), "UPDATE users SET email = $1 WHERE id = $2",
		pg.PII("alice@example.com"), int64(7))
	if capturedFormatted == "" {
		t.Fatal("hook never fired")
	}
	if strings.Contains(capturedFormatted, "alice") {
		t.Errorf("hook formatted args leaked PII: %q", capturedFormatted)
	}
	if !strings.Contains(capturedFormatted, "redacted") {
		t.Errorf("hook formatted args missing redaction marker: %q", capturedFormatted)
	}
}

func TestEntityPIIColumnWrappedInUpdate(t *testing.T) {
	type secretUser struct {
		ID    int64  `drop:"id,primaryKey,autoIncrement"`
		Email string `drop:"email,notNull,unique,pii"`
		Name  string `drop:"name,notNull"`
	}
	tbl := pg.AutoTable[secretUser]("users")
	ent := pg.NewEntity[secretUser](tbl)
	if !tbl.Col("email").IsPII() {
		t.Fatal("email column should be flagged PII")
	}

	drv := &piiCapturingDriver{}
	var lastArgs []any
	hook := func(_ context.Context, e drops.QueryEvent) { lastArgs = e.Args }
	db := pg.New(drv).WithHook(hook)

	u := secretUser{ID: 7, Email: "alice@example.com", Name: "Alice"}
	if err := ent.Update(db, context.Background(), &u); err != nil {
		// fakeRows from piiCapturingDriver doesn't supply data; we
		// only care about the wire path here.
		_ = err
	}
	if lastArgs == nil {
		t.Fatal("hook never fired")
	}
	// Email arg should be wrapped; name should not.
	foundWrapped := false
	for _, a := range lastArgs {
		if pg.IsPII(a) {
			foundWrapped = true
		}
		if s, ok := a.(string); ok && s == "alice@example.com" {
			t.Errorf("PII arg appeared unwrapped in hook payload: %v", lastArgs)
		}
	}
	if !foundWrapped {
		t.Errorf("expected at least one PII-wrapped arg, got %#v", lastArgs)
	}
}

func TestColumnAsPIISetsFlag(t *testing.T) {
	col := pg.Text("email").NotNull().AsPII()
	if !col.Column.IsPII() {
		t.Error("AsPII should mark the column")
	}
}
