package integration_test

import (
	"context"
	"testing"

	"github.com/bernardoforcillo/drops/clickhouse"
	"github.com/bernardoforcillo/drops/integration"
)

// Where ClickHouse puts NULLs in an ORDER BY, executed rather than
// recited. The keyset guard in clickhouse/cursor.go is written against
// one specific answer — NULLS LAST in both directions, unlike
// PostgreSQL's "NULL is the largest value" and SQLite's "NULL is the
// smallest" — and that answer came out of ClickHouse's documentation
// and its ORDER BY parser, not out of a server. This is the server.
//
// DESC is the case that separates the three rules: PostgreSQL would
// lead with the NULLs here and SQLite would trail them for the
// opposite reason, while ClickHouse leaves them at the end because its
// NULL placement does not move with the sort direction.
func TestCHOrderByLeavesNullsLastInBothDirections(t *testing.T) {
	db := openCH(t)
	tbl := clickhouse.NewTable(integration.UniqueName(t, "nullorder"))
	dropCH(t, db, tbl)

	id := clickhouse.Add(tbl, clickhouse.UInt64("id"))
	note := clickhouse.Add(tbl, clickhouse.String("note").Nullable())
	tbl.Engine(clickhouse.MergeTree()).OrderBy(id)
	execCH(t, db, clickhouse.CreateTable(tbl))

	ctx := context.Background()
	for _, r := range []struct {
		id   uint64
		note *string
	}{
		{1, chStrPtr("a")},
		{2, nil},
		{3, chStrPtr("c")},
	} {
		if _, err := db.Insert(tbl).Row(id.Val(r.id), note.ValPtr(r.note)).Exec(ctx); err != nil {
			t.Fatalf("insert %d: %v", r.id, err)
		}
	}

	read := func(spec clickhouse.CursorSpec) []uint64 {
		t.Helper()
		var got []uint64
		if err := db.Select(id).From(tbl).OrderByCursor(spec).All(ctx, &got); err != nil {
			t.Fatalf("select: %v", err)
		}
		return got
	}

	asc := clickhouse.NewCursorSpec(
		clickhouse.OrderKey{Col: note},
		clickhouse.OrderKey{Col: id},
	)
	if got := read(asc); !sameCHIDs(got, []uint64{1, 3, 2}) {
		t.Errorf("ASC default = %v, want [1 3 2] — the NULL row must be last", got)
	}

	desc := clickhouse.NewCursorSpec(
		clickhouse.OrderKey{Col: note, Desc: true},
		clickhouse.OrderKey{Col: id, Desc: true},
	)
	if got := read(desc); !sameCHIDs(got, []uint64{3, 1, 2}) {
		t.Errorf("DESC default = %v, want [3 1 2] — ClickHouse keeps the NULL last descending too", got)
	}

	descFirst := clickhouse.NewCursorSpec(
		clickhouse.OrderKey{Col: note, Desc: true, Nulls: clickhouse.NullsFirst},
		clickhouse.OrderKey{Col: id, Desc: true},
	)
	if got := read(descFirst); !sameCHIDs(got, []uint64{2, 3, 1}) {
		t.Errorf("DESC NULLS FIRST = %v, want [2 3 1]", got)
	}

	ascLast := clickhouse.NewCursorSpec(
		clickhouse.OrderKey{Col: note, Nulls: clickhouse.NullsLast},
		clickhouse.OrderKey{Col: id},
	)
	if got := read(ascLast); !sameCHIDs(got, []uint64{1, 3, 2}) {
		t.Errorf("ASC NULLS LAST = %v, want [1 3 2]", got)
	}
}

// The guard is only right if a walk that uses it sees every row once.
// A cursor sitting on a NULL used to render `note > ?` bound to NULL,
// which matches nothing — so the page after the NULL row came back
// empty and looked exactly like the end of the table.
func TestCHKeysetWalkVisitsEveryRowAcrossANull(t *testing.T) {
	db := openCH(t)
	tbl := clickhouse.NewTable(integration.UniqueName(t, "nullwalk"))
	dropCH(t, db, tbl)

	id := clickhouse.Add(tbl, clickhouse.UInt64("id"))
	note := clickhouse.Add(tbl, clickhouse.String("note").Nullable())
	tbl.Engine(clickhouse.MergeTree()).OrderBy(id)
	execCH(t, db, clickhouse.CreateTable(tbl))

	ctx := context.Background()
	// Two NULLs in the middle of the order so the walk has to enter
	// the NULL block, step within it on the tiebreaker, and leave it.
	rows := []struct {
		id   uint64
		note *string
	}{
		{1, chStrPtr("a")},
		{2, chStrPtr("b")},
		{3, nil},
		{4, nil},
		{5, chStrPtr("c")},
	}
	for _, r := range rows {
		if _, err := db.Insert(tbl).Row(id.Val(r.id), note.ValPtr(r.note)).Exec(ctx); err != nil {
			t.Fatalf("insert %d: %v", r.id, err)
		}
	}

	type row struct {
		ID   uint64  `drop:"id"`
		Note *string `drop:"note"`
	}

	walk := func(name string, spec clickhouse.CursorSpec) []uint64 {
		t.Helper()
		var seen []uint64
		var cur clickhouse.Cursor
		// One row per page, so every boundary in the table — including
		// both edges of the NULL block — becomes a cursor.
		for range len(rows) + 2 {
			var page []row
			q := db.Select(id, note).From(tbl).OrderByCursor(spec).Limit(1)
			if cur != "" {
				q = q.AfterCursor(spec, cur)
			}
			if err := q.All(ctx, &page); err != nil {
				t.Fatalf("%s page: %v", name, err)
			}
			if len(page) == 0 {
				break
			}
			last := page[len(page)-1]
			seen = append(seen, last.ID)
			next, err := clickhouse.EncodeCursor(spec, last.Note, last.ID)
			if err != nil {
				t.Fatalf("%s EncodeCursor: %v", name, err)
			}
			cur = next
		}
		return seen
	}

	asc := clickhouse.NewCursorSpec(
		clickhouse.OrderKey{Col: note},
		clickhouse.OrderKey{Col: id},
	)
	if got := walk("asc", asc); !sameCHIDs(got, []uint64{1, 2, 5, 3, 4}) {
		t.Errorf("ascending walk = %v, want [1 2 5 3 4] — every row once, NULLs last", got)
	}

	desc := clickhouse.NewCursorSpec(
		clickhouse.OrderKey{Col: note, Desc: true},
		clickhouse.OrderKey{Col: id, Desc: true},
	)
	if got := walk("desc", desc); !sameCHIDs(got, []uint64{5, 2, 1, 4, 3}) {
		t.Errorf("descending walk = %v, want [5 2 1 4 3] — NULLs stay last descending", got)
	}

	first := clickhouse.NewCursorSpec(
		clickhouse.OrderKey{Col: note, Nulls: clickhouse.NullsFirst},
		clickhouse.OrderKey{Col: id},
	)
	if got := walk("nulls-first", first); !sameCHIDs(got, []uint64{3, 4, 1, 2, 5}) {
		t.Errorf("NULLS FIRST walk = %v, want [3 4 1 2 5]", got)
	}
}

func chStrPtr(s string) *string { return &s }

func sameCHIDs(got, want []uint64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
