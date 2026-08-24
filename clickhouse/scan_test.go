package clickhouse_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/clickhouse"
)

// scanRows is a canned cursor carrying real values, which the package's
// fakeRows cannot: it is empty by construction.
type scanRows struct {
	cols []string
	data [][]any
	pos  int
}

func (r *scanRows) Next() bool {
	if r.pos >= len(r.data) {
		return false
	}
	r.pos++
	return true
}

func (r *scanRows) Scan(dest ...any) error {
	row := r.data[r.pos-1]
	for i, d := range dest {
		switch p := d.(type) {
		case *any:
			*p = row[i]
		case *int64:
			*p = row[i].(int64)
		case *string:
			*p = row[i].(string)
		case *time.Time:
			*p = row[i].(time.Time)
		default:
			return errors.New("scanRows: unexpected destination")
		}
	}
	return nil
}

func (r *scanRows) Columns() ([]string, error) { return r.cols, nil }
func (r *scanRows) Close() error               { return nil }
func (r *scanRows) Err() error                 { return nil }

// scanDriver replies to every query with the same canned cursor.
type scanDriver struct{ rows func() drops.Rows }

func (scanDriver) Exec(context.Context, string, ...any) (drops.Result, error) {
	return nil, errors.New("unexpected Exec")
}
func (d scanDriver) Query(context.Context, string, ...any) (drops.Rows, error) {
	return d.rows(), nil
}
func (scanDriver) Begin(context.Context) (drops.Tx, error) {
	return nil, errors.New("unexpected Begin")
}

// chScanAudit is the shape the scanner has to reach through: a struct
// that factors the timestamps every row carries into one embedded type,
// and keeps that type unexported because it is an implementation detail
// of the package that declares it.
type chScanAudit struct {
	CreatedAt time.Time
}

type chScanEvent struct {
	ID   int64
	Name string
	chScanAudit
}

// This package kept its own copy of the reflection walk, which skipped
// unexported fields before it looked for embedded ones — so the
// timestamps went to the discard sink and the row came back with a zero
// CreatedAt and nothing anywhere saying why. Every other dialect binds
// the same struct through the root scanner; this one has to agree.
func TestScanReachesThroughAnUnexportedEmbeddedStruct(t *testing.T) {
	created := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	cols := []string{"id", "name", "createdAt"}
	row := []any{int64(7), "seven", created}

	db := clickhouse.New(scanDriver{rows: func() drops.Rows {
		return &scanRows{cols: cols, data: [][]any{row}}
	}})

	var got []chScanEvent
	if err := db.Select().From(clickhouse.NewTable("events")).All(context.Background(), &got); err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if !got[0].CreatedAt.Equal(created) {
		t.Errorf("timestamp promoted out of the unexported embedded struct was dropped: got %v, want %v",
			got[0].CreatedAt, created)
	}

	// The root scanner is the reference — the same struct reaches it
	// whenever a caller goes through drops.One instead of a builder.
	var want chScanEvent
	if err := drops.ScanOne(&scanRows{cols: cols, data: [][]any{row}}, &want); err != nil {
		t.Fatalf("drops.ScanOne: %v", err)
	}
	if got[0] != want {
		t.Errorf("clickhouse and root scanners disagree\n  clickhouse: %+v\n  root:       %+v", got[0], want)
	}
}

// An embedded time.Time receives a column; it does not lend its fields.
// The fork walked into it, found nothing exported, and left the column
// unbound.
type chScanStamped struct {
	ID int64
	time.Time
}

func TestScanTreatsAnEmbeddedScalarAsAColumn(t *testing.T) {
	at := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	db := clickhouse.New(scanDriver{rows: func() drops.Rows {
		return &scanRows{cols: []string{"id", "time"}, data: [][]any{{int64(3), at}}}
	}})

	var got []chScanStamped
	if err := db.Select().From(clickhouse.NewTable("events")).All(context.Background(), &got); err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 || !got[0].Time.Equal(at) {
		t.Errorf("embedded time.Time was walked into instead of scanned: %+v", got)
	}
}

// A name reachable at two depths belongs to the shallower field —
// otherwise the walk order decides which field a column lands in, and
// the answer changes with an unrelated edit to the declaration.
type ChScanInner struct{ Name string }

type chScanOuter struct {
	Name string
	ChScanInner
}

func TestScanPrefersTheShallowerFieldOnACollision(t *testing.T) {
	db := clickhouse.New(scanDriver{rows: func() drops.Rows {
		return &scanRows{cols: []string{"name"}, data: [][]any{{"outer"}}}
	}})

	var got []chScanOuter
	if err := db.Select().From(clickhouse.NewTable("events")).All(context.Background(), &got); err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 || got[0].Name != "outer" || got[0].ChScanInner.Name != "" {
		t.Errorf("column landed in the embedded field: %+v", got)
	}
}

// A single-column result into a slice of scalars — SELECT count(),
// SELECT id — is a shape the root scanner reads and this one did not.
// []int64 was rejected outright, and a []time.Time was taken apart
// field-by-field: a time.Time is a struct, so the walk matched no
// column and the caller got zero timestamps with a nil error.
func TestScanAllReadsASliceOfScalars(t *testing.T) {
	at := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	db := clickhouse.New(scanDriver{rows: func() drops.Rows {
		return &scanRows{cols: []string{"t"}, data: [][]any{{at}, {at.Add(time.Hour)}}}
	}})

	var stamps []time.Time
	if err := db.Select().From(clickhouse.NewTable("events")).All(context.Background(), &stamps); err != nil {
		t.Fatalf("All into []time.Time: %v", err)
	}
	if len(stamps) != 2 || !stamps[0].Equal(at) {
		t.Errorf("a []time.Time was taken apart field-by-field instead of scanned: %v", stamps)
	}

	dbi := clickhouse.New(scanDriver{rows: func() drops.Rows {
		return &scanRows{cols: []string{"id"}, data: [][]any{{int64(1)}, {int64(2)}}}
	}})
	var ids []int64
	if err := dbi.Select().From(clickhouse.NewTable("events")).All(context.Background(), &ids); err != nil {
		t.Fatalf("All into []int64: %v", err)
	}
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Errorf("got %v, want [1 2]", ids)
	}
}
