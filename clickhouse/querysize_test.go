package clickhouse_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/clickhouse"
	"github.com/bernardoforcillo/drops/dropstest"
)

// One statement may be at most max_query_size bytes, 262144 by default,
// measured on the text AFTER the driver substitutes the arguments in.
// It is a ceiling a bulk write reaches on an ordinary batch, and the
// server's answer to it is a SYNTAX ERROR at a position where nothing
// is wrong.
//
// These pin the refusal drops makes instead, in the two properties that
// matter: it happens BEFORE anything is sent, and it says what to do.

type qsRow struct {
	ID   int64  `drop:"id"`
	Body string `drop:"body"`
}

func qsEntity(t *testing.T) (*clickhouse.Entity[qsRow], *dropstest.Driver, *clickhouse.DB) {
	t.Helper()
	tbl := clickhouse.NewTable("qs_rows")
	id := clickhouse.Add(tbl, clickhouse.Int64("id"))
	clickhouse.Add(tbl, clickhouse.String("body"))
	tbl.Engine(clickhouse.MergeTree()).OrderBy(id)
	drv := dropstest.New()
	return clickhouse.NewEntity[qsRow](tbl), drv, clickhouse.New(drv)
}

func TestAStatementOverTheQuerySizeIsRefusedBeforeItIsSent(t *testing.T) {
	ent, drv, db := qsEntity(t)

	// 512 rows of a 1 KiB body is half a megabyte of literal, twice
	// over the ceiling and nowhere near a parameter limit — which is
	// the point: the quantity that runs out on ClickHouse is bytes.
	rows := make([]qsRow, 512)
	for i := range rows {
		rows[i] = qsRow{ID: int64(i), Body: strings.Repeat("x", 1024)}
	}

	_, err := ent.CreateMany(db, context.Background(), rows)
	if !errors.Is(err, clickhouse.ErrQueryTooLarge) {
		t.Fatalf("CreateMany: %v, want ErrQueryTooLarge", err)
	}
	// The refusal is worth nothing if the statement went anyway.
	if got := drv.Statements(); len(got) != 0 {
		t.Errorf("a refused batch still sent %d statement(s)", len(got))
	}
	// And it has to say what to do, because the caller's next move is
	// to split the batch and they should not have to work that out
	// from a byte count.
	if !strings.Contains(err.Error(), "batches") {
		t.Errorf("the refusal does not say what to do:\n%v", err)
	}
}

func TestABatchUnderTheQuerySizeIsSent(t *testing.T) {
	ent, drv, db := qsEntity(t)

	rows := make([]qsRow, 64)
	for i := range rows {
		rows[i] = qsRow{ID: int64(i), Body: strings.Repeat("x", 512)}
	}

	if _, err := ent.CreateMany(db, context.Background(), rows); err != nil {
		t.Fatalf("CreateMany under the ceiling: %v", err)
	}
	if got := drv.Statements(); len(got) != 1 {
		t.Fatalf("sent %d statement(s), want 1", len(got))
	}
}

// The ceiling counts the query tag, because the server does.
//
// A tag is a comment appended to the statement, and a comment is text
// the server reads and measures like any other. Checking before the tag
// went on would be measuring a string nobody sends.
func TestTheQueryTagCountsTowardTheCeiling(t *testing.T) {
	drv := dropstest.New()
	db := clickhouse.New(drv)

	// Just under on its own; over once a long tag is appended.
	body := strings.Repeat("x", 262144-64)
	ctx := drops.WithQueryTag(context.Background(), "job", strings.Repeat("t", 256))

	err := func() error {
		_, err := db.Exec(ctx, "SELECT ?", body)
		return err
	}()
	if !errors.Is(err, clickhouse.ErrQueryTooLarge) {
		t.Fatalf("Exec: %v, want ErrQueryTooLarge once the tag is counted", err)
	}
}
