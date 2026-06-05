package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Live queries / reactive subscriptions — the feature that turns
// drops into a substrate for real-time UIs (leaderboards, presence,
// inventory). A trigger on the watched table publishes change
// events via pg_notify; the application LISTENs and receives them
// as typed Go values.
//
//	// Setup once (drops emits this DDL; typically run with the table).
//	for _, stmt := range pg.InstallChangeFeed(Players) {
//	    db.Exec(ctx, stmt)
//	}
//
//	// Subscribe — typed channel of changes.
//	ch, err := pg.Subscribe[Player](db, ctx, PlayerEntity,
//	    pg.SubscribeOptions{Hydrate: true})
//	for ev := range ch {
//	    switch ev.Op {
//	    case pg.OpInsert:
//	        // ev.Row is the new player
//	    case pg.OpUpdate:
//	        // ev.Row is the current player
//	    case pg.OpDelete:
//	        // ev.ID identifies the gone row
//	    }
//	}
//
// The NOTIFY payload is intentionally small — just operation + id —
// so it fits comfortably inside PG's 8KB cap. With Hydrate enabled
// drops fetches the row for insert / update events so the channel
// delivers typed values, not raw IDs.

// ChangeOp is the kind of mutation captured by the trigger.
type ChangeOp string

const (
	// OpInsert fires after a successful INSERT.
	OpInsert ChangeOp = "INSERT"
	// OpUpdate fires after a successful UPDATE.
	OpUpdate ChangeOp = "UPDATE"
	// OpDelete fires after a successful DELETE.
	OpDelete ChangeOp = "DELETE"
)

// ChangeEvent is one row change captured by the trigger, before
// any typed hydration. Subscribe[T] wraps it in TypedChange[T] with
// the row fetched.
type ChangeEvent struct {
	// Op is the SQL operation — INSERT, UPDATE, DELETE.
	Op ChangeOp `json:"op"`

	// ID is the primary-key value of the affected row, coerced
	// to text by the trigger so heterogeneous PKs (int / uuid /
	// text) flow through the same channel.
	ID string `json:"id"`
}

// TypedChange[T] decorates a ChangeEvent with the optional fetched
// row body.
type TypedChange[T any] struct {
	// Op is the underlying SQL operation.
	Op ChangeOp
	// ID is the primary-key value (text-coerced).
	ID string
	// Row is the fetched row body. Nil for OpDelete (the row is
	// already gone) and for any event when Hydrate is false.
	Row *T
	// At is the wall-clock time the event arrived in-process —
	// approximate by ~one network hop from the trigger fire.
	At time.Time
}

// ChangeFeedOptions tunes the trigger DDL produced by
// InstallChangeFeed.
type ChangeFeedOptions struct {
	// Channel is the pg_notify channel name. Defaults to
	// "drops_<tablename>" — derived from the table so multiple
	// feeds coexist in the same database.
	Channel string
}

// InstallChangeFeed returns the DDL statements that wire a NOTIFY
// trigger onto t. The trigger fires AFTER INSERT/UPDATE/DELETE and
// emits a JSON payload of {"op": ..., "id": ...} on the channel.
//
// Single-column primary keys only — composite PKs would need a
// different payload shape and are not yet supported.
func InstallChangeFeed(t *Table, opts ...ChangeFeedOptions) ([]string, error) {
	if t == nil {
		return nil, errors.New("drops/pg: InstallChangeFeed nil table")
	}
	pk := tableSinglePK(t)
	if pk == nil {
		return nil, fmt.Errorf("drops/pg: InstallChangeFeed: table %q needs a single-column PRIMARY KEY", t.Name())
	}
	channel := ""
	if len(opts) > 0 && opts[0].Channel != "" {
		channel = opts[0].Channel
	}
	if channel == "" {
		channel = "drops_" + t.Name()
	}
	fn := changeFeedFuncName(t)
	trg := changeFeedTrgName(t)
	tableName := t.Name()
	pkName := pk.Name()
	createFunc := fmt.Sprintf(`CREATE OR REPLACE FUNCTION %s() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify(
        '%s',
        json_build_object(
            'op', TG_OP,
            'id', COALESCE(NEW."%s"::text, OLD."%s"::text)
        )::text
    );
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql`, fn, channel, pkName, pkName)
	dropTrg := fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON "%s"`, trg, tableName)
	createTrg := fmt.Sprintf(`CREATE TRIGGER %s
AFTER INSERT OR UPDATE OR DELETE ON "%s"
FOR EACH ROW EXECUTE FUNCTION %s()`, trg, tableName, fn)
	return []string{createFunc, dropTrg, createTrg}, nil
}

// UninstallChangeFeed returns the DDL to remove a previously-
// installed feed on t.
func UninstallChangeFeed(t *Table) []string {
	tableName := t.Name()
	return []string{
		fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON "%s"`, changeFeedTrgName(t), tableName),
		fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, changeFeedFuncName(t)),
	}
}

func changeFeedFuncName(t *Table) string {
	return fmt.Sprintf(`"dropsNotify_%s"`, t.Name())
}

func changeFeedTrgName(t *Table) string {
	return fmt.Sprintf(`"dropsNotify_%sTrg"`, t.Name())
}

// tableSinglePK returns the single PK column when the table has
// exactly one PK, nil otherwise.
func tableSinglePK(t *Table) *Column {
	if len(t.compositePK) > 0 {
		return nil
	}
	for _, c := range t.columns {
		if c.primary {
			return c
		}
	}
	return nil
}

// SubscribeOptions controls subscription behaviour.
type SubscribeOptions struct {
	// Hydrate fetches the full row for insert/update events so
	// downstream consumers receive typed values instead of bare
	// IDs. Disabled by default — enable when the consumer needs
	// the row body and the per-event Get cost is acceptable.
	Hydrate bool

	// Channel overrides the pg_notify channel. Defaults to
	// "drops_<tablename>" matching InstallChangeFeed's default.
	Channel string

	// Buffer sizes the returned channel; events are dropped when
	// the channel is full so a slow consumer doesn't backlog
	// memory. Defaults to 64.
	Buffer int
}

// Subscribe returns a typed channel of change events for the
// entity's table. The channel closes when ctx is done or the
// driver's underlying LISTEN terminates.
//
// drops dispatches via the Listener interface (see listen.go).
// Drivers that don't expose listen return ErrListenNotSupported.
func Subscribe[T any](db *DB, ctx context.Context, e *Entity[T], opts ...SubscribeOptions) (<-chan TypedChange[T], error) {
	opt := SubscribeOptions{}
	if len(opts) > 0 {
		opt = opts[0]
	}
	if opt.Buffer <= 0 {
		opt.Buffer = 64
	}
	channel := opt.Channel
	if channel == "" {
		channel = "drops_" + e.table.Name()
	}

	raw, err := Listen[ChangeEvent](db, ctx, channel)
	if err != nil {
		return nil, err
	}

	out := make(chan TypedChange[T], opt.Buffer)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-raw:
				if !ok {
					return
				}
				tc := TypedChange[T]{Op: ev.Op, ID: ev.ID, At: time.Now()}
				if opt.Hydrate && ev.Op != OpDelete && ev.ID != "" {
					if row, err := e.Get(db, ctx, ev.ID); err == nil {
						tc.Row = &row
					}
				}
				select {
				case out <- tc:
				case <-ctx.Done():
					return
				default:
					// Drop event when the consumer is
					// behind — backpressure is better than
					// unbounded memory growth on a
					// runaway producer.
				}
			}
		}
	}()
	return out, nil
}

// MarshalJSON / UnmarshalJSON on ChangeEvent keep the Op field as
// the canonical PG label ("INSERT" / "UPDATE" / "DELETE") so the
// payload is human-readable in psql.
//
// The default encoding/json behaviour already produces this shape;
// the methods are provided so changes to the struct don't silently
// alter the wire format.
var _ json.Marshaler = (*ChangeEvent)(nil)
var _ json.Unmarshaler = (*ChangeEvent)(nil)

// MarshalJSON renders the event as `{"op":...,"id":...}`.
func (e *ChangeEvent) MarshalJSON() ([]byte, error) {
	type alias ChangeEvent
	return json.Marshal((*alias)(e))
}

// UnmarshalJSON parses a payload of `{"op":...,"id":...}`.
func (e *ChangeEvent) UnmarshalJSON(data []byte) error {
	type alias ChangeEvent
	return json.Unmarshal(data, (*alias)(e))
}
