package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bernardoforcillo/drops/pg"
)

// The convention.
//
// A CLI cannot read a Go variable. The schema drops migrates lives in
// Go — pg.NewTable, pg.Add, pg.References — and evaluating it means
// running it, so `drops` writes a small program that imports the
// schema package, calls into it, and prints the answer as JSON on
// stdout. `go run` compiles that program with the real compiler
// against the real package, which is the only way the answer is the
// same one the application gets.
//
// The one thing the generated program has to know is where to call:
//
//	// db/schema/schema.go
//	func Schema() *pg.Schema {
//	    return pg.NewSchema(Users, Posts)
//	}
//
// That is the whole contract. Anything the function returns is what
// gets diffed, pushed and generated from, so a table the schema
// package declares but does not register is deliberately invisible —
// which is how a table that another tool owns stays out of drops's way.
const schemaFuncHint = `add a Schema function to the package:

    func Schema() *pg.Schema {
        return pg.NewSchema(Users, Posts)  // every table drops should manage
    }`

// bridge is a generated program compiled against the user's schema
// package. It has exactly three jobs, one per subcommand that needs a
// Go schema rather than a database.
type bridgeMode string

const (
	// bridgeSnapshot prints pg.BuildSnapshot(Schema()) as JSON.
	bridgeSnapshot bridgeMode = "snapshot"
	// bridgeGenerate runs pg.GenerateMigration and prints the result.
	bridgeGenerate bridgeMode = "generate"
	// bridgePush runs pg.Push against a live database.
	bridgePush bridgeMode = "push"
)

// bridgeRequest is the JSON the CLI hands the generated program on
// stdin. One struct for all three modes; a mode reads the fields it
// needs.
type bridgeRequest struct {
	Mode                 bridgeMode `json:"mode"`
	DSN                  string     `json:"dsn,omitempty"`
	Dir                  string     `json:"dir,omitempty"`
	Name                 string     `json:"name,omitempty"`
	SchemaName           string     `json:"schemaName,omitempty"`
	Safe                 bool       `json:"safe,omitempty"`
	WithDown             bool       `json:"withDown,omitempty"`
	DryRun               bool       `json:"dryRun,omitempty"`
	DropUnmanagedIndexes bool       `json:"dropUnmanagedIndexes,omitempty"`

	// Renames answers the rename questions generate would otherwise
	// stop on. See rename.go.
	Renames []bridgeRename `json:"renames,omitempty"`
}

// bridgeRename is one answer, in the shape pg.RenameDecision has. An
// answer of "no" may leave To empty, and then it covers every pair the
// diff offered for that object.
type bridgeRename struct {
	Kind     string `json:"kind"`
	Table    string `json:"table,omitempty"`
	From     string `json:"from"`
	To       string `json:"to,omitempty"`
	IsRename bool   `json:"rename"`
}

// bridgeCandidate is one question, as pg.RenameCandidate poses it.
type bridgeCandidate struct {
	Kind     string `json:"kind"`
	Table    string `json:"table,omitempty"`
	From     string `json:"from"`
	To       string `json:"to"`
	FromType string `json:"fromType,omitempty"`
	ToType   string `json:"toType,omitempty"`
}

// bridgeReply is the JSON the generated program prints. Only the
// fields belonging to the request's mode are populated.
type bridgeReply struct {
	Snapshot json.RawMessage   `json:"snapshot,omitempty"`
	Generate *bridgeGenerated  `json:"generate,omitempty"`
	Push     *bridgePushResult `json:"push,omitempty"`
}

type bridgeGenerated struct {
	Tag     string `json:"tag"`
	Idx     int    `json:"idx"`
	SQL     string `json:"sql"`
	DownSQL string `json:"downSql"`
	NoOp    bool   `json:"noOp"`

	// Renames is what the migration renamed rather than dropped, and
	// RenameLog says whether the answers were written to
	// meta/_renames.json — which happens whenever there was a decision
	// to record, including one that declined a rename.
	Renames   []bridgeRename `json:"renames,omitempty"`
	RenameLog bool           `json:"renameLog,omitempty"`

	// RenameCandidates is non-empty when nothing was generated because
	// the change could be a rename and no answer was on hand.
	// RenameMessage is what drops/pg said about it, passed through
	// whole so the CLI reports the library's wording rather than a
	// second version of it.
	RenameCandidates []bridgeCandidate `json:"renameCandidates,omitempty"`
	RenameMessage    string            `json:"renameMessage,omitempty"`
}

type bridgePushResult struct {
	Statements []string       `json:"statements"`
	Applied    bool           `json:"applied"`
	Notices    []bridgeNotice `json:"notices"`
}

type bridgeNotice struct {
	Rule    string `json:"rule"`
	Table   string `json:"table"`
	Object  string `json:"object"`
	Message string `json:"message"`
	SQL     string `json:"sql"`
}

// schemaPackage is a located Go package that declares a schema.
type schemaPackage struct {
	ImportPath string
	Dir        string
	ModuleDir  string
}

// locateSchema resolves a package pattern (./db/schema, or an import
// path) to the package on disk and checks the convention before
// anything is compiled, so the common mistake is reported as itself
// rather than as a compiler error inside a generated file.
func locateSchema(ctx context.Context, pattern string) (*schemaPackage, error) {
	if pattern == "" {
		// Not having been told which package to read is the command
		// line being wrong, the same as not having been told which
		// database to touch.
		return nil, usagef("--schema is required: point it at the Go package that declares your tables, e.g. --schema ./db/schema")
	}
	var out, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "go", "list", "-json", pattern)
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go list %s: %v\n%s\nrun drops from inside the Go module that declares the schema, and pass --schema as a package pattern such as ./db/schema", pattern, err, strings.TrimSpace(stderr.String()))
	}
	var listed struct {
		ImportPath string
		Dir        string
		Name       string
		Module     struct{ Dir string }
	}
	if err := json.Unmarshal(out.Bytes(), &listed); err != nil {
		return nil, fmt.Errorf("parse go list output for %s: %w", pattern, err)
	}
	if listed.Module.Dir == "" {
		return nil, fmt.Errorf("package %s is not inside a Go module; drops needs one so it can compile a program that imports your schema", pattern)
	}
	if err := checkSchemaFunc(listed.Dir, listed.ImportPath); err != nil {
		return nil, err
	}
	return &schemaPackage{ImportPath: listed.ImportPath, Dir: listed.Dir, ModuleDir: listed.Module.Dir}, nil
}

// checkSchemaFunc looks for the convention in the package's source. It
// reads names, not types — the compiler stays the authority on whether
// the function returns what it should — but it turns the usual mistake
// into a sentence that says what to write.
func checkSchemaFunc(dir, importPath string) error {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv != nil || fn.Name.Name != "Schema" {
					continue
				}
				if fn.Type.Params.NumFields() != 0 {
					return fmt.Errorf("%s.Schema takes arguments; drops calls it with none — %s", importPath, schemaFuncHint)
				}
				return nil
			}
		}
	}
	return fmt.Errorf("package %s declares no Schema function, so drops has nothing to read — %s", importPath, schemaFuncHint)
}

// runBridge writes the generated program, runs it with `go run`, and
// decodes its reply.
//
// The program is written inside the schema package's module because
// that is the only place an import of the schema package resolves, and
// `go run` is invoked from the working directory the user started in
// so a relative --dir means what it says on the command line.
func runBridge(ctx context.Context, pkg *schemaPackage, req bridgeRequest) (*bridgeReply, error) {
	if req.Mode == bridgePush {
		if err := requireDriver(ctx, pkg); err != nil {
			return nil, err
		}
	}
	dir, err := os.MkdirTemp(pkg.ModuleDir, ".drops-bridge-")
	if err != nil {
		return nil, fmt.Errorf("create the directory for the generated program: %w", err)
	}
	defer os.RemoveAll(dir)

	src := renderBridge(pkg.ImportPath, req.Mode == bridgePush)
	main := filepath.Join(dir, "main.go")
	if err := os.WriteFile(main, []byte(src), 0o644); err != nil {
		return nil, fmt.Errorf("write the generated program: %w", err)
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "go", "run", main)
	cmd.Stdin = bytes.NewReader(body)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	// The generated program prints its progress on stderr; showing it
	// is how a push that takes a minute looks like one.
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
	}
	if runErr != nil {
		return nil, fmt.Errorf("evaluating the schema in %s failed: %w", pkg.ImportPath, runErr)
	}
	var reply bridgeReply
	if err := json.Unmarshal(stdout.Bytes(), &reply); err != nil {
		return nil, fmt.Errorf("the generated program printed something that is not a drops reply: %w\n%s", err, excerpt(stdout.String()))
	}
	return &reply, nil
}

// pgxImport is the package the generated program imports when it has
// to open a connection.
const pgxImport = "github.com/jackc/pgx/v5/stdlib"

// requireDriver checks that the user's module can provide that import
// before the generated program is compiled against it.
//
// This is the bill for making the CLI its own module. The binary
// carries pgx, but `go run` resolves the generated program's imports
// in the module that declares the schema, not in the one the binary
// was built from — so `drops push`, alone among the commands, needs a
// driver in the user's go.mod. Saying so here beats a compiler error
// inside a file the user never wrote.
func requireDriver(ctx context.Context, pkg *schemaPackage) error {
	cmd := exec.CommandContext(ctx, "go", "list", "-e", "-f", "{{if .Error}}{{.Error}}{{end}}", pgxImport)
	cmd.Dir = pkg.ModuleDir
	out, err := cmd.Output()
	if err == nil && strings.TrimSpace(string(out)) == "" {
		return nil
	}
	return fmt.Errorf("push needs a PostgreSQL driver in the module that declares the schema: drops compiles a program there to evaluate it, and that program opens the connection\nadd it with: go get github.com/jackc/pgx/v5")
}

// loadSnapshot returns the snapshot of the Go schema in pkg.
func loadSnapshot(ctx context.Context, pkg *schemaPackage) (*pg.Snapshot, error) {
	reply, err := runBridge(ctx, pkg, bridgeRequest{Mode: bridgeSnapshot})
	if err != nil {
		return nil, err
	}
	if len(reply.Snapshot) == 0 {
		return nil, fmt.Errorf("the generated program returned no snapshot")
	}
	return pg.UnmarshalSnapshot(reply.Snapshot)
}

func excerpt(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		return s[:400] + "..."
	}
	return s
}

// renderBridge produces the program's source. It is deliberately
// dull: every decision it could make is one the CLI has already made,
// and code that only exists inside a temporary directory is code no
// test can reach.
//
// withDB decides whether the program carries the code that opens a
// database. Only push needs it, and it is the one part of the bridge
// that costs the user's module a dependency — see requireDriver — so
// generate, drift and status keep compiling in a module that has
// nothing but drops in it.
func renderBridge(importPath string, withDB bool) string {
	src := strings.ReplaceAll(bridgeTemplate, "$IMPORT$", importPath)
	imports, connect := bridgeStubImports, bridgeStubConnect
	if withDB {
		imports, connect = bridgeDBImports, bridgeDBConnect
	}
	src = strings.ReplaceAll(src, "$DBIMPORTS$", imports)
	return strings.ReplaceAll(src, "$CONNECT$", connect)
}

const bridgeTemplate = `// Code generated by drops. DO NOT EDIT.
//
// This program exists for one process lifetime. It imports the
// schema package, calls the drops/pg function the CLI asked for, and
// prints the answer as JSON.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	userschema "$IMPORT$"
	"github.com/bernardoforcillo/drops/pg"
$DBIMPORTS$)

type request struct {
	Mode                 string ` + "`json:\"mode\"`" + `
	DSN                  string ` + "`json:\"dsn\"`" + `
	Dir                  string ` + "`json:\"dir\"`" + `
	Name                 string ` + "`json:\"name\"`" + `
	SchemaName           string ` + "`json:\"schemaName\"`" + `
	Safe                 bool   ` + "`json:\"safe\"`" + `
	WithDown             bool   ` + "`json:\"withDown\"`" + `
	DryRun               bool   ` + "`json:\"dryRun\"`" + `
	DropUnmanagedIndexes bool   ` + "`json:\"dropUnmanagedIndexes\"`" + `
	Renames              []struct {
		Kind     string ` + "`json:\"kind\"`" + `
		Table    string ` + "`json:\"table\"`" + `
		From     string ` + "`json:\"from\"`" + `
		To       string ` + "`json:\"to\"`" + `
		IsRename bool   ` + "`json:\"rename\"`" + `
	} ` + "`json:\"renames\"`" + `
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "drops:", err)
		os.Exit(1)
	}
}

func run() error {
	var req request
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		return fmt.Errorf("read request: %w", err)
	}
	schema := userschema.Schema()
	if schema == nil {
		return fmt.Errorf("$IMPORT$.Schema() returned nil")
	}
	reply := map[string]any{}
	ctx := context.Background()

	switch req.Mode {
	case "snapshot":
		body, err := pg.BuildSnapshot(schema).Marshal()
		if err != nil {
			return err
		}
		reply["snapshot"] = json.RawMessage(body)

	case "generate":
		decisions := make([]pg.RenameDecision, 0, len(req.Renames))
		for _, r := range req.Renames {
			decisions = append(decisions, pg.RenameDecision{
				Rename: pg.Rename{
					Kind:  pg.RenameKind(r.Kind),
					Table: r.Table,
					From:  r.From,
					To:    r.To,
				},
				IsRename: r.IsRename,
			})
		}
		res, err := pg.GenerateMigration(pg.GenerateOptions{
			Schema:   schema,
			Dir:      req.Dir,
			Name:     req.Name,
			Safe:     req.Safe,
			WithDown: req.WithDown,
			Renames:  decisions,
		})
		if err != nil {
			// An ambiguous rename is a question, not a failure: it
			// travels back as data so the CLI can put it to whoever is
			// there, or report it and stop when nobody is.
			var amb *pg.RenameAmbiguityError
			if !errors.As(err, &amb) {
				return err
			}
			cands := make([]map[string]any, 0, len(amb.Candidates))
			for _, c := range amb.Candidates {
				cands = append(cands, map[string]any{
					"kind": string(c.Kind), "table": c.Table,
					"from": c.From, "to": c.To,
					"fromType": c.FromType, "toType": c.ToType,
				})
			}
			reply["generate"] = map[string]any{
				"renameCandidates": cands, "renameMessage": amb.Error(),
			}
			break
		}
		renamed := make([]map[string]any, 0, len(res.Renames))
		for _, r := range res.Renames {
			renamed = append(renamed, map[string]any{
				"kind": string(r.Kind), "table": r.Table,
				"from": r.From, "to": r.To, "rename": true,
			})
		}
		reply["generate"] = map[string]any{
			"tag": res.Tag, "idx": res.Idx, "sql": res.SQL,
			"downSql": res.DownSQL, "noOp": res.NoOp, "renames": renamed,
			"renameLog": len(res.RenameLog) > 0,
		}

	case "push":
		db, closeDB, err := connect(ctx, req.DSN)
		if err != nil {
			return err
		}
		defer closeDB()
		res, err := pg.Push(ctx, db, schema, pg.PushOptions{
			Schema:               req.SchemaName,
			Safe:                 req.Safe,
			DryRun:               req.DryRun,
			DropUnmanagedIndexes: req.DropUnmanagedIndexes,
		})
		if err != nil {
			return err
		}
		notices := make([]map[string]any, 0, len(res.Notices))
		for _, n := range res.Notices {
			notices = append(notices, map[string]any{
				"rule": n.Rule, "table": n.Table, "object": n.Object,
				"message": n.Message, "sql": n.SQL,
			})
		}
		reply["push"] = map[string]any{
			"statements": res.Statements, "applied": res.Applied, "notices": notices,
		}

	default:
		return fmt.Errorf("unknown mode %q", req.Mode)
	}
	return json.NewEncoder(os.Stdout).Encode(reply)
}

$CONNECT$`

// bridgeDBImports and bridgeDBConnect are the half of the generated
// program that opens a connection: pgx, wrapped by the adapter drops
// ships for it. The CLI cannot do this part on the user's behalf —
// evaluating a Go schema means running Go inside the user's module,
// and pg.Push needs both the schema value and the server in one
// process.
const bridgeDBImports = `	"strings"

	"github.com/bernardoforcillo/drops/stdlib"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	pgxstdlib "github.com/jackc/pgx/v5/stdlib"
`

const bridgeDBConnect = `func connect(ctx context.Context, dsn string) (*pg.DB, func(), error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, nil, err
	}
	cfg.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		if !boilerplate(n) {
			fmt.Fprintln(os.Stderr, "drops: server notice:", n.Message)
		}
	}
	sqlDB := pgxstdlib.OpenDB(*cfg)
	sqlDB.SetMaxOpenConns(1)
	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return nil, nil, err
	}
	return pg.New(stdlib.New(sqlDB)), func() { sqlDB.Close() }, nil
}

// boilerplate reports whether a notice says only that an
// IF [NOT] EXISTS clause found nothing to do.
func boilerplate(n *pgconn.Notice) bool {
	skip := map[string]bool{"42P06": true, "42P07": true, "42710": true, "42704": true, "42P01": true}
	return n != nil && strings.EqualFold(n.Severity, "notice") && skip[n.Code]
}
`

// The stub keeps the template a single program. The CLI only asks for
// push with withDB set, so nothing reaches this — but a generated file
// that does not compile is one nobody can debug, and a mode that grew
// a DSN later would fail here saying what is missing rather than
// somewhere stranger.
const bridgeStubImports = ""

const bridgeStubConnect = `func connect(context.Context, string) (*pg.DB, func(), error) {
	return nil, nil, fmt.Errorf("this program was generated without a database driver")
}
`
