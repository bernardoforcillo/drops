package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

// AuditLog records who-changed-what-when for every Create / Update /
// Delete on the entities attached to it. The audit row is written in the
// SAME transaction as the business mutation, so a rollback rolls back
// both.
//
//	audit := sqlite.NewAuditLog(db, "audit_events")
//	sqlite.WithAudit(UserEntity, audit)
//	ctx = sqlite.WithActor(ctx, currentUserID)
//	UserEntity.Update(db, ctx, &u) // also writes an audit row
type AuditLog struct {
	db    *DB
	table string
}

// AuditEvent is the row written per mutation.
type AuditEvent struct {
	Entity  string
	Op      string
	PK      json.RawMessage
	Payload json.RawMessage
	Actor   string
}

// NewAuditLog binds the log to db and a destination table.
func NewAuditLog(db *DB, table string) *AuditLog {
	if table == "" {
		table = "audit_events"
	}
	return &AuditLog{db: db, table: table}
}

// NewAuditTable declares the canonical audit table.
func NewAuditTable(name string) *Table {
	t := NewTable(name)
	Add(t, BigSerial("id").PrimaryKey())
	Add(t, Text("entity").NotNull())
	Add(t, Text("op").NotNull())
	Add(t, JSON("pk"))
	Add(t, JSON("payload"))
	Add(t, Text("actor"))
	Add(t, Timestamp("createdAt", false).NotNull().Default("CURRENT_TIMESTAMP"))
	return t
}

// Record inserts ev using tx so the audit row lives or dies with the
// surrounding transaction.
func (a *AuditLog) Record(tx *DB, ctx context.Context, ev AuditEvent) error {
	sql := fmt.Sprintf(
		`INSERT INTO %s ("entity", "op", "pk", "payload", "actor") VALUES (?, ?, ?, ?, ?)`,
		quoteIdent(a.table))
	_, err := tx.Exec(ctx, sql, ev.Entity, ev.Op, ev.PK, ev.Payload, ev.Actor)
	return err
}

type actorCtxKey int

const actorKey actorCtxKey = 1

// WithActor annotates ctx with an actor identifier.
func WithActor(ctx context.Context, actor any) context.Context {
	return context.WithValue(ctx, actorKey, actor)
}

// ActorFrom returns the actor stored on ctx, or "" when absent.
func ActorFrom(ctx context.Context) string {
	v := ctx.Value(actorKey)
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

// WithAudit attaches log to the entity so subsequent Create / Update /
// Delete record audit events in the same transaction.
func WithAudit[T any](e *Entity[T], log *AuditLog) *Entity[T] {
	e.audit = &auditWiring{log: log}
	return e
}

// auditWiring is the type-erased handle stored on Entity[T].
type auditWiring struct {
	log *AuditLog
}

// recordAudit writes an audit event for op when auditing is enabled.
func (e *Entity[T]) recordAudit(tx *DB, ctx context.Context, op string, row *T, pkv any) error {
	if e.audit == nil {
		return nil
	}
	pkJSON, err := json.Marshal(pkv)
	if err != nil {
		return err
	}
	var payload json.RawMessage
	if row != nil {
		raw, err := json.Marshal(row)
		if err != nil {
			return err
		}
		payload = raw
	}
	return e.audit.log.Record(tx, ctx, AuditEvent{
		Entity:  e.table.Name(),
		Op:      op,
		PK:      pkJSON,
		Payload: payload,
		Actor:   ActorFrom(ctx),
	})
}

// pkValue returns r's primary-key field via reflection.
func (e *Entity[T]) pkValue(r *T) any {
	if e.pkField == nil {
		return nil
	}
	return reflect.ValueOf(r).Elem().FieldByIndex(e.pkField).Interface()
}

// ErrAuditTableMissing is returned when an audit operation fails because
// the configured table does not exist.
var ErrAuditTableMissing = errors.New("drops/sqlite: audit table not present; create it via NewAuditTable + migration")
