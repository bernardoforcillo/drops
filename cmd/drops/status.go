package main

import (
	"context"
	"fmt"
	"time"

	"github.com/bernardoforcillo/drops/pg"
)

// runStatus answers the three questions an operator has before
// touching anything: what is applied, what is waiting, and whether the
// database holds history this directory cannot explain.
func runStatus(ctx context.Context, args []string) error {
	fs := newFlagSet("status", "show what is applied, what is pending and what is unaccounted for")
	dsn := fs.String("dsn", "", "PostgreSQL connection string; defaults to $DROPS_PG_DSN or $DATABASE_URL")
	dir := fs.String("dir", "drizzle", "migration directory")
	schemaPkg := fs.String("schema", "", "Go package exporting func Schema() *pg.Schema; when given, drift is reported too")
	pgSchema := fs.String("pg-schema", "public", "PostgreSQL schema to introspect for the drift check")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireDSN(*dsn); err != nil {
		return err
	}
	if err := requireMigrationDir(*dir); err != nil {
		return err
	}
	db, closeDB, err := connect(ctx, *dsn)
	if err != nil {
		return err
	}
	defer closeDB()

	m := migratorFor(db, *dir)
	status, err := m.Status(ctx)
	if err != nil {
		return err
	}
	applied, pending := 0, 0
	fmt.Printf("%-4s %-34s %-9s %s\n", "idx", "tag", "state", "created")
	for i, s := range status {
		state, when := "pending", "-"
		if s.Applied {
			state = "applied"
			applied++
		} else {
			pending++
		}
		if s.When > 0 {
			when = time.UnixMilli(s.When).UTC().Format("2006-01-02 15:04:05Z")
		}
		fmt.Printf("%-4d %-34s %-9s %s\n", i, s.Tag, state, when)
	}
	fmt.Printf("\n%d applied, %d pending\n", applied, pending)

	extra, err := unaccountedHashes(ctx, db, m)
	if err != nil {
		return err
	}
	if len(extra) > 0 {
		fmt.Printf("\n%d applied migration(s) in the database that %s does not account for:\n", len(extra), *dir)
		for _, h := range extra {
			fmt.Println("  " + h)
		}
		fmt.Println("  a migration file was edited after it was applied, or it came from another branch")
	}

	if *schemaPkg == "" {
		return nil
	}
	pkg, err := locateSchema(ctx, *schemaPkg)
	if err != nil {
		return err
	}
	repo, err := loadSnapshot(ctx, pkg)
	if err != nil {
		return err
	}
	live, err := pg.Introspect(ctx, db, pg.IntrospectOptions{Schemas: []string{*pgSchema}})
	if err != nil {
		return fmt.Errorf("introspect: %w", err)
	}
	report := pg.DetectDrift(repo, live)
	if report.InSync {
		fmt.Println("\nthe database matches the Go schema")
		return nil
	}
	fmt.Printf("\ndrift: %d statement(s) would bring the database up to the Go schema — run `drops drift` for the detail\n",
		len(report.PendingMigrations))
	if pending > 0 {
		return nil
	}
	// Every migration is applied and the schema still disagrees, so
	// the gap is not something a pending migration will close.
	return findingError{fmt.Errorf("the database and the Go schema disagree with no pending migration to explain it")}
}
