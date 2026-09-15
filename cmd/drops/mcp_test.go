package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// mcpSession drives the protocol loop over a script of requests and
// returns the responses, so the wire behaviour can be pinned without
// a database. The server it drives is the one a person gets from a
// configuration carrying nothing but a DSN. Tool calls that need a
// connection are covered by their own packages' tests; what matters
// here is the framing, and what the tools that need no connection
// answer.
func mcpSession(t *testing.T, lines ...string) []rpcResponse {
	t.Helper()
	return mcpSessionOn(t, &mcpServer{}, lines...)
}

// mcpSessionOn is mcpSession against a server configured the way the
// operator's flags would have configured it.
func mcpSessionOn(t *testing.T, srv *mcpServer, lines ...string) []rpcResponse {
	t.Helper()
	in := strings.NewReader(strings.Join(lines, "\n") + "\n")
	var out bytes.Buffer
	if err := serveMCP(context.Background(), srv, in, &out); err != nil {
		t.Fatalf("serveMCP: %v", err)
	}
	var got []rpcResponse
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var r rpcResponse
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("response is not JSON: %q", line)
		}
		got = append(got, r)
	}
	return got
}

func TestMCPInitialize(t *testing.T) {
	got := mcpSession(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if len(got) != 1 {
		t.Fatalf("got %d responses, want 1", len(got))
	}
	res, _ := got[0].Result.(map[string]any)
	if res["protocolVersion"] != mcpProtocolVersion {
		t.Errorf("protocolVersion = %v", res["protocolVersion"])
	}
	caps, _ := res["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok {
		t.Errorf("no tools capability advertised: %v", caps)
	}
	info, _ := res["serverInfo"].(map[string]any)
	if info["name"] != "drops" || info["version"] != version {
		t.Errorf("serverInfo = %v", info)
	}
}

// A notification carries no id and must not be answered — replying to
// one is a protocol error, not a courtesy.
func TestMCPNotificationsGetNoReply(t *testing.T) {
	got := mcpSession(t,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":null,"method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":7,"method":"ping"}`,
	)
	if len(got) != 1 {
		t.Fatalf("got %d responses, want 1 (only the ping)", len(got))
	}
	if string(got[0].ID) != "7" {
		t.Errorf("answered id %s, want 7", got[0].ID)
	}
}

// A stray byte on the wire must not end the session.
func TestMCPSurvivesMalformedLines(t *testing.T) {
	got := mcpSession(t,
		`not json at all`,
		``,
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
	)
	if len(got) != 1 || got[0].Error != nil {
		t.Fatalf("session did not recover: %+v", got)
	}
}

func TestMCPToolsListIsReadOnly(t *testing.T) {
	got := mcpSession(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	res, _ := got[0].Result.(map[string]any)
	tools, _ := res["tools"].([]any)
	if len(tools) == 0 {
		t.Fatal("no tools advertised")
	}
	names := map[string]bool{}
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		name, _ := tool["name"].(string)
		names[name] = true
		if tool["description"] == nil {
			t.Errorf("tool %q has no description", name)
		}
		if tool["inputSchema"] == nil {
			t.Errorf("tool %q has no inputSchema", name)
		}
	}
	for _, want := range []string{"schema", "explain", "selectivity", "replication", "drift", "safety", "migrations"} {
		if !names[want] {
			t.Errorf("tool %q is not advertised", want)
		}
	}
	// An assistant with a production DSN must not be able to change
	// the database. "Should not" has to mean "cannot": a tool
	// description is not an access control, so the mutating verbs
	// are simply absent.
	for _, forbidden := range []string{"push", "migrate", "exec", "query", "drop", "pull", "baseline"} {
		if names[forbidden] {
			t.Errorf("%q is exposed over MCP; every tool here must be read-only", forbidden)
		}
	}
}

func TestMCPUnknownMethodAndTool(t *testing.T) {
	got := mcpSession(t,
		`{"jsonrpc":"2.0","id":1,"method":"nope"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"nope","arguments":{}}}`,
	)
	if len(got) != 2 {
		t.Fatalf("got %d responses", len(got))
	}
	// An unknown method is a protocol error.
	if got[0].Error == nil {
		t.Error("unknown method did not produce an error")
	}
	// An unknown tool is a tool result the assistant can read and
	// recover from, not a transport failure that ends the turn.
	if got[1].Error != nil {
		t.Errorf("unknown tool surfaced as a protocol error: %v", got[1].Error)
	}
	res, _ := got[1].Result.(map[string]any)
	if res["isError"] != true {
		t.Errorf("unknown tool did not report isError: %v", res)
	}
}

// A tool called with arguments it cannot use reports why, in the
// result, rather than reaching the database.
func TestMCPToolArgumentValidation(t *testing.T) {
	got := mcpSession(t,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"explain","arguments":{"sql":"  "}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"selectivity","arguments":{"table":"t"}}}`,
	)
	for i, r := range got {
		res, _ := r.Result.(map[string]any)
		if res["isError"] != true {
			t.Errorf("response %d did not report a bad argument: %v", i, res)
		}
	}
}

// Responses have to be one JSON object per line, or the transport
// desynchronises.
func TestMCPFramingIsLineDelimited(t *testing.T) {
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n")
	var out bytes.Buffer
	if err := serveMCP(context.Background(), &mcpServer{}, in, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines for 2 requests:\n%s", len(lines), out.String())
	}
	for _, l := range lines {
		if strings.Contains(l, "\n") {
			t.Error("a response spans more than one line")
		}
	}
}

// toolCall renders a tools/call request with its arguments marshalled,
// so a test can hold SQL in a Go string rather than in JSON escapes.
func toolCall(t *testing.T, id int, name string, args map[string]any) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"name": name, "arguments": args})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":%s}`, id, body)
}

// mcpResult unwraps a tool result into its text and whether the tool
// reported a failure.
func mcpResult(t *testing.T, r rpcResponse) (string, bool) {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("protocol error: %v", r.Error)
	}
	res, ok := r.Result.(map[string]any)
	if !ok {
		t.Fatalf("result is not an object: %v", r.Result)
	}
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("result carries no content: %v", res)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return text, res["isError"] == true
}

func mcpToolText(t *testing.T, r rpcResponse) string {
	t.Helper()
	text, isErr := mcpResult(t, r)
	if isErr {
		t.Fatalf("tool reported an error: %s", text)
	}
	return text
}

func mcpToolError(t *testing.T, r rpcResponse) string {
	t.Helper()
	text, isErr := mcpResult(t, r)
	if !isErr {
		t.Fatalf("tool succeeded where it should have refused:\n%s", text)
	}
	return text
}

// The whole point of the safety tool is the sentence "this one loses
// data". It has to name the statement, the rule and what goes — and it
// has to do it without a connection, which is what the nil database
// behind this server proves.
func TestMCPSafetyNamesWhatWouldBeLost(t *testing.T) {
	got := mcpSession(t, toolCall(t, 1, "safety", map[string]any{
		"sql": `ALTER TABLE "users" DROP COLUMN "email";
			CREATE INDEX "users_name_idx" ON "users" ("name");`,
	}))
	text := mcpToolText(t, got[0])
	for _, want := range []string{
		"2 statement(s) read",
		"drop-column",
		"the column's data is gone",
		"create-index-not-concurrent",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("safety did not report %q:\n%s", want, text)
		}
	}
}

func TestMCPSafetyOnAMigrationThatCostsNothing(t *testing.T) {
	got := mcpSession(t, toolCall(t, 1, "safety", map[string]any{
		"sql": `ALTER TABLE "users" ADD COLUMN "nickname" text;`,
	}))
	text := mcpToolText(t, got[0])
	if !strings.Contains(text, "Nothing here destroys data") {
		t.Errorf("a harmless migration was not reported as harmless:\n%s", text)
	}
}

// A DROP somebody commented out is not a DROP. Reporting one is the
// false positive that teaches a reader to stop believing the tool.
func TestMCPSafetyReadsPastComments(t *testing.T) {
	got := mcpSession(t, toolCall(t, 1, "safety", map[string]any{
		"sql": "-- DROP TABLE \"users\";\nALTER TABLE \"users\" ADD COLUMN \"nickname\" text;",
	}))
	text := mcpToolText(t, got[0])
	if strings.Contains(text, "drop-table") {
		t.Errorf("a commented-out DROP was reported as data loss:\n%s", text)
	}
}

func TestMCPSafetyNeedsStatements(t *testing.T) {
	got := mcpSession(t, toolCall(t, 1, "safety", map[string]any{"sql": "  -- nothing here\n"}))
	if msg := mcpToolError(t, got[0]); !strings.Contains(msg, "sql") {
		t.Errorf("safety did not say what it was missing: %s", msg)
	}
}

// The Go package is a flag, so a server started without one cannot be
// talked into drift by a tool argument. What it can do is say what to
// add to the configuration.
func TestMCPDriftWithoutASchemaPackage(t *testing.T) {
	got := mcpSession(t, toolCall(t, 1, "drift", map[string]any{}))
	msg := mcpToolError(t, got[0])
	if !strings.Contains(msg, "--schema") {
		t.Errorf("drift did not name the flag it needs: %s", msg)
	}
}

func TestMCPMigrationsWithoutAJournal(t *testing.T) {
	got := mcpSessionOn(t, &mcpServer{dir: t.TempDir()},
		toolCall(t, 1, "migrations", map[string]any{}))
	msg := mcpToolError(t, got[0])
	if !strings.Contains(msg, "meta/_journal.json") {
		t.Errorf("migrations did not name the journal it could not find: %s", msg)
	}
}

// The rules read one statement at a time, so the split decides what
// the verdict is about.
func TestSplitSQL(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
		want []string
	}{
		{
			name: "semicolons",
			sql:  `DROP TABLE a; DROP TABLE b;`,
			want: []string{"DROP TABLE a", "DROP TABLE b"},
		},
		{
			name: "drizzle-kit breakpoints",
			sql:  "DROP TABLE a\n--> statement-breakpoint\nDROP TABLE b",
			want: []string{"DROP TABLE a", "DROP TABLE b"},
		},
		{
			name: "a semicolon inside a literal is not a boundary",
			sql:  `INSERT INTO t VALUES ('a;b'); DROP TABLE a`,
			want: []string{"INSERT INTO t VALUES ('a;b')", "DROP TABLE a"},
		},
		{
			name: "a doubled quote does not end the literal",
			sql:  `INSERT INTO t VALUES ('it''s; fine'); DROP TABLE a`,
			want: []string{"INSERT INTO t VALUES ('it''s; fine')", "DROP TABLE a"},
		},
		{
			name: "a function body keeps its semicolons",
			sql:  "CREATE FUNCTION f() RETURNS trigger AS $$ BEGIN RETURN NEW; END; $$ LANGUAGE plpgsql; DROP TABLE a",
			want: []string{
				"CREATE FUNCTION f() RETURNS trigger AS $$ BEGIN RETURN NEW; END; $$ LANGUAGE plpgsql",
				"DROP TABLE a",
			},
		},
		{
			name: "a placeholder is not a dollar quote",
			sql:  `DELETE FROM t WHERE id = $1; DROP TABLE a`,
			want: []string{"DELETE FROM t WHERE id = $1", "DROP TABLE a"},
		},
		{
			name: "comments go, and what they hide goes with them",
			sql:  "-- DROP TABLE a;\nDROP TABLE b; /* DROP TABLE c; */",
			want: []string{"DROP TABLE b"},
		},
		{
			name: "nothing to analyse",
			sql:  "  \n\t",
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := splitSQL(tc.sql)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d statement(s), want %d:\n%q", len(got), len(tc.want), got)
			}
			for i := range got {
				if flat := strings.Join(strings.Fields(got[i]), " "); flat != tc.want[i] {
					t.Errorf("statement %d = %q, want %q", i, flat, tc.want[i])
				}
			}
		})
	}
}
