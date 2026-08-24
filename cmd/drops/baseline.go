package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bernardoforcillo/drops/pg"
)

// journalFile mirrors meta/_journal.json.
//
// pg owns this format and writes it from GenerateMigration, but the
// type is unexported and baseline is the one command that has to
// create a journal without generating a migration. The keys are
// drizzle-kit's; changing them here would break both tools at once.
type journalFile struct {
	Version string         `json:"version"`
	Dialect string         `json:"dialect"`
	Entries []journalEntry `json:"entries"`
}

type journalEntry struct {
	Idx         int    `json:"idx"`
	Version     string `json:"version"`
	When        int64  `json:"when"`
	Tag         string `json:"tag"`
	Breakpoints bool   `json:"breakpoints"`
}

// runBaseline adopts a database that already has its tables: it writes
// the migration that would have created what is there, records the
// snapshot beside it, and marks the whole thing as applied without
// running a statement against the data.
//
// From then on the database has a history, so `drops generate` has a
// previous snapshot to diff against and `drops migrate` has somewhere
// to start.
func runBaseline(ctx context.Context, args []string) error {
	fs := newFlagSet("baseline", "adopt an existing database: snapshot it and mark the history as applied")
	dsn := fs.String("dsn", "", "PostgreSQL connection string; defaults to $DROPS_PG_DSN or $DATABASE_URL")
	dir := fs.String("dir", "drizzle", "migration directory to create")
	name := fs.String("name", "baseline", "name for the first migration")
	pgSchema := fs.String("pg-schema", "public", "PostgreSQL schema to introspect")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireDSN(*dsn); err != nil {
		return err
	}
	// The migration baseline writes is applied by `drops migrate`,
	// which has the same blind spot about schemas that push does.
	if err := requirePublicSchema("baseline", *pgSchema); err != nil {
		return err
	}
	journalPath := filepath.Join(*dir, "meta", "_journal.json")
	if _, err := os.Stat(journalPath); err == nil {
		return fmt.Errorf("%s already exists: %s already has a history, and baselining over one would tell the database that migrations it never ran are applied", journalPath, *dir)
	}

	db, closeDB, err := connect(ctx, *dsn)
	if err != nil {
		return err
	}
	defer closeDB()

	live, err := pg.Introspect(ctx, db, pg.IntrospectOptions{Schemas: []string{*pgSchema}})
	if err != nil {
		return fmt.Errorf("introspect: %w", err)
	}
	if len(live.Tables) == 0 {
		return fmt.Errorf("schema %q has no tables, so there is nothing to adopt — declare the schema in Go and run `drops generate`", *pgSchema)
	}
	statements := pg.Diff(pg.EmptySnapshot(), live)
	if len(statements) == 0 {
		return fmt.Errorf("schema %q has %d table(s) but none of them could be described as SQL; nothing was written", *pgSchema, len(live.Tables))
	}

	tag := fmt.Sprintf("%04d_%s", 0, *name)
	sql := strings.Join(statements, "\n"+pg.StatementBreakpoint+"\n") + "\n"
	snapshot, err := live.Marshal()
	if err != nil {
		return err
	}
	when := time.Now().UnixMilli()
	journal, err := json.MarshalIndent(journalFile{
		Version: "7",
		Dialect: "postgresql",
		Entries: []journalEntry{{Idx: 0, Version: "7", When: when, Tag: tag, Breakpoints: true}},
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(*dir, "meta"), 0o755); err != nil {
		return err
	}
	files := map[string][]byte{
		filepath.Join(*dir, tag+".sql"):                   []byte(sql),
		filepath.Join(*dir, "meta", "0000_snapshot.json"): snapshot,
		journalPath: append(journal, '\n'),
	}
	for path, body := range files {
		if err := os.WriteFile(path, body, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}

	// Hash the file the way the migrator will read it, rather than the
	// bytes we happen to be holding: if the two ever disagreed, the
	// baseline would be applied a second time against tables that
	// already exist.
	m := migratorFor(db, *dir)
	if _, err := m.Status(ctx); err != nil {
		return err
	}
	entries, err := m.LoadEntries()
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := recordMigration(ctx, db, e.Hash, when); err != nil {
			return fmt.Errorf("record %s as applied: %w", e.Tag, err)
		}
	}

	fmt.Printf("adopted %d table(s) from schema %q\n", len(live.Tables), *pgSchema)
	fmt.Printf("  wrote %s (%d statement(s)), and recorded it as already applied\n", filepath.Join(*dir, tag+".sql"), len(statements))
	fmt.Printf("  wrote %s\n", filepath.Join(*dir, "meta", "0000_snapshot.json"))
	fmt.Printf("  wrote %s\n", journalPath)
	fmt.Println("\nnothing was run against the database. `drops pull` will give you the Go schema to diff against next.")
	return nil
}
