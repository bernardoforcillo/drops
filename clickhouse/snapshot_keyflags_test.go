package clickhouse_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/clickhouse"
)

// keyFlagTable has a column whose name is a substring of another
// column's — "s" inside "ts" — which is the whole of the trap.
func keyFlagTable() *clickhouse.Table {
	t := clickhouse.NewTable("hits")
	id := clickhouse.Add(t, clickhouse.UInt64("id"))
	ts := clickhouse.Add(t, clickhouse.DateTime("ts", "UTC"))
	clickhouse.Add(t, clickhouse.String("s"))
	clickhouse.Add(t, clickhouse.String("user_ts_label"))
	return t.Engine(clickhouse.MergeTree()).
		OrderBy(ts, id).
		PartitionBy(clickhouse.ToYYYYMM(ts)).
		SampleBy(id)
}

// The declared side's key flags are what a snapshot carries to the
// next reader, so a column the partition expression merely contains
// the name of must not be flagged as part of it.
func TestDeclaredKeyFlagsMatchWholeColumnNames(t *testing.T) {
	snap := clickhouse.BuildSnapshot(clickhouse.NewSchema(keyFlagTable()))
	ts := snap.Tables["hits"]
	if ts == nil {
		t.Fatal("no snapshot for hits")
	}
	for _, c := range []struct {
		name      string
		partition bool
		sampling  bool
	}{
		{"ts", true, false},
		{"id", false, true},
		{"s", false, false},
		{"user_ts_label", false, false},
	} {
		got := ts.Columns[c.name]
		if got == nil {
			t.Fatalf("no snapshot for column %q", c.name)
		}
		if got.InPartitionKey != c.partition {
			t.Errorf("%q: InPartitionKey = %v, want %v", c.name, got.InPartitionKey, c.partition)
		}
		if got.InSamplingKey != c.sampling {
			t.Errorf("%q: InSamplingKey = %v, want %v", c.name, got.InSamplingKey, c.sampling)
		}
	}
}

// The flags are serialised, so a wrong one is not a transient
// mis-read: it is written to the file the next reader believes.
func TestWrongKeyFlagsAreNotWrittenToTheSnapshotFile(t *testing.T) {
	snap := clickhouse.BuildSnapshot(clickhouse.NewSchema(keyFlagTable()))
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back clickhouse.Snapshot
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c := back.Tables["hits"].Columns["s"]; c.InPartitionKey {
		t.Errorf("the snapshot file says the partition key names \"s\":\n%s", trim(string(raw)))
	}
}

func trim(s string) string {
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return strings.TrimSpace(s)
}
