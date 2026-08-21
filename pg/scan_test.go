package pg_test

import (
	"strings"
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

// A single-column result into a slice of scalars is the shape SELECT id
// produces, and drops.All[int64] over a pg builder already reads it —
// so pg's own All has to read it too. It used to reject an []int64
// outright, and, worse, take a []time.Time apart field-by-field: a
// time.Time is a struct, so the walk matched no column, and the caller
// got a slice of zero timestamps and a nil error.
func TestScanAllReadsASliceOfScalars(t *testing.T) {
	at := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	stamps := func() *fakeRows {
		return &fakeRows{cols: []string{"t"}, data: [][]any{{at}, {at.Add(time.Hour)}}}
	}

	var got []time.Time
	if err := pg.ScanAll(stamps(), &got); err != nil {
		t.Fatalf("pg.ScanAll into []time.Time: %v", err)
	}
	var want []time.Time
	if err := drops.ScanAll(stamps(), &want); err != nil {
		t.Fatalf("drops.ScanAll into []time.Time: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("pg read %d rows, root read %d", len(got), len(want))
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Errorf("[%d] pg %v, root %v", i, got[i], want[i])
		}
	}
	if got[0].IsZero() {
		t.Error("a []time.Time was taken apart field-by-field instead of scanned")
	}

	var ids []int64
	if err := pg.ScanAll(&fakeRows{cols: []string{"id"}, data: [][]any{{int64(1)}, {int64(2)}}}, &ids); err != nil {
		t.Fatalf("pg.ScanAll into []int64: %v", err)
	}
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Errorf("got %v, want [1 2]", ids)
	}
}

// The width check the root scanner makes has to be made here too, or a
// two-column result silently binds its first column and drops the rest.
func TestScanAllRejectsAWideResultIntoScalars(t *testing.T) {
	var ids []int64
	err := pg.ScanAll(&fakeRows{cols: []string{"id", "name"}, data: [][]any{{int64(1), "a"}}}, &ids)
	if err == nil {
		t.Fatal("a two-column result into []int64 should be an error")
	}
	if !strings.Contains(err.Error(), "single-column") {
		t.Errorf("the error should say what is wrong: %v", err)
	}
}
