package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/sqlite"
)

// sqlitePageNote has a nullable ordering column, which is the ordinary
// shape of a "sort by the optional field" endpoint and the one the
// cursor used to lose rows on.
type sqlitePageNote struct {
	ID   int64   `drop:"id"`
	Note *string `drop:"note"`
}

// walkSQLitePages follows the cursor to exhaustion and returns the ids
// in the order they were handed out. Limit 1 so every page boundary —
// and so every cursor — is exercised.
func walkSQLitePages(t *testing.T, db *sqlite.DB, ent *sqlite.Entity[sqlitePageNote], order ...sqlite.OrderingColumn) []int64 {
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
// comparison, (note, id) > (?, ?). Neither half survives a NULL.
// SQLite sorts NULL smallest, so ascending the very first page ends on
// one and gob refuses to encode the nil pointer at all; descending they
// are the tail, and the comparison against the last non-NULL row yields
// NULL for every one of them, so the walk stops short with nothing
// anywhere reporting it.
func TestSQLitePageWalksPastANullOrderingColumn(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()
	tbl := sqlite.NewTable("page_null_walk")
	id := sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
	note := sqlite.Add(tbl, sqlite.Text("note").Nullable())
	exec(t, db, sqlite.CreateTable(tbl))

	a, b := "a", "b"
	for i, v := range []*string{&a, &b, nil, nil} {
		if _, err := db.Insert(tbl).Values(id.Val(int64(i+1)), note.ValPtr(v)).Exec(ctx); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	ent := sqlite.NewEntity[sqlitePageNote](tbl)

	// SQLite sorts NULL as the smallest value, so ASC is NULLS FIRST:
	// the two NULLs by id, then the two notes.
	if got := walkSQLitePages(t, db, ent, sqlite.Asc(note), sqlite.Asc(id)); !sameIDs(got, 3, 4, 1, 2) {
		t.Errorf("ascending walk visited %v, want 3 4 1 2", got)
	}
	// DESC is NULLS LAST, read the other way round.
	if got := walkSQLitePages(t, db, ent, sqlite.Desc(note), sqlite.Desc(id)); !sameIDs(got, 2, 1, 4, 3) {
		t.Errorf("descending walk visited %v, want 2 1 4 3", got)
	}
}

type sqlitePageStamped struct {
	ID        int64     `drop:"id"`
	CreatedAt time.Time `drop:"createdAt"`
}

// The gob cursor could not carry a time.Time inside an interface —
// encoding/gob wants a gob.Register nothing performed — so paging by a
// timestamp, the column keyset pagination exists for, failed on the
// first page boundary.
func TestSQLitePageWalksATimestampOrderingColumn(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()
	tbl := sqlite.NewTable("page_ts_walk")
	id := sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
	created := sqlite.Add(tbl, sqlite.Timestamp("createdAt", true).NotNull())
	exec(t, db, sqlite.CreateTable(tbl))

	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		if _, err := db.Insert(tbl).
			Values(id.Val(int64(i+1)), created.Val(base.Add(time.Duration(i)*time.Hour))).
			Exec(ctx); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	ent := sqlite.NewEntity[sqlitePageStamped](tbl)
	var seen []int64
	cursor := ""
	for i := 0; i < 16; i++ {
		p, err := ent.Page(db).OrderBy(sqlite.Asc(created), sqlite.Asc(id)).Limit(1).After(cursor).All(ctx)
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
