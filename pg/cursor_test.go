package pg_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

func TestCursorEncodeDecodeRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	cases := []any{int64(7), "hello", true, 1.5, now, []byte("payload")}
	spec := pg.NewCursorSpec(
		pg.OrderKey{Col: pg.Add(pg.NewTable("t"), pg.BigInt("a")).Column},
		pg.OrderKey{Col: pg.Add(pg.NewTable("t"), pg.Text("b")).Column},
		pg.OrderKey{Col: pg.Add(pg.NewTable("t"), pg.Boolean("c")).Column},
		pg.OrderKey{Col: pg.Add(pg.NewTable("t"), pg.DoublePrecision("d")).Column},
		pg.OrderKey{Col: pg.Add(pg.NewTable("t"), pg.Timestamp("e", true)).Column},
		pg.OrderKey{Col: pg.Add(pg.NewTable("t"), pg.Bytea("f")).Column},
	)
	cur, err := pg.EncodeCursor(spec, cases...)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := cur.Decode()
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(got) != len(cases) {
		t.Fatalf("len: %d vs %d", len(got), len(cases))
	}
	for i, want := range cases {
		switch w := want.(type) {
		case time.Time:
			if !got[i].(time.Time).Equal(w) {
				t.Errorf("[%d] time: got %v want %v", i, got[i], w)
			}
		case []byte:
			gb, ok := got[i].([]byte)
			if !ok || string(gb) != string(w) {
				t.Errorf("[%d] bytes: got %v want %v", i, got[i], w)
			}
		default:
			if got[i] != want {
				t.Errorf("[%d] got %v (%T) want %v (%T)", i, got[i], got[i], want, want)
			}
		}
	}
}

func TestCursorEncodeRejectsLenMismatch(t *testing.T) {
	spec := pg.NewCursorSpec(
		pg.OrderKey{Col: pg.Add(pg.NewTable("t"), pg.BigInt("id")).Column},
	)
	if _, err := pg.EncodeCursor(spec, int64(1), int64(2)); err == nil {
		t.Error("expected len mismatch error")
	}
}

func TestCursorDecodeRejectsCorrupt(t *testing.T) {
	if _, err := pg.Cursor("not-base64!").Decode(); err == nil {
		t.Error("expected base64 error")
	}
	if _, err := pg.Cursor("").Decode(); err == nil {
		t.Error("expected empty-cursor error")
	}
}

func TestSelectAfterCursorEmitsKeysetWhere(t *testing.T) {
	users := pg.NewTable("users")
	createdAt := pg.Add(users, pg.Timestamp("createdAt", true))
	id := pg.Add(users, pg.BigInt("id"))
	spec := pg.NewCursorSpec(
		pg.OrderKey{Col: createdAt.Column, Desc: true},
		pg.OrderKey{Col: id.Column, Desc: true},
	)

	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cur, err := pg.EncodeCursor(spec, when, int64(42))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	q := pg.New(noopDriver{}).Select(id).
		From(users).
		OrderByCursor(spec).
		AfterCursor(spec, cur).
		Limit(50)

	sql, _ := q.ToSQL()
	// Must contain row-wise expansion (OR between rows, AND inside).
	if !strings.Contains(sql, "ORDER BY") || !strings.Contains(sql, "DESC") {
		t.Errorf("missing ORDER BY DESC: %s", sql)
	}
	if !strings.Contains(sql, ` < $`) || !strings.Contains(sql, " OR ") {
		t.Errorf("missing keyset OR/strict comparison: %s", sql)
	}
	if !strings.Contains(sql, " = $") {
		t.Errorf("missing equality on leading key: %s", sql)
	}
}

func TestSelectBeforeCursorReversesComparison(t *testing.T) {
	posts := pg.NewTable("posts")
	id := pg.Add(posts, pg.BigInt("id"))
	spec := pg.NewCursorSpec(
		pg.OrderKey{Col: id.Column}, // ASC
	)
	cur, _ := pg.EncodeCursor(spec, int64(100))

	q := pg.New(noopDriver{}).Select(id).
		From(posts).
		OrderByCursor(spec).
		BeforeCursor(spec, cur).
		Limit(10)
	sql, _ := q.ToSQL()
	// ASC + backward → strict comparator is '<'
	if !strings.Contains(sql, ` < $`) {
		t.Errorf("BeforeCursor on ASC should emit <, got: %s", sql)
	}
}

func TestSelectAfterCursorEmptyIsNoOp(t *testing.T) {
	users := pg.NewTable("users")
	id := pg.Add(users, pg.BigInt("id"))
	spec := pg.NewCursorSpec(pg.OrderKey{Col: id.Column})

	q := pg.New(noopDriver{}).Select(id).
		From(users).
		OrderByCursor(spec).
		AfterCursor(spec, "").
		Limit(10)
	sql, _ := q.ToSQL()
	if strings.Contains(sql, "WHERE") {
		t.Errorf("empty cursor should not add WHERE: %s", sql)
	}
}

func TestSelectAfterCursorMalformedFlagsFalse(t *testing.T) {
	users := pg.NewTable("users")
	id := pg.Add(users, pg.BigInt("id"))
	spec := pg.NewCursorSpec(pg.OrderKey{Col: id.Column})

	q := pg.New(noopDriver{}).Select(id).
		From(users).
		OrderByCursor(spec).
		AfterCursor(spec, "garbage!!!").
		Limit(10)
	sql, _ := q.ToSQL()
	if !strings.Contains(sql, "FALSE") {
		t.Errorf("malformed cursor should bail to FALSE, got: %s", sql)
	}
}

func TestOrderByCursorRespectsNulls(t *testing.T) {
	t1 := pg.NewTable("t")
	c := pg.Add(t1, pg.Text("c"))
	spec := pg.NewCursorSpec(
		pg.OrderKey{Col: c.Column, Desc: true, Nulls: pg.NullsLast},
	)
	q := pg.New(noopDriver{}).Select(c).From(t1).OrderByCursor(spec)
	sql, _ := q.ToSQL()
	if !strings.Contains(sql, "NULLS LAST") {
		t.Errorf("missing NULLS LAST: %s", sql)
	}
}

// noopDriver is a do-nothing drops.Driver for rendering-only tests.
type noopDriver struct{}

func (noopDriver) Exec(_ context.Context, _ string, _ ...any) (drops.Result, error) {
	return nil, nil
}
func (noopDriver) Query(_ context.Context, _ string, _ ...any) (drops.Rows, error) {
	return nil, nil
}
func (noopDriver) Begin(_ context.Context) (drops.Tx, error) { return nil, nil }
