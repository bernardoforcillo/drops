package clickhouse_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/clickhouse"
)

// enginelessTable is a table nobody called Engine on — the whole of
// the mistake.
func enginelessTable() *clickhouse.Table {
	t := clickhouse.NewTable("orphans")
	id := clickhouse.Add(t, clickhouse.UUID("id"))
	return t.OrderBy(id)
}

// The CREATE TABLE for a table with no engine renders its ENGINE as a
// Go comment, which is a syntax error on the wire. Push is the
// tooling ErrEngineRequired was written for, so it has to answer with
// the error rather than send the statement.
func TestPushRefusesATableWithNoEngine(t *testing.T) {
	db, d := pushDB(t)
	_, err := clickhouse.Push(context.Background(), db,
		clickhouse.NewSchema(schemaTable(), enginelessTable()))
	if !errors.Is(err, clickhouse.ErrEngineRequired) {
		t.Fatalf("err = %v, want ErrEngineRequired", err)
	}
	if !strings.Contains(err.Error(), "orphans") {
		t.Errorf("the error has to name the table that is missing one: %v", err)
	}
	// Before it opens a connection: a push that cannot succeed should
	// not have read the server to find that out.
	if len(d.queries) != 0 || len(d.execs) != 0 {
		t.Errorf("Push talked to the server first: queries = %q, execs = %q", d.queries, d.execs)
	}
}

// The refusal is about the declaration, not about what the server has,
// so a dry run is refused on the same terms — otherwise DryRun would
// report a plan whose CREATE TABLE cannot run.
func TestPushDryRunRefusesATableWithNoEngine(t *testing.T) {
	db, _ := pushDB(t)
	_, err := clickhouse.Push(context.Background(), db,
		clickhouse.NewSchema(enginelessTable()), clickhouse.PushOptions{DryRun: true})
	if !errors.Is(err, clickhouse.ErrEngineRequired) {
		t.Fatalf("err = %v, want ErrEngineRequired", err)
	}
}

// AllowRefused is about the changes ClickHouse will not make, not
// about statements it cannot parse.
func TestPushEngineCheckIsNotWaivedByAllowRefused(t *testing.T) {
	db, d := pushDB(t)
	_, err := clickhouse.Push(context.Background(), db,
		clickhouse.NewSchema(enginelessTable()), clickhouse.PushOptions{AllowRefused: true})
	if !errors.Is(err, clickhouse.ErrEngineRequired) {
		t.Fatalf("err = %v, want ErrEngineRequired", err)
	}
	if len(d.execs) != 0 {
		t.Errorf("Push ran %q", d.execs)
	}
}

// No sentinel string reaches the wire in the case that used to send
// one: the marker is a Go comment, and a comment is not DDL.
func TestNoStatementCarriesTheEngineMarker(t *testing.T) {
	db, d := pushDB(t)
	_, _ = clickhouse.Push(context.Background(), db,
		clickhouse.NewSchema(schemaTable(), enginelessTable()))
	for _, s := range append(append([]string{}, d.queries...), d.execs...) {
		if strings.Contains(s, "engine missing") {
			t.Errorf("a sentinel string went to the server: %s", s)
		}
	}
}
