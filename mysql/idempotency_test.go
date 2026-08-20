package mysql_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/mysql"
)

// The key column's collation is the difference between an idempotent
// endpoint and one that hands one client another client's response:
// MySQL's default collation is case-insensitive.
func TestIdempotencyTableDDL(t *testing.T) {
	create, _ := sqlOf(mysql.CreateTable(mysql.NewIdempotencyTable("idem")))
	if !strings.Contains(create, "`key` VARCHAR(255) COLLATE utf8mb4_bin NOT NULL") {
		t.Errorf("keys must compare case-sensitively:\n%s", create)
	}
	// A plain BLOB stops at 64KB, and a response past that is
	// truncated outright under a permissive sql_mode.
	if !strings.Contains(create, "`response` LONGBLOB") {
		t.Errorf("the response column should not cap at 64KB:\n%s", create)
	}
	if !strings.Contains(create, "PRIMARY KEY (`key`)") {
		t.Errorf("key:\n%s", create)
	}
}

func TestIdempotencyRunRejectsAnEmptyKey(t *testing.T) {
	store := mysql.NewIdempotencyStore(mysql.New(&obDriver{}), "idem", time.Hour)
	if _, err := store.Run(context.Background(), "", nil); !errors.Is(err, mysql.ErrEmptyKey) {
		t.Fatalf("err = %v, want ErrEmptyKey", err)
	}
}

func TestIdempotencyFirstCallRunsAndStores(t *testing.T) {
	d := &obDriver{rowsFor: func(sql string) [][]any {
		if strings.Contains(sql, "FOR UPDATE") {
			return [][]any{{[]byte(nil), int64(0)}}
		}
		return nil
	}}
	store := mysql.NewIdempotencyStore(mysql.New(d), "idem", time.Hour)
	ran := 0
	got, err := store.Run(context.Background(), "req-1", func(*mysql.DB) ([]byte, error) {
		ran++
		return []byte(`{"ok":true}`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if ran != 1 {
		t.Errorf("the closure ran %d times", ran)
	}
	if string(got) != `{"ok":true}` {
		t.Errorf("result = %s", got)
	}

	// The claim must not be INSERT IGNORE, which would downgrade
	// every error on the statement — not only the duplicate — to a
	// warning.
	claim, _ := d.queryContaining(t, "INSERT INTO `idem`")
	if strings.Contains(claim, "IGNORE") {
		t.Errorf("the claim should not swallow unrelated errors:\n%s", claim)
	}
	if !strings.Contains(claim, "ON DUPLICATE KEY UPDATE `key` = `key`") {
		t.Errorf("claim SQL:\n%s", claim)
	}
	// The row has to be locked for the callback's duration, and a
	// locking read is also what makes the wait visible under
	// REPEATABLE READ.
	sel, _ := d.queryContaining(t, "SELECT `response`")
	if !strings.Contains(sel, "FOR UPDATE") {
		t.Errorf("the claim must be locked:\n%s", sel)
	}
	if d.countContaining("SET `response`") != 1 {
		t.Error("the response should have been written once")
	}
}

func TestIdempotencyReplayReturnsTheCachedResponse(t *testing.T) {
	d := &obDriver{rowsFor: func(sql string) [][]any {
		if strings.Contains(sql, "FOR UPDATE") {
			return [][]any{{[]byte(`{"paymentId":9}`), int64(1)}}
		}
		return nil
	}}
	store := mysql.NewIdempotencyStore(mysql.New(d), "idem", time.Hour)
	got, err := store.Run(context.Background(), "req-1", func(*mysql.DB) ([]byte, error) {
		t.Error("a completed key must not re-run the closure")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"paymentId":9}` {
		t.Errorf("result = %s", got)
	}
	if d.countContaining("SET `response`") != 0 {
		t.Error("a replay writes nothing")
	}
}

func TestIdempotencyFailedClosureLeavesNoClaim(t *testing.T) {
	d := &obDriver{rowsFor: func(sql string) [][]any {
		if strings.Contains(sql, "FOR UPDATE") {
			return [][]any{{[]byte(nil), int64(0)}}
		}
		return nil
	}}
	store := mysql.NewIdempotencyStore(mysql.New(d), "idem", time.Hour)
	boom := errors.New("payment declined")
	if _, err := store.Run(context.Background(), "req-1", func(*mysql.DB) ([]byte, error) {
		return nil, boom
	}); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if d.countContaining("SET `response`") != 0 {
		t.Error("a failed closure must not mark the key completed")
	}
}

func TestRunJSONRoundTrips(t *testing.T) {
	type resp struct {
		PaymentID int `json:"paymentId"`
	}
	d := &obDriver{rowsFor: func(sql string) [][]any {
		if strings.Contains(sql, "FOR UPDATE") {
			return [][]any{{[]byte(`{"paymentId":42}`), int64(1)}}
		}
		return nil
	}}
	store := mysql.NewIdempotencyStore(mysql.New(d), "idem", time.Hour)
	out, err := mysql.RunJSON(store, context.Background(), "req-1", func(*mysql.DB) (resp, error) {
		t.Error("the cached response should have short-circuited this")
		return resp{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.PaymentID != 42 {
		t.Errorf("out = %+v", out)
	}
}

// The expiry cutoff is bound from Go: a DATETIME carries no zone, so
// comparing it against the server's NOW() would be wrong by the
// server's time_zone rather than merely imprecise.
func TestIdempotencyCleanupBindsItsCutoff(t *testing.T) {
	d := &obDriver{}
	store := mysql.NewIdempotencyStore(mysql.New(d), "idem", time.Hour)
	if _, err := store.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	q, args := d.queryContaining(t, "DELETE FROM `idem`")
	if strings.Contains(q, "NOW(") || strings.Contains(q, "CURRENT_TIMESTAMP") {
		t.Errorf("the server's clock must not appear:\n%s", q)
	}
	if _, ok := args[0].(time.Time); !ok {
		t.Errorf("cutoff = %T, want a Go time", args[0])
	}
}

func TestIdempotencyDefaults(t *testing.T) {
	store := mysql.NewIdempotencyStore(mysql.New(&obDriver{}), "", 0)
	if store.Table() != "idempotency_keys" {
		t.Errorf("table = %q", store.Table())
	}
}
