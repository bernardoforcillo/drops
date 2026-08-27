package integration_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/pg"
)

// A server-side cursor is DECLARE, FETCH and CLOSE inside a
// transaction. Every one of those is a statement PostgreSQL either
// accepts or does not, and the batching only means anything if the
// server really hands rows back in the batches drops asked for.

func streamFixture(t *testing.T, rows int) (*pg.DB, string) {
	t.Helper()
	db := openPG(t)
	tbl := pg.NewTable(integration.UniqueName(t, "stream"))
	pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	pg.Add(tbl, pg.Text("payload").NotNull())
	dropPG(t, db, tbl)
	execPG(t, db, pg.CreateTable(tbl))

	if rows > 0 {
		if _, err := db.Exec(context.Background(), fmt.Sprintf(
			`INSERT INTO %q SELECT g, 'row-' || g FROM generate_series(1, $1) g`, tbl.Name()),
			rows); err != nil {
			t.Fatal(err)
		}
	}
	return db, tbl.Name()
}

func TestPGStreamRowsReadsEverything(t *testing.T) {
	db, tbl := streamFixture(t, 2500)
	ctx := context.Background()

	var seen int
	var lastID int64
	err := pg.StreamRows(ctx, db, pg.StreamOptions{Batch: 500},
		fmt.Sprintf(`SELECT "id", "payload" FROM %q ORDER BY "id"`, tbl), nil,
		func(row drops.Rows) error {
			var id int64
			var payload string
			if err := row.Scan(&id, &payload); err != nil {
				return err
			}
			seen++
			if id != lastID+1 {
				return fmt.Errorf("row %d arrived after %d — the cursor skipped or repeated", id, lastID)
			}
			lastID = id
			if payload != fmt.Sprintf("row-%d", id) {
				return fmt.Errorf("row %d carries %q", id, payload)
			}
			return nil
		})
	if err != nil {
		t.Fatalf("StreamRows: %v", err)
	}
	if seen != 2500 {
		t.Errorf("read %d rows, want 2500", seen)
	}
}

// The batch size has to reach the server, or the cursor is buying
// nothing over a plain query.
func TestPGStreamQueryHonoursTheBatchSize(t *testing.T) {
	db, tbl := streamFixture(t, 1050)
	ctx := context.Background()

	var batches []int
	err := pg.StreamQuery(ctx, db, pg.StreamOptions{Batch: 400},
		fmt.Sprintf(`SELECT "id" FROM %q ORDER BY "id"`, tbl), nil,
		func(rows drops.Rows) error {
			n := 0
			for rows.Next() {
				n++
			}
			batches = append(batches, n)
			return rows.Err()
		})
	if err != nil {
		t.Fatalf("StreamQuery: %v", err)
	}
	want := []int{400, 400, 250}
	if len(batches) != len(want) {
		t.Fatalf("got batches %v, want %v", batches, want)
	}
	for i := range want {
		if batches[i] != want[i] {
			t.Errorf("batch %d had %d rows, want %d", i, batches[i], want[i])
		}
	}
}

// A result whose size is an exact multiple of the batch needs one more
// FETCH to discover it has ended, and must not deliver an empty batch
// to the caller.
func TestPGStreamQueryExactBoundary(t *testing.T) {
	db, tbl := streamFixture(t, 200)
	ctx := context.Background()

	var batches []int
	err := pg.StreamQuery(ctx, db, pg.StreamOptions{Batch: 100},
		fmt.Sprintf(`SELECT "id" FROM %q ORDER BY "id"`, tbl), nil,
		func(rows drops.Rows) error {
			n := 0
			for rows.Next() {
				n++
			}
			batches = append(batches, n)
			return rows.Err()
		})
	if err != nil {
		t.Fatalf("StreamQuery: %v", err)
	}
	// Two full batches and one empty one, which the loop uses to
	// learn the result ended.
	total := 0
	for _, n := range batches {
		total += n
	}
	if total != 200 {
		t.Errorf("read %d rows, want 200", total)
	}
}

func TestPGStreamQueryEmptyResult(t *testing.T) {
	db, tbl := streamFixture(t, 0)
	ctx := context.Background()

	var rowsSeen int
	err := pg.StreamRows(ctx, db, pg.StreamOptions{Batch: 100},
		fmt.Sprintf(`SELECT "id" FROM %q`, tbl), nil,
		func(drops.Rows) error { rowsSeen++; return nil })
	if err != nil {
		t.Fatalf("StreamRows on an empty table: %v", err)
	}
	if rowsSeen != 0 {
		t.Errorf("the callback ran %d times on an empty result", rowsSeen)
	}
}

// Stopping early is the whole point of a cursor over a materialised
// result, and it has to leave the transaction and the cursor clean.
func TestPGStreamStopsEarly(t *testing.T) {
	db, tbl := streamFixture(t, 5000)
	ctx := context.Background()

	var seen int
	err := pg.StreamRows(ctx, db, pg.StreamOptions{Batch: 100},
		fmt.Sprintf(`SELECT "id" FROM %q ORDER BY "id"`, tbl), nil,
		func(drops.Rows) error {
			seen++
			if seen == 5 {
				return pg.ErrStopStream
			}
			return nil
		})
	if err != nil {
		t.Fatalf("ErrStopStream surfaced as a failure: %v", err)
	}
	if seen != 5 {
		t.Errorf("read %d rows after stopping at 5", seen)
	}

	// The connection is usable afterwards, which it would not be if
	// the transaction had been left open or aborted.
	if _, err := db.Exec(ctx, "SELECT 1"); err != nil {
		t.Errorf("the connection is unusable after an early stop: %v", err)
	}
}

func TestPGStreamPropagatesCallbackErrors(t *testing.T) {
	db, tbl := streamFixture(t, 500)
	boom := errors.New("sink refused")
	err := pg.StreamRows(context.Background(), db, pg.StreamOptions{Batch: 100},
		fmt.Sprintf(`SELECT "id" FROM %q`, tbl), nil,
		func(drops.Rows) error { return boom })
	if !errors.Is(err, boom) {
		t.Errorf("got %v, want the callback's error", err)
	}
}

// Bound parameters have to survive into the DECLARE, or the cursor is
// string-built SQL with a different name.
func TestPGStreamBindsParameters(t *testing.T) {
	db, tbl := streamFixture(t, 100)
	ctx := context.Background()

	var seen int
	err := pg.StreamRows(ctx, db, pg.StreamOptions{Batch: 10},
		fmt.Sprintf(`SELECT "id" FROM %q WHERE "id" > $1 AND "id" <= $2 ORDER BY "id"`, tbl),
		[]any{40, 60},
		func(drops.Rows) error { seen++; return nil })
	if err != nil {
		t.Fatalf("StreamRows with parameters: %v", err)
	}
	if seen != 20 {
		t.Errorf("read %d rows, want 20", seen)
	}
}

// WITH HOLD is a different statement and the server has to accept it.
func TestPGStreamWithHold(t *testing.T) {
	db, tbl := streamFixture(t, 300)
	ctx := context.Background()

	var seen int
	err := pg.StreamRows(ctx, db, pg.StreamOptions{Batch: 100, Hold: true},
		fmt.Sprintf(`SELECT "id" FROM %q ORDER BY "id"`, tbl), nil,
		func(drops.Rows) error { seen++; return nil })
	if err != nil {
		t.Fatalf("StreamRows WITH HOLD: %v", err)
	}
	if seen != 300 {
		t.Errorf("read %d rows, want 300", seen)
	}
}
