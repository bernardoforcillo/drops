package integration_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/pg"
)

// A [pg.ReplicationStream] over PostgreSQL's SQL interface to logical
// decoding.
//
// The real thing needs a connection opened in replication mode and a
// decoder for the wire format, both a driver's job. But the server
// exposes the same decoding over ordinary SQL, and the pair of
// functions it exposes has exactly the shape the interface asks for:
//
//	pg_logical_slot_peek_changes    reads without consuming  → Next
//	pg_replication_slot_advance     confirms a position      → Ack
//
// Peek rather than get, deliberately. `pg_logical_slot_get_changes`
// consumes as it reads, which would make every read an
// acknowledgement and hide the exact property these tests are here to
// check: that nothing is confirmed until the consumer says so.
//
// The output plugin is `test_decoding`, which ships with every
// PostgreSQL build. Its format is text and documented as unstable
// between versions — fine for a test fixture, which is all this is.
// A production consumer uses pgoutput through a driver that speaks the
// replication protocol.

var errStopTest = errors.New("integration: stop the stream")

// errStreamDrained ends a read once the slot has stayed empty for
// idleFor. A real stream blocks on its socket indefinitely, which is
// right in production and wrong in a test: a test that has seen
// everything it wrote wants to stop, and stopping on a context
// deadline would add that deadline to every run.
var errStreamDrained = errors.New("integration: nothing left in the slot")

// idleFor is how long the slot must stay empty before a read gives up.
// Generous enough to cover a concurrent writer's commit and short
// enough that a drained test does not sit there.
const idleFor = 400 * time.Millisecond

type sqlStream struct {
	db     *pg.DB
	slot   string
	tables map[string]bool

	// mu serialises access to the slot. PostgreSQL allows one session
	// on a replication slot at a time and refuses a second with
	// SQLSTATE 55006, so a peek from the reader and an advance from
	// the consumer cannot overlap — which they otherwise would, since
	// mirror.LogicalSource reads on its own goroutine and the pump
	// acknowledges on another. A real stream is one connection and
	// has the property for free.
	mu  sync.Mutex
	buf []pg.Message
	i   int

	// off is how many decoded rows have been delivered since the last
	// acknowledgement. Peeking does not consume, so every peek
	// returns the same set from the confirmed position onwards, and
	// without this a reader that never acknowledges — which is
	// exactly what ReassembleDeferred is — would replay the same
	// messages forever. A real stream is a socket and has no such
	// problem.
	off  int
	acks int

	closed bool
}

// newSQLStream returns a stream over slot. Only changes to the named
// tables are surfaced; the suite shares one database and another
// test's writes would otherwise arrive in the middle of this one's
// transactions.
func newSQLStream(db *pg.DB, slot string, tables ...string) *sqlStream {
	set := make(map[string]bool, len(tables))
	for _, t := range tables {
		set[t] = true
	}
	return &sqlStream{db: db, slot: slot, tables: set}
}

func (s *sqlStream) Close() error { s.closed = true; return nil }

// Next returns the next decoded message, polling the slot until one
// appears or ctx ends.
func (s *sqlStream) Next(ctx context.Context) (pg.Message, error) {
	idleSince := time.Time{}
	for {
		s.mu.Lock()
		if s.i < len(s.buf) {
			m := s.buf[s.i]
			s.i++
			s.mu.Unlock()
			return m, nil
		}
		s.mu.Unlock()

		if err := ctx.Err(); err != nil {
			return pg.Message{}, err
		}
		s.mu.Lock()
		msgs, err := s.fetch(ctx, s.off+200)
		if err == nil {
			if len(msgs) > s.off {
				msgs = msgs[s.off:]
			} else {
				msgs = nil
			}
			if len(msgs) > 0 {
				s.buf, s.i = msgs, 0
				s.off += len(msgs)
			}
		}
		s.mu.Unlock()
		if err != nil {
			return pg.Message{}, err
		}
		if len(msgs) > 0 {
			idleSince = time.Time{}
			continue
		}

		// Nothing pending. A real stream would block on the socket;
		// here a short sleep keeps the poll from spinning while a
		// concurrent writer commits, and idleFor decides when there
		// is nothing more coming.
		if idleSince.IsZero() {
			idleSince = time.Now()
		} else if time.Since(idleSince) > idleFor {
			return pg.Message{}, errStreamDrained
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			return pg.Message{}, ctx.Err()
		}
	}
}

// Ack confirms a position with the server, which is what releases the
// WAL behind it.
func (s *sqlStream) Ack(ctx context.Context, lsn uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acks++
	_, err := s.db.Exec(ctx,
		`SELECT pg_replication_slot_advance($1, $2::pg_lsn)`, s.slot, pg.FormatLSN(lsn))
	if err != nil {
		return err
	}
	// Everything up to here is confirmed, so anything still buffered
	// from before it has been superseded, and the skip count starts
	// again from the new confirmed position.
	s.buf, s.i, s.off = nil, 0, 0
	return nil
}

// peek returns what the slot currently holds without consuming it, for
// a test that wants to assert on the pending set directly.
func (s *sqlStream) peek(t *testing.T, limit int) []pg.Message {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	msgs, err := s.fetch(context.Background(), limit)
	if err != nil {
		t.Fatalf("peeking the slot: %v", err)
	}
	return msgs
}

func (s *sqlStream) fetch(ctx context.Context, limit int) ([]pg.Message, error) {
	rows, err := s.db.Query(ctx,
		`SELECT lsn::text, xid::text, data
		 FROM pg_logical_slot_peek_changes($1, NULL, $2)`, s.slot, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []pg.Message
	for rows.Next() {
		var lsnText, xidText, data string
		if err := rows.Scan(&lsnText, &xidText, &data); err != nil {
			return nil, err
		}
		lsn, err := pg.ParseLSN(lsnText)
		if err != nil {
			return nil, err
		}
		msg, ok, err := s.parse(lsn, xidText, data)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, msg)
		}
	}
	return out, rows.Err()
}

// parse turns one test_decoding line into a message. It reports ok
// false for a change to a table this stream is not watching — the
// BEGIN and COMMIT around it still travel, which is why a transaction
// containing only such changes arrives empty rather than not at all.
func (s *sqlStream) parse(lsn uint64, xidText, data string) (pg.Message, bool, error) {
	var xid uint32
	fmt.Sscanf(xidText, "%d", &xid)

	switch {
	case strings.HasPrefix(data, "BEGIN"):
		return pg.Message{Kind: pg.MessageBegin, LSN: lsn, XID: xid}, true, nil
	case strings.HasPrefix(data, "COMMIT"):
		return pg.Message{Kind: pg.MessageCommit, LSN: lsn, XID: xid}, true, nil
	case !strings.HasPrefix(data, "table "):
		// test_decoding also emits "message:" lines for logical
		// messages, which nothing here produces.
		return pg.Message{}, false, nil
	}

	// table public.name: OP: col[type]:value ...
	rest := strings.TrimPrefix(data, "table ")
	colon := strings.Index(rest, ": ")
	if colon < 0 {
		return pg.Message{}, false, fmt.Errorf("integration: unparsable change line %q", data)
	}
	qualified := rest[:colon]
	rest = rest[colon+2:]

	schema, table := "public", qualified
	if dot := strings.IndexByte(qualified, '.'); dot >= 0 {
		schema, table = qualified[:dot], qualified[dot+1:]
	}
	if len(s.tables) > 0 && !s.tables[table] {
		return pg.Message{}, false, nil
	}

	colon = strings.Index(rest, ": ")
	if colon < 0 {
		return pg.Message{}, false, fmt.Errorf("integration: unparsable change line %q", data)
	}
	verb := rest[:colon]
	body := rest[colon+2:]

	msg := pg.Message{Kind: pg.MessageChange, LSN: lsn, XID: xid, Schema: schema, Table: table}
	switch verb {
	case "INSERT":
		msg.Op = pg.OpInsert
		msg.New = parseTuple(body)
	case "UPDATE":
		msg.Op = pg.OpUpdate
		// With REPLICA IDENTITY FULL the line is
		// "old-key: <tuple> new-tuple: <tuple>"; with the default
		// identity it is just the new tuple.
		if i := strings.Index(body, " new-tuple: "); i >= 0 && strings.HasPrefix(body, "old-key: ") {
			msg.Old = parseTuple(strings.TrimPrefix(body[:i], "old-key: "))
			msg.New = parseTuple(body[i+len(" new-tuple: "):])
		} else {
			msg.New = parseTuple(body)
		}
	case "DELETE":
		msg.Op = pg.OpDelete
		msg.Old = parseTuple(body)
	default:
		return pg.Message{}, false, fmt.Errorf("integration: unknown operation %q in %q", verb, data)
	}
	return msg, true, nil
}

// parseTuple reads "name[type]:value name[type]:value ..." into a map.
//
// Values arrive as text: quoted with ” doubling for anything
// string-like, bare for numbers and null. Everything is kept as a Go
// string, which is what test_decoding gives and enough for the
// assertions here — a production decoder hands back typed values, and
// mirror's key coercion accepts both.
func parseTuple(s string) map[string]any {
	out := map[string]any{}
	i := 0
	for i < len(s) {
		for i < len(s) && s[i] == ' ' {
			i++
		}
		open := strings.IndexByte(s[i:], '[')
		if open < 0 {
			break
		}
		name := s[i : i+open]
		i += open + 1
		close := strings.IndexByte(s[i:], ']')
		if close < 0 {
			break
		}
		i += close + 1
		if i >= len(s) || s[i] != ':' {
			break
		}
		i++

		if i < len(s) && s[i] == '\'' {
			i++
			var b strings.Builder
			for i < len(s) {
				if s[i] == '\'' {
					if i+1 < len(s) && s[i+1] == '\'' {
						b.WriteByte('\'')
						i += 2
						continue
					}
					i++
					break
				}
				b.WriteByte(s[i])
				i++
			}
			out[name] = b.String()
			continue
		}
		start := i
		for i < len(s) && s[i] != ' ' {
			i++
		}
		v := s[start:i]
		if v == "null" {
			out[name] = nil
		} else {
			out[name] = v
		}
	}
	return out
}
