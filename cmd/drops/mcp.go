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
	"time"

	"github.com/bernardoforcillo/drops/pg"
)

// "drops mcp" — the schema, the plans, the drift and the replication
// state, as tools an assistant can call.
//
// The rest of this binary is for a person at a terminal. This
// subcommand speaks the Model Context Protocol over stdin and stdout
// instead, so an assistant working in the repository can ask the
// database directly rather than being told about it second hand:
// which columns a table actually has, what plan a query gets, whether
// the predicate it is about to index is selective, whether a
// replication slot is falling behind, where the live database and the
// schema declared in Go have parted company, and which of the
// statements that would close that gap destroy data.
//
//	{ "mcpServers": { "drops": {
//	    "command": "drops",
//	    "args": ["mcp", "--dsn", "postgres://...", "--schema", "./db/schema"]
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
// Two tools need saying twice, because each has a mutating twin
// somewhere in this binary.
//
// EXPLAIN is run without ANALYZE, so the statement is planned and
// never executed. An assistant can ask for the plan of a DELETE
// without deleting anything. "safety" goes further and does not reach
// the server at all: it reads the statements as text.
//
// "migrations" answers the question `drops status` answers, and does
// not answer it the way status does. Status goes through
// pg.DrizzleMigrator.Status, which creates the history table when it
// is missing — the right thing for a command that is about to apply
// migrations, and a write. This one reads the journal off disk,
// selects the applied hashes, and reports a missing history table as
// what it is: nothing has been applied here yet.
//
// # What the operator decides, and what the assistant decides
//
// The DSN has always been a flag rather than a tool argument. The Go
// schema package and the migration directory are flags for the same
// reason, and it is a stronger one than tidiness: evaluating a Go
// schema means compiling and running it, so a tool argument naming
// the package to evaluate would let anything that can call a tool run
// code of its choosing out of the module. What this server may read
// is settled once, in the configuration a person wrote.
//
// Read-only is a promise about the database. Evaluating the schema
// package writes a temporary directory inside the module and runs the
// program it holds, exactly as `drops drift` does and for the same
// reason — a schema built out of pg.NewTable is a Go value, and the
// only thing that can evaluate one is Go.

const mcpProtocolVersion = "2024-11-05"

func runMCP(ctx context.Context, args []string) error {
	fs := newFlagSet("mcp", "Serve the schema, plans, drift and replication state over the Model Context Protocol")
	dsn := fs.String("dsn", "", "PostgreSQL connection string (else $DROPS_PG_DSN, $DATABASE_URL)")
	schemaPkg := fs.String("schema", "", "Go package exporting func Schema() *pg.Schema; without it the drift tool has nothing to compare the database against")
	dir := fs.String("dir", "drizzle", "migration directory the migrations tool reads")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	db, closeDB, err := connect(ctx, *dsn)
	if err != nil {
		return err
	}
	defer closeDB()

	return serveMCP(ctx, &mcpServer{db: db, schemaPkg: *schemaPkg, dir: *dir}, os.Stdin, os.Stdout)
}

// mcpServer is what a tool call is answered from: the connection, and
// the two things the operator settled on the command line rather than
// leaving to the assistant. Both may be absent — a server started with
// nothing but a DSN answers everything that only needs one, and says
// what to add to the configuration for the rest.
type mcpServer struct {
	db *pg.DB
	// schemaPkg is the Go package pattern that declares the schema,
	// empty when the server was started without one.
	schemaPkg string
	// dir is the migration directory the migrations tool reads.
	dir string
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
func serveMCP(ctx context.Context, srv *mcpServer, in io.Reader, out io.Writer) error {
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

		result, err := dispatchMCP(ctx, srv, req)
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

func dispatchMCP(ctx context.Context, srv *mcpServer, req rpcRequest) (any, error) {
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
		text, err := callMCPTool(ctx, srv, p.Name, p.Arguments)
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
		map[string]any{
			"name": "drift",
			"description": "Compare the live database with the schema declared in Go and report " +
				"both directions of disagreement — what the database is missing, and what it has " +
				"that the schema does not — naming the statements that would destroy data. Nothing " +
				"is applied. Needs the server started with --schema.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"schema": str(`PostgreSQL schema to introspect (default "public")`),
				},
			},
		},
		map[string]any{
			"name": "safety",
			"description": "Read migration SQL and report what applying it would cost: which " +
				"statements destroy data and are refused unattended, and which merely lock, rewrite " +
				"or invalidate a plan while they run. Text analysis — the statements are never sent " +
				"to the server, so SQL that has not been written to a file yet can be checked.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"sql"},
				"properties": map[string]any{
					"sql": str("the statements to analyse; a whole migration is fine, they are split " +
						"on semicolons and on drizzle-kit statement breakpoints"),
				},
			},
		},
		map[string]any{
			"name": "migrations",
			"description": "List the migration history: what is applied, what is pending, and what " +
				"the database records that the migration directory cannot account for. Reads the " +
				"journal and the history table without creating either.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

func callMCPTool(ctx context.Context, srv *mcpServer, name string, raw json.RawMessage) (string, error) {
	switch name {
	case "schema":
		return mcpSchema(ctx, srv.db, raw)
	case "explain":
		return mcpExplain(ctx, srv.db, raw)
	case "selectivity":
		return mcpSelectivity(ctx, srv.db, raw)
	case "replication":
		return mcpReplication(ctx, srv.db)
	case "drift":
		return mcpDrift(ctx, srv, raw)
	case "safety":
		return mcpSafety(raw)
	case "migrations":
		return mcpMigrations(ctx, srv)
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

func mcpDrift(ctx context.Context, srv *mcpServer, raw json.RawMessage) (string, error) {
	var args struct {
		Schema string `json:"schema"`
	}
	_ = json.Unmarshal(raw, &args)
	if args.Schema == "" {
		args.Schema = "public"
	}
	if srv.schemaPkg == "" {
		return "", errors.New(`drift compares the database against the schema declared in Go, and this server was started without one: add "--schema", "./db/schema" to the args in the MCP configuration, and start the server from inside the module that declares it`)
	}
	// The package is evaluated on every call rather than once at
	// startup. A drift report is about the schema as it is now, and
	// the assistant asking for one has usually just edited it.
	pkg, err := locateSchema(ctx, srv.schemaPkg)
	if err != nil {
		return "", err
	}
	repo, err := loadSnapshot(ctx, pkg)
	if err != nil {
		return "", err
	}
	live, err := pg.Introspect(ctx, srv.db, pg.IntrospectOptions{Schemas: []string{args.Schema}})
	if err != nil {
		return "", fmt.Errorf("introspect: %w", err)
	}

	report := pg.DetectDrift(repo, live)
	if report.InSync {
		return "in sync: the database matches the Go schema.", nil
	}
	var b strings.Builder
	if len(report.PendingMigrations) > 0 {
		fmt.Fprintf(&b, "%d statement(s) would bring the database up to the Go schema:\n", len(report.PendingMigrations))
		for _, stmt := range report.PendingMigrations {
			// collapse rather than oneLine: the CLI truncates to fit a
			// terminal, and a statement cut off at column 90 is a
			// statement an assistant will reason about wrongly.
			fmt.Fprintf(&b, "  %s\n", collapse(stmt))
		}
		writeDestructive(&b, report.PendingMigrations)
	}
	if len(report.UnauthorizedChanges) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%d statement(s) would bring the Go schema up to the database:\n", len(report.UnauthorizedChanges))
		for _, stmt := range report.UnauthorizedChanges {
			fmt.Fprintf(&b, "  %s\n", collapse(stmt))
		}
	}
	fmt.Fprintf(&b, "\n%s\n", driftCaveats)
	return b.String(), nil
}

// writeDestructive names the statements in a plan that destroy
// something. It is the classifier the CLI's own gate uses, so the
// answer an assistant gets here is the answer `drops push` would give
// when it stopped.
func writeDestructive(b *strings.Builder, plan []string) {
	found := destructive(plan)
	if len(found) == 0 {
		b.WriteString("  none of them destroys data.\n")
		return
	}
	fmt.Fprintf(b, "\n%d of those statement(s) destroy data, and `drops push` refuses them "+
		"unless the operator passes --allow-destructive:\n", len(found))
	for _, r := range found {
		fmt.Fprintf(b, "  %s\n    %s\n    %s\n", r.Rule, collapse(r.Statement), r.Reason)
	}
}

func mcpSafety(raw json.RawMessage) (string, error) {
	var args struct {
		SQL string `json:"sql"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	stmts := splitSQL(args.SQL)
	if len(stmts) == 0 {
		return "", errors.New("safety needs a sql argument holding at least one statement")
	}
	refusals := destructive(stmts)
	warns := warnings(stmts, nil)

	var b strings.Builder
	fmt.Fprintf(&b, "%d statement(s) read; nothing was sent to the server.\n", len(stmts))
	if len(refusals) == 0 && len(warns) == 0 {
		b.WriteString("\nNothing here destroys data, and no safety rule fired.\n")
		return b.String(), nil
	}
	if len(refusals) > 0 {
		fmt.Fprintf(&b, "\n%d statement(s) destroy data — what they remove is gone afterwards and "+
			"no rollback brings it back, so `drops push` and `drops migrate` refuse them unless the "+
			"operator passes --allow-destructive:\n", len(refusals))
		for _, r := range refusals {
			fmt.Fprintf(&b, "  %s\n    %s\n    %s\n", r.Rule, collapse(r.Statement), r.Reason)
		}
	}
	if len(warns) > 0 {
		fmt.Fprintf(&b, "\n%d safety warning(s) — these hurt while the statement runs and are "+
			"correct once it has:\n", len(warns))
		for _, w := range warns {
			fmt.Fprintf(&b, "  %s %s\n    %s\n    %s\n    fix: %s\n",
				w.Severity, w.Rule, collapse(w.Statement), w.Message, w.Suggestion)
		}
	}
	return b.String(), nil
}

func mcpMigrations(ctx context.Context, srv *mcpServer) (string, error) {
	if err := requireMigrationDir(srv.dir); err != nil {
		return "", err
	}
	entries, err := migratorFor(srv.db, srv.dir).LoadEntries()
	if err != nil {
		return "", err
	}
	applied, err := appliedHashes(ctx, srv.db)
	// A missing history table is an answer, not a failure: it is what
	// a database nothing has been applied to looks like, and creating
	// it in order to report on it is the write this server does not
	// do. On a database that has never been migrated the schema is
	// missing too, and PostgreSQL says so with a different code.
	noHistory := errors.Is(err, pg.ErrUndefinedTable) || errors.Is(err, pg.ErrInvalidSchemaName)
	if err != nil && !noHistory {
		return "", err
	}

	inDatabase := make(map[string]bool, len(applied))
	for _, h := range applied {
		inDatabase[h] = true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "migration directory: %s\n\n", srv.dir)
	fmt.Fprintf(&b, "%-4s %-34s %-9s %s\n", "idx", "tag", "state", "created")
	countApplied, countPending := 0, 0
	inJournal := make(map[string]bool, len(entries))
	for i, e := range entries {
		inJournal[e.Hash] = true
		state, when := "pending", "-"
		if inDatabase[e.Hash] {
			state = "applied"
			countApplied++
		} else {
			countPending++
		}
		if e.When > 0 {
			when = time.UnixMilli(e.When).UTC().Format("2006-01-02 15:04:05Z")
		}
		fmt.Fprintf(&b, "%-4d %-34s %-9s %s\n", i, e.Tag, state, when)
	}
	fmt.Fprintf(&b, "\n%d applied, %d pending\n", countApplied, countPending)
	if noHistory {
		fmt.Fprintf(&b, "\nThere is no %s.%s table in the database, so nothing has been applied "+
			"here yet — `drops migrate` creates it on its first run.\n", pg.DrizzleSchema, pg.DrizzleTable)
		return b.String(), nil
	}

	var extra []string
	for _, h := range applied {
		if !inJournal[h] {
			extra = append(extra, h)
		}
	}
	if len(extra) > 0 {
		fmt.Fprintf(&b, "\n%d applied migration(s) in the database that %s does not account for:\n",
			len(extra), srv.dir)
		for _, h := range extra {
			fmt.Fprintf(&b, "  %s\n", h)
		}
		b.WriteString("  a migration file was edited after it was applied, or it came from another branch\n")
	}
	return b.String(), nil
}

// splitSQL breaks a script into statements, less its comments.
//
// The safety rules read one statement at a time — a rule matches a
// pattern inside a statement, and the destructive classifier reports
// the first rule that fires on each and moves on — so a whole script
// handed over as one string is a script whose second DROP is never
// mentioned. drops writes the drizzle-kit breakpoint between its own
// statements and everything else uses semicolons, so both split here.
//
// A semicolon ends a statement everywhere except inside a string, a
// quoted identifier, a comment, and a dollar-quoted body — the last of
// which is how a migration carrying a function body puts semicolons in
// the middle of one statement. What follows is a scanner for exactly
// those four, which is less than a SQL parser and enough to stop it
// splitting a trigger function in half.
//
// Comments are dropped rather than carried along, because the rules
// match text: a DROP TABLE somebody commented out would otherwise be
// reported as data loss, which is the one kind of false positive that
// teaches a reader to stop believing the output.
func splitSQL(sql string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}
	for _, script := range strings.Split(sql, pg.StatementBreakpoint) {
		for i := 0; i < len(script); {
			switch {
			case strings.HasPrefix(script[i:], "--"):
				// Up to the newline, which is left for the next pass
				// to write: dropping it too would join two lines into
				// one and a rule that reads them apart would misread
				// the result.
				if nl := strings.IndexByte(script[i:], '\n'); nl >= 0 {
					i += nl
				} else {
					i = len(script)
				}
				cur.WriteByte(' ')
			case strings.HasPrefix(script[i:], "/*"):
				i = skipBlockComment(script, i)
				cur.WriteByte(' ')
			case script[i] == '\'' || script[i] == '"':
				end := skipQuoted(script, i)
				cur.WriteString(script[i:end])
				i = end
			case script[i] == '$':
				end, ok := skipDollarQuoted(script, i)
				if !ok {
					// A parameter placeholder, not a quote.
					cur.WriteByte(script[i])
					i++
					continue
				}
				cur.WriteString(script[i:end])
				i = end
			case script[i] == ';':
				flush()
				i++
			default:
				cur.WriteByte(script[i])
				i++
			}
		}
		flush()
	}
	return out
}

// skipQuoted returns the index just past the string literal or quoted
// identifier that starts at i. Either is closed by its own quote,
// which is doubled to escape it; in an E'...' string a backslash
// escapes the character after it as well.
func skipQuoted(s string, i int) int {
	quote := s[i]
	escapes := quote == '\'' && i > 0 && (s[i-1] == 'e' || s[i-1] == 'E') &&
		(i == 1 || !identByte(s[i-2]))
	for j := i + 1; j < len(s); j++ {
		switch {
		case escapes && s[j] == '\\':
			j++
		case s[j] == quote:
			if j+1 < len(s) && s[j+1] == quote {
				j++
				continue
			}
			return j + 1
		}
	}
	return len(s)
}

// skipBlockComment returns the index just past the comment starting at
// i. PostgreSQL nests them, so this counts depth rather than looking
// for the first "*/".
func skipBlockComment(s string, i int) int {
	depth := 0
	for j := i; j < len(s); {
		switch {
		case strings.HasPrefix(s[j:], "/*"):
			depth++
			j += 2
		case strings.HasPrefix(s[j:], "*/"):
			depth--
			j += 2
			if depth == 0 {
				return j
			}
		default:
			j++
		}
	}
	return len(s)
}

// skipDollarQuoted returns the index just past the dollar-quoted body
// that starts at i, and whether one starts there at all. $$ and $tag$
// open one; $1 is a parameter and opens nothing.
func skipDollarQuoted(s string, i int) (int, bool) {
	j := i + 1
	for j < len(s) && identByte(s[j]) {
		// A tag is an identifier and cannot start with a digit, which
		// is the whole of what keeps $1 a parameter placeholder.
		if j == i+1 && s[j] >= '0' && s[j] <= '9' {
			return i, false
		}
		j++
	}
	if j >= len(s) || s[j] != '$' {
		return i, false
	}
	tag := s[i : j+1]
	if k := strings.Index(s[j+1:], tag); k >= 0 {
		return j + 1 + k + len(tag), true
	}
	// An unterminated body runs to the end; there is no statement
	// boundary left to find in it either way.
	return len(s), true
}

// identByte reports whether c can appear in an unquoted identifier,
// which is what a dollar-quote tag is.
func identByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
