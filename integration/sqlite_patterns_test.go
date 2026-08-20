package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/sqlite"
)

// An idempotency key must make a second call return the first call's
// result without running the body again. Both halves depend on a
// uniqueness constraint being enforced inside a transaction, which is
// not something a fake driver can demonstrate.
func TestIdempotencyAgainstTheEngine(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	exec(t, db, sqlite.CreateTable(sqlite.NewIdempotencyTable("idem")))
	store := sqlite.NewIdempotencyStore(db, "idem", time.Hour)

	tbl := sqlite.NewTable("charges")
	sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey().AutoIncrement())
	amount := sqlite.Add(tbl, sqlite.Integer("amount").NotNull())
	exec(t, db, sqlite.CreateTable(tbl))

	calls := 0
	charge := func() ([]byte, error) {
		return store.Run(ctx, "order-1", func(tx *sqlite.DB) ([]byte, error) {
			calls++
			if _, err := tx.Insert(tbl).Values(amount.Val(500)).Exec(ctx); err != nil {
				return nil, err
			}
			return []byte(`{"charged":500}`), nil
		})
	}

	first, err := charge()
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := charge()
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("replay returned a different result: %s vs %s", first, second)
	}
	if calls != 1 {
		t.Errorf("the body ran %d times; an idempotency key that re-runs it is not one", calls)
	}

	var n int64
	if err := db.Select(sqlite.CountAll()).From(tbl).One(ctx, &n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("the replay charged again: %d rows", n)
	}

	// A different key must run.
	if _, err := store.Run(ctx, "order-2", func(tx *sqlite.DB) ([]byte, error) {
		calls++
		return []byte(`{}`), nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("a distinct key did not run: calls = %d", calls)
	}
}

// A saga that fails partway must compensate the steps that succeeded,
// in reverse. That the compensations actually undo their writes is
// only observable against a database.
func TestSagaCompensatesAgainstTheEngine(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	tbl := sqlite.NewTable("ledger")
	sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey().AutoIncrement())
	entry := sqlite.Add(tbl, sqlite.Text("entry").NotNull())
	exec(t, db, sqlite.CreateTable(tbl))

	add := func(what string) sqlite.SagaStepFn {
		return func(ctx context.Context, tx *sqlite.DB, _ *sqlite.SagaState) error {
			_, err := tx.Insert(tbl).Values(entry.Val(what)).Exec(ctx)
			return err
		}
	}
	remove := func(what string) sqlite.SagaStepFn {
		return func(ctx context.Context, tx *sqlite.DB, _ *sqlite.SagaState) error {
			_, err := tx.Delete(tbl).Where(entry.Eq(what)).Exec(ctx)
			return err
		}
	}
	boom := errors.New("third step fails")

	saga := sqlite.NewSaga("checkout").
		Step("reserve", add("reserve"), remove("reserve")).
		Step("charge", add("charge"), remove("charge")).
		Step("ship", func(context.Context, *sqlite.DB, *sqlite.SagaState) error { return boom }, nil)

	err := saga.Run(db, ctx, &sqlite.SagaState{})
	if err == nil {
		t.Fatal("the saga reported success despite a failing step")
	}
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the step's error", err)
	}

	var rows []struct{ Entry string }
	if err := db.Select(entry).From(tbl).All(ctx, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("compensation left %d rows behind: %+v", len(rows), rows)
	}
}

// A single-column result is the most ordinary query there is, and
// until the integration suite tried one, drops could not scan it —
// One demanded a struct, so SELECT count(*) needed a throwaway type.
func TestScalarResultsScan(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	tbl := sqlite.NewTable("counts")
	sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey().AutoIncrement())
	label := sqlite.Add(tbl, sqlite.Text("label").NotNull())
	exec(t, db, sqlite.CreateTable(tbl))
	for _, l := range []string{"a", "b", "c"} {
		if _, err := db.Insert(tbl).Values(label.Val(l)).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}

	var n int64
	if err := db.Select(sqlite.CountAll()).From(tbl).One(ctx, &n); err != nil {
		t.Fatalf("count into *int64: %v", err)
	}
	if n != 3 {
		t.Errorf("count = %d, want 3", n)
	}

	var s string
	if err := db.Select(label).From(tbl).OrderBy(label.Asc()).Limit(1).One(ctx, &s); err != nil {
		t.Fatalf("text into *string: %v", err)
	}
	if s != "a" {
		t.Errorf("label = %q", s)
	}

	// A multi-column result into a scalar has to be refused, not
	// silently truncated to the first column.
	var wrong int64
	err := db.Select(sqlite.CountAll(), sqlite.CountAll()).From(tbl).One(ctx, &wrong)
	if err == nil {
		t.Error("two columns scanned into one scalar without complaint")
	}

	// And a struct destination must keep working.
	var row struct {
		ID    int64
		Label string
	}
	if err := db.Select(tbl.Col("id"), label).From(tbl).Limit(1).One(ctx, &row); err != nil {
		t.Fatalf("struct destination: %v", err)
	}
	if row.ID == 0 || row.Label == "" {
		t.Errorf("row = %+v", row)
	}
}

// Dec built as Inc(-delta) wraps on an unsigned handle, and the
// statement it renders is a flawless addition of the wrapped value. No
// engine will complain; the counter simply climbs when it was told to
// fall. Only the stored number settles it.
func TestDecLowersAnUnsignedCounter(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	tbl := sqlite.NewTable(integration.UniqueName(t, "seatmaps"))
	id := sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
	seats := sqlite.Add(tbl, sqlite.Custom[uint32]("seats", "INTEGER").NotNull())
	exec(t, db, sqlite.CreateTable(tbl))

	type seatmap struct {
		ID    int64  `drop:"id"`
		Seats uint32 `drop:"seats"`
	}
	ent := sqlite.NewEntity[seatmap](tbl)

	if _, err := db.Insert(tbl).Values(id.Val(1), seats.Val(100)).Exec(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := ent.Patch(db, ctx, int64(1), sqlite.Dec(seats, uint32(5))); err != nil {
		t.Fatalf("Dec: %v", err)
	}

	var got uint32
	if err := db.Select(seats).From(tbl).Where(id.Eq(int64(1))).One(ctx, &got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got != 95 {
		t.Fatalf("seats = %d after Dec(5) from 100, want 95", got)
	}
}
