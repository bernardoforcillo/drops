package d1_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/cloudflare/d1"
)

// batchOK builds a /raw envelope with one result per statement.
func batchOK(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = `{"success":true,"meta":{"changes":1,"last_row_id":` +
			string(rune('0'+i+1)) + `,"rows_read":0,"rows_written":1},"results":{"columns":[],"rows":[]}}`
	}
	return `{"result":[` + strings.Join(parts, ",") + `],"success":true,"errors":[],"messages":[]}`
}

// The central claim of the design: a transaction is one request, sent
// at commit, not one request per statement.
func TestTransactionSendsOneRequestAtCommit(t *testing.T) {
	s := newServer(t, func(int) (int, string) { return http.StatusOK, batchOK(3) })
	drv := s.driver()

	tx, err := drv.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for _, sql := range []string{
		"INSERT INTO orders (id) VALUES (?)",
		"INSERT INTO lines (order_id, sku) VALUES (?, ?)",
		"UPDATE stock SET n = n - 1 WHERE sku = ?",
	} {
		if _, err := tx.Exec(context.Background(), sql, 1, "sku"); err != nil {
			t.Fatalf("Exec: %v", err)
		}
	}
	if s.requests != 0 {
		t.Fatalf("%d request(s) before Commit, want 0", s.requests)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if s.requests != 1 {
		t.Fatalf("%d request(s) after Commit, want 1", s.requests)
	}

	sql, params := s.lastBody()
	for _, want := range []string{"INSERT INTO orders", "INSERT INTO lines", "UPDATE stock"} {
		if !strings.Contains(sql, want) {
			t.Errorf("batch is missing %q:\n%s", want, sql)
		}
	}
	if got := strings.Count(sql, ";"); got != 2 {
		t.Errorf("%d separators for 3 statements, want 2", got)
	}
	if len(params) != 6 {
		t.Errorf("params = %#v, want the three statements' parameters flattened in order", params)
	}
}

// Before the commit there is no honest row count to give, so the
// result says so rather than inventing a zero.
func TestPendingResultBecomesRealAfterCommit(t *testing.T) {
	s := newServer(t, func(int) (int, string) { return http.StatusOK, batchOK(1) })
	tx, _ := s.driver().Begin(context.Background())

	res, err := tx.Exec(context.Background(), "UPDATE t SET a = 1")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if _, err := res.RowsAffected(); !errors.Is(err, d1.ErrPending) {
		t.Errorf("RowsAffected before commit = %v, want ErrPending", err)
	}
	if r, isD1 := res.(*d1.Result); !isD1 || !r.Pending() {
		t.Error("Pending() should be true before the commit")
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		t.Errorf("RowsAffected after commit = %d, %v, want 1", n, err)
	}
}

// Reading inside the transaction cannot work and must not pretend to:
// running the SELECT outside the buffer would break read-your-writes
// silently.
func TestQueryInsideATransactionIsRefused(t *testing.T) {
	s := newServer(t, func(int) (int, string) { return http.StatusOK, batchOK(1) })
	tx, _ := s.driver().Begin(context.Background())
	defer func() { _ = tx.Rollback(context.Background()) }()

	rows, err := tx.Query(context.Background(), "SELECT 1")
	if !errors.Is(err, d1.ErrTxQuery) {
		t.Errorf("err = %v, want ErrTxQuery", err)
	}
	if rows != nil {
		t.Errorf("rows = %#v, want nil alongside the error", rows)
	}
	if s.requests != 0 {
		t.Errorf("%d request(s) — a refused Query must not reach the network", s.requests)
	}
}

func TestRollbackSendsNothingAndSettlesResults(t *testing.T) {
	s := newServer(t, func(int) (int, string) { return http.StatusOK, batchOK(1) })
	tx, _ := s.driver().Begin(context.Background())

	res, _ := tx.Exec(context.Background(), "DELETE FROM t")
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if s.requests != 0 {
		t.Errorf("%d request(s) on rollback, want 0 — nothing had been sent", s.requests)
	}
	// A result kept across the rollback must answer, not hang on
	// ErrPending forever.
	if _, err := res.RowsAffected(); !errors.Is(err, d1.ErrTxDone) {
		t.Errorf("RowsAffected after rollback = %v, want ErrTxDone", err)
	}
}

func TestEmptyTransactionCommitsWithoutARequest(t *testing.T) {
	s := newServer(t, func(int) (int, string) { return http.StatusOK, batchOK(1) })
	tx, _ := s.driver().Begin(context.Background())
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("Commit of an empty transaction: %v", err)
	}
	if s.requests != 0 {
		t.Errorf("%d request(s), want 0 — an empty batch is an error at D1", s.requests)
	}
}

func TestFinishedTransactionRefusesFurtherWork(t *testing.T) {
	s := newServer(t, func(int) (int, string) { return http.StatusOK, batchOK(1) })
	tx, _ := s.driver().Begin(context.Background())
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := tx.Exec(context.Background(), "INSERT INTO t VALUES (1)"); !errors.Is(err, d1.ErrTxDone) {
		t.Errorf("Exec after commit = %v, want ErrTxDone", err)
	}
	if err := tx.Commit(context.Background()); !errors.Is(err, d1.ErrTxDone) {
		t.Errorf("second Commit = %v, want ErrTxDone", err)
	}
	if err := tx.Rollback(context.Background()); !errors.Is(err, d1.ErrTxDone) {
		t.Errorf("Rollback after commit = %v, want ErrTxDone", err)
	}
}

func TestNestedTransactionIsRefused(t *testing.T) {
	s := newServer(t, func(int) (int, string) { return http.StatusOK, batchOK(1) })
	tx, _ := s.driver().Begin(context.Background())
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Begin(context.Background()); !errors.Is(err, d1.ErrNestedTx) {
		t.Errorf("nested Begin = %v, want ErrNestedTx", err)
	}
}

// drops.InTx is the shape callers actually write, so it has to work
// end to end for a write-only unit of work.
func TestInTxCommitsAWriteOnlyUnitOfWork(t *testing.T) {
	s := newServer(t, func(int) (int, string) { return http.StatusOK, batchOK(2) })
	drv := s.driver()

	err := drops.InTx(context.Background(), drv, func(tx drops.Tx) error {
		if _, err := tx.Exec(context.Background(), "INSERT INTO a VALUES (?)", 1); err != nil {
			return err
		}
		_, err := tx.Exec(context.Background(), "INSERT INTO b VALUES (?)", 2)
		return err
	})
	if err != nil {
		t.Fatalf("InTx: %v", err)
	}
	if s.requests != 1 {
		t.Errorf("%d request(s), want 1", s.requests)
	}
}

// A callback that fails must leave nothing behind, which for a
// buffered transaction means nothing was ever sent.
func TestInTxRollsBackWithoutSendingAnything(t *testing.T) {
	s := newServer(t, func(int) (int, string) { return http.StatusOK, batchOK(1) })
	sentinel := errors.New("nope")

	err := drops.InTx(context.Background(), s.driver(), func(tx drops.Tx) error {
		if _, err := tx.Exec(context.Background(), "INSERT INTO a VALUES (1)"); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("InTx = %v, want the callback's error", err)
	}
	if s.requests != 0 {
		t.Errorf("%d request(s) after a failed callback, want 0", s.requests)
	}
}

// Batch --------------------------------------------------------------

func TestBatchRunReturnsOneResultPerStatement(t *testing.T) {
	s := newServer(t, func(int) (int, string) { return http.StatusOK, batchOK(2) })
	b := d1.NewBatch(s.driver()).
		Add("INSERT INTO t (a) VALUES (?)", 1).
		Add("INSERT INTO t (a) VALUES (?)", 2)

	if b.Len() != 2 {
		t.Errorf("Len = %d, want 2", b.Len())
	}
	results, err := b.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("%d result(s), want 2", len(results))
	}
	for i, r := range results {
		if n, err := r.RowsAffected(); err != nil || n != 1 {
			t.Errorf("result %d: RowsAffected = %d, %v", i, n, err)
		}
	}
	if s.requests != 1 {
		t.Errorf("%d request(s), want 1", s.requests)
	}
}

func TestBatchHoldsBindingErrorsUntilRun(t *testing.T) {
	s := newServer(t, func(int) (int, string) { return http.StatusOK, batchOK(1) })
	type bad struct{ X int }
	b := d1.NewBatch(s.driver()).
		Add("INSERT INTO t VALUES (?)", 1).
		Add("INSERT INTO t VALUES (?)", bad{})

	if b.Err() == nil {
		t.Error("Err() should report the binding failure")
	}
	if _, err := b.Run(context.Background()); !errors.Is(err, d1.ErrUnsupportedParam) {
		t.Errorf("Run = %v, want ErrUnsupportedParam", err)
	}
	if s.requests != 0 {
		t.Errorf("%d request(s), want 0", s.requests)
	}
}

func TestEmptyBatchIsAnError(t *testing.T) {
	s := newServer(t, func(int) (int, string) { return http.StatusOK, batchOK(1) })
	if _, err := d1.NewBatch(s.driver()).Run(context.Background()); !errors.Is(err, d1.ErrNoStatements) {
		t.Errorf("empty Run = %v, want ErrNoStatements", err)
	}
}

// A failed batch has to settle every result it handed out, or a
// caller holding one waits on ErrPending forever.
func TestFailedCommitSettlesEveryPendingResult(t *testing.T) {
	s := newServer(t, func(int) (int, string) {
		return http.StatusOK, `{"result":null,"success":false,"errors":[{"code":7500,"message":"no such table: t"}],"messages":[]}`
	})
	tx, _ := s.driver().Begin(context.Background())
	a, _ := tx.Exec(context.Background(), "INSERT INTO t VALUES (1)")
	b, _ := tx.Exec(context.Background(), "INSERT INTO t VALUES (2)")

	if err := tx.Commit(context.Background()); err == nil {
		t.Fatal("Commit should have failed")
	}
	for i, res := range []drops.Result{a, b} {
		_, err := res.RowsAffected()
		if errors.Is(err, d1.ErrPending) {
			t.Errorf("result %d is still pending after a failed commit", i)
		}
		if !errors.Is(err, d1.ErrUndefinedTable) {
			t.Errorf("result %d: err = %v, want the batch's failure", i, err)
		}
	}
}

// The batch body must stay valid SQL when a caller wrote trailing
// semicolons of their own.
func TestTrailingSemicolonsDoNotDoubleUp(t *testing.T) {
	s := newServer(t, func(int) (int, string) { return http.StatusOK, batchOK(2) })
	if _, err := d1.NewBatch(s.driver()).
		Add("INSERT INTO t VALUES (1);").
		Add("INSERT INTO t VALUES (2);").
		Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var body struct {
		SQL string `json:"sql"`
	}
	if err := json.Unmarshal([]byte(s.bodies[0]), &body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body.SQL, ";;") || strings.Contains(body.SQL, ";\n;") {
		t.Errorf("doubled separator in batch body:\n%s", body.SQL)
	}
}
