package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/bernardoforcillo/drops/pg"
)

// "drops mcp" — the schema, the plans and the replication state, as
// tools an assistant can call.
//
// The rest of this binary is for a person at a terminal. This
// subcommand speaks the Model Context Protocol over stdin and stdout
// instead, so an assistant working in the repository can ask the
// database directly rather than being told about it second hand:
// which columns a table actually has, what plan a query gets, whether
// the predicate it is about to index is selective, whether a
// replication slot is falling behind.
//
//	{ "mcpServers": { "drops": {
//	    "command": "drops",
//	    "args": ["mcp", "--dsn", "postgres://..."]
//	} } }
//
// # Every tool is read-only, and that is a design decision
//
// Nothing here migrates, pushes, writes or drops. An assistant with a
// production DSN should be able to answer questions about the
// database and should not be able to change it — and "should not" has
// to mean "cannot", because a tool description is not an access
// control. The commands that change things stay where a person runs
// them, with the confirmation prompts and the exit codes they already
// have.
//
// EXPLAIN is the one that needs saying twice: it is run without
// ANALYZE, so the statement is planned and never executed. An
// assistant can ask for the plan of a DELETE without deleting
// anything.

const mcpProtocolVersion = "2024-11-05"

func runMCP(ctx context.Context, args []string) error {
	fs := newFlagSet("mcp", "Serve the schema, plans and replication state over the Model Context Protocol")
	dsn := fs.String("dsn", "", "PostgreSQL connection string (else $DROPS_PG_DSN, $DATABASE_URL)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	db, closeDB, err := connect(ctx, *dsn)
	if err != nil {
		return err
	}
	defer closeDB()

	return serveMCP(ctx, db, os.Stdin, os.Stdout)
}

// --- JSON-RPC ---------------------------------------------------------

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// serveMCP runs the protocol loop. Messages are newline-delimited
// JSON objects, one per line, which is what the stdio transport
// specifies.
func serveMCP(ctx context.Context, db *pg.DB, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	// A schema dump is large and a plan can be too; the default 64KB
	// line limit is not enough for what comes back, and a request
	// carrying a long statement can exceed it too.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	enc := json.NewEncoder(out)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			// A malformed line has no id to answer against, so
			// there is nothing to reply to. Dropping it keeps the
			// session alive, which is better than exiting on a
			// stray byte.
			continue
		}
		// A notification carries no id and takes no reply — that is
		// how "notifications/initialized" arrives, and answering it
		// is a protocol error rather than a courtesy.
		notification := len(req.ID) == 0 || string(req.ID) == "null"

		result, err := dispatchMCP(ctx, db, req)
		if notification {
			continue
		}
		resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
		if err != nil {
			resp.Error = &rpcError{Code: -32603, Message: err.Error()}
		} else {
			resp.Result = result
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func dispatchMCP(ctx context.Context, db *pg.DB, req rpcRequest) (any, error) {
	switch req.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "drops", "version": version},
		}, nil

	case "ping":
		return map[string]any{}, nil

	case "tools/list":
		return map[string]any{"tools": mcpTools()}, nil

	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("bad tools/call params: %w", err)
		}
		text, err := callMCPTool(ctx, db, p.Name, p.Arguments)
		if err != nil {
			// A tool that fails reports through the result rather
			// than as a protocol error: the assistant is meant to
			// read the message and try something else, and a
			// transport-level error would just end the turn.
			return map[string]any{
				"isError": true,
				"content": []any{map[string]any{"type": "text", "text": err.Error()}},
			}, nil
		}
		return map[string]any{
			"content": []any{map[string]any{"type": "text", "text": text}},
		}, nil

	default:
		return nil, fmt.Errorf("unknown method %q", req.Method)
	}
}

// --- Tools ------------------------------------------------------------

func mcpTools() []any {
	str := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}
	return []any{
		map[string]any{
			"name": "schema",
			"description": "Introspect the live database and return its tables, columns, " +
				"indexes and constraints as JSON. Read-only.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"schema": str(`PostgreSQL schema to introspect (default "public")`),
					"table":  str("restrict the output to one table"),
				},
			},
		},
		map[string]any{
			"name": "explain",
			"description": "Return the query plan for a statement, with its structural " +
				"fingerprint and the indexes it uses. Planning only — the statement is " +
				"never executed, so it is safe to explain a DELETE.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"sql"},
				"properties": map[string]any{
					"sql": str("the statement to plan; use $1, $2 for parameters"),
				},
			},
		},
		map[string]any{
			"name": "selectivity",
			"description": "Estimate what fraction of a table's rows carry each value of a " +
				"column, to decide whether a predicate is worth an index. Reads the column " +
				"(sample it on a large table).",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"table", "column"},
				"properties": map[string]any{
					"table":  str("table name"),
					"column": str("column name"),
					"values": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "candidate values to report; omit for the most common ones",
					},
					"sample": map[string]any{
						"type":        "number",
						"description": "fraction of the table to read, 0-1; omit to read all of it",
					},
				},
			},
		},
		map[string]any{
			"name": "replication",
			"description": "List the logical replication slots with their lag in bytes and " +
				"whether a consumer is attached. An inactive slot retains WAL until the disk fills.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

func callMCPTool(ctx context.Context, db *pg.DB, name string, raw json.RawMessage) (string, error) {
	switch name {
	case "schema":
		return mcpSchema(ctx, db, raw)
	case "explain":
		return mcpExplain(ctx, db, raw)
	case "selectivity":
		return mcpSelectivity(ctx, db, raw)
	case "replication":
		return mcpReplication(ctx, db)
	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
}

func mcpSchema(ctx context.Context, db *pg.DB, raw json.RawMessage) (string, error) {
	var args struct {
		Schema string `json:"schema"`
		Table  string `json:"table"`
	}
	_ = json.Unmarshal(raw, &args)
	if args.Schema == "" {
		args.Schema = "public"
	}
	snap, err := pg.Introspect(ctx, db, pg.IntrospectOptions{Schemas: []string{args.Schema}})
	if err != nil {
		return "", err
	}
	if args.Table != "" {
		// Filtering here rather than in the query keeps one code
		// path: introspection is a dozen catalog queries and a
		// per-table variant of each is a second thing to keep right.
		for key, tbl := range snap.Tables {
			if tbl.Name != args.Table {
				delete(snap.Tables, key)
			}
		}
		if len(snap.Tables) == 0 {
			return "", fmt.Errorf("no table %q in schema %q", args.Table, args.Schema)
		}
	}
	out, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func mcpExplain(ctx context.Context, db *pg.DB, raw json.RawMessage) (string, error) {
	var args struct {
		SQL string `json:"sql"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	if strings.TrimSpace(args.SQL) == "" {
		return "", errors.New("explain needs a sql argument")
	}
	plan, err := pg.Explain(db, ctx, args.SQL)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "fingerprint: %s\n", plan.Fingerprint())
	fmt.Fprintf(&b, "estimated cost: %.2f, estimated rows: %d\n", plan.TotalCost, plan.PlanRows)

	used := plan.IndexesUsed()
	if len(used) == 0 {
		b.WriteString("indexes used: none\n")
	} else {
		rels := make([]string, 0, len(used))
		for rel := range used {
			rels = append(rels, rel)
		}
		sort.Strings(rels)
		b.WriteString("indexes used:\n")
		for _, rel := range rels {
			fmt.Fprintf(&b, "  %s: %s\n", rel, strings.Join(used[rel], ", "))
		}
	}
	for _, n := range plan.Nodes() {
		if strings.HasPrefix(n.Type, "Seq Scan") && n.Relation != "" {
			fmt.Fprintf(&b, "sequential scan on %s (estimated %d rows)\n", n.Relation, n.PlanRows)
		}
	}
	b.WriteString("\nplan:\n")
	b.Write(plan.JSON)
	return b.String(), nil
}

func mcpSelectivity(ctx context.Context, db *pg.DB, raw json.RawMessage) (string, error) {
	var args struct {
		Table  string   `json:"table"`
		Column string   `json:"column"`
		Values []string `json:"values"`
		Sample float64  `json:"sample"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	if args.Table == "" || args.Column == "" {
		return "", errors.New("selectivity needs a table and a column")
	}
	sketch, err := pg.SketchColumn(ctx, db, args.Table, args.Column,
		pg.SketchOptions{Sample: args.Sample})
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s.%s over %d rows (at least %d distinct values)\n",
		args.Table, args.Column, sketch.Total(), sketch.DistinctEstimate())
	if len(args.Values) == 0 {
		// Without candidates there is nothing to report per value: a
		// Count-Min sketch stores counters, not the values behind
		// them, so it cannot enumerate what it holds.
		fmt.Fprintf(&b, "\nPass \"values\" to estimate specific ones. "+
			"Rule of thumb: below 0.05 an index scan pays, above 0.2 it does not.\n")
		return b.String(), nil
	}
	candidates := make([]any, len(args.Values))
	for i, v := range args.Values {
		candidates[i] = v
	}
	b.WriteString("\nvalue: estimated rows (selectivity)\n")
	for _, c := range sketch.TopValues(candidates...) {
		fmt.Fprintf(&b, "  %v: %d (%.4f)", c.Value, c.Estimate, c.Selectivity)
		switch {
		case c.Selectivity <= 0.05:
			b.WriteString(" — selective, an index scan pays\n")
		case c.Selectivity >= 0.2:
			b.WriteString(" — not selective, the planner will read the table\n")
		default:
			b.WriteString("\n")
		}
	}
	if e := sketch.ExpectedError(); e > 0 {
		fmt.Fprintf(&b, "\nEstimates never undercount and may overcount by up to ~%.0f rows.\n", e)
	}
	return b.String(), nil
}

func mcpReplication(ctx context.Context, db *pg.DB) (string, error) {
	slots, err := pg.Slots(ctx, db)
	if err != nil {
		return "", err
	}
	if len(slots) == 0 {
		return "No logical replication slots.", nil
	}
	var b strings.Builder
	for _, s := range slots {
		lag, lagErr := pg.SlotLag(ctx, db, s.Name)
		fmt.Fprintf(&b, "%s (%s)", s.Name, s.Plugin)
		if s.Temporary {
			b.WriteString(" temporary")
		}
		if s.Active {
			b.WriteString(" active")
		} else {
			b.WriteString(" INACTIVE")
		}
		if lagErr == nil {
			fmt.Fprintf(&b, " lag=%d bytes", lag)
		}
		fmt.Fprintf(&b, " confirmed=%s\n", pg.FormatLSN(s.ConfirmedFlushLSN))
		if !s.Active && !s.Temporary {
			b.WriteString("  warning: a permanent slot with no consumer retains WAL " +
				"indefinitely and will eventually fill the disk\n")
		}
	}
	return b.String(), nil
}
