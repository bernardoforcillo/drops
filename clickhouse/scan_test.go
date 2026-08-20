package clickhouse_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/clickhouse"
	"github.com/bernardoforcillo/drops/dropstest"
)

// clickhouse carried a copy of the pg reflection walk, drift included,
// so it lost the same rows for the same reasons. These pin the shapes
// that copy got wrong through the builder a caller actually uses.

// chAudit is the bookkeeping struct a row type embeds without
// exporting — the case the old walk dropped on the floor.
type chAudit struct {
	Ts   time.Time
	Kind string
}

type chEvent struct {
	ID uint64
	chAudit
}

// ChKey is exported, so it exercises the other half: two fields
// claiming "id", where the outer one must win however the two are
// ordered.
type ChKey struct{ ID uint64 }

type chShadowFirst struct {
	ID string
	ChKey
}

type chTagged struct {
	Label string `drop:"b"`
	B     string
}

func chTable(t *testing.T, name string) *clickhouse.Table {
	t.Helper()
	tbl := clickhouse.NewTable(name)
	clickhouse.Add(tbl, clickhouse.UInt64("id"))
	clickhouse.Add(tbl, clickhouse.DateTime("ts", "UTC"))
	clickhouse.Add(tbl, clickhouse.String("kind"))
	return tbl
}

func TestSelectOneResolvesColumnsLikeTheRootScanner(t *testing.T) {
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
			cols: []string{"id", "ts", "kind"},
			row:  []any{uint64(1), when, "click"},
			dest: &chEvent{},
			want: &chEvent{ID: 1, chAudit: chAudit{Ts: when, Kind: "click"}},
		},
		{
			name: "outer field declared first shadows the embedded one",
			cols: []string{"id"},
			row:  []any{"a1"},
			dest: &chShadowFirst{},
			want: &chShadowFirst{ID: "a1"},
		},
		{
			name: "tag beats another field's camelCase form",
			cols: []string{"b"},
			row:  []any{"tagged"},
			dest: &chTagged{},
			want: &chTagged{Label: "tagged"},
		},
		{
			name: "a column with no field is discarded",
			cols: []string{"id", "unrelated"},
			row:  []any{uint64(4), "ignored"},
			dest: &chEvent{},
			want: &chEvent{ID: 4},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := clickhouse.New(dropstest.New().Rows(tc.cols, tc.row))
			if err := db.Select().From(chTable(t, "scan_one")).One(context.Background(), tc.dest); err != nil {
				t.Fatalf("One: %v", err)
			}
			if !reflect.DeepEqual(tc.dest, tc.want) {
				t.Errorf("got = %+v, want %+v", tc.dest, tc.want)
			}
		})
	}
}

func TestSelectAllWalksUnexportedEmbeddedStruct(t *testing.T) {
	when := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	db := clickhouse.New(dropstest.New().Rows(
		[]string{"id", "ts", "kind"},
		[]any{uint64(1), when, "click"},
		[]any{uint64(2), when, "view"},
	))
	var got []chEvent
	if err := db.Select().From(chTable(t, "scan_all")).All(context.Background(), &got); err != nil {
		t.Fatalf("All: %v", err)
	}
	want := []chEvent{
		{ID: 1, chAudit: chAudit{Ts: when, Kind: "click"}},
		{ID: 2, chAudit: chAudit{Ts: when, Kind: "view"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got = %+v, want %+v", got, want)
	}
}

// The root scanner raises its own sentinel; the package one is what
// clickhouse callers compare against, and moving the scanner must not
// move that.
func TestEmptyCursorStillReportsTheClickhouseSentinel(t *testing.T) {
	db := clickhouse.New(dropstest.New().Rows([]string{"id"}))
	var got chEvent
	err := db.Select().From(chTable(t, "scan_empty")).One(context.Background(), &got)
	if !errors.Is(err, clickhouse.ErrNoRows) {
		t.Errorf("got = %v, want %v", err, clickhouse.ErrNoRows)
	}
}
