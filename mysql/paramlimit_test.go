package mysql_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/mysql"
)

// One statement may carry at most 65535 bound parameters. It is a limit a bulk write
// reaches on an ordinary batch — eight columns and 8192 rows — and the
// server's answer to it names the protocol, not the call.
//
// These pin the refusal drops makes instead, in the two properties that
// matter: it happens BEFORE anything is sent, and it says what to do.

type plRow struct {
	ID int64  `drop:"id"`
	A  string `drop:"a"`
	B  string `drop:"b"`
	C  string `drop:"c"`
	D  string `drop:"d"`
	E  string `drop:"e"`
	F  string `drop:"f"`
	G  string `drop:"g"`
}

func plEntity(t *testing.T) (*mysql.Entity[plRow], *dropstest.Driver, *mysql.DB) {
	t.Helper()
	tbl := mysql.NewTable("pl_rows")
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		mysql.Add(tbl, mysql.Text(n).NotNull())
	}
	drv := dropstest.New()
	return mysql.NewEntity[plRow](tbl), drv, mysql.New(drv)
}

func TestABatchOverTheParameterLimitIsRefusedBeforeItIsSent(t *testing.T) {
	ent, drv, db := plEntity(t)

	// Seven bound columns per row — the key is left to the server —
	// so 9363 rows is 65541 parameters, over.
	rows := make([]plRow, 9363)
	for i := range rows {
		rows[i] = plRow{A: "a", B: "b", C: "c", D: "d", E: "e", F: "f", G: "g"}
	}

	_, err := ent.CreateMany(db, context.Background(), rows)
	if !errors.Is(err, mysql.ErrTooManyParameters) {
		t.Fatalf("CreateMany: %v, want ErrTooManyParameters", err)
	}
	// The refusal is worth nothing if the statement went anyway.
	if got := drv.Statements(); len(got) != 0 {
		t.Errorf("a refused batch still sent %d statement(s)", len(got))
	}
	// And it has to say what to do, because the caller's next move is
	// a decision about a transaction boundary that drops cannot make.
	for _, want := range []string{"65541", "65535", "batches"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n  %v", want, err)
		}
	}
}

// One under the limit is sent, so the check is a ceiling rather than a
// cliff a batch falls off early.
func TestABatchAtTheParameterLimitIsSent(t *testing.T) {
	ent, drv, db := plEntity(t)

	// 9362 rows x 7 = 65534 parameters.
	rows := make([]plRow, 9362)
	for i := range rows {
		rows[i] = plRow{A: "a", B: "b", C: "c", D: "d", E: "e", F: "f", G: "g"}
	}
	if _, err := ent.CreateMany(db, context.Background(), rows); err != nil {
		t.Fatalf("CreateMany: %v", err)
	}
	if got := drv.Statements(); len(got) != 1 {
		t.Fatalf("statements = %d, want 1", len(got))
	}
	if n := len(drv.Statements()[0].Args); n != 65534 {
		t.Errorf("args = %d, want 65534", n)
	}
}

// The check is on the statement rather than on the builder, so a read
// with an oversized IN list is refused on the same terms — the shape
// that grows quietly as a page size is raised.
func TestAnOversizedInListIsRefusedToo(t *testing.T) {
	tbl := mysql.NewTable("pl_reads")
	id := mysql.Add(tbl, mysql.BigInt("id").NotNull())
	drv := dropstest.New()
	db := mysql.New(drv)

	ids := make([]int64, 70535)
	for i := range ids {
		ids[i] = int64(i)
	}
	var out []struct{}
	err := db.Select(id).From(tbl).Where(mysql.In(id, ids)).All(context.Background(), &out)
	if !errors.Is(err, mysql.ErrTooManyParameters) {
		t.Fatalf("All: %v, want ErrTooManyParameters", err)
	}
	if got := drv.Statements(); len(got) != 0 {
		t.Errorf("a refused read still sent %d statement(s)", len(got))
	}
}
