package mysql_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/mysql"
)

// --- a driver that records statements and replays scripted rows ------

type obDriver struct {
	queries []string
	args    [][]any

	// rowsFor answers a query with a scripted result set. A nil
	// result means "no rows".
	rowsFor func(sql string) [][]any

	// failFor lets a test make one statement fail, keyed by a
	// substring of its SQL.
	failFor map[string]error

	lastID int64
}

func (d *obDriver) record(sql string, args []any) {
	d.queries = append(d.queries, sql)
	d.args = append(d.args, args)
}

func (d *obDriver) fail(sql string) error {
	for frag, err := range d.failFor {
		if strings.Contains(sql, frag) {
			return err
		}
	}
	return nil
}

func (d *obDriver) Exec(_ context.Context, sql string, args ...any) (drops.Result, error) {
	d.record(sql, args)
	if err := d.fail(sql); err != nil {
		return nil, err
	}
	return obResult{id: d.lastID}, nil
}

func (d *obDriver) Query(_ context.Context, sql string, args ...any) (drops.Rows, error) {
	d.record(sql, args)
	if err := d.fail(sql); err != nil {
		return nil, err
	}
	var data [][]any
	if d.rowsFor != nil {
		data = d.rowsFor(sql)
	}
	return &obRows{data: data}, nil
}

func (d *obDriver) Begin(context.Context) (drops.Tx, error) { return &obTx{d}, nil }

// queryContaining returns the first recorded statement holding frag,
// with its arguments.
func (d *obDriver) queryContaining(t *testing.T, frag string) (string, []any) {
	t.Helper()
	for i, q := range d.queries {
		if strings.Contains(q, frag) {
			return q, d.args[i]
		}
	}
	t.Fatalf("no statement contained %q; got:\n%s", frag, strings.Join(d.queries, "\n---\n"))
	return "", nil
}

func (d *obDriver) countContaining(frag string) int {
	n := 0
	for _, q := range d.queries {
		if strings.Contains(q, frag) {
			n++
		}
	}
	return n
}

type obTx struct{ *obDriver }

func (*obTx) Commit(context.Context) error   { return nil }
func (*obTx) Rollback(context.Context) error { return nil }

type obResult struct{ id int64 }

func (obResult) RowsAffected() (int64, error)   { return 1, nil }
func (r obResult) LastInsertId() (int64, error) { return r.id, nil }

type obRows struct {
	data [][]any
	pos  int
	cur  []any
}

func (r *obRows) Next() bool {
	if r.pos >= len(r.data) {
		return false
	}
	r.cur = r.data[r.pos]
	r.pos++
	return true
}

// Scan assigns by reflection so the fake covers every destination the
// production scanners use — including the **string ones the nullable
// columns need.
func (r *obRows) Scan(dest ...any) error {
	if len(dest) != len(r.cur) {
		return fmt.Errorf("scan into %d destinations, row has %d values", len(dest), len(r.cur))
	}
	for i, d := range dest {
		dv := reflect.ValueOf(d).Elem()
		sv := reflect.ValueOf(r.cur[i])
		if !sv.IsValid() {
			dv.Set(reflect.Zero(dv.Type()))
			continue
		}
		if !sv.Type().AssignableTo(dv.Type()) {
			return fmt.Errorf("cannot assign %s to %s", sv.Type(), dv.Type())
		}
		dv.Set(sv)
	}
	return nil
}

func (*obRows) Columns() ([]string, error) { return nil, nil }
func (*obRows) Close() error               { return nil }
func (*obRows) Err() error                 { return nil }

func ptr[T any](v T) *T { return &v }

// --- schema ------------------------------------------------------------

// The DDL is where the two collation decisions live, so it is worth
// pinning: MySQL's default collation is case-insensitive, and an
// aggregate id or an idempotency key that folds case is a different
// key than the caller wrote.
func TestOutboxTableDDL(t *testing.T) {
	tbl := mysql.NewOutboxTable("outbox")
	stmts := mysql.OutboxDDL(tbl)
	if len(stmts) != 3 {
		t.Fatalf("want CREATE TABLE + 2 indexes, got %d statements", len(stmts))
	}
	create, _ := sqlOf(stmts[0])
	for _, want := range []string{
		"`kind` VARCHAR(255) NOT NULL",
		"`aggregateID` VARCHAR(255)",
		// The binary collation is declared once on the table rather
		// than on each column: a column-level COLLATE does not come
		// back out of information_schema the way it went in, so Diff
		// would MODIFY the column on every push.
		"COLLATE=utf8mb4_bin",
		"`createdAt` DATETIME(6) NOT NULL",
		"`payload` JSON NOT NULL",
		"PRIMARY KEY (`id`)",
		"AUTO_INCREMENT",
	} {
		if !strings.Contains(create, want) {
			t.Errorf("CREATE TABLE is missing %q:\n%s", want, create)
		}
	}
	// No column may carry a server-side default for a timestamp:
	// drops writes them from Go so nothing is compared against the
	// server's clock.
	if strings.Contains(create, "CURRENT_TIMESTAMP") {
		t.Errorf("timestamps must not default to the server clock:\n%s", create)
	}

	// MySQL has no partial index, so "only the pending rows" is
	// expressed by leading with the column the predicate tests for
	// NULL — which is also what keeps ORDER BY id free of a sort.
	drain, _ := sqlOf(stmts[1])
	if !strings.Contains(drain, "(`publishedAt`, `id`)") {
		t.Errorf("drain index should lead with publishedAt: %s", drain)
	}
	agg, _ := sqlOf(stmts[2])
	if !strings.Contains(agg, "(`publishedAt`, `aggregateType`, `aggregateID`, `id`)") {
		t.Errorf("aggregate index shape: %s", agg)
	}
}

// An index name is the table's name plus a suffix, and MySQL stops at
// 64 bytes — so a long but legal table name must not produce an
// illegal index name.
func TestOutboxIndexNamesFitTheIdentifierLimit(t *testing.T) {
	long := strings.Repeat("t", 60)
	names := map[string]bool{}
	for _, stmt := range mysql.OutboxDDL(mysql.NewOutboxTable(long))[1:] {
		sql, _ := sqlOf(stmt)
		name := strings.SplitN(strings.TrimPrefix(sql, "CREATE INDEX `"), "`", 2)[0]
		if len(name) > 64 {
			t.Errorf("index name is %d bytes: %q", len(name), name)
		}
		names[name] = true
	}
	if len(names) != 2 {
		t.Errorf("the two indexes collapsed onto one name: %v", names)
	}
}

// --- emit --------------------------------------------------------------

func TestOutboxEmitSQL(t *testing.T) {
	d := &obDriver{}
	ob := mysql.NewOutbox(mysql.New(d), "outbox")
	if err := ob.Emit(mysql.New(d), context.Background(), "user.created", map[string]int{"id": 1}); err != nil {
		t.Fatal(err)
	}
	q, args := d.queryContaining(t, "INSERT INTO `outbox`")
	if strings.Count(q, "?") != 7 {
		t.Errorf("emit should bind 7 columns:\n%s", q)
	}
	if len(args) != 7 {
		t.Fatalf("want 7 args, got %d", len(args))
	}
	if args[1] != nil || args[2] != nil {
		t.Errorf("an unset aggregate must be NULL, got %v / %v", args[1], args[2])
	}
	if _, ok := args[5].(time.Time); !ok {
		t.Errorf("createdAt should be bound from Go, got %T", args[5])
	}
	if string(args[3].(json.RawMessage)) != `{"id":1}` {
		t.Errorf("payload = %s", args[3])
	}
}

func TestOutboxEmitRejectsEmptyKind(t *testing.T) {
	d := &obDriver{}
	ob := mysql.NewOutbox(mysql.New(d), "outbox")
	if err := ob.Emit(mysql.New(d), context.Background(), "", nil); err == nil {
		t.Fatal("an empty kind should be refused")
	}
}

// MySQL has no RETURNING; the id of the row just emitted comes from
// the driver's LastInsertId, and one row per statement is what makes
// that unambiguous.
func TestOutboxEmitWithIDReadsLastInsertID(t *testing.T) {
	d := &obDriver{lastID: 4242}
	ob := mysql.NewOutbox(mysql.New(d), "outbox")
	id, err := ob.EmitWithID(mysql.New(d), context.Background(), "user.created", nil, mysql.EmitOptions{
		AggregateType: "user", AggregateID: "7",
		Headers: map[string]string{"traceparent": "abc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != 4242 {
		t.Errorf("id = %d, want the driver's LastInsertId", id)
	}
	_, args := d.queryContaining(t, "INSERT INTO `outbox`")
	if args[1] != "user" || args[2] != "7" {
		t.Errorf("aggregate args = %v / %v", args[1], args[2])
	}
	if string(args[4].(json.RawMessage)) != `{"traceparent":"abc"}` {
		t.Errorf("headers = %s", args[4])
	}
}

// --- drain -------------------------------------------------------------

func TestDrainLockingModes(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode mysql.DrainLocking
		want string
		deny string
	}{
		{"default is skip locked", mysql.LockSkipLocked, "FOR UPDATE SKIP LOCKED", ""},
		{"blocking fallback", mysql.LockForUpdate, "FOR UPDATE", "SKIP LOCKED"},
		{"single worker", mysql.LockNone, "LIMIT ?", "FOR UPDATE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &obDriver{}
			ob := mysql.NewOutbox(mysql.New(d), "outbox").WithLocking(tc.mode)
			if _, err := ob.Drain(context.Background(), 10); err != nil {
				t.Fatal(err)
			}
			q := d.queries[0]
			if !strings.Contains(q, tc.want) {
				t.Errorf("want %q in:\n%s", tc.want, q)
			}
			if tc.deny != "" && strings.Contains(q, tc.deny) {
				t.Errorf("did not want %q in:\n%s", tc.deny, q)
			}
			if ob.Locking() != tc.mode {
				t.Errorf("Locking() = %v", ob.Locking())
			}
		})
	}
}

// A server that cannot parse SKIP LOCKED must say so. Degrading to an
// unlocked drain would deliver every event twice; degrading silently
// to a blocking one would hide a deployment on a server drops does not
// really support.
func TestDrainReportsMissingSkipLocked(t *testing.T) {
	d := &obDriver{failFor: map[string]error{
		"SKIP LOCKED": errors.New("Error 1064 (42000): You have an error in your SQL syntax; check the manual ... near 'SKIP LOCKED' at line 6"),
	}}
	ob := mysql.NewOutbox(mysql.New(d), "outbox")
	_, err := ob.Drain(context.Background(), 10)
	if !errors.Is(err, mysql.ErrSkipLockedUnsupported) {
		t.Fatalf("err = %v, want ErrSkipLockedUnsupported", err)
	}
	if !strings.Contains(err.Error(), "ProbeLocking") {
		t.Errorf("the error should name the way out: %v", err)
	}
}

// A syntax error about something else is not a SKIP LOCKED problem and
// must not be relabelled as one.
func TestDrainPassesThroughUnrelatedSyntaxErrors(t *testing.T) {
	boom := errors.New("Error 1064 (42000): You have an error in your SQL syntax near 'FROM'")
	d := &obDriver{failFor: map[string]error{"SELECT": boom}}
	ob := mysql.NewOutbox(mysql.New(d), "outbox")
	_, err := ob.Drain(context.Background(), 10)
	if errors.Is(err, mysql.ErrSkipLockedUnsupported) {
		t.Fatalf("unrelated 1064 was relabelled: %v", err)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the driver's error", err)
	}
}

func TestProbeLockingFallsBackToBlocking(t *testing.T) {
	d := &obDriver{failFor: map[string]error{
		"SKIP LOCKED": errors.New("Error 1064 (42000): syntax error near 'SKIP LOCKED'"),
	}}
	ob := mysql.NewOutbox(mysql.New(d), "outbox")
	mode, err := ob.ProbeLocking(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if mode != mysql.LockForUpdate {
		t.Errorf("mode = %v, want LockForUpdate — never LockNone, which would duplicate deliveries", mode)
	}
	if ob.Locking() != mysql.LockForUpdate {
		t.Errorf("ProbeLocking should also set the mode, got %v", ob.Locking())
	}
	if _, err := ob.Drain(context.Background(), 1); err != nil {
		t.Fatalf("the drain after a probe should work: %v", err)
	}
}

func TestProbeLockingKeepsSkipLockedWhenSupported(t *testing.T) {
	d := &obDriver{}
	ob := mysql.NewOutbox(mysql.New(d), "outbox")
	mode, err := ob.ProbeLocking(context.Background())
	if err != nil || mode != mysql.LockSkipLocked {
		t.Fatalf("mode = %v, err = %v", mode, err)
	}
}

func TestDrainScansNullableColumns(t *testing.T) {
	created := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	d := &obDriver{rowsFor: func(string) [][]any {
		return [][]any{
			{int64(1), "user.created", ptr("user"), ptr("7"),
				json.RawMessage(`{"a":1}`), []byte(`{"traceparent":"tp"}`), 2, ptr("boom"), created},
			{int64(2), "other", (*string)(nil), (*string)(nil),
				json.RawMessage(`null`), []byte(nil), 0, (*string)(nil), created},
		}
	}}
	ob := mysql.NewOutbox(mysql.New(d), "outbox")
	events, err := ob.Drain(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events", len(events))
	}
	if events[0].AggregateType != "user" || events[0].AggregateID != "7" {
		t.Errorf("aggregate = %q / %q", events[0].AggregateType, events[0].AggregateID)
	}
	if events[0].Headers["traceparent"] != "tp" {
		t.Errorf("headers = %v", events[0].Headers)
	}
	if events[0].LastError != "boom" || events[0].Attempts != 2 {
		t.Errorf("failure state = %q / %d", events[0].LastError, events[0].Attempts)
	}
	if events[1].AggregateID != "" || events[1].Headers != nil {
		t.Errorf("NULL columns should scan to zero values: %+v", events[1])
	}
}

// --- per-aggregate drain -----------------------------------------------

func TestDrainAggregateTakesAndReleasesTheLock(t *testing.T) {
	d := &obDriver{rowsFor: func(sql string) [][]any {
		if strings.Contains(sql, "GET_LOCK") {
			return [][]any{{ptr(int64(1))}}
		}
		return [][]any{{int64(1), "k", ptr("cart"), ptr("7"),
			json.RawMessage(`{}`), []byte(nil), 0, (*string)(nil), time.Now()}}
	}}
	ob := mysql.NewOutbox(mysql.New(d), "outbox")
	called := false
	err := ob.DrainAggregate(context.Background(), "cart", "7", 10, func(tx *mysql.DB, events []mysql.OutboxEvent) error {
		called = true
		if len(events) != 1 {
			t.Errorf("got %d events", len(events))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Error("the callback should have run")
	}
	_, args := d.queryContaining(t, "GET_LOCK")
	if args[0] != "drops:outbox:cart:7" {
		t.Errorf("lock name = %v", args[0])
	}
	// The lock belongs to the session, not the transaction, so
	// committing does not release it — the code must.
	if d.countContaining("RELEASE_LOCK") != 1 {
		t.Errorf("the named lock was not released:\n%s", strings.Join(d.queries, "\n"))
	}
	// The aggregate predicate has to be NULL-safe: an event emitted
	// without an aggregate type still belongs to its id's stream.
	q, _ := d.queryContaining(t, "aggregateID` = ?")
	if !strings.Contains(q, "`aggregateType` <=> ?") {
		t.Errorf("want MySQL's NULL-safe equality:\n%s", q)
	}
}

func TestDrainAggregateReleasesTheLockWhenTheCallbackFails(t *testing.T) {
	d := &obDriver{rowsFor: func(sql string) [][]any {
		if strings.Contains(sql, "GET_LOCK") {
			return [][]any{{ptr(int64(1))}}
		}
		return [][]any{{int64(1), "k", ptr("cart"), ptr("7"),
			json.RawMessage(`{}`), []byte(nil), 0, (*string)(nil), time.Now()}}
	}}
	ob := mysql.NewOutbox(mysql.New(d), "outbox")
	boom := errors.New("handler exploded")
	err := ob.DrainAggregate(context.Background(), "cart", "7", 10, func(*mysql.DB, []mysql.OutboxEvent) error {
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if d.countContaining("RELEASE_LOCK") != 1 {
		t.Error("a failing callback must still release the lock")
	}
}

func TestDrainAggregateSkipsWhenAnotherWorkerHoldsTheLock(t *testing.T) {
	d := &obDriver{rowsFor: func(sql string) [][]any {
		if strings.Contains(sql, "GET_LOCK") {
			return [][]any{{ptr(int64(0))}}
		}
		t.Error("no rows should be read without the lock")
		return nil
	}}
	ob := mysql.NewOutbox(mysql.New(d), "outbox")
	err := ob.DrainAggregate(context.Background(), "cart", "7", 10, func(*mysql.DB, []mysql.OutboxEvent) error {
		t.Error("the callback must not run without the lock")
		return nil
	})
	if err != nil {
		t.Fatalf("a missed lock is not an error: %v", err)
	}
	if d.countContaining("RELEASE_LOCK") != 0 {
		t.Error("a lock we never took must not be released")
	}
}

// GET_LOCK answers NULL on a server-side failure. That is neither an
// acquisition nor a clean miss, and treating it as a miss would skip
// the aggregate silently forever.
func TestDrainAggregateReportsANullLockAnswer(t *testing.T) {
	d := &obDriver{rowsFor: func(sql string) [][]any {
		if strings.Contains(sql, "GET_LOCK") {
			return [][]any{{(*int64)(nil)}}
		}
		return nil
	}}
	ob := mysql.NewOutbox(mysql.New(d), "outbox")
	err := ob.DrainAggregate(context.Background(), "cart", "7", 10, func(*mysql.DB, []mysql.OutboxEvent) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "GET_LOCK") {
		t.Fatalf("err = %v, want a GET_LOCK failure", err)
	}
}

// MySQL 8.0 rejects a lock name over 64 characters outright, where
// MariaDB accepts it — so the name is folded rather than passed on.
func TestAggregateLockNameStaysWithinMySQLsLimit(t *testing.T) {
	long := strings.Repeat("a", 200)
	names := map[string]bool{}
	for _, id := range []string{long, long + "b"} {
		d := &obDriver{rowsFor: func(sql string) [][]any {
			if strings.Contains(sql, "GET_LOCK") {
				return [][]any{{ptr(int64(0))}}
			}
			return nil
		}}
		ob := mysql.NewOutbox(mysql.New(d), "outbox")
		if err := ob.DrainAggregate(context.Background(), "cart", id, 1, nil); err != nil {
			t.Fatal(err)
		}
		_, args := d.queryContaining(t, "GET_LOCK")
		name, _ := args[0].(string)
		if len(name) > 64 {
			t.Fatalf("lock name is %d bytes: %q", len(name), name)
		}
		names[name] = true
	}
	if len(names) != 2 {
		t.Errorf("two aggregates collapsed onto one lock name: %v", names)
	}
}

func TestDrainAggregateRequiresAnID(t *testing.T) {
	ob := mysql.NewOutbox(mysql.New(&obDriver{}), "outbox")
	if err := ob.DrainAggregate(context.Background(), "cart", "", 1, nil); err == nil {
		t.Fatal("an empty aggregate id should be refused")
	}
}

// --- marking and cleanup ------------------------------------------------

func TestMarkPublishedExpandsAnINList(t *testing.T) {
	d := &obDriver{}
	ob := mysql.NewOutbox(mysql.New(d), "outbox")
	if err := ob.MarkPublished(context.Background(), 1, 2, 3); err != nil {
		t.Fatal(err)
	}
	q, args := d.queryContaining(t, "UPDATE")
	// MySQL has no array type, so drops/pg's "= ANY($1)" becomes an
	// expanded IN list.
	if !strings.Contains(q, "WHERE `id` IN (?, ?, ?)") {
		t.Errorf("markPublished SQL:\n%s", q)
	}
	if len(args) != 4 {
		t.Errorf("want publishedAt + 3 ids, got %d args", len(args))
	}
}

func TestMarkPublishedWithNoIDsIsANoOp(t *testing.T) {
	d := &obDriver{}
	ob := mysql.NewOutbox(mysql.New(d), "outbox")
	if err := ob.MarkPublished(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(d.queries) != 0 {
		t.Errorf("no ids should issue no statement: %v", d.queries)
	}
}

func TestMarkFailed(t *testing.T) {
	d := &obDriver{}
	ob := mysql.NewOutbox(mysql.New(d), "outbox")
	ctx := context.Background()

	if err := ob.MarkFailed(ctx, 5, 3, time.Time{}, "boom"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.queries[0], "`failedAt`") {
		t.Errorf("a terminal failure parks the row:\n%s", d.queries[0])
	}
	if err := ob.MarkFailed(ctx, 5, 1, time.Now().Add(time.Minute), "boom"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.queries[1], "`availableAt`") || strings.Contains(d.queries[1], "`failedAt`") {
		t.Errorf("a retry reschedules instead:\n%s", d.queries[1])
	}
}

// Cleanup keeps the newest row so the AUTO_INCREMENT counter has a
// floor it cannot fall below — see the method's doc for why an outbox
// emptied to zero rows can hand out an id that was already delivered.
func TestCleanupKeepsTheNewestRow(t *testing.T) {
	d := &obDriver{rowsFor: func(sql string) [][]any {
		if strings.Contains(sql, "MAX(`id`)") {
			return [][]any{{int64(97)}}
		}
		return nil
	}}
	ob := mysql.NewOutbox(mysql.New(d), "outbox")
	if _, err := ob.Cleanup(context.Background(), time.Hour); err != nil {
		t.Fatal(err)
	}
	q, args := d.queryContaining(t, "DELETE")
	if !strings.Contains(q, "`id` < ?") {
		t.Errorf("the delete should stop below the newest id:\n%s", q)
	}
	if args[1] != int64(97) {
		t.Errorf("floor = %v, want the table's MAX(id)", args[1])
	}
}

func TestCleanupOnAnEmptyTableDeletesNothing(t *testing.T) {
	d := &obDriver{rowsFor: func(sql string) [][]any {
		if strings.Contains(sql, "MAX(`id`)") {
			return [][]any{{int64(0)}}
		}
		return nil
	}}
	ob := mysql.NewOutbox(mysql.New(d), "outbox")
	n, err := ob.Cleanup(context.Background(), 0)
	if err != nil || n != 0 {
		t.Fatalf("n = %d, err = %v", n, err)
	}
	if d.countContaining("DELETE") != 0 {
		t.Error("an empty table needs no DELETE")
	}
}

// --- worker -------------------------------------------------------------

func oneEventDriver(attempts int) *obDriver {
	return &obDriver{rowsFor: func(sql string) [][]any {
		if strings.Contains(sql, "SELECT `id`, `kind`") {
			return [][]any{{int64(11), "user.created", (*string)(nil), (*string)(nil),
				json.RawMessage(`{}`), []byte(nil), attempts, (*string)(nil), time.Now()}}
		}
		return nil
	}}
}

func TestWorkerMarksPublishedOnSuccess(t *testing.T) {
	d := oneEventDriver(0)
	ob := mysql.NewOutbox(mysql.New(d), "outbox")
	seen := 0
	w := mysql.NewOutboxWorker(ob).OnEvent(func(context.Context, mysql.OutboxEvent) error {
		seen++
		return nil
	})
	if err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seen != 1 {
		t.Errorf("handler ran %d times", seen)
	}
	q, _ := d.queryContaining(t, "SET `publishedAt`")
	if !strings.Contains(q, "IN (?)") {
		t.Errorf("publish SQL:\n%s", q)
	}
}

func TestWorkerReschedulesAFailure(t *testing.T) {
	d := oneEventDriver(0)
	ob := mysql.NewOutbox(mysql.New(d), "outbox")
	w := mysql.NewOutboxWorker(ob).OnEvent(func(context.Context, mysql.OutboxEvent) error {
		return errors.New("nope")
	})
	if err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	q, args := d.queryContaining(t, "SET `attempts`")
	if !strings.Contains(q, "`availableAt`") {
		t.Errorf("a retryable failure reschedules:\n%s", q)
	}
	if args[0] != 1 {
		t.Errorf("attempts = %v, want 1", args[0])
	}
}

func TestWorkerParksAnEventAtMaxAttempts(t *testing.T) {
	d := oneEventDriver(2)
	ob := mysql.NewOutbox(mysql.New(d), "outbox")
	w := mysql.NewOutboxWorker(ob).WithMaxAttempts(3).OnEvent(func(context.Context, mysql.OutboxEvent) error {
		return errors.New("nope")
	})
	if err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	q, _ := d.queryContaining(t, "SET `attempts`")
	if !strings.Contains(q, "`failedAt`") {
		t.Errorf("the third failure of three should park the row:\n%s", q)
	}
}

func TestWorkerBatchHandlerPublishesTheWholeBatch(t *testing.T) {
	d := oneEventDriver(0)
	ob := mysql.NewOutbox(mysql.New(d), "outbox")
	got := 0
	w := mysql.NewOutboxWorker(ob).OnBatch(func(_ context.Context, events []mysql.OutboxEvent) error {
		got = len(events)
		return nil
	})
	if err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("batch size = %d", got)
	}
	if d.countContaining("SET `publishedAt`") != 1 {
		t.Error("a successful batch is published in one statement")
	}
}

func TestWorkerWithoutAHandler(t *testing.T) {
	w := mysql.NewOutboxWorker(mysql.NewOutbox(mysql.New(&obDriver{}), "outbox"))
	if err := w.Run(context.Background()); !errors.Is(err, mysql.ErrNoHandler) {
		t.Fatalf("err = %v, want ErrNoHandler", err)
	}
	if err := w.Tick(context.Background()); !errors.Is(err, mysql.ErrNoHandler) {
		t.Fatalf("err = %v, want ErrNoHandler", err)
	}
}

func TestWorkerRunStopsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := mysql.NewOutboxWorker(mysql.NewOutbox(mysql.New(&obDriver{}), "outbox")).
		WithInterval(time.Millisecond).
		OnEvent(func(context.Context, mysql.OutboxEvent) error { return nil })
	if err := w.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// The two drain indexes have to reach the server by whichever route
// the application uses. OutboxDDL renders them as statements; a
// Schema-and-Push deployment never calls it, so the table has to carry
// them itself. That they are the same two is the point — a table
// pushed and a table created by hand must not differ.
func TestOutboxTableCarriesItsIndexesForPush(t *testing.T) {
	tbl := mysql.NewOutboxTable("outbox")
	registered := tbl.Indexes()
	if len(registered) != 2 {
		t.Fatalf("the table registered %d indexes; Push would create that many", len(registered))
	}
	fromDDL := mysql.OutboxDDL(tbl)[1:]
	if len(fromDDL) != len(registered) {
		t.Fatalf("OutboxDDL renders %d indexes, the table carries %d", len(fromDDL), len(registered))
	}
	for i := range registered {
		want, _ := sqlOf(fromDDL[i])
		got, _ := sqlOf(registered[i])
		if want != got {
			t.Errorf("index %d differs between the two routes:\n%s\n%s", i, want, got)
		}
	}
}

// The fold that keeps a lock name inside MySQL 8's 64-character limit
// cuts bytes, so an aggregate id of non-ASCII text can be sliced
// mid-rune. MariaDB accepts the mangled name and MySQL's own limit is
// stated in characters, so nothing here would notice — hence the
// check on the name itself.
func TestAggregateLockNameStaysValidUTF8(t *testing.T) {
	for n := 1; n <= 80; n++ {
		id := strings.Repeat("世", n)
		d := &obDriver{rowsFor: func(sql string) [][]any {
			if strings.Contains(sql, "GET_LOCK") {
				return [][]any{{ptr(int64(0))}}
			}
			return nil
		}}
		ob := mysql.NewOutbox(mysql.New(d), "outbox")
		if err := ob.DrainAggregate(context.Background(), "cart", id, 1, nil); err != nil {
			t.Fatal(err)
		}
		_, args := d.queryContaining(t, "GET_LOCK")
		name, _ := args[0].(string)
		if len(name) > 64 {
			t.Fatalf("n=%d: lock name is %d bytes", n, len(name))
		}
		if !utf8.ValidString(name) {
			t.Fatalf("n=%d: lock name is not valid UTF-8: %q", n, name)
		}
	}
}
