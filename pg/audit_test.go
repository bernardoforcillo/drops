package pg_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// auditDriver tracks audit-related INSERTs separately from
// business-table operations so tests can assert on the audit
// payload.
type auditDriver struct {
	mu       sync.Mutex
	audits   []auditRow
	queries  []string
	returnID int64
}

type auditRow struct {
	entity  string
	op      string
	pk      json.RawMessage
	payload json.RawMessage
	actor   string
}

func (d *auditDriver) Exec(_ context.Context, sql string, args ...any) (drops.Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.queries = append(d.queries, sql)
	if strings.Contains(sql, "audit_events") {
		row := auditRow{
			entity: args[0].(string),
			op:     args[1].(string),
		}
		if v, ok := args[2].(json.RawMessage); ok {
			row.pk = v
		}
		if v, ok := args[3].(json.RawMessage); ok {
			row.payload = v
		}
		if s, ok := args[4].(string); ok {
			row.actor = s
		}
		d.audits = append(d.audits, row)
	}
	return idemResult{}, nil
}
func (d *auditDriver) Query(_ context.Context, sql string, _ ...any) (drops.Rows, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.queries = append(d.queries, sql)
	switch {
	case strings.HasPrefix(sql, "INSERT INTO \"users\""):
		// Return the inserted row so RETURNING captures it.
		return &fakeRows{
			cols: []string{"id", "name", "email"},
			data: [][]any{{d.returnID, "Alice", "a@x"}},
		}, nil
	case strings.HasPrefix(sql, "UPDATE \"users\""):
		return &fakeRows{
			cols: []string{"id", "name", "email"},
			data: [][]any{{int64(7), "Alice-new", "a@x"}},
		}, nil
	}
	return &fakeRows{}, nil
}
func (d *auditDriver) Begin(_ context.Context) (drops.Tx, error) { return &auditTx{drv: d}, nil }

type auditTx struct{ drv *auditDriver }

func (tx *auditTx) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	return tx.drv.Exec(ctx, sql, args...)
}
func (tx *auditTx) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	return tx.drv.Query(ctx, sql, args...)
}
func (tx *auditTx) Begin(ctx context.Context) (drops.Tx, error)  { return tx.drv.Begin(ctx) }
func (tx *auditTx) Commit(_ context.Context) error               { return nil }
func (tx *auditTx) Rollback(_ context.Context) error             { return nil }

func TestAuditOnCreate(t *testing.T) {
	_, ent := entUsersSchema()
	drv := &auditDriver{returnID: 7}
	db := pg.New(drv)
	audit := pg.NewAuditLog(db, "audit_events")
	pg.WithAudit(ent, audit)

	ctx := pg.WithActor(context.Background(), "admin-9")
	u := entUser{Name: "Alice", Email: "a@x"}
	if err := ent.Create(db, ctx, &u); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(drv.audits) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(drv.audits))
	}
	ev := drv.audits[0]
	if ev.op != "create" || ev.entity != "users" || ev.actor != "admin-9" {
		t.Errorf("audit event: %+v", ev)
	}
	if !strings.Contains(string(ev.payload), `"Alice"`) {
		t.Errorf("payload should carry the new row: %s", ev.payload)
	}
}

func TestAuditOnUpdate(t *testing.T) {
	_, ent := entUsersSchema()
	drv := &auditDriver{}
	db := pg.New(drv)
	audit := pg.NewAuditLog(db, "audit_events")
	pg.WithAudit(ent, audit)

	ctx := pg.WithActor(context.Background(), int64(42))
	u := entUser{ID: 7, Name: "Alice", Email: "a@x"}
	if err := ent.Update(db, ctx, &u); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(drv.audits) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(drv.audits))
	}
	if drv.audits[0].op != "update" || drv.audits[0].actor != "42" {
		t.Errorf("audit event: %+v", drv.audits[0])
	}
}

func TestAuditOnDelete(t *testing.T) {
	_, ent := entUsersSchema()
	drv := &auditDriver{}
	db := pg.New(drv)
	audit := pg.NewAuditLog(db, "audit_events")
	pg.WithAudit(ent, audit)

	if _, err := ent.Delete(db, context.Background(), int64(7)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(drv.audits) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(drv.audits))
	}
	if drv.audits[0].op != "delete" {
		t.Errorf("op: %s", drv.audits[0].op)
	}
}

func TestAuditDisabledIsZeroCost(t *testing.T) {
	_, ent := entUsersSchema()
	drv := &auditDriver{returnID: 5}
	db := pg.New(drv)
	u := entUser{Name: "Bob", Email: "b@x"}
	if err := ent.Create(db, context.Background(), &u); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(drv.audits) != 0 {
		t.Errorf("audit should NOT fire when not attached, got %d events", len(drv.audits))
	}
}

func TestAuditTableDDL(t *testing.T) {
	tbl := pg.NewAuditTable("audit_events")
	for _, c := range []string{"id", "entity", "op", "pk", "payload", "actor", "createdAt"} {
		if tbl.Col(c) == nil {
			t.Errorf("audit table missing %q", c)
		}
	}
	if !tbl.Col("id").IsPrimaryKey() {
		t.Error("id should be PK")
	}
}

func TestActorContextRoundTrip(t *testing.T) {
	ctx := pg.WithActor(context.Background(), "u-1")
	if pg.ActorFrom(ctx) != "u-1" {
		t.Errorf("actor: %s", pg.ActorFrom(ctx))
	}
	if pg.ActorFrom(context.Background()) != "" {
		t.Error("missing actor should return empty string")
	}
}
