package mirror

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bernardoforcillo/drops/pg"
)

// A mirror fed from the write-ahead log instead of from an outbox.
//
// [OutboxSource] asks the application to write every change twice:
// once to the row and once to the outbox, in the same transaction.
// That is what buys the guarantee — the two either both land or
// neither does — and it is a real cost. Every writer has to
// cooperate, a writer that forgets is invisible until the mirror is
// wrong, and the write volume doubles.
//
// PostgreSQL is already writing every change down. [LogicalSource]
// reads that record instead:
//
//   - Nothing is asked of the writer. A path that nobody remembered
//     to instrument is mirrored anyway, including one that predates
//     the mirror.
//   - The ordering is the database's own. A commit LSN orders
//     transactions the way they committed, across every table, which
//     is stronger than the per-key ordering an outbox id gives.
//   - The history is the server's. A consumer that was down replays
//     from where it stopped, as far back as the slot's retention —
//     which is until the disk fills, so see [pg.SlotLag].
//
// What it costs instead is operational. A replication slot is a
// server-side object that outlives the process, and an abandoned one
// retains WAL until the volume is full. Nothing in this package can
// prevent that; [pg.InactiveSlots] and [pg.SlotLag] are what make it
// visible.
//
//	stream, err := pg.Stream(db, ctx, "mirror_orders", 0, nil)
//	src, err := mirror.NewLogicalSource(stream, mirror.LogicalOptions{
//	    Tables: map[string]string{"orders": "id"},
//	})
//	go src.Run(ctx)                    // pumps the stream
//	pump := mirror.NewPump(src, sink)  // drains it like any other source
//
// # Which source to choose
//
// Take the outbox when the mirror carries something the row does not
// — a derived field, a join, an event the application invents. Take
// the log when the mirror is a copy of rows, which is the common
// case and the one this exists for. Do not point both at one mirror:
// they number their changes independently in the same version band,
// so the higher number would win regardless of which was newer.

// ErrSourceClosed is returned by [LogicalSource.Fetch] once the
// stream has ended and its buffer is drained. It is how the pump
// learns that replaying further is not merely slow but impossible.
var ErrSourceClosed = errors.New("drops/mirror: logical source is closed")

// LogicalOptions configures a [LogicalSource].
type LogicalOptions struct {
	// Tables selects the relations to mirror and names each one's key
	// column. Keys are either a bare table name, which matches the
	// table in any schema, or "schema.table", which matches exactly.
	// An exact match wins over a bare one.
	//
	// A relation that is not listed is dropped. That is deliberate:
	// a publication is usually wider than one mirror, and a change to
	// a table this mirror does not hold has no key to address and
	// nowhere to go.
	Tables map[string]string

	// Buffer is how many decoded transactions may wait in memory
	// between the stream reader and Fetch. Default 64.
	//
	// It is a backpressure setting, and the direction it pushes
	// matters: when the buffer is full the reader stops taking
	// messages off the stream, the slot stops advancing, and WAL
	// accumulates on the server. A larger buffer trades memory here
	// for disk there.
	Buffer int

	// Wait bounds how long Fetch blocks for a first transaction when
	// none is buffered. Default 250ms.
	//
	// Fetch has to return promptly with nothing rather than block
	// indefinitely — [Source] documents an empty batch as "nothing
	// right now" and the pump is built on that — but returning
	// instantly would spin. This is the compromise.
	Wait time.Duration
}

// LogicalSource adapts a [pg.ReplicationStream] to [Source].
//
// It has one moving part more than the other sources: [Run] must be
// running for [Fetch] to see anything. The split exists because a
// replication stream is push-shaped and a [Source] is pull-shaped,
// and something has to hold the boundary.
type LogicalSource struct {
	stream pg.ReplicationStream
	tables map[string]string
	wait   time.Duration

	txs  chan pg.Transaction
	done chan struct{}

	mu      sync.Mutex
	runErr  error
	stopped bool
}

// NewLogicalSource wraps a stream as a [Source].
func NewLogicalSource(stream pg.ReplicationStream, opts LogicalOptions) (*LogicalSource, error) {
	if stream == nil {
		return nil, errors.New("drops/mirror: NewLogicalSource needs a stream")
	}
	if len(opts.Tables) == 0 {
		return nil, errors.New("drops/mirror: NewLogicalSource needs at least one table in Tables")
	}
	tables := make(map[string]string, len(opts.Tables))
	for name, key := range opts.Tables {
		if name == "" || key == "" {
			return nil, fmt.Errorf("drops/mirror: NewLogicalSource: table %q has an empty name or key column", name)
		}
		tables[name] = key
	}
	buffer := opts.Buffer
	if buffer <= 0 {
		buffer = 64
	}
	wait := opts.Wait
	if wait <= 0 {
		wait = 250 * time.Millisecond
	}
	return &LogicalSource{
		stream: stream,
		tables: tables,
		wait:   wait,
		txs:    make(chan pg.Transaction, buffer),
		done:   make(chan struct{}),
	}, nil
}

// Run reads the stream and buffers whole transactions until ctx ends
// or the stream fails. It blocks; run it in its own goroutine.
//
// Nothing is acknowledged here, which is why it uses
// [pg.ReassembleDeferred] rather than [pg.Reassemble]: acknowledging
// releases the WAL behind the position, and at hand-over the sinks
// have not seen the changes yet. The acknowledgement waits for the
// commit function [Fetch] returns, which the pump calls only after
// every sink has accepted the batch.
//
// That is also why empty transactions and keepalives travel through
// the buffer instead of being confirmed on the spot. A position
// confirmed out of order takes everything before it, including a
// transaction still sitting in this buffer.
//
// The consequence is the same at-least-once contract every [Source]
// has: a crash between apply and ack replays the batch, and
// idempotent sinks absorb the replay.
func (s *LogicalSource) Run(ctx context.Context) error {
	defer close(s.txs)
	err := pg.ReassembleDeferred(ctx, s.stream, func(ctx context.Context, tx pg.Transaction) error {
		select {
		case s.txs <- tx:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-s.done:
			return ErrSourceClosed
		}
	})
	s.mu.Lock()
	s.runErr = err
	s.mu.Unlock()
	return err
}

// Stop unblocks a [Run] that is waiting on a full buffer. It does not
// close the stream — whoever opened it owns it.
func (s *LogicalSource) Stop() {
	s.mu.Lock()
	if !s.stopped {
		s.stopped = true
		close(s.done)
	}
	s.mu.Unlock()
}

// Fetch returns the changes of one or more whole transactions,
// together with the function that acknowledges them.
//
// Transactions are never split across batches. max is a floor rather
// than a ceiling for that reason: once it is reached the batch ends,
// but the transaction that reached it is carried entire. A mirror
// that applied half a transaction would be briefly wrong in a way
// nothing detects, and no batch size is worth that.
func (s *LogicalSource) Fetch(ctx context.Context, max int) ([]Change, func(context.Context) error, error) {
	if max <= 0 {
		max = 1
	}
	first, ok, err := s.first(ctx)
	if err != nil || !ok {
		return nil, nil, err
	}

	ackLSN := first.CommitLSN
	changes, err := s.decode(first)
	if err != nil {
		return nil, nil, err
	}

	// Take whatever else is already buffered, without waiting.
	for len(changes) < max {
		select {
		case tx, open := <-s.txs:
			if !open {
				return s.finish(changes, ackLSN)
			}
			more, err := s.decode(tx)
			if err != nil {
				return nil, nil, err
			}
			changes = append(changes, more...)
			ackLSN = tx.CommitLSN
		default:
			return s.finish(changes, ackLSN)
		}
	}
	return s.finish(changes, ackLSN)
}

// first waits up to the configured Wait for a transaction, and
// reports whether one arrived.
func (s *LogicalSource) first(ctx context.Context) (pg.Transaction, bool, error) {
	timer := time.NewTimer(s.wait)
	defer timer.Stop()
	select {
	case tx, open := <-s.txs:
		if !open {
			return pg.Transaction{}, false, s.closedErr()
		}
		return tx, true, nil
	case <-timer.C:
		return pg.Transaction{}, false, nil
	case <-ctx.Done():
		return pg.Transaction{}, false, ctx.Err()
	}
}

// closedErr reports why the stream ended: the reader's error when it
// had one, [ErrSourceClosed] otherwise.
func (s *LogicalSource) closedErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runErr != nil && !errors.Is(s.runErr, context.Canceled) {
		return s.runErr
	}
	return ErrSourceClosed
}

// finish pairs a batch with the acknowledgement of its last commit.
//
// One ack covers the whole batch because a stream position is
// cumulative: confirming an LSN confirms everything before it. That
// is also why the batches must be acknowledged in order, and why
// [Fetch] hands over one commit function rather than one per
// transaction.
func (s *LogicalSource) finish(changes []Change, ackLSN uint64) ([]Change, func(context.Context) error, error) {
	// A batch with no changes still gets a commit function rather
	// than an acknowledgement issued here. Every transaction in it
	// touched only tables this mirror does not hold — there is
	// nothing to apply, but the position still has to move, or a busy
	// table nobody mirrors would hold the slot back forever. [Pump]
	// calls the function in that case too, and routing both cases
	// through it keeps the acknowledgements in one order.
	return changes, func(ctx context.Context) error {
		return s.stream.Ack(ctx, ackLSN)
	}, nil
}

// decode turns one source transaction into the changes this mirror
// holds, dropping the relations it does not.
func (s *LogicalSource) decode(tx pg.Transaction) ([]Change, error) {
	out := make([]Change, 0, len(tx.Changes))
	for _, m := range tx.Changes {
		keyCol, ok := s.keyColumn(m.Schema, m.Table)
		if !ok {
			continue
		}
		row := m.New
		op := OpInsert
		switch m.Op {
		case pg.OpInsert:
			op = OpInsert
		case pg.OpUpdate:
			op = OpUpdate
		case pg.OpDelete:
			op = OpDelete
			// A delete carries no new row. Its key comes from the
			// old one, which the table's REPLICA IDENTITY decides
			// the contents of — the key columns by default, which is
			// exactly enough. See [pg.ReplicaIdentityFull] when a
			// sink needs more.
			row = nil
		default:
			return nil, fmt.Errorf("drops/mirror: logical source: unknown operation %q on %s.%s at %s",
				m.Op, m.Schema, m.Table, pg.FormatLSN(m.LSN))
		}

		src := m.New
		if src == nil {
			src = m.Old
		}
		raw, present := src[keyCol]
		if !present {
			return nil, fmt.Errorf("drops/mirror: logical source: %s.%s at %s carries no column %q — is the table's REPLICA IDENTITY set?",
				m.Schema, m.Table, pg.FormatLSN(m.LSN), keyCol)
		}
		key, ok := logicalKeyText(raw)
		if !ok {
			return nil, fmt.Errorf("drops/mirror: logical source: key %q of %s.%s decoded as %T, which cannot address a mirrored row",
				keyCol, m.Schema, m.Table, raw)
		}

		out = append(out, Change{
			Op:  op,
			Key: key,
			Row: row,
			// Every change in a transaction takes the transaction's
			// commit position. They are simultaneous as far as any
			// observer is concerned — nothing outside the
			// transaction saw an order among them — and giving them
			// one version keeps a sink from inventing one.
			Version: pg.LSNVersion(tx.CommitLSN),
			At:      tx.CommitTime,
		})
	}
	return out, nil
}

// keyColumn resolves a relation to its key column: the exact
// "schema.table" entry when there is one, the bare table name
// otherwise.
func (s *LogicalSource) keyColumn(schema, table string) (string, bool) {
	if schema != "" {
		if col, ok := s.tables[schema+"."+table]; ok {
			return col, true
		}
	}
	col, ok := s.tables[table]
	return col, ok
}

// logicalKeyText renders a decoded key the way [Change.Key] wants it.
//
// It accepts more than [reseedKeyText] does, and the asymmetry is not
// an oversight: a reseed walks a key *range*, which only an integer
// key supports, while a stream is handed whatever the table's key
// actually is — and uuid and text keys are ordinary. What both refuse
// is a value with no unambiguous text form, because a key that
// renders two ways addresses two different mirrored rows.
//
// A float is refused for exactly that reason. So is a struct a
// decoder happened to produce: fmt would give it a shape, and that
// shape is the decoder's business rather than a stable identity.
func logicalKeyText(v any) (string, bool) {
	switch k := v.(type) {
	case string:
		if k == "" {
			return "", false
		}
		return k, true
	case []byte:
		if len(k) == 0 {
			return "", false
		}
		return string(k), true
	case int64:
		return strconv.FormatInt(k, 10), true
	case int32:
		return strconv.FormatInt(int64(k), 10), true
	case int16:
		return strconv.FormatInt(int64(k), 10), true
	case int:
		return strconv.FormatInt(int64(k), 10), true
	case uint64:
		return strconv.FormatUint(k, 10), true
	case uint32:
		return strconv.FormatUint(uint64(k), 10), true
	case fmt.Stringer:
		// A driver's own uuid type is the case this is for: it has
		// one canonical text form and says so.
		s := k.String()
		if strings.TrimSpace(s) == "" {
			return "", false
		}
		return s, true
	default:
		return "", false
	}
}
