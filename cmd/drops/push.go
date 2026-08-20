package main

import (
	"context"
	"fmt"
	"os"
)

// runPush diffs the Go schema against the live database and applies
// the difference, with no migration file in between.
//
// The plan is computed twice: once to show and to judge, once to
// apply. That is a second introspection and a second expression probe
// against the server — the price of letting the CLI, rather than the
// generated program, decide whether the plan may run at all.
func runPush(ctx context.Context, args []string) error {
	fs := newFlagSet("push", "apply the difference between the Go schema and the live database")
	schemaPkg := fs.String("schema", "", "Go package that exports func Schema() *pg.Schema (required)")
	dsn := fs.String("dsn", "", "PostgreSQL connection string; defaults to $DROPS_PG_DSN or $DATABASE_URL")
	pgSchema := fs.String("pg-schema", "public", "PostgreSQL schema to introspect and push into")
	safe := fs.Bool("safe", false, "wrap creative and destructive DDL in IF [NOT] EXISTS")
	dryRun := fs.Bool("dry-run", false, "print the statements and apply nothing")
	allow := fs.Bool("allow-destructive", false, "apply statements that destroy data or objects")
	dropIdx := fs.Bool("drop-unmanaged-indexes", false, "drop indexes the database has and the Go schema does not declare")
	if err := parseFlags(fs, args); err != nil {
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
	}
	reply, err := runBridge(ctx, pkg, req)
	if err != nil {
		return err
	}
	plan := reply.Push
	if plan == nil {
		return fmt.Errorf("the generated program returned no plan")
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
