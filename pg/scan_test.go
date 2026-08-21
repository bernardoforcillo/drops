package pg_test

import (
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// scanAudit is the shape the scanner has to reach through: a struct
// that factors the timestamps every row carries into one embedded
// type, and keeps that type unexported because it is an implementation
// detail of the package that declares it.
type scanAudit struct {
	CreatedAt time.Time
	UpdatedAt time.Time
}

type scanArticle struct {
	ID    int64
	Title string
	scanAudit
}

func TestScanReachesThroughAnUnexportedEmbeddedStruct(t *testing.T) {
	created := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	updated := created.Add(time.Hour)
	cols := []string{"id", "title", "createdAt", "updatedAt"}
	row := []any{int64(7), "seven", created, updated}

	var got scanArticle
	if err := pg.ScanOne(&fakeRows{cols: cols, data: [][]any{row}}, &got); err != nil {
		t.Fatalf("pg.ScanOne: %v", err)
	}
	// The root scanner is the reference — the same struct reaches it
	// whenever a caller goes through drops.One instead of a pg builder.
	var want scanArticle
	if err := drops.ScanOne(&fakeRows{cols: cols, data: [][]any{row}}, &want); err != nil {
		t.Fatalf("drops.ScanOne: %v", err)
	}
	if got != want {
		t.Errorf("pg and root scanners disagree\n  pg:   %+v\n  root: %+v", got, want)
	}
	if !got.CreatedAt.Equal(created) || !got.UpdatedAt.Equal(updated) {
		t.Errorf("timestamps promoted out of the unexported embedded struct were dropped: got %v/%v, want %v/%v",
			got.CreatedAt, got.UpdatedAt, created, updated)
	}
}

// An embedded time.Time receives a column; it does not lend its fields.
// The root scanner says so, and pg has to say the same.
type scanStamped struct {
	ID int64
	time.Time
}

func TestScanTreatsAnEmbeddedScalarAsAColumn(t *testing.T) {
	at := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	cols := []string{"id", "time"}
	row := []any{int64(3), at}

	var got scanStamped
	if err := pg.ScanOne(&fakeRows{cols: cols, data: [][]any{row}}, &got); err != nil {
		t.Fatalf("pg.ScanOne: %v", err)
	}
	if !got.Time.Equal(at) {
		t.Errorf("embedded time.Time was walked into instead of scanned: got %v, want %v", got.Time, at)
	}
}

// A name reachable at two depths belongs to the shallower field —
// otherwise the walk order decides which field a column lands in, and
// the answer changes with an unrelated edit to the declaration. The
// rule has to hold for an embedded type either way round, because
// whether the embedded type is exported says nothing about which of
// the two fields the caller meant.
type ScanInnerName struct{ Title string }

type scanInnerName struct{ Title string }

type scanOuterExported struct {
	Title string
	ScanInnerName
}

type scanOuterUnexported struct {
	Title string
	scanInnerName
}

func TestScanPrefersTheShallowerFieldOnACollision(t *testing.T) {
	var exported scanOuterExported
	if err := pg.ScanOne(&fakeRows{cols: []string{"title"}, data: [][]any{{"outer"}}}, &exported); err != nil {
		t.Fatalf("pg.ScanOne: %v", err)
	}
	if exported.Title != "outer" || exported.ScanInnerName.Title != "" {
		t.Errorf("exported embed: column landed in the embedded field: outer=%q inner=%q",
			exported.Title, exported.ScanInnerName.Title)
	}

	var unexported scanOuterUnexported
	if err := pg.ScanOne(&fakeRows{cols: []string{"title"}, data: [][]any{{"outer"}}}, &unexported); err != nil {
		t.Fatalf("pg.ScanOne: %v", err)
	}
	if unexported.Title != "outer" || unexported.scanInnerName.Title != "" {
		t.Errorf("unexported embed: column landed in the embedded field: outer=%q inner=%q",
			unexported.Title, unexported.scanInnerName.Title)
	}
}
