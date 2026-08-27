package pg

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bernardoforcillo/drops"
)

// Change data capture, from the log rather than from a trigger.
//
// drops already has two ways to learn that a row changed, and both
// cost the writer something. [InstallChangeFeed] puts a trigger on
// the table and publishes through pg_notify — small payloads, no
// replay, and nothing at all if no session is listening.
// [mirror.OutboxSource] is durable and ordered, but every transaction
// has to write its own changes into the outbox, which means the
// application has to cooperate and every write is two writes.
//
// PostgreSQL already keeps a complete, ordered, durable record of
// every change: the write-ahead log. Logical replication decodes it
// into rows. A consumer of that log costs the writer nothing, cannot
// miss a change, and can be replayed from any point the server still
// has — which is the shape a mirror wants and neither of the other
// two can offer.
//
// This file is the part of that a driver-agnostic toolkit can own:
//
//   - Slot lifecycle in plain SQL — [CreateSlot], [EnsureSlot],
//     [DropSlot], [SlotStatus], [Slots].
//   - The operational half nobody ships — [SlotLag],
//     [InactiveSlots], [DropInactiveSlots]. An abandoned slot pins
//     WAL until the disk fills, and it is the most common way a
//     logical replication deployment takes an outage.
//   - [LSNVersion], which puts a WAL position into the band
//     [mirror.Change.Version] orders by, so a mirror fed from the log
//     inherits the database's own commit order.
//   - The contract a driver implements to stream — [LogicalStreamer]
//     and [ReplicationStream] — and the transaction reassembly on top
//     of it, [Reassemble].
//   - [WithSnapshot], which is what makes an initial copy and the
//     stream that follows it join without a gap or a duplicate.
//
// Streaming itself needs the replication sub-protocol, which is a
// different connection mode and not something the three-method
// [drops.Driver] can express. So it arrives the way COPY and LISTEN
// already do in this package: an interface a driver may implement,
// probed by duck typing, with a documented adapter. See
// [LogicalStreamer].

// LSNVersion maps a WAL position into the live band that
// [mirror.Change.Version] orders by.
//
// A WAL position is the sequence number a CDC pipeline is built on:
// within one server it is total, monotonic, and assigned in commit
// order, which is strictly stronger than what an outbox id promises —
// the outbox id orders commits per key, the LSN orders them outright.
//
// The mapping is the identity plus the band floor and cannot
// overflow: the band is 2^63 wide and an LSN that large would mean a
// server had written nine exabytes of WAL. Do not feed one mirror
// from both an LSN source and an outbox source, though — both land in
// the same band with unrelated numbering, and the higher number wins
// regardless of which is newer.
//
// LSNs are plain uint64 throughout this package; [ParseLSN] and
// [FormatLSN] convert to and from the "16/B374D848" form the server
// prints.
func LSNVersion(lsn uint64) uint64 {
	const liveVersionBase = 1 << 63
	return liveVersionBase + lsn
}

// --- Slot lifecycle -------------------------------------------------

// Output plugins. wal2json emits JSON and is the one to reach for
// when the consumer is written in Go and the volume is moderate;
// pgoutput is built in, emits the binary protocol, and needs a
// decoder — most Go drivers that support replication ship one.
const (
	// PluginPgOutput is PostgreSQL's built-in logical decoding
	// plugin. It requires a PUBLICATION naming the tables to
	// replicate, which is a catalog object rather than a slot
	// parameter — see [Publication].
	PluginPgOutput = "pgoutput"

	// PluginWal2JSON emits each transaction as a JSON document. It
	// is not built in; the server needs the extension installed.
	PluginWal2JSON = "wal2json"
)

// SlotStatus is what pg_replication_slots reports about one slot.
type SlotStatus struct {
	// Name is the slot name.
	Name string

	// Plugin is the output plugin the slot decodes with.
	Plugin string

	// Temporary reports whether the slot disappears when the session
	// that made it disconnects.
	Temporary bool

	// Active reports whether a consumer is attached right now. An
	// inactive non-temporary slot is the dangerous state: it keeps
	// its position, so the server keeps every WAL segment after it,
	// forever, whether or not anybody ever comes back. See
	// [InactiveSlots].
	Active bool

	// RestartLSN is the oldest WAL position the slot may still need.
	// Everything from here on is retained on disk for it.
	RestartLSN uint64

	// ConfirmedFlushLSN is the position the consumer has confirmed.
	// The gap between this and the server's current position is the
	// lag — see [SlotLag].
	ConfirmedFlushLSN uint64
}

// SlotHandle is what creating a slot yields.
type SlotHandle struct {
	// Name is the slot name.
	Name string

	// ConsistentPoint is the LSN the slot starts from: every change
	// committed after it will be decoded, and every change before it
	// is already visible in SnapshotName's snapshot. That equality
	// is the whole reason the two travel together.
	ConsistentPoint uint64

	// SnapshotName identifies the exported snapshot, when the slot
	// was created over a replication connection that exports one.
	// Empty for a slot created through [CreateSlot], which uses
	// ordinary SQL and cannot export a snapshot — see [WithSnapshot]
	// for what this is for and why the distinction matters.
	SnapshotName string
}

// CreateSlot creates a logical replication slot and returns its
// consistent point.
//
// The slot is permanent unless temporary is set, and a permanent slot
// outlives the process that made it — which is the point (a consumer
// that restarts resumes where it left off) and the hazard (a consumer
// that never comes back pins WAL forever). Whatever creates a
// permanent slot owes the deployment a [DropSlot] and a monitor on
// [SlotLag].
//
// It cannot export a snapshot: pg_create_logical_replication_slot is
// an ordinary function and snapshot export belongs to the replication
// protocol. Use a [LogicalStreamer] when the initial copy has to line
// up with the stream; see [WithSnapshot].
func CreateSlot(ctx context.Context, db *DB, name, plugin string, temporary bool) (SlotHandle, error) {
	if err := validateSlotName(name); err != nil {
		return SlotHandle{}, err
	}
	if plugin == "" {
		return SlotHandle{}, errors.New("drops/pg: CreateSlot requires an output plugin")
	}
	rows, err := db.Query(ctx,
		`SELECT slot_name, lsn::text FROM pg_create_logical_replication_slot($1, $2, $3)`,
		name, plugin, temporary)
	if err != nil {
		return SlotHandle{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return SlotHandle{}, err
		}
		return SlotHandle{}, fmt.Errorf("drops/pg: creating slot %q returned no row", name)
	}
	var slotName, lsnText string
	if err := rows.Scan(&slotName, &lsnText); err != nil {
		return SlotHandle{}, err
	}
	lsn, err := ParseLSN(lsnText)
	if err != nil {
		return SlotHandle{}, err
	}
	return SlotHandle{Name: slotName, ConsistentPoint: lsn}, rows.Err()
}

// EnsureSlot creates the slot if it is missing and reports whether it
// did.
//
// A concurrent creator is not an error here: two processes starting
// at once both want the slot to exist, and the loser of the race gets
// SQLSTATE 42710 ([ErrDuplicateObject]) for a slot that is now
// present, which is the outcome it asked for. Anything else
// propagates.
func EnsureSlot(ctx context.Context, db *DB, name, plugin string) (created bool, err error) {
	status, err := SlotStatusOf(ctx, db, name)
	if err != nil {
		return false, err
	}
	if status != nil {
		return false, nil
	}
	if _, err := CreateSlot(ctx, db, name, plugin, false); err != nil {
		if errors.Is(err, ErrDuplicateObject) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// DropSlot removes a slot. Dropping one that does not exist is an
// error from the server (SQLSTATE 42704), which this passes through:
// a consumer that thinks it owns a slot and finds none has a problem
// worth surfacing.
//
// A slot that is currently active cannot be dropped; the server
// refuses with [ErrObjectInUse]. Stop the consumer first.
func DropSlot(ctx context.Context, db *DB, name string) error {
	if err := validateSlotName(name); err != nil {
		return err
	}
	_, err := db.Exec(ctx, `SELECT pg_drop_replication_slot($1)`, name)
	return err
}

// SlotStatusOf returns the slot's row from pg_replication_slots, or
// nil when there is no such slot.
func SlotStatusOf(ctx context.Context, db *DB, name string) (*SlotStatus, error) {
	if err := validateSlotName(name); err != nil {
		return nil, err
	}
	list, err := querySlots(ctx, db,
		`WHERE slot_name = $1 AND slot_type = 'logical'`, name)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}

// Slots returns every logical replication slot on the server.
func Slots(ctx context.Context, db *DB) ([]SlotStatus, error) {
	return querySlots(ctx, db, `WHERE slot_type = 'logical'`)
}

// InactiveSlots returns the logical slots with no consumer attached.
//
// This is the query to put on a schedule. A permanent slot with
// nobody reading it is not idle — it is accumulating. PostgreSQL
// keeps every WAL segment the slot might still need, so an
// abandoned slot grows pg_wal without bound until the volume fills
// and the server stops accepting writes. It is the single most common
// way a logical replication deployment takes an outage, and the only
// warning is a number nobody is looking at.
//
// prefix filters by slot-name prefix so a deployment only sees its
// own slots; pass "" for all of them. Note the race this cannot
// solve: a consumer that is restarting looks exactly like one that is
// gone. Give [DropInactiveSlots] a grace period rather than acting on
// one reading.
func InactiveSlots(ctx context.Context, db *DB, prefix string) ([]SlotStatus, error) {
	if prefix == "" {
		return querySlots(ctx, db, `WHERE slot_type = 'logical' AND NOT active`)
	}
	// LIKE with the prefix escaped: a slot-name prefix is caller
	// input and _ and % are wildcards in it.
	return querySlots(ctx, db,
		`WHERE slot_type = 'logical' AND NOT active AND slot_name LIKE $1 ESCAPE '\'`,
		escapeLikePrefix(prefix)+"%")
}

// DropInactiveSlots drops the named slots, skipping any that have
// become active since they were listed, and returns the ones it
// dropped.
//
// The re-check is not decoration. Between an [InactiveSlots] reading
// and this call, a consumer that was merely restarting can have
// reattached, and dropping its slot loses its position — the consumer
// comes back, finds nothing, and either starts from now (silently
// missing everything in between) or has to be reseeded. The filter is
// applied in the same statement as the drop so there is no window at
// all.
func DropInactiveSlots(ctx context.Context, db *DB, names []string) ([]string, error) {
	var dropped []string
	for _, name := range names {
		if err := validateSlotName(name); err != nil {
			return dropped, err
		}
		rows, err := db.Query(ctx, `
			SELECT pg_drop_replication_slot(slot_name)::text, slot_name
			FROM pg_replication_slots
			WHERE slot_name = $1 AND slot_type = 'logical' AND NOT active`, name)
		if err != nil {
			return dropped, err
		}
		var got string
		for rows.Next() {
			var discard string
			if err := rows.Scan(&discard, &got); err != nil {
				rows.Close()
				return dropped, err
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return dropped, err
		}
		if got != "" {
			dropped = append(dropped, got)
		}
	}
	return dropped, nil
}

// SlotLag returns how many bytes of WAL the slot has not confirmed —
// pg_current_wal_lsn() minus the slot's confirmed_flush_lsn.
//
// It is the one number to alert on. A consumer that is keeping up
// holds this near zero; one that has fallen behind or died holds a
// number that only grows, and the growth is measured against the
// disk pg_wal lives on.
//
// Returns [ErrNoSuchSlot] when the slot does not exist, because a
// monitor reading zero for a slot that is gone is worse than one that
// reports the truth.
func SlotLag(ctx context.Context, db *DB, name string) (int64, error) {
	if err := validateSlotName(name); err != nil {
		return 0, err
	}
	rows, err := db.Query(ctx, `
		SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), confirmed_flush_lsn)::bigint
		FROM pg_replication_slots
		WHERE slot_name = $1`, name)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("%w: %s", ErrNoSuchSlot, name)
	}
	var lag int64
	if err := rows.Scan(&lag); err != nil {
		return 0, err
	}
	return lag, rows.Err()
}

// ErrNoSuchSlot is returned by [SlotLag] for a slot that is not on
// the server.
var ErrNoSuchSlot = errors.New("drops/pg: no such replication slot")

// CurrentWALLSN returns the server's current write-ahead log
// position. On a standby it returns the last replayed position
// instead, since pg_current_wal_lsn() is not available there.
func CurrentWALLSN(ctx context.Context, db *DB) (uint64, error) {
	rows, err := db.Query(ctx, `
		SELECT CASE WHEN pg_is_in_recovery()
		            THEN pg_last_wal_replay_lsn()
		            ELSE pg_current_wal_lsn() END::text`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, err
		}
		return 0, errors.New("drops/pg: current WAL LSN returned no row")
	}
	var text string
	if err := rows.Scan(&text); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return ParseLSN(text)
}

func querySlots(ctx context.Context, db *DB, where string, args ...any) ([]SlotStatus, error) {
	rows, err := db.Query(ctx, `
		SELECT slot_name,
		       coalesce(plugin, ''),
		       temporary,
		       active,
		       coalesce(restart_lsn::text, '0/0'),
		       coalesce(confirmed_flush_lsn::text, '0/0')
		FROM pg_replication_slots `+where+`
		ORDER BY slot_name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SlotStatus
	for rows.Next() {
		var s SlotStatus
		var restart, flush string
		if err := rows.Scan(&s.Name, &s.Plugin, &s.Temporary, &s.Active, &restart, &flush); err != nil {
			return nil, err
		}
		if s.RestartLSN, err = ParseLSN(restart); err != nil {
			return nil, err
		}
		if s.ConfirmedFlushLSN, err = ParseLSN(flush); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// validateSlotName rejects anything PostgreSQL would not accept as a
// slot name.
//
// The name is a bound parameter everywhere it is used here, so this
// is not an injection guard — it is a better error than the server's.
// PostgreSQL restricts slot names to lowercase letters, digits and
// underscores, and a name that breaks the rule fails at creation
// time, which is usually somewhere far from where it was chosen.
func validateSlotName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: replication slot name is empty", ErrInvalidIdentifier)
	}
	if len(name) > 63 {
		return fmt.Errorf("%w: replication slot name %q is longer than 63 characters", ErrInvalidIdentifier, name)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_':
		default:
			return fmt.Errorf("%w: replication slot name %q may only contain [a-z0-9_]", ErrInvalidIdentifier, name)
		}
	}
	return nil
}

// escapeLikePrefix neutralises the LIKE metacharacters in a
// user-supplied prefix.
func escapeLikePrefix(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// --- Publications ---------------------------------------------------

// Publication returns the DDL creating a publication over tables —
// the catalog object [PluginPgOutput] decides what to decode from.
//
//	stmts := pg.Publication("drops_mirror", Orders, OrderLines)
//	for _, s := range stmts { db.Exec(ctx, s) }
//
// The gotcha worth stating plainly, because it fails silently: a
// table that is not in the publication produces no messages at all.
// Not an error, not a warning — the consumer simply never hears about
// it, and the first sign is a mirror that is missing a table's rows
// and has nothing in its logs. Adding a table later means altering
// the publication; the slot does not need recreating.
//
// The second one costs data rather than time. An UPDATE or DELETE
// decodes with the *old* row only if the table's REPLICA IDENTITY
// says so. The default identity carries the primary key and nothing
// else, which is enough to address a mirrored row but not to see what
// a column changed from. [ReplicaIdentityFull] is the fix and it is
// not free — it widens every UPDATE's WAL record to the whole row.
func Publication(name string, tables ...*Table) []string {
	if len(tables) == 0 {
		return nil
	}
	names := make([]string, 0, len(tables))
	for _, t := range tables {
		names = append(names, qualifiedTable(t))
	}
	return []string{
		fmt.Sprintf("CREATE PUBLICATION %s FOR TABLE %s",
			quoteIdent(name), strings.Join(names, ", ")),
	}
}

// ReplicaIdentityFull returns the DDL making t's UPDATE and DELETE
// records carry every column's old value rather than just the key.
//
// Needed when a consumer has to see what a value changed *from* — a
// mirror that computes a delta, an audit trail, a sink that keys on
// something other than the primary key. It widens every UPDATE and
// DELETE record in the WAL to a full row, so it is a real cost on a
// wide table and should be set where it earns its keep, not
// everywhere.
func ReplicaIdentityFull(t *Table) string {
	return fmt.Sprintf("ALTER TABLE %s REPLICA IDENTITY FULL", qualifiedTable(t))
}

// --- Snapshot handoff -----------------------------------------------

// ErrNoSnapshot is returned by [WithSnapshot] for a handle that
// carries no exported snapshot.
var ErrNoSnapshot = errors.New("drops/pg: slot handle carries no exported snapshot")

// WithSnapshot runs fn inside a transaction that reads the database
// exactly as it stood at the slot's consistent point.
//
// This is the piece that makes an initial copy correct, and it is the
// piece a reseed usually gets wrong. The problem: a mirror has to be
// filled with what is already there and then kept up to date by the
// stream, and the two have to meet exactly. Copy first and start the
// stream after, and every change in between is lost. Start the stream
// first and copy after, and the copy overwrites changes the stream
// already delivered. Either way nothing reports it.
//
// A slot created over a replication connection solves it by handing
// back both halves of the same instant: an LSN, and the name of a
// snapshot showing the database as of that LSN. Read the tables in
// that snapshot and start the stream at that LSN, and the two fit
// together with no gap and no overlap.
//
//	handle, err := streamer.CreateReplicationSlot(ctx, "mirror", pg.PluginPgOutput,
//	    pg.SlotOptions{ExportSnapshot: true})
//	// Fill the mirror from the database as it was at handle.ConsistentPoint.
//	err = pg.WithSnapshot(ctx, db, handle, func(snap *pg.DB) error {
//	    return fillMirror(ctx, snap)
//	})
//	// Then stream from exactly there.
//	stream, err := streamer.StartReplication(ctx, handle.Name, handle.ConsistentPoint, nil)
//
// fn receives a *DB bound to the snapshot transaction; every read
// through it sees that instant. The transaction is REPEATABLE READ
// and READ ONLY, so it cannot write and cannot see anything newer,
// and it is rolled back on the way out — there is nothing to commit.
//
// The snapshot has to be imported on a *different* connection from
// the one that created the slot, and it stays importable only while
// that connection is open. Keep the streamer's connection alive for
// the duration of the copy.
//
// A long snapshot holds back vacuum on everything it can see. Copying
// a large table this way is the intended use and is fine; leaving the
// transaction open while something else is decided is not.
func WithSnapshot(ctx context.Context, db *DB, handle SlotHandle, fn func(*DB) error) error {
	if handle.SnapshotName == "" {
		return ErrNoSnapshot
	}
	if err := validateSnapshotName(handle.SnapshotName); err != nil {
		return err
	}
	return db.InTx(ctx, func(tx *DB) error {
		// SET TRANSACTION SNAPSHOT takes a string literal and will
		// not take a parameter, which is why the name is validated
		// against PostgreSQL's own format above rather than bound.
		if _, err := tx.Exec(ctx,
			"SET TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ ONLY"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			"SET TRANSACTION SNAPSHOT '"+handle.SnapshotName+"'"); err != nil {
			return err
		}
		return fn(tx)
	})
}

// validateSnapshotName checks the name against the format PostgreSQL
// exports — two eight-digit hex groups and a decimal counter, as in
// "00000003-0000001B-1".
//
// It has to be checked rather than bound: SET TRANSACTION SNAPSHOT
// accepts a literal only. The name always comes from the server
// rather than from a user, so this is a guard against a driver
// returning something unexpected, not against an attacker — but it is
// the one place in this package where a value is concatenated into
// SQL, and that is reason enough for it to be exact.
func validateSnapshotName(s string) error {
	fail := func() error {
		return fmt.Errorf("%w: snapshot name %q is not in PostgreSQL's exported format", ErrInvalidIdentifier, s)
	}
	parts := strings.Split(s, "-")
	if len(parts) != 3 {
		return fail()
	}
	for _, p := range parts[:2] {
		if len(p) != 8 {
			return fail()
		}
		if _, err := strconv.ParseUint(p, 16, 32); err != nil {
			return fail()
		}
	}
	if parts[2] == "" {
		return fail()
	}
	for i := 0; i < len(parts[2]); i++ {
		if parts[2][i] < '0' || parts[2][i] > '9' {
			return fail()
		}
	}
	return nil
}

// --- Streaming contract ---------------------------------------------

// ErrStreamNotSupported is returned by [Stream] when the driver does
// not implement [LogicalStreamer].
var ErrStreamNotSupported = errors.New("drops/pg: driver does not support logical replication")

// SlotOptions configures slot creation over a replication connection.
type SlotOptions struct {
	// Temporary makes the slot vanish when the replication
	// connection closes. Right for a one-off resync, wrong for a
	// consumer that has to survive a restart.
	Temporary bool

	// ExportSnapshot asks the server to export a snapshot at the
	// slot's consistent point, which is what [WithSnapshot] needs.
	// The snapshot is importable only while the creating connection
	// stays open.
	ExportSnapshot bool
}

// LogicalStreamer is the contract a driver implements to open a
// logical replication stream.
//
// drops imports no driver, and replication is a connection *mode*
// rather than a statement — the client asks for it at startup — so
// this cannot go through [drops.Driver]. It follows the pattern
// [Copier] and [Listener] already use in this package: implement it
// alongside your Driver and drops finds it by type assertion.
//
// With pgx the adapter is small, because pgx/v5 ships the protocol in
// pglogrepl:
//
//	type pgxStreamer struct{ conn *pgconn.PgConn }  // replication=database
//
//	func (s pgxStreamer) CreateReplicationSlot(ctx context.Context,
//	    name, plugin string, opts pg.SlotOptions) (pg.SlotHandle, error) {
//	    res, err := pglogrepl.CreateReplicationSlot(ctx, s.conn, name, plugin,
//	        pglogrepl.CreateReplicationSlotOptions{
//	            Temporary:      opts.Temporary,
//	            SnapshotAction: snapshotAction(opts.ExportSnapshot),
//	        })
//	    if err != nil { return pg.SlotHandle{}, err }
//	    lsn, err := pg.ParseLSN(res.ConsistentPoint)
//	    return pg.SlotHandle{
//	        Name:            res.SlotName,
//	        ConsistentPoint: lsn,
//	        SnapshotName:    res.SnapshotName,
//	    }, err
//	}
//
// StartReplication returns a stream positioned at start. Passing 0
// means "resume from the slot's confirmed position", which is what a
// restarting consumer wants and what makes the slot worth having.
type LogicalStreamer interface {
	CreateReplicationSlot(ctx context.Context, name, plugin string, opts SlotOptions) (SlotHandle, error)
	StartReplication(ctx context.Context, slot string, start uint64, pluginArgs map[string]string) (ReplicationStream, error)
}

// ReplicationStream is an open logical replication stream.
//
// Next blocks until a message is available or ctx ends. Ack tells the
// server the consumer has durably handled everything up to and
// including an LSN, which is what lets the server release the WAL
// behind it — a stream that never acks is a slot that never advances,
// and the disk fills at the rate of the write load.
type ReplicationStream interface {
	Next(ctx context.Context) (Message, error)
	Ack(ctx context.Context, lsn uint64) error
	Close() error
}

// MessageKind classifies a decoded replication message.
type MessageKind string

const (
	// MessageBegin opens a transaction.
	MessageBegin MessageKind = "begin"
	// MessageChange is one row mutation.
	MessageChange MessageKind = "change"
	// MessageCommit closes a transaction. Its LSN is the end of the
	// commit record, and it is the only LSN safe to acknowledge.
	MessageCommit MessageKind = "commit"
	// MessageKeepalive is the server checking in. It carries no
	// data and exists so a stream on an idle database can still
	// advance its acknowledged position.
	MessageKeepalive MessageKind = "keepalive"
)

// Message is one decoded item from a replication stream.
type Message struct {
	// Kind is what the message is.
	Kind MessageKind

	// LSN is the message's position in the WAL. For
	// [MessageCommit] it is the end of the commit record.
	LSN uint64

	// XID is the transaction id, when the decoder reports one.
	XID uint32

	// CommitTime is when the transaction committed, on
	// [MessageBegin] and [MessageCommit].
	CommitTime time.Time

	// Schema and Table name the relation, on [MessageChange].
	Schema string
	Table  string

	// Op is the mutation kind, on [MessageChange]. It reuses the
	// change feed's [ChangeOp] — [OpInsert], [OpUpdate],
	// [OpDelete] — so a consumer that switches on one vocabulary can
	// be moved from a trigger feed to the WAL without rewriting the
	// switch.
	Op ChangeOp

	// New holds the row's column values after the mutation. Nil for
	// a delete.
	New map[string]any

	// Old holds the row's column values before the mutation, as far
	// as the table's REPLICA IDENTITY carries them: the key columns
	// by default, every column under [ReplicaIdentityFull]. Nil for
	// an insert.
	Old map[string]any
}

// Stream opens a replication stream through a driver that implements
// [LogicalStreamer], or returns [ErrStreamNotSupported].
//
// It unwraps the driver stack on the way down, so a streamer stays
// reachable through [RetryCachedPlans], [StatementRegistry.Wrap] and
// [Replicated] — which routes to the primary, the only node a slot
// can live on.
func Stream(db *DB, ctx context.Context, slot string, start uint64, pluginArgs map[string]string) (ReplicationStream, error) {
	s, ok := logicalStreamer(db.Driver())
	if !ok {
		return nil, ErrStreamNotSupported
	}
	if err := validateSlotName(slot); err != nil {
		return nil, err
	}
	return s.StartReplication(ctx, slot, start, pluginArgs)
}

// Streamer returns the driver's [LogicalStreamer], if it has one.
// Use it to create a slot with an exported snapshot, which [Stream]
// deliberately does not do — slot creation is a deployment decision
// and opening a stream is not.
func Streamer(db *DB) (LogicalStreamer, bool) { return logicalStreamer(db.Driver()) }

// logicalStreamer walks the driver's Unwrap chain looking for a
// streamer, the same way the other capability probes in this package
// reach through a wrapper.
func logicalStreamer(drv drops.Driver) (LogicalStreamer, bool) {
	for drv != nil {
		if s, ok := drv.(LogicalStreamer); ok {
			return s, true
		}
		u, ok := drv.(interface{ Unwrap() drops.Driver })
		if !ok {
			return nil, false
		}
		drv = u.Unwrap()
	}
	return nil, false
}

// --- Transaction reassembly -----------------------------------------

// Transaction is one source transaction, whole.
type Transaction struct {
	// XID is the source transaction id, when the decoder reports
	// one.
	XID uint32

	// CommitLSN is the end of the transaction's commit record. It is
	// the position to acknowledge and the number to order by: it is
	// assigned at commit, so it orders transactions the way the
	// database committed them rather than the way they started.
	CommitLSN uint64

	// CommitTime is when the transaction committed.
	CommitTime time.Time

	// Changes are the row mutations, in the order they were decoded.
	Changes []Message
}

// ErrStreamOutOfOrder is returned by [Reassemble] when the stream
// breaks the begin/change/commit framing.
var ErrStreamOutOfOrder = errors.New("drops/pg: replication stream out of order")

// Reassemble reads a stream and calls fn once per complete source
// transaction.
//
// A logical stream arrives as begin, changes, commit, and only the
// whole group is meaningful: half of a transfer is not a smaller
// transfer, it is a wrong balance. Handing a consumer individual
// changes makes atomicity its problem, and a consumer that gets it
// wrong produces a mirror that is briefly incorrect in a way nothing
// detects. So the framing is enforced here, once.
//
// fn is called with the transaction only after its commit is seen.
// When fn returns nil the commit LSN is acknowledged, which is what
// releases the WAL behind it — so fn must not return nil until it has
// durably handled the transaction. Return an error and nothing is
// acknowledged and the loop stops; the next run resumes from the last
// acknowledged position and replays from there. That makes delivery
// at-least-once, and a consumer of it has to be idempotent. It is the
// same contract [mirror.Source] states, for the same reason.
//
// An empty transaction — a commit with no changes, which PostgreSQL
// emits for a transaction that touched nothing in the publication —
// is acknowledged without calling fn. There is nothing to hand over
// and the position still has to move, or an idle publication would
// hold the slot back indefinitely.
//
// Keepalives are acknowledged at their own LSN when no transaction is
// open. Inside one they are ignored: acknowledging a position in the
// middle of a transaction would tell the server the consumer is done
// with changes it has not seen.
//
// Reassemble returns when ctx ends, when the stream fails, or when fn
// returns an error.
func Reassemble(ctx context.Context, stream ReplicationStream, fn func(context.Context, Transaction) error) error {
	return reassemble(ctx, stream, fn, true)
}

// ReassembleDeferred is [Reassemble] without the acknowledgement: fn
// is handed every transaction and nothing is ever confirmed to the
// server. The caller acknowledges, later, with [ReplicationStream.Ack].
//
// It exists for a consumer that cannot say "handled" at the moment it
// is handed a transaction — the shape [mirror.LogicalSource] has,
// where a transaction is buffered here and only durable once the
// mirror's sinks have taken it. Acknowledging at hand-over would
// release the WAL behind changes that are still in memory, and a
// crash would lose them with nothing to report it.
//
// Two differences follow from that, and both matter:
//
//   - fn is called for *every* commit, including the empty ones a
//     transaction that touched nothing in the publication produces.
//     [Reassemble] can quietly acknowledge those; here they have to
//     travel, because their position cannot be confirmed ahead of a
//     transaction the caller is still holding.
//   - A keepalive arrives as a transaction with no changes, carrying
//     the keepalive's own position. It is what lets a consumer on an
//     idle database advance the slot, and it is delivered rather than
//     acted on for the same reason.
//
// So a caller must confirm positions in the order it received them,
// and must never confirm one it has not finished with: an
// acknowledgement is cumulative and takes everything before it.
func ReassembleDeferred(ctx context.Context, stream ReplicationStream, fn func(context.Context, Transaction) error) error {
	return reassemble(ctx, stream, fn, false)
}

func reassemble(ctx context.Context, stream ReplicationStream, fn func(context.Context, Transaction) error, ack bool) error {
	var open bool
	var tx Transaction

	for {
		msg, err := stream.Next(ctx)
		if err != nil {
			return err
		}
		switch msg.Kind {
		case MessageBegin:
			if open {
				return fmt.Errorf("%w: BEGIN inside an open transaction at %s", ErrStreamOutOfOrder, FormatLSN(msg.LSN))
			}
			open = true
			tx = Transaction{XID: msg.XID, CommitTime: msg.CommitTime}

		case MessageChange:
			if !open {
				return fmt.Errorf("%w: change outside a transaction at %s", ErrStreamOutOfOrder, FormatLSN(msg.LSN))
			}
			tx.Changes = append(tx.Changes, msg)

		case MessageCommit:
			if !open {
				return fmt.Errorf("%w: COMMIT without a BEGIN at %s", ErrStreamOutOfOrder, FormatLSN(msg.LSN))
			}
			open = false
			tx.CommitLSN = msg.LSN
			if !msg.CommitTime.IsZero() {
				tx.CommitTime = msg.CommitTime
			}
			if len(tx.Changes) > 0 || !ack {
				if err := fn(ctx, tx); err != nil {
					return err
				}
			}
			if ack {
				if err := stream.Ack(ctx, msg.LSN); err != nil {
					return err
				}
			}
			tx = Transaction{}

		case MessageKeepalive:
			if open {
				continue
			}
			if !ack {
				if err := fn(ctx, Transaction{CommitLSN: msg.LSN, CommitTime: msg.CommitTime}); err != nil {
					return err
				}
				continue
			}
			if err := stream.Ack(ctx, msg.LSN); err != nil {
				return err
			}
		}
	}
}

// qualifiedTable renders a table for DDL, schema-qualified when it
// declares a schema.
func qualifiedTable(t *Table) string {
	if s := t.Schema(); s != "" {
		return quoteIdent(s) + "." + quoteIdent(t.Name())
	}
	return quoteIdent(t.Name())
}
