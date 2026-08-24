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

// A cursor sitting on a NULL used to render `note > $1` bound to nil,
// which under SQL's three-valued logic matches nothing — so the caller
// got an empty page rather than the rest of the walk, on every page
// from then on, with nothing anywhere saying why. The guard has to be
// written against where the ORDER BY puts the NULLs instead.
func TestKeysetGuardPagesPastANull(t *testing.T) {
	tbl := pg.NewTable("notes")
	note := pg.Add(tbl, pg.Text("note"))
	id := pg.Add(tbl, pg.BigInt("id").NotNull())
	db := pg.New(noopDriver{})

	// Ascending is NULLS LAST by default: the NULLs are the tail of
	// the walk, so nothing on that key follows one and only the
	// tiebreaker contributes.
	asc := pg.NewCursorSpec(pg.OrderKey{Col: note.Column}, pg.OrderKey{Col: id.Column})
	atNull, err := pg.EncodeCursor(asc, nil, int64(7))
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	sql, _ := db.Select().From(tbl).OrderByCursor(asc).AfterCursor(asc, atNull).ToSQL()
	if strings.Contains(sql, `"notes"."note" >`) {
		t.Errorf("nothing sorts past a trailing NULL: %s", sql)
	}
	if !strings.Contains(sql, `(("notes"."note" IS NULL) AND ("notes"."id" > $1))`) {
		t.Errorf("tiebreaker must carry the walk through the NULL block: %s", sql)
	}

	// Ascending past a value on a nullable column has to reach the
	// NULLs that sort after it.
	atValue, err := pg.EncodeCursor(asc, "m", int64(7))
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	sql, _ = db.Select().From(tbl).OrderByCursor(asc).AfterCursor(asc, atValue).ToSQL()
	if !strings.Contains(sql, `(("notes"."note" > $1) OR ("notes"."note" IS NULL))`) {
		t.Errorf("ascending on a nullable column must include the NULLs ahead: %s", sql)
	}

	// Descending is NULLS FIRST by default, so past a NULL lies every
	// non-NULL row.
	desc := pg.NewCursorSpec(
		pg.OrderKey{Col: note.Column, Desc: true},
		pg.OrderKey{Col: id.Column, Desc: true},
	)
	descAtNull, err := pg.EncodeCursor(desc, nil, int64(7))
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	sql, _ = db.Select().From(tbl).OrderByCursor(desc).AfterCursor(desc, descAtNull).ToSQL()
	if !strings.Contains(sql, `("notes"."note" IS NOT NULL)`) {
		t.Errorf("descending past a leading NULL must reach the non-NULL rows: %s", sql)
	}
	if !strings.Contains(sql, `(("notes"."note" IS NULL) AND ("notes"."id" < $1))`) {
		t.Errorf("descending tiebreaker inside the NULL block: %s", sql)
	}
}

// The NULLS placement the spec asks for is the one the guard is built
// against — the default is not the only answer PostgreSQL accepts.
func TestKeysetGuardFollowsTheDeclaredNullsPlacement(t *testing.T) {
	tbl := pg.NewTable("notes")
	note := pg.Add(tbl, pg.Text("note"))
	id := pg.Add(tbl, pg.BigInt("id").NotNull())
	db := pg.New(noopDriver{})

	spec := pg.NewCursorSpec(
		pg.OrderKey{Col: note.Column, Nulls: pg.NullsFirst},
		pg.OrderKey{Col: id.Column},
	)
	atNull, err := pg.EncodeCursor(spec, nil, int64(7))
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	sql, _ := db.Select().From(tbl).OrderByCursor(spec).AfterCursor(spec, atNull).ToSQL()
	if !strings.Contains(sql, `("notes"."note" IS NOT NULL)`) {
		t.Errorf("ASC NULLS FIRST: past a NULL lies every non-NULL row: %s", sql)
	}

	atValue, err := pg.EncodeCursor(spec, "m", int64(7))
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	sql, _ = db.Select().From(tbl).OrderByCursor(spec).AfterCursor(spec, atValue).ToSQL()
	if strings.Contains(sql, `"notes"."note" IS NULL) `) {
		t.Errorf("ASC NULLS FIRST: the NULLs are behind, not ahead: %s", sql)
	}
}

// A NOT NULL column must not pay for a disjunct that can never match.
func TestKeysetGuardOmitsTheNullBranchOnANotNullColumn(t *testing.T) {
	tbl := pg.NewTable("notes")
	id := pg.Add(tbl, pg.BigInt("id").NotNull())
	spec := pg.NewCursorSpec(pg.OrderKey{Col: id.Column})
	cur, err := pg.EncodeCursor(spec, int64(7))
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	sql, _ := pg.New(noopDriver{}).Select().From(tbl).AfterCursor(spec, cur).ToSQL()
	if strings.Contains(sql, "IS NULL") {
		t.Errorf("NOT NULL column should not get a NULL disjunct: %s", sql)
	}
}

// Paging backward reverses the order, and reversing the order reverses
// the NULL placement with it.
func TestKeysetGuardReversesTheNullPlacementWhenPagingBackward(t *testing.T) {
	tbl := pg.NewTable("notes")
	note := pg.Add(tbl, pg.Text("note"))
	id := pg.Add(tbl, pg.BigInt("id").NotNull())
	spec := pg.NewCursorSpec(pg.OrderKey{Col: note.Column}, pg.OrderKey{Col: id.Column})
	atNull, err := pg.EncodeCursor(spec, nil, int64(7))
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	sql, _ := pg.New(noopDriver{}).Select().From(tbl).BeforeCursor(spec, atNull).ToSQL()
	// ASC NULLS LAST read backwards is DESC NULLS FIRST: before a NULL
	// lies every non-NULL row.
	if !strings.Contains(sql, `("notes"."note" IS NOT NULL)`) {
		t.Errorf("backward past a NULL must reach the non-NULL rows: %s", sql)
	}
}

// A nullable column is a pointer on the row struct, so that is the
// shape the last row of a page hands to EncodeCursor.
func TestCursorEncodesAPointerAsItsPointee(t *testing.T) {
	tbl := pg.NewTable("notes")
	note := pg.Add(tbl, pg.Text("note"))
	spec := pg.NewCursorSpec(pg.OrderKey{Col: note.Column})

	v := "m"
	fromPtr, err := pg.EncodeCursor(spec, &v)
	if err != nil {
		t.Fatalf("EncodeCursor(&v): %v", err)
	}
	fromVal, err := pg.EncodeCursor(spec, v)
	if err != nil {
		t.Fatalf("EncodeCursor(v): %v", err)
	}
	if fromPtr != fromVal {
		t.Errorf("pointer and value cursors differ: %q vs %q", fromPtr, fromVal)
	}

	var null *string
	fromNil, err := pg.EncodeCursor(spec, null)
	if err != nil {
		t.Fatalf("EncodeCursor((*string)(nil)): %v", err)
	}
	vals, err := fromNil.Decode()
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(vals) != 1 || vals[0] != nil {
		t.Errorf("a nil pointer is the NULL the guard pages past, got %#v", vals)
	}
}
