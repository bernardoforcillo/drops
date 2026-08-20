package pg_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/pg"
)

// pg used to resolve columns to fields with its own reflection walk,
// and every case below is one the copy answered differently from the
// root scanner every other dialect uses. They are here rather than only
// in the root package's scan_test.go because the thing under test is
// that pg does not answer them on its own: delete the delegation in
// pg/scan.go and these fail again, one per drift.

// scanAudit is the shape pg.Audit encourages — bookkeeping columns
// factored into an embedded struct that the row type does not export.
type scanAudit struct {
	CreatedAt time.Time
	Note      string
}

type scanArticle struct {
	ID int64
	scanAudit
}

// ScanKey is an exported embedded type on purpose: an unexported one
// exposes the drift in the walk, an exported one the drift in the
// tie-break, and each needs its own pin.
type ScanKey struct{ ID int64 }

// scanShadowFirst declares its own ID above the embedded one, which is
// the order a last-write-wins walk got wrong: the outer field is the
// one ordinary Go field access reaches, so it is the one the column
// belongs to.
type scanShadowFirst struct {
	ID string
	ScanKey
}

type scanShadowLast struct {
	ScanKey
	ID string
}

type scanTagged struct {
	Label string `drop:"b"`
	B     string
}

// scanColliding has two fields whose camelCase forms are both "sku".
type scanColliding struct {
	SKU int64
	Sku int64
}

// scanReading embeds a time.Time, which is a destination for a column
// rather than a struct to look inside.
type scanReading struct {
	ID int64
	time.Time
}

type scanOptions struct {
	Name string `drop:"n,notnull"`
}

type scanSkipped struct {
	ID   int64
	Skip string `drop:"-"`
}

type scanUnexported struct {
	ID     int64
	secret string
}

// scanRows hands back a cursor over canned data. dropstest converts the
// way a driver does — a nil into a *string, an int64 into an int — so a
// mis-bound column surfaces as a scan error instead of a panic.
func scanRows(t *testing.T, cols []string, data ...[]any) drops.Rows {
	t.Helper()
	rows, err := dropstest.New().Rows(cols, data...).Query(context.Background(), "q")
	if err != nil {
		t.Fatalf("canned query: %v", err)
	}
	return rows
}

func TestScanOneResolvesColumnsLikeTheRootScanner(t *testing.T) {
	when := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name string
		cols []string
		row  []any
		dest any
		want any
	}{
		{
			name: "unexported embedded struct keeps its promoted fields",
			cols: []string{"id", "createdAt", "note"},
			row:  []any{int64(1), when, "draft"},
			dest: &scanArticle{},
			want: &scanArticle{ID: 1, scanAudit: scanAudit{CreatedAt: when, Note: "draft"}},
		},
		{
			name: "outer field declared first shadows the embedded one",
			cols: []string{"id"},
			row:  []any{"a1"},
			dest: &scanShadowFirst{},
			want: &scanShadowFirst{ID: "a1"},
		},
		{
			name: "outer field declared last shadows the embedded one",
			cols: []string{"id"},
			row:  []any{"a1"},
			dest: &scanShadowLast{},
			want: &scanShadowLast{ID: "a1"},
		},
		{
			name: "tag beats another field's camelCase form",
			cols: []string{"b"},
			row:  []any{"tagged"},
			dest: &scanTagged{},
			want: &scanTagged{Label: "tagged"},
		},
		{
			name: "camelCase collision goes to the first-declared field",
			cols: []string{"sku"},
			row:  []any{int64(9)},
			dest: &scanColliding{},
			want: &scanColliding{SKU: 9},
		},
		{
			name: "embedded time.Time receives a column instead of lending fields",
			cols: []string{"id", "time"},
			row:  []any{int64(1), when},
			dest: &scanReading{},
			want: &scanReading{ID: 1, Time: when},
		},
		{
			name: "tag options after the comma are not part of the column name",
			cols: []string{"n"},
			row:  []any{"v"},
			dest: &scanOptions{},
			want: &scanOptions{Name: "v"},
		},
		{
			name: "drop:- leaves the field alone",
			cols: []string{"id", "Skip"},
			row:  []any{int64(1), "written"},
			dest: &scanSkipped{},
			want: &scanSkipped{ID: 1},
		},
		{
			name: "unexported field is not a destination",
			cols: []string{"id", "secret"},
			row:  []any{int64(1), "shh"},
			dest: &scanUnexported{},
			want: &scanUnexported{ID: 1},
		},
		{
			name: "a column with no field is discarded",
			cols: []string{"id", "unrelated"},
			row:  []any{int64(4), "ignored"},
			dest: &scanArticle{},
			want: &scanArticle{ID: 4},
		},
		{
			name: "a field with no column keeps its zero value",
			cols: []string{"id"},
			row:  []any{int64(4)},
			dest: &scanArticle{},
			want: &scanArticle{ID: 4},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := pg.ScanOne(scanRows(t, tc.cols, tc.row), tc.dest); err != nil {
				t.Fatalf("ScanOne: %v", err)
			}
			if !reflect.DeepEqual(tc.dest, tc.want) {
				t.Errorf("got = %+v, want %+v", tc.dest, tc.want)
			}
		})
	}
}

func TestScanAllWalksUnexportedEmbeddedStruct(t *testing.T) {
	when := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	var got []scanArticle
	rows := scanRows(t, []string{"id", "createdAt", "note"},
		[]any{int64(1), when, "first"},
		[]any{int64(2), when, "second"},
	)
	if err := pg.ScanAll(rows, &got); err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	want := []scanArticle{
		{ID: 1, scanAudit: scanAudit{CreatedAt: when, Note: "first"}},
		{ID: 2, scanAudit: scanAudit{CreatedAt: when, Note: "second"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got = %+v, want %+v", got, want)
	}
}

// A single-column result into a slice of scalars is the most ordinary
// query there is, and the pg scanner used to refuse it as "slice
// element must be struct or *struct".
func TestScanAllAcceptsASliceOfScalars(t *testing.T) {
	var ids []int64
	if err := pg.ScanAll(scanRows(t, []string{"id"}, []any{int64(1)}, []any{int64(2)}), &ids); err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	if want := []int64{1, 2}; !reflect.DeepEqual(ids, want) {
		t.Errorf("got = %v, want %v", ids, want)
	}
}

// The scanner moved but the sentinel must not: pg callers write
// errors.Is(err, pg.ErrNoRows), and drops.ErrNoRows arriving instead
// would turn every one of those checks into a silently missed empty
// result.
func TestEmptyCursorStillReportsThePgSentinel(t *testing.T) {
	t.Run("ScanOne", func(t *testing.T) {
		var got scanArticle
		err := pg.ScanOne(scanRows(t, []string{"id"}), &got)
		if !errors.Is(err, pg.ErrNoRows) {
			t.Errorf("got = %v, want %v", err, pg.ErrNoRows)
		}
	})
	t.Run("SelectBuilder.One", func(t *testing.T) {
		tbl := pg.NewTable("scan_empty")
		pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
		db := pg.New(dropstest.New().Rows([]string{"id"}))
		var got scanArticle
		err := db.Select().From(tbl).One(context.Background(), &got)
		if !errors.Is(err, pg.ErrNoRows) {
			t.Errorf("got = %v, want %v", err, pg.ErrNoRows)
		}
	})
}

// The builder path is the one users travel, and the embedded-audit row
// type is what it used to lose on the way.
func TestSelectOneScansPromotedFields(t *testing.T) {
	when := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	tbl := pg.NewTable("scan_articles")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	pg.Add(tbl, pg.Timestamp("createdAt", true).NotNull())
	pg.Add(tbl, pg.Text("note").NotNull())

	db := pg.New(dropstest.New().Rows(
		[]string{"id", "createdAt", "note"},
		[]any{int64(7), when, "draft"},
	))
	var got scanArticle
	if err := db.Select().From(tbl).One(context.Background(), &got); err != nil {
		t.Fatalf("One: %v", err)
	}
	want := scanArticle{ID: 7, scanAudit: scanAudit{CreatedAt: when, Note: "draft"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got = %+v, want %+v", got, want)
	}
}
