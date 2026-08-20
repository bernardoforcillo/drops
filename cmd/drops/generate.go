package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// runGenerate diffs the Go schema against the last snapshot in the
// migration directory and writes the migration that closes the gap.
// Nothing here touches a database — that is the point of generating a
// migration rather than pushing one.
func runGenerate(ctx context.Context, args []string) error {
	fs := newFlagSet("generate", "write a migration for the difference between the Go schema and the last snapshot")
	schemaPkg := fs.String("schema", "", "Go package that exports func Schema() *pg.Schema (required)")
	dir := fs.String("dir", "drizzle", "migration directory to read the history from and write into")
	name := fs.String("name", "", "name for the migration; a random one is chosen when empty")
	safe := fs.Bool("safe", false, "wrap creative and destructive DDL in IF [NOT] EXISTS so the migration can be re-run")
	noDown := fs.Bool("no-down", false, "skip the paired <tag>.down.sql rollback script")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	migrationDir, err := relativeDir(*dir)
	if err != nil {
		return usageError{err}
	}
	pkg, err := locateSchema(ctx, *schemaPkg)
	if err != nil {
		return err
	}
	reply, err := runBridge(ctx, pkg, bridgeRequest{
		Mode:     bridgeGenerate,
		Dir:      migrationDir,
		Name:     *name,
		Safe:     *safe,
		WithDown: !*noDown,
	})
	if err != nil {
		return err
	}
	res := reply.Generate
	if res == nil {
		return fmt.Errorf("the generated program returned no migration")
	}
	if res.NoOp {
		fmt.Println("no changes: the Go schema and the last snapshot in", *dir, "agree")
		return nil
	}

	statements := splitMigration(res.SQL)
	fmt.Printf("wrote %s (%d statement(s))\n", filepath.Join(*dir, res.Tag+".sql"), len(statements))
	if res.DownSQL != "" {
		fmt.Printf("wrote %s\n", filepath.Join(*dir, res.Tag+".down.sql"))
	}
	fmt.Printf("wrote %s\n", filepath.Join(*dir, "meta", fmt.Sprintf("%04d_snapshot.json", res.Idx)))
	for _, s := range statements {
		fmt.Println("  " + oneLine(s))
	}
	printWarnings(os.Stdout, statements)
	// Generating is not applying, so a destructive statement is
	// reported rather than refused — this is the review step.
	if found := destructive(statements); len(found) > 0 {
		fmt.Printf("\n%d destructive statement(s) in this migration; review before applying:\n", len(found))
		for _, r := range found {
			fmt.Printf("  %-22s %s\n", r.Rule, oneLine(r.Statement))
		}
	}
	return nil
}
