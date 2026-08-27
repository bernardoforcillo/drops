package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// mcpSession drives the protocol loop over a script of requests and
// returns the responses, so the wire behaviour can be pinned without
// a database. Tool calls that need one are covered by their own
// packages' tests; what matters here is the framing.
func mcpSession(t *testing.T, lines ...string) []rpcResponse {
	t.Helper()
	in := strings.NewReader(strings.Join(lines, "\n") + "\n")
	var out bytes.Buffer
	if err := serveMCP(context.Background(), nil, in, &out); err != nil {
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
	for _, want := range []string{"schema", "explain", "selectivity", "replication"} {
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
	if err := serveMCP(context.Background(), nil, in, &out); err != nil {
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
