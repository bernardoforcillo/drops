package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

// AuditLog records who-changed-what-when for every Create / Update
// / Delete on the entities attached to it. The audit row is written
// in the SAME transaction as the business mutation, so a rollback
// rolls back both — required for any compliance regime where audit
// trails must be authoritative.
//
//	audit := pg.NewAuditLog(db, "audit_events")
//	pg.WithAudit(UserEntity, audit)
//	pg.WithAudit(PostEntity, audit)
//
// The actor is read from ctx via pg.WithActor:
//
//	ctx = pg.WithActor(ctx, currentUserID)
//	UserEntity.Update(db, ctx, &u)
//	// inserts an audit row: (entity=users, op=update,
//	//                       pk=42, payload={...},
//	//                       actor=currentUserID, createdAt=now())
type AuditLog struct {
	db    *DB
	table string
}

// AuditEvent is the row written per mutation.
type AuditEvent struct {
	Entity  string          // table name
	Op      string          // "create" | "update" | "delete"
	PK      json.RawMessage // pk value, JSON-encoded
	Payload json.RawMessage // row snapshot (Create / Update only)
	Actor   string          // from ctx via WithActor, "" if absent
}

// NewAuditLog binds the log to db and a destination table. Pair
// with NewAuditTable() to provision matching DDL.
func NewAuditLog(db *DB, table string) *AuditLog {
	if table == "" {
		table = "audit_events"
	}
	return &AuditLog{db: db, table: table}
}

// NewAuditTable declares the canonical audit table:
//
//	id        bigserial PRIMARY KEY,
//	entity    text       NOT NULL,
//	op        text       NOT NULL,
//	pk        jsonb,
//	payload   jsonb,
//	actor     text,
//	createdAt timestamptz NOT NULL DEFAULT now()
func NewAuditTable(name string) *Table {
	t := NewTable(name)
	Add(t, BigSerial("id").PrimaryKey())
	Add(t, Text("entity").NotNull())
	Add(t, Text("op").NotNull())
	Add(t, JSONB("pk"))
	Add(t, JSONB("payload"))
	Add(t, Text("actor"))
	Add(t, Timestamp("createdAt", true).NotNull().Default("now()"))
	return t
}

// Record inserts ev using tx so the audit row lives or dies with
// the surrounding transaction.
func (a *AuditLog) Record(tx *DB, ctx context.Context, ev AuditEvent) error {
	sql := fmt.Sprintf(
		`INSERT INTO %q ("entity", "op", "pk", "payload", "actor") VALUES ($1, $2, $3, $4, $5)`,
		a.table,
	)
	_, err := tx.Exec(ctx, sql, ev.Entity, ev.Op, ev.PK, ev.Payload, ev.Actor)
	return err
}

// ----------------------------------------------------------------------
// Actor context
// ----------------------------------------------------------------------

type actorCtxKey int

const actorKey actorCtxKey = 1

// WithActor annotates ctx with an actor identifier. Pass anything
// that fmt prints sensibly (string id, int64 user id, struct
// implementing Stringer).
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

// ----------------------------------------------------------------------
// Entity wiring
// ----------------------------------------------------------------------

// WithAudit attaches log to the entity so subsequent Create /
// Update / Delete calls record audit events in the same
// transaction. Free function because Go does not allow generic
// methods.
func WithAudit[T any](e *Entity[T], log *AuditLog) *Entity[T] {
	e.audit = &auditWiring{log: log}
	return e
}

// auditWiring is the type-erased handle stored on Entity[T].
// Keeping it small means Entity stays the same shape regardless
// of whether the user opts in.
type auditWiring struct {
	log *AuditLog
}

// recordCreate / recordUpdate / recordDelete are the helpers the
// Entity methods call.
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

// pkValue returns r's primary-key field via reflection. Used by
// audit + tenant scopes that need the PK without going through the
// builder.
func (e *Entity[T]) pkValue(r *T) any {
	if e.pkField == nil {
		return nil
	}
	return reflect.ValueOf(r).Elem().FieldByIndex(e.pkField).Interface()
}

// ErrAuditTableMissing is returned when an audit operation fails
// because the configured table does not exist. Surfaces a clearer
// error than the raw "relation does not exist".
var ErrAuditTableMissing = errors.New("drops/pg: audit table not present; create it via NewAuditTable + migration")
