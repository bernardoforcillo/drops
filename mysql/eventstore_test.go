package mysql_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/mysql"
)

// The whole optimistic-concurrency contract rests on this table
// definition, so it is pinned here: one CREATE TABLE has to be enough
// to make a second writer at the same version fail.
func TestEventStoreTableEnforcesTheStreamKey(t *testing.T) {
	create, _ := sqlOf(mysql.CreateTable(mysql.NewEventStoreTable("events")))
	if !strings.Contains(create, "PRIMARY KEY (`aggregateType`, `aggregateID`, `version`)") {
		t.Errorf("the stream key must be the primary key:\n%s", create)
	}
	// MySQL insists an AUTO_INCREMENT column leads an index; the
	// unique on the id is what satisfies that and what Stream's
	// high-watermark reads.
	if !strings.Contains(create, "AUTO_INCREMENT") || !strings.Contains(create, "UNIQUE KEY `uq_events_id` (`id`)") {
		t.Errorf("the append offset needs its own unique index:\n%s", create)
	}
	if !strings.Contains(create, "`aggregateID` VARCHAR(255) NOT NULL") {
		t.Errorf("stream key column:\n%s", create)
	}
	// The collation rides on the table so the declared type and the
	// one information_schema reports back are the same string; that
	// the server really compares case-sensitively is pinned by
	// TestMySQLEventStoreStreamKeysAreCaseSensitive.
	if !strings.Contains(create, "COLLATE=utf8mb4_bin") {
		t.Errorf("stream keys must compare case-sensitively:\n%s", create)
	}
}

func TestSnapshotTableIsKeyedOnTheAggregate(t *testing.T) {
	create, _ := sqlOf(mysql.CreateTable(mysql.NewSnapshotTable("snaps")))
	if !strings.Contains(create, "PRIMARY KEY (`aggregateType`, `aggregateID`)") {
		t.Errorf("snapshot key:\n%s", create)
	}
}

func TestAppendNumbersVersionsFromExpected(t *testing.T) {
	d := &obDriver{}
	db := mysql.New(d)
	store := mysql.NewEventStore(db, "events")
	err := store.Append(db, context.Background(), "match", "abc", 4,
		mysql.EventInput{Type: "started", Payload: map[string]int{"a": 1}},
		mysql.EventInput{Type: "joined", Payload: json.RawMessage(`{"b":2}`), Headers: map[string]string{"tp": "1"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	q, args := d.queryContaining(t, "INSERT INTO `events`")
	if !strings.Contains(q, "VALUES (?, ?, ?, ?, ?, ?, ?), (?, ?, ?, ?, ?, ?, ?)") {
		t.Errorf("both events belong in one statement:\n%s", q)
	}
	if args[2] != int64(5) || args[9] != int64(6) {
		t.Errorf("versions = %v, %v; want expectedVersion+1 and +2", args[2], args[9])
	}
	if _, ok := args[6].(time.Time); !ok {
		t.Errorf("createdAt should be bound from Go, got %T", args[6])
	}
	if args[12] == nil {
		t.Error("headers should have been encoded")
	}
	if args[5] != nil {
		t.Errorf("absent headers should bind NULL, got %v", args[5])
	}
}

func TestAppendValidation(t *testing.T) {
	d := &obDriver{}
	db := mysql.New(d)
	store := mysql.NewEventStore(db, "events")
	ctx := context.Background()

	if err := store.Append(db, ctx, "", "abc", -1, mysql.EventInput{Type: "x"}); err == nil {
		t.Error("an empty aggregate type should be refused")
	}
	if err := store.Append(db, ctx, "match", "abc", -1, mysql.EventInput{}); err == nil {
		t.Error("an event without a type should be refused")
	}
	if err := store.Append(db, ctx, "match", "abc", -1); err != nil {
		t.Errorf("appending nothing is not an error: %v", err)
	}
	if len(d.queries) != 0 {
		t.Errorf("nothing should have reached the server: %v", d.queries)
	}
}

// A duplicate stream key is how a writer learns it raced. MySQL says
// 1062; the caller should only ever see ErrConcurrencyConflict.
func TestAppendTranslatesDuplicateKeyToAConflict(t *testing.T) {
	d := &obDriver{failFor: map[string]error{
		"INSERT INTO `events`": errors.New("Error 1062 (23000): Duplicate entry 'match-abc-5' for key 'PRIMARY'"),
	}}
	db := mysql.New(d)
	store := mysql.NewEventStore(db, "events")
	err := store.Append(db, context.Background(), "match", "abc", 4, mysql.EventInput{Type: "started"})
	if !errors.Is(err, mysql.ErrConcurrencyConflict) {
		t.Fatalf("err = %v, want ErrConcurrencyConflict", err)
	}
}

// A lock-wait timeout is not a lost version race — the transaction
// simply did not happen — so it must not be dressed up as one.
func TestAppendDoesNotTranslateALockWaitTimeout(t *testing.T) {
	boom := errors.New("Error 1205 (HY000): Lock wait timeout exceeded; try restarting transaction")
	d := &obDriver{failFor: map[string]error{"INSERT INTO `events`": boom}}
	db := mysql.New(d)
	store := mysql.NewEventStore(db, "events")
	err := store.Append(db, context.Background(), "match", "abc", 4, mysql.EventInput{Type: "started"})
	if errors.Is(err, mysql.ErrConcurrencyConflict) {
		t.Fatalf("a lock wait timeout was reported as a conflict: %v", err)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadAndStreamSQL(t *testing.T) {
	d := &obDriver{}
	store := mysql.NewEventStore(mysql.New(d), "events")
	ctx := context.Background()

	if _, err := store.Load(ctx, "match", "abc", 3); err != nil {
		t.Fatal(err)
	}
	q, args := d.queryContaining(t, "`version` > ?")
	if !strings.Contains(q, "ORDER BY `version`") {
		t.Errorf("replay must be in version order:\n%s", q)
	}
	if args[2] != int64(3) {
		t.Errorf("fromVersion = %v", args[2])
	}

	if _, err := store.Stream(ctx, 10, 0); err != nil {
		t.Fatal(err)
	}
	q, args = d.queryContaining(t, "`id` > ?")
	if !strings.Contains(q, "ORDER BY `id`") || !strings.Contains(q, "LIMIT ?") {
		t.Errorf("stream SQL:\n%s", q)
	}
	if args[1] != 100 {
		t.Errorf("a non-positive limit should default to 100, got %v", args[1])
	}
}

func TestLatestVersionOfAnEmptyStream(t *testing.T) {
	d := &obDriver{rowsFor: func(sql string) [][]any {
		if strings.Contains(sql, "MAX(`version`)") {
			return [][]any{{int64(-1)}}
		}
		return nil
	}}
	store := mysql.NewEventStore(mysql.New(d), "events")
	v, err := store.LatestVersion(context.Background(), "match", "nope")
	if err != nil {
		t.Fatal(err)
	}
	if v != -1 {
		t.Errorf("version = %d, want -1 for a stream that does not exist", v)
	}
}

func TestScanEventRows(t *testing.T) {
	created := time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC)
	d := &obDriver{rowsFor: func(string) [][]any {
		return [][]any{
			{int64(3), "match", "abc", int64(0), "started", json.RawMessage(`{"x":1}`), []byte(`{"tp":"9"}`), created},
			{int64(4), "match", "abc", int64(1), "joined", json.RawMessage(`{}`), []byte(nil), created},
		}
	}}
	store := mysql.NewEventStore(mysql.New(d), "events")
	events, err := store.Load(context.Background(), "match", "abc", -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events", len(events))
	}
	if events[0].Offset != 3 || events[0].Version != 0 || events[0].Type != "started" {
		t.Errorf("event = %+v", events[0])
	}
	if events[0].Headers["tp"] != "9" {
		t.Errorf("headers = %v", events[0].Headers)
	}
	if events[1].Headers != nil {
		t.Errorf("a NULL headers column should stay nil, got %v", events[1].Headers)
	}
	if !events[0].CreatedAt.Equal(created) {
		t.Errorf("createdAt = %v", events[0].CreatedAt)
	}
}

// MySQL's upsert names no conflict target and spells the incoming row
// VALUES(col) — the alias form MySQL 8.0.19 introduced does not exist
// on MariaDB.
func TestSaveSnapshotUpserts(t *testing.T) {
	d := &obDriver{}
	store := mysql.NewEventStore(mysql.New(d), "events")
	err := store.SaveSnapshot(context.Background(), "snaps", mysql.AggregateSnapshot{
		AggregateType: "match", AggregateID: "abc", Version: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	q, args := d.queryContaining(t, "INSERT INTO `snaps`")
	if !strings.Contains(q, "ON DUPLICATE KEY UPDATE `version` = VALUES(`version`)") {
		t.Errorf("snapshot upsert:\n%s", q)
	}
	if strings.Contains(q, " AS new") {
		t.Errorf("the row-alias form is MySQL-only:\n%s", q)
	}
	// A nil state is stored as JSON null rather than SQL NULL: the
	// column is NOT NULL, and "no state" is a value.
	if string(args[3].(json.RawMessage)) != "null" {
		t.Errorf("state = %v", args[3])
	}
}

func TestSaveSnapshotRequiresAnAggregate(t *testing.T) {
	store := mysql.NewEventStore(mysql.New(&obDriver{}), "events")
	if err := store.SaveSnapshot(context.Background(), "snaps", mysql.AggregateSnapshot{}); err == nil {
		t.Fatal("a snapshot without an aggregate should be refused")
	}
}

func TestLoadSnapshotMissing(t *testing.T) {
	store := mysql.NewEventStore(mysql.New(&obDriver{}), "events")
	_, ok, err := store.LoadSnapshot(context.Background(), "snaps", "match", "abc")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("no row means no snapshot")
	}
}
