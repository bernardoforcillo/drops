package pg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
		if raw, err = e.redactPII(raw); err != nil {
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

// redactPII replaces the value of every column declared [Col.AsPII] in
// the marshalled row snapshot.
//
// A column flagged PII is redacted in the query log, in a tracer's
// attributes and in any accidental fmt call — that is what the flag
// promised, end to end. The audit trail was the hole in it: the payload
// is json.Marshal of the whole struct, so the value the log would not
// print was written verbatim into a table that outlives the log by
// design. An audit trail is kept for years, is read by more people than
// the logs are, and is frequently the one thing exported to a system
// with different access rules.
//
// The rewrite is on the marshalled JSON rather than on the struct,
// because the payload's SHAPE is what an existing consumer parses:
// keys, nesting and every non-PII value come out byte-identical to
// what they were, and only the flagged values change. Rebuilding the
// object from the entity's columns instead would have re-keyed it by
// column name, which is a different document.
//
// A row with no PII column allocates nothing and returns the original
// bytes — the case every entity that never called AsPII is in.
func (e *Entity[T]) redactPII(raw json.RawMessage) (json.RawMessage, error) {
	keys := e.piiJSONKeys()
	if len(keys) == 0 {
		return raw, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		// A T that marshals to something other than an object — a
		// type with its own MarshalJSON answering with an array or a
		// string. Nothing here can find a field in it to redact, and
		// storing a payload this cannot vouch for is worse than
		// storing none: the flag's promise is that the value is not
		// there.
		return nil, fmt.Errorf("drops/pg: audit payload for %q carries PII columns and does not marshal to a JSON object, so they cannot be redacted; drop WithAudit or the AsPII flag: %w",
			e.table.Name(), err)
	}
	redacted := json.RawMessage(`"<redacted>"`)
	for _, k := range keys {
		if _, ok := obj[k]; ok {
			obj[k] = redacted
		}
	}
	// Encoded without HTML escaping, so the marker reads as
	// "<redacted>" in the audit table rather than as
	// "\u003credacted\u003e". The two are the same JSON string and
	// decode identically; the difference is only visible to the person
	// reading the row, which is who the marker is for. json.Marshal
	// escapes by default and there is no argument for it here — the
	// payload is a column, not a document served to a browser.
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(obj); err != nil {
		return nil, err
	}
	// Encode appends a newline that Marshal does not.
	return json.RawMessage(bytes.TrimRight(out.Bytes(), "\n")), nil
}

// piiJSONKeys returns the JSON object keys of the entity's PII columns.
//
// The key is derived the way encoding/json derives it — the name in the
// struct tag, or the field's own name when there is none — because it
// has to match what json.Marshal just wrote. A field tagged `json:"-"`
// contributes no key and needs none: it is not in the payload either.
func (e *Entity[T]) piiJSONKeys() []string {
	var out []string
	for _, cf := range e.colFields {
		if !cf.col.IsPII() {
			continue
		}
		f := e.rowType.FieldByIndex(cf.field)
		name := f.Name
		if tag, ok := f.Tag.Lookup("json"); ok {
			switch tagged, _, _ := strings.Cut(tag, ","); tagged {
			case "-":
				continue
			case "":
			default:
				name = tagged
			}
		}
		out = append(out, name)
	}
	return out
}

// pkValue returns r's primary key as the single value the audit
// trail's rowID column holds. Used by audit + tenant scopes that need
// the key without going through the builder.
//
// A composite key is joined rather than truncated to its first
// column: an audit row that identified only half a key would point at
// a set of rows instead of the one that changed.
func (e *Entity[T]) pkValue(r *T) any {
	if len(e.pkFields) == 0 {
		return nil
	}
	return auditKey(e.pkValuesOf(r))
}

// ErrAuditTableMissing is returned when an audit operation fails
// because the configured table does not exist. Surfaces a clearer
// error than the raw "relation does not exist".
var ErrAuditTableMissing = errors.New("drops/pg: audit table not present; create it via NewAuditTable + migration")
