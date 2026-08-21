package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
)

// runPush diffs the Go schema against the live database and applies
// the difference, with no migration file in between.
//
// The plan is computed at least twice: once to show and to judge, once
// to apply. That is a second introspection and a second expression
// probe against the server — the price of letting the CLI, rather than
// the generated program, decide whether the plan may run at all. Under
// --interactive it is computed once more per round of rename
// questions, because answering one can raise another.
func runPush(ctx context.Context, args []string) error {
	fs := newFlagSet("push", "apply the difference between the Go schema and the live database")
	schemaPkg := fs.String("schema", "", "Go package that exports func Schema() *pg.Schema (required)")
	dsn := fs.String("dsn", "", "PostgreSQL connection string; defaults to $DROPS_PG_DSN or $DATABASE_URL")
	pgSchema := fs.String("pg-schema", "public", "PostgreSQL schema to introspect and push into")
	safe := fs.Bool("safe", false, "wrap creative and destructive DDL in IF [NOT] EXISTS")
	dryRun := fs.Bool("dry-run", false, "print the statements and apply nothing")
	allow := fs.Bool("allow-destructive", false, "apply statements that destroy data or objects; it does not answer a rename question")
	dropIdx := fs.Bool("drop-unmanaged-indexes", false, "drop indexes the database has and the Go schema does not declare")
	interactive := fs.Bool("interactive", false,
		"ask on stdin about each change that could be a rename, instead of refusing")
	answers := &renameAnswers{}
	answers.register(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	decisions, err := answers.decisions()
	if err != nil {
		return err
	}
	if _, err := resolveDSN(*dsn); err != nil {
		return usageError{err}
	}
	if err := requirePublicSchema("push", *pgSchema); err != nil {
		return err
	}
	pkg, err := locateSchema(ctx, *schemaPkg)
	if err != nil {
		return err
	}

	req := bridgeRequest{
		Mode:                 bridgePush,
		DSN:                  mustDSN(*dsn),
		SchemaName:           *pgSchema,
		Safe:                 *safe,
		DryRun:               true,
		DropUnmanagedIndexes: *dropIdx,
		Renames:              decisions,
	}
	plan, err := planPush(ctx, pkg, req)
	if err != nil {
		return err
	}
	// A change that could be a rename produced no plan at all. Ask, if
	// the command line asked to be asked; otherwise stop.
	//
	// --allow-destructive is not what settles this, and the two must
	// not be run together into one permission. Whether a column is
	// being renamed or dropped is a question about what the change
	// means; whether a change that destroys data may run is a question
	// about what may be done. Letting the second answer the first would
	// make "yes, drop the table I told you to drop" also mean "and
	// guess about the column you asked me about", which is the guess
	// this whole path exists to refuse.
	//
	// Answering can raise questions of its own — see runGenerate — so
	// the loop runs until the push has nothing left to ask.
	asked := ""
	stdin := bufio.NewReader(os.Stdin)
	for len(plan.RenameCandidates) > 0 {
		if !*interactive {
			// Exit 3, the code for a run that worked and refused.
			return findingError{errors.New(plan.RenameMessage + interactiveHint)}
		}
		if plan.RenameMessage == asked {
			return findingError{errors.New(plan.RenameMessage)}
		}
		asked = plan.RenameMessage
		answered, err := promptRenames(stdin, os.Stdout, plan.RenameCandidates)
		if err != nil {
			return err
		}
		req.Renames = append(req.Renames, answered...)
		if plan, err = planPush(ctx, pkg, req); err != nil {
			return err
		}
	}
	printNotices(plan.Notices)
	if len(plan.Statements) == 0 {
		fmt.Println("no changes: the database already matches the Go schema")
		return nil
	}
	fmt.Printf("%d statement(s) to apply:\n", len(plan.Statements))
	for _, s := range plan.Statements {
		fmt.Println("  " + oneLine(s))
	}
	printWarnings(os.Stdout, plan.Statements)
	if err := guard(os.Stdout, plan.Statements, *allow); err != nil {
		return err
	}
	if *dryRun {
		fmt.Println("\n--dry-run: nothing was applied")
		return nil
	}

	req.DryRun = false
	applied, err := runBridge(ctx, pkg, req)
	if err != nil {
		return err
	}
	if applied.Push == nil || !applied.Push.Applied {
		return fmt.Errorf("the push reported nothing applied; the database may have changed between the plan and the apply — re-run to see the current difference")
	}
	fmt.Printf("\napplied %d statement(s)\n", len(applied.Push.Statements))
	return nil
}

// planPush runs the push in DryRun and returns what it plans to do —
// or, when the change could be a rename, the question it stopped on.
func planPush(ctx context.Context, pkg *schemaPackage, req bridgeRequest) (*bridgePushResult, error) {
	req.DryRun = true
	reply, err := runBridge(ctx, pkg, req)
	if err != nil {
		return nil, err
	}
	if reply.Push == nil {
		return nil, fmt.Errorf("the generated program returned no plan")
	}
	return reply.Push, nil
}

// mustDSN resolves the connection string after resolveDSN has already
// confirmed there is one.
func mustDSN(flagValue string) string {
	dsn, _ := resolveDSN(flagValue)
	return dsn
}

// printNotices reports the differences Push saw and declined to act
// on, with the statement it withheld where there is one.
func printNotices(notices []bridgeNotice) {
	if len(notices) == 0 {
		return
	}
	fmt.Printf("%d notice(s) — differences push saw and did not act on:\n", len(notices))
	for _, n := range notices {
		fmt.Printf("  %s: %s\n", n.Rule, n.Message)
		if n.SQL != "" {
			fmt.Printf("    withheld: %s\n", oneLine(n.SQL))
		}
	}
	fmt.Println()
}
