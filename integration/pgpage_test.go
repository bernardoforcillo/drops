package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/pg"
)

// pageNote has a nullable ordering column, which is the ordinary shape
// of a "sort by the optional field" endpoint and the one the cursor
// used to lose rows on.
type pageNote struct {
	ID   int64   `drop:"id"`
	Note *string `drop:"note"`
}

// walkPages follows the cursor to exhaustion and returns the ids in the
// order they were handed out. Limit 1 so every page boundary — and so
// every cursor — is exercised.
func walkPages(t *testing.T, db *pg.DB, ent *pg.Entity[pageNote], order ...pg.OrderingColumn) []int64 {
	t.Helper()
	ctx := context.Background()
	var seen []int64
	cursor := ""
	for i := 0; i < 16; i++ {
		p, err := ent.Page(db).OrderBy(order...).Limit(1).After(cursor).All(ctx)
		if err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		for _, item := range p.Items {
			seen = append(seen, item.ID)
		}
		if !p.HasMore {
			return seen
		}
		cursor = p.NextCursor
	}
	t.Fatalf("cursor never terminated after 16 pages: %v", seen)
	return nil
}

// Entity.Page kept its own cursor: a gob payload compared with a row
// comparison, (note, id) > ($1, $2). Neither half survives a NULL. Rows
// 3 and 4 have no note, so ascending — PostgreSQL's NULLS LAST — the
// comparison against the last non-NULL row yields NULL for them, the
// page comes back short, HasMore is false, and the walk ends two rows
// early with nothing anywhere reporting it. Descending puts the NULLs
// first, so the very first page ends on one and gob refuses to encode
// the nil pointer at all.
func TestPGPageWalksPastANullOrderingColumn(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	tbl := pg.NewTable(integration.UniqueName(t, "page_null"))
	dropPG(t, db, tbl)
	id := pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	note := pg.Add(tbl, pg.Text("note").Nullable())
	execPG(t, db, pg.CreateTable(tbl))

	a, b := "a", "b"
	for i, v := range []*string{&a, &b, nil, nil} {
		if _, err := db.Insert(tbl).Row(id.Val(int64(i+1)), note.ValPtr(v)).Exec(ctx); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	ent := pg.NewEntity[pageNote](tbl)

	// ASC is NULLS LAST: the two notes, then the two NULLs by id.
	if got := walkPages(t, db, ent, pg.Asc(note), pg.Asc(id)); !sameIDs(got, 1, 2, 3, 4) {
		t.Errorf("ascending walk visited %v, want 1 2 3 4", got)
	}
	// DESC is NULLS FIRST: the NULLs by descending id, then the notes.
	if got := walkPages(t, db, ent, pg.Desc(note), pg.Desc(id)); !sameIDs(got, 4, 3, 2, 1) {
		t.Errorf("descending walk visited %v, want 4 3 2 1", got)
	}
}

// pageStamped orders by a timestamp, which is what keyset pagination
// exists for and what the cursor package documentation uses as its
// example.
type pageStamped struct {
	ID        int64     `drop:"id"`
	CreatedAt time.Time `drop:"createdAt"`
}

// The gob cursor could not carry a time.Time inside an interface —
// encoding/gob wants a gob.Register for that and nothing performed one
// — so paging by a timestamp failed on the first page boundary with
// "gob: type not registered for interface: time.Time", from inside a
// builder that never told the caller it was using gob.
func TestPGPageWalksATimestampOrderingColumn(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	tbl := pg.NewTable(integration.UniqueName(t, "page_ts"))
	dropPG(t, db, tbl)
	id := pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	created := pg.Add(tbl, pg.Timestamp("createdAt", true).NotNull())
	execPG(t, db, pg.CreateTable(tbl))

	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		if _, err := db.Insert(tbl).
			Row(id.Val(int64(i+1)), created.Val(base.Add(time.Duration(i)*time.Hour))).
			Exec(ctx); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	ctx = context.Background()
	entTS := pg.NewEntity[pageStamped](tbl)
	var seen []int64
	cursor := ""
	for i := 0; i < 16; i++ {
		p, err := entTS.Page(db).OrderBy(pg.Asc(created), pg.Asc(id)).Limit(1).After(cursor).All(ctx)
		if err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		for _, item := range p.Items {
			seen = append(seen, item.ID)
		}
		if !p.HasMore {
			break
		}
		cursor = p.NextCursor
	}
	if !sameIDs(seen, 1, 2, 3, 4) {
		t.Errorf("timestamp walk visited %v, want 1 2 3 4", seen)
	}
}

func sameIDs(got []int64, want ...int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
