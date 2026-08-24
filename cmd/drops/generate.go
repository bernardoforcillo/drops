package main

import (
	"bufio"
	"context"
	"errors"
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

	migrationDir, err := relativeDir(*dir)
	if err != nil {
		return usageError{err}
	}
	pkg, err := locateSchema(ctx, *schemaPkg)
	if err != nil {
		return err
	}
	generate := func(renames []bridgeRename) (*bridgeGenerated, error) {
		reply, err := runBridge(ctx, pkg, bridgeRequest{
			Mode:     bridgeGenerate,
			Dir:      migrationDir,
			Name:     *name,
			Safe:     *safe,
			WithDown: !*noDown,
			Renames:  renames,
		})
		if err != nil {
			return nil, err
		}
		if reply.Generate == nil {
			return nil, fmt.Errorf("the generated program returned no migration")
		}
		return reply.Generate, nil
	}

	res, err := generate(decisions)
	if err != nil {
		return err
	}
	// A change that could be a rename generated nothing. Ask, if the
	// command line asked to be asked; otherwise stop, because the
	// alternative is the DROP COLUMN that loses the column.
	//
	// Answering can raise questions of its own: agreeing that "users"
	// became "people" is what makes a column renamed inside it visible
	// as a question at all, so the prompt runs until the generator has
	// nothing left to ask. The loop stops if a round comes back with
	// exactly the questions it went in with, which would otherwise mean
	// asking the same thing forever.
	asked := ""
	stdin := bufio.NewReader(os.Stdin)
	for len(res.RenameCandidates) > 0 {
		if !*interactive {
			// Exit 3, the code for a run that worked and refused: the
			// schema was read, the diff was computed, and the answer to
			// what it means is the one thing that was missing.
			return findingError{errors.New(res.RenameMessage + interactiveHint)}
		}
		if res.RenameMessage == asked {
			return findingError{errors.New(res.RenameMessage)}
		}
		asked = res.RenameMessage
		answered, err := promptRenames(stdin, os.Stdout, res.RenameCandidates)
		if err != nil {
			return err
		}
		decisions = append(decisions, answered...)
		if res, err = generate(decisions); err != nil {
			return err
		}
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
	if res.RenameLog {
		fmt.Printf("wrote %s\n", filepath.Join(*dir, "meta", "_renames.json"))
	}
	for _, s := range statements {
		fmt.Println("  " + oneLine(s))
	}
	printWarnings(os.Stdout, statements, decisions)
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
