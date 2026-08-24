package clickhouse_test

import (
	"math"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/clickhouse"
)

// A cursor sitting on a NULL used to render `note > ?` bound to nil,
// which under three-valued logic matches nothing — an empty page
// rather than the rest of the walk, on every page from then on, with
// nothing anywhere saying why. The guard has to be written against
// where the ORDER BY puts the NULLs instead.
//
// ClickHouse puts them last by default in *both* directions, which is
// neither PostgreSQL's rule nor SQLite's, so DESC is the case that
// tells the three apart.
func TestKeysetGuardPagesPastANull(t *testing.T) {
	tbl := clickhouse.NewTable("notes")
	note := clickhouse.Add(tbl, clickhouse.String("note").Nullable())
	id := clickhouse.Add(tbl, clickhouse.UInt64("id"))
	db := clickhouse.New(nil)

	asc := clickhouse.NewCursorSpec(
		clickhouse.OrderKey{Col: note},
		clickhouse.OrderKey{Col: id},
	)
	atNull, err := clickhouse.EncodeCursor(asc, nil, uint64(7))
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	sql, _ := db.Select().From(tbl).OrderByCursor(asc).AfterCursor(asc, atNull).ToSQL()
	if strings.Contains(sql, `"notes"."note" >`) {
		t.Errorf("nothing sorts past a trailing NULL: %s", sql)
	}
	if !strings.Contains(sql, `(("notes"."note" IS NULL) AND ("notes"."id" > ?))`) {
		t.Errorf("tiebreaker must carry the walk through the NULL block: %s", sql)
	}

	atValue, err := clickhouse.EncodeCursor(asc, "m", uint64(7))
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	sql, _ = db.Select().From(tbl).OrderByCursor(asc).AfterCursor(asc, atValue).ToSQL()
	if !strings.Contains(sql, `(("notes"."note" > ?) OR ("notes"."note" IS NULL))`) {
		t.Errorf("ascending on a nullable column must reach the NULLs ahead: %s", sql)
	}

	// Descending. PostgreSQL would put the NULLs first here and
	// SQLite last-by-accident-of-direction; ClickHouse keeps them
	// last, so a descending walk still has the NULL block ahead of
	// it and a NULL is still the end of the road.
	desc := clickhouse.NewCursorSpec(
		clickhouse.OrderKey{Col: note, Desc: true},
		clickhouse.OrderKey{Col: id, Desc: true},
	)
	descAtValue, err := clickhouse.EncodeCursor(desc, "m", uint64(7))
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	sql, _ = db.Select().From(tbl).OrderByCursor(desc).AfterCursor(desc, descAtValue).ToSQL()
	if !strings.Contains(sql, `(("notes"."note" < ?) OR ("notes"."note" IS NULL))`) {
		t.Errorf("descending must still reach the NULLs, which ClickHouse leaves at the end: %s", sql)
	}

	descAtNull, err := clickhouse.EncodeCursor(desc, nil, uint64(7))
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	sql, _ = db.Select().From(tbl).OrderByCursor(desc).AfterCursor(desc, descAtNull).ToSQL()
	if strings.Contains(sql, `"notes"."note" <`) {
		t.Errorf("nothing sorts past a NULL descending either: %s", sql)
	}
	if !strings.Contains(sql, `(("notes"."note" IS NULL) AND ("notes"."id" < ?))`) {
		t.Errorf("descending tiebreaker inside the NULL block: %s", sql)
	}
}

// NULLS FIRST is the placement ClickHouse accepts and does not
// default to, so the guard has to follow the spec rather than the
// default.
func TestKeysetGuardFollowsTheDeclaredNullsPlacement(t *testing.T) {
	tbl := clickhouse.NewTable("notes")
	note := clickhouse.Add(tbl, clickhouse.String("note").Nullable())
	id := clickhouse.Add(tbl, clickhouse.UInt64("id"))
	db := clickhouse.New(nil)

	spec := clickhouse.NewCursorSpec(
		clickhouse.OrderKey{Col: note, Nulls: clickhouse.NullsFirst},
		clickhouse.OrderKey{Col: id},
	)
	atNull, err := clickhouse.EncodeCursor(spec, nil, uint64(7))
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	sql, _ := db.Select().From(tbl).OrderByCursor(spec).AfterCursor(spec, atNull).ToSQL()
	if !strings.Contains(sql, `("notes"."note" IS NOT NULL)`) {
		t.Errorf("past a leading NULL lies every non-NULL row: %s", sql)
	}

	atValue, err := clickhouse.EncodeCursor(spec, "m", uint64(7))
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	sql, _ = db.Select().From(tbl).OrderByCursor(spec).AfterCursor(spec, atValue).ToSQL()
	if strings.Contains(sql, `"notes"."note" IS NULL`) {
		t.Errorf("with the NULLs behind, no row past a value is NULL: %s", sql)
	}
}

// The IS NULL disjunct can never match on a column the schema says is
// NOT NULL, and it costs the planner one more range to merge.
func TestKeysetGuardOmitsTheNullBranchOnANotNullColumn(t *testing.T) {
	tbl := clickhouse.NewTable("notes")
	note := clickhouse.Add(tbl, clickhouse.String("note"))
	id := clickhouse.Add(tbl, clickhouse.UInt64("id"))
	db := clickhouse.New(nil)

	spec := clickhouse.NewCursorSpec(
		clickhouse.OrderKey{Col: note},
		clickhouse.OrderKey{Col: id},
	)
	cur, err := clickhouse.EncodeCursor(spec, "m", uint64(7))
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	sql, _ := db.Select().From(tbl).OrderByCursor(spec).AfterCursor(spec, cur).ToSQL()
	if strings.Contains(sql, "IS NULL") {
		t.Errorf("NOT NULL column must not carry a NULL branch: %s", sql)
	}
}

// Paging backward reverses the order, which reverses the direction and
// the NULL placement together.
func TestKeysetGuardReversesTheNullPlacementWhenPagingBackward(t *testing.T) {
	tbl := clickhouse.NewTable("notes")
	note := clickhouse.Add(tbl, clickhouse.String("note").Nullable())
	id := clickhouse.Add(tbl, clickhouse.UInt64("id"))
	db := clickhouse.New(nil)

	spec := clickhouse.NewCursorSpec(
		clickhouse.OrderKey{Col: note},
		clickhouse.OrderKey{Col: id},
	)
	// Forward, the NULLs are the tail; backward they are the head, so
	// a backward walk standing on a NULL has every non-NULL row ahead.
	atNull, err := clickhouse.EncodeCursor(spec, nil, uint64(7))
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	sql, _ := db.Select().From(tbl).OrderByCursor(spec).BeforeCursor(spec, atNull).ToSQL()
	if !strings.Contains(sql, `("notes"."note" IS NOT NULL)`) {
		t.Errorf("backward past a NULL must reach the non-NULL rows: %s", sql)
	}
}

// A nullable column is a pointer field on the row struct, so the value
// the last row of a page hands back is a *string, not a string. A nil
// one is the NULL the guard knows how to page past — refusing it as an
// unsupported type would make a nullable key unpageable.
func TestCursorEncodesAPointerAsItsPointee(t *testing.T) {
	tbl := clickhouse.NewTable("notes")
	note := clickhouse.Add(tbl, clickhouse.String("note").Nullable())
	spec := clickhouse.NewCursorSpec(clickhouse.OrderKey{Col: note})

	s := "m"
	cur, err := clickhouse.EncodeCursor(spec, &s)
	if err != nil {
		t.Fatalf("EncodeCursor(*string): %v", err)
	}
	vals, err := cur.Decode()
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(vals) != 1 || vals[0] != "m" {
		t.Errorf("pointer must round-trip as its pointee, got %#v", vals)
	}

	var nilp *string
	cur, err = clickhouse.EncodeCursor(spec, nilp)
	if err != nil {
		t.Fatalf("EncodeCursor((*string)(nil)): %v", err)
	}
	vals, err = cur.Decode()
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(vals) != 1 || vals[0] != nil {
		t.Errorf("a nil pointer is a NULL, got %#v", vals)
	}
}

// ClickHouse sorts NaN into the gap between the values and the NULLs,
// so a Float64 key can hold one and a page can end on it. The cursor
// cannot carry it: JSON has no NaN. That is fine as long as it is
// said out loud — encoding the value used to drop the marshal error
// on the floor, leaving a null in the payload that decoded back as a
// plain 0.0, so a page that ended on NaN resumed from zero and
// silently replayed every positive row.
func TestCursorRefusesValuesJSONCannotCarry(t *testing.T) {
	tbl := clickhouse.NewTable("m")
	v := clickhouse.Add(tbl, clickhouse.Float64("v"))
	spec := clickhouse.NewCursorSpec(clickhouse.OrderKey{Col: v})

	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		cur, err := clickhouse.EncodeCursor(spec, bad)
		if err == nil {
			vals, derr := cur.Decode()
			t.Errorf("EncodeCursor(%v) must fail; got cursor %q decoding to %#v (err %v)", bad, cur, vals, derr)
			continue
		}
		if !strings.Contains(err.Error(), "cursor value") {
			t.Errorf("EncodeCursor(%v) error should name the value: %v", bad, err)
		}
	}

	// float32 reaches the same guard.
	if _, err := clickhouse.EncodeCursor(spec, float32(math.Inf(1))); err == nil {
		t.Errorf("EncodeCursor(float32 +Inf) must fail")
	}
}
