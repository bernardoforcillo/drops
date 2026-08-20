package dropstest_test

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/dropstest"
)

// The recorder's contract is that a test can read what a builder sent
// with ordinary comparisons, so the suite asserts with DeepEqual on
// whole []Statement values rather than on a matcher: if that is
// unpleasant here it will be unpleasant everywhere it is used.

func TestRecordsExecAndQuery(t *testing.T) {
	ctx := context.Background()
	drv := dropstest.New()

	if _, err := drv.Exec(ctx, `DELETE FROM "users" WHERE "id" = $1`, int64(7)); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	rows, err := drv.Query(ctx, `SELECT * FROM "users" WHERE "name" = $1`, "Ada")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := drv.Exec(ctx, `VACUUM`); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	want := []dropstest.Statement{
		{Op: dropstest.OpExec, SQL: `DELETE FROM "users" WHERE "id" = $1`, Args: []any{int64(7)}},
		{Op: dropstest.OpQuery, SQL: `SELECT * FROM "users" WHERE "name" = $1`, Args: []any{"Ada"}},
		{Op: dropstest.OpExec, SQL: `VACUUM`},
	}
	if got := drv.Statements(); !reflect.DeepEqual(got, want) {
		t.Errorf("Statements() got = %v, want %v", got, want)
	}

	wantSQL := []string{
		`DELETE FROM "users" WHERE "id" = $1`,
		`SELECT * FROM "users" WHERE "name" = $1`,
		`VACUUM`,
	}
	if got := drv.SQL(); !reflect.DeepEqual(got, wantSQL) {
		t.Errorf("SQL() got = %v, want %v", got, wantSQL)
	}
	if got, want := drv.LastSQL(), `VACUUM`; got != want {
		t.Errorf("LastSQL() got = %v, want %v", got, want)
	}
}

// Recorded arguments must be a copy: a builder that reuses one args
// slice across statements is a plausible optimisation, and a recorder
// aliasing it would report the last statement's arguments for all of
// them.
func TestRecordedArgsAreSnapshots(t *testing.T) {
	ctx := context.Background()
	drv := dropstest.New()

	args := []any{int64(1)}
	if _, err := drv.Exec(ctx, `SELECT $1`, args...); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	args[0] = int64(99)

	if got, want := drv.Last().Args, []any{int64(1)}; !reflect.DeepEqual(got, want) {
		t.Errorf("Last().Args got = %v, want %v", got, want)
	}
	drv.Statements()[0].Args[0] = int64(42)
	if got, want := drv.Last().Args, []any{int64(1)}; !reflect.DeepEqual(got, want) {
		t.Errorf("after mutating the returned copy, Last().Args got = %v, want %v", got, want)
	}
}

func TestAffectedAndDefaults(t *testing.T) {
	ctx := context.Background()
	drv := dropstest.New()

	res, err := drv.Exec(ctx, `UPDATE "users" SET "name" = $1`, "Ada")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("RowsAffected: %v", err)
	}
	if want := int64(1); n != want {
		t.Errorf("default RowsAffected got = %v, want %v", n, want)
	}

	drv.Affected(0)
	res, err = drv.Exec(ctx, `UPDATE "users" SET "name" = $1`, "Ada")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if n, _ = res.RowsAffected(); n != 0 {
		t.Errorf("after Affected(0), RowsAffected got = %v, want %v", n, 0)
	}
}

type user struct {
	ID   int64
	Name string
	Bio  *string
	Tag  sql.NullString
}

func TestQueuedRowsScanThroughScanAll(t *testing.T) {
	ctx := context.Background()
	bio := "builder of engines"
	drv := dropstest.New().
		Rows([]string{"id", "name", "bio", "tag"},
			[]any{int64(1), "Ada", bio, "founder"},
			[]any{int64(2), "Grace", nil, nil}).
		Rows([]string{"id"}, []any{int64(9)})

	rows, err := drv.Query(ctx, `SELECT "id", "name", "bio", "tag" FROM "users"`)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var got []user
	if err := drops.ScanAll(rows, &got); err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	want := []user{
		{ID: 1, Name: "Ada", Bio: &bio, Tag: sql.NullString{String: "founder", Valid: true}},
		{ID: 2, Name: "Grace"},
	}
	if len(got) != len(want) {
		t.Fatalf("ScanAll got %d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].Name != want[i].Name || got[i].Tag != want[i].Tag {
			t.Errorf("row %d got = %v, want %v", i, got[i], want[i])
		}
		switch {
		case want[i].Bio == nil && got[i].Bio != nil:
			t.Errorf("row %d bio got = %v, want nil", i, *got[i].Bio)
		case want[i].Bio != nil && (got[i].Bio == nil || *got[i].Bio != *want[i].Bio):
			t.Errorf("row %d bio got = %v, want %v", i, got[i].Bio, *want[i].Bio)
		}
	}

	// The second queued set answers the second query, and the queue is
	// then drained: a third query gets the empty fallback, not a replay.
	rows, err = drv.Query(ctx, `SELECT "id" FROM "users"`)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var ids []int64
	if err := drops.ScanAll(rows, &ids); err != nil {
		t.Fatalf("ScanAll scalars: %v", err)
	}
	if want := []int64{9}; !reflect.DeepEqual(ids, want) {
		t.Errorf("scalar ScanAll got = %v, want %v", ids, want)
	}

	rows, err = drv.Query(ctx, `SELECT "id" FROM "users"`)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	ids = nil
	if err := drops.ScanAll(rows, &ids); err != nil {
		t.Fatalf("ScanAll after drain: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("after the queue drained, got = %v, want no rows", ids)
	}
}

func TestAlwaysRowsAnswersEveryQuery(t *testing.T) {
	ctx := context.Background()
	drv := dropstest.New().
		Rows([]string{"id"}, []any{int64(1)}).
		AlwaysRows([]string{"id"}, []any{int64(7)}, []any{int64(8)})

	wants := [][]int64{{1}, {7, 8}, {7, 8}}
	for i, want := range wants {
		rows, err := drv.Query(ctx, `SELECT "id" FROM "users"`)
		if err != nil {
			t.Fatalf("Query %d: %v", i, err)
		}
		var got []int64
		if err := drops.ScanAll(rows, &got); err != nil {
			t.Fatalf("ScanAll %d: %v", i, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("query %d got = %v, want %v", i, got, want)
		}
	}
}

// ScanOne over an empty default result must reach drops.ErrNoRows
// rather than a nil cursor or a panic: "no rows" is the answer half the
// tests that use this package are written to check.
func TestEmptyResultIsNoRows(t *testing.T) {
	ctx := context.Background()
	drv := dropstest.New()

	rows, err := drv.Query(ctx, `SELECT "id" FROM "users"`)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var got user
	if err := drops.ScanOne(rows, &got); !errors.Is(err, drops.ErrNoRows) {
		t.Errorf("ScanOne got = %v, want %v", err, drops.ErrNoRows)
	}
}

func TestErrorInjection(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("connection reset")

	t.Run("exec", func(t *testing.T) {
		drv := dropstest.New().FailNext(boom)
		if _, err := drv.Exec(ctx, `INSERT INTO "users" DEFAULT VALUES`); !errors.Is(err, boom) {
			t.Errorf("Exec err got = %v, want %v", err, boom)
		}
		want := []dropstest.Statement{{Op: dropstest.OpExec, SQL: `INSERT INTO "users" DEFAULT VALUES`}}
		if got := drv.Statements(); !reflect.DeepEqual(got, want) {
			t.Errorf("the failed statement must still be recorded: got = %v, want %v", got, want)
		}
		if _, err := drv.Exec(ctx, `INSERT INTO "users" DEFAULT VALUES`); err != nil {
			t.Errorf("second Exec got = %v, want the injected error to be spent", err)
		}
	})

	t.Run("query keeps the queued rows", func(t *testing.T) {
		drv := dropstest.New().Rows([]string{"id"}, []any{int64(1)}).FailNext(boom)
		rows, err := drv.Query(ctx, `SELECT "id" FROM "users"`)
		if !errors.Is(err, boom) {
			t.Errorf("Query err got = %v, want %v", err, boom)
		}
		if rows != nil {
			t.Errorf("Query rows got = %v, want an explicitly nil cursor", rows)
		}
		rows, err = drv.Query(ctx, `SELECT "id" FROM "users"`)
		if err != nil {
			t.Fatalf("second Query: %v", err)
		}
		var got []int64
		if err := drops.ScanAll(rows, &got); err != nil {
			t.Fatalf("ScanAll: %v", err)
		}
		if want := []int64{1}; !reflect.DeepEqual(got, want) {
			t.Errorf("a failed query must not consume the queue: got = %v, want %v", got, want)
		}
	})

	t.Run("cancelled ctx", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		drv := dropstest.New()
		if _, err := drv.Exec(cancelled, `VACUUM`); !errors.Is(err, context.Canceled) {
			t.Errorf("Exec err got = %v, want %v", err, context.Canceled)
		}
		if got := drv.Statements(); len(got) != 0 {
			t.Errorf("a statement that never ran must not be recorded: got = %v", got)
		}
	})
}

func TestTransactionRecording(t *testing.T) {
	ctx := context.Background()

	t.Run("commit", func(t *testing.T) {
		drv := dropstest.New()
		tx, err := drv.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE "users" SET "name" = $1`, "Ada"); err != nil {
			t.Fatalf("tx Exec: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		want := []dropstest.Statement{
			{Op: dropstest.OpBegin},
			{Op: dropstest.OpExec, SQL: `UPDATE "users" SET "name" = $1`, Args: []any{"Ada"}},
			{Op: dropstest.OpCommit},
		}
		if got := drv.Statements(); !reflect.DeepEqual(got, want) {
			t.Errorf("Statements() got = %v, want %v", got, want)
		}
		// Last skips the boundary, or every assertion about SQL run
		// inside a transaction would read "commit".
		if got, want := drv.LastSQL(), `UPDATE "users" SET "name" = $1`; got != want {
			t.Errorf("LastSQL() got = %v, want %v", got, want)
		}
	})

	t.Run("rollback", func(t *testing.T) {
		drv := dropstest.New()
		tx, err := drv.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("Rollback: %v", err)
		}
		want := []dropstest.Statement{{Op: dropstest.OpBegin}, {Op: dropstest.OpRollback}}
		if got := drv.Statements(); !reflect.DeepEqual(got, want) {
			t.Errorf("Statements() got = %v, want %v", got, want)
		}
	})

	t.Run("use after close", func(t *testing.T) {
		drv := dropstest.New()
		tx, err := drv.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if err := tx.Commit(ctx); !errors.Is(err, dropstest.ErrTxDone) {
			t.Errorf("second Commit got = %v, want %v", err, dropstest.ErrTxDone)
		}
		if err := tx.Rollback(ctx); !errors.Is(err, dropstest.ErrTxDone) {
			t.Errorf("Rollback after Commit got = %v, want %v", err, dropstest.ErrTxDone)
		}
		if _, err := tx.Exec(ctx, `VACUUM`); !errors.Is(err, dropstest.ErrTxDone) {
			t.Errorf("Exec after Commit got = %v, want %v", err, dropstest.ErrTxDone)
		}
		if _, err := tx.Query(ctx, `SELECT 1`); !errors.Is(err, dropstest.ErrTxDone) {
			t.Errorf("Query after Commit got = %v, want %v", err, dropstest.ErrTxDone)
		}
		want := []dropstest.Statement{{Op: dropstest.OpBegin}, {Op: dropstest.OpCommit}}
		if got := drv.Statements(); !reflect.DeepEqual(got, want) {
			t.Errorf("a closed transaction must record nothing further: got = %v, want %v", got, want)
		}
	})

	t.Run("statements interleave with boundaries", func(t *testing.T) {
		drv := dropstest.New()
		if _, err := drv.Exec(ctx, `SET search_path = "app"`); err != nil {
			t.Fatalf("Exec: %v", err)
		}
		tx, err := drv.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE "tenant"`); err != nil {
			t.Fatalf("tx Exec: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		wantOps := []dropstest.Op{
			dropstest.OpExec, dropstest.OpBegin, dropstest.OpExec, dropstest.OpCommit,
		}
		var gotOps []dropstest.Op
		for _, s := range drv.Statements() {
			gotOps = append(gotOps, s.Op)
		}
		if !reflect.DeepEqual(gotOps, wantOps) {
			t.Errorf("ordering got = %v, want %v", gotOps, wantOps)
		}
	})
}

func TestReset(t *testing.T) {
	ctx := context.Background()
	drv := dropstest.New().Rows([]string{"id"}, []any{int64(1)}).Affected(9).FailNext(errors.New("boom"))
	drv.Reset()

	if got := drv.Statements(); len(got) != 0 {
		t.Errorf("Statements() after Reset got = %v, want none", got)
	}
	res, err := drv.Exec(ctx, `VACUUM`)
	if err != nil {
		t.Fatalf("Exec after Reset: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Errorf("RowsAffected after Reset got = %v, want %v", n, 1)
	}
	rows, err := drv.Query(ctx, `SELECT "id" FROM "users"`)
	if err != nil {
		t.Fatalf("Query after Reset: %v", err)
	}
	var ids []int64
	if err := drops.ScanAll(rows, &ids); err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("queued rows survived Reset: got = %v", ids)
	}
}

func TestScanRejectsMismatchedWidth(t *testing.T) {
	ctx := context.Background()
	drv := dropstest.New().Rows([]string{"id", "name"}, []any{int64(1)})

	rows, err := drv.Query(ctx, `SELECT "id", "name" FROM "users"`)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var got []user
	err = drops.ScanAll(rows, &got)
	if err == nil {
		t.Fatalf("ScanAll got = nil, want an error naming the short row")
	}
}

// The recorder is shared by every goroutine a test starts, and reading
// it while writes continue is the normal shape — a table test that
// checks how many statements a concurrent workload sent does exactly
// that. Run under -race this is the guard on Statements' mutex.
func TestConcurrentUse(t *testing.T) {
	ctx := context.Background()
	drv := dropstest.New().AlwaysRows([]string{"id"}, []any{int64(1)})

	const workers, each = 8, 50
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if _, err := drv.Exec(ctx, `UPDATE "users" SET "n" = $1`, w); err != nil {
					t.Errorf("Exec: %v", err)
					return
				}
				rows, err := drv.Query(ctx, `SELECT "id" FROM "users"`)
				if err != nil {
					t.Errorf("Query: %v", err)
					return
				}
				var ids []int64
				if err := drops.ScanAll(rows, &ids); err != nil {
					t.Errorf("ScanAll: %v", err)
					return
				}
				if len(ids) != 1 {
					t.Errorf("got %d rows, want 1", len(ids))
					return
				}
			}
		}(w)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < workers*each; i++ {
			_ = drv.Statements()
			_ = drv.Last()
			_ = drv.SQL()
		}
	}()
	wg.Wait()

	if got, want := len(drv.Statements()), workers*each*2; got != want {
		t.Errorf("recorded %v statements, want %v", got, want)
	}
}

// Every value a driver hands a scanner arrives typed, and the recorder
// must widen it the way a real one does — otherwise a test that passes
// here fails against Postgres for a reason the double concealed.
func TestScanConversions(t *testing.T) {
	ctx := context.Background()
	type row struct {
		Small  int
		Big    int64
		Ratio  float64
		Raw    []byte
		Absent *string
	}
	drv := dropstest.New().Rows(
		[]string{"small", "big", "ratio", "raw", "absent"},
		[]any{int64(3), 4, float32(0.5), "bytes", nil},
	)
	rows, err := drv.Query(ctx, `SELECT * FROM "t"`)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var got row
	if err := drops.ScanOne(rows, &got); err != nil {
		t.Fatalf("ScanOne: %v", err)
	}
	want := row{Small: 3, Big: 4, Ratio: 0.5, Raw: []byte("bytes")}
	if got.Small != want.Small || got.Big != want.Big || got.Ratio != want.Ratio ||
		string(got.Raw) != string(want.Raw) || got.Absent != nil {
		t.Errorf("got = %v, want %v", got, want)
	}
}

// An integer converts to a string in Go as a rune, so a canned int64(65)
// landing in a string field as "A" is a conversion no driver performs
// and no test author intends.
func TestScanRefusesIntegerToString(t *testing.T) {
	ctx := context.Background()
	drv := dropstest.New().Rows([]string{"name"}, []any{int64(65)})
	rows, err := drv.Query(ctx, `SELECT "name" FROM "users"`)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var got struct{ Name string }
	if err := drops.ScanOne(rows, &got); err == nil {
		t.Errorf("ScanOne got = nil (Name = %q), want a conversion error", got.Name)
	}
}
