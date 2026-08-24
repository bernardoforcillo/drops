package main

import (
	"context"
	"fmt"

	"github.com/bernardoforcillo/drops/pg"
)

// runDrift reports both directions of disagreement between the Go
// schema and a live database, and exits 3 when there is any — so it
// can be the last step of a deploy pipeline rather than something
// somebody remembers to read.
func runDrift(ctx context.Context, args []string) error {
	fs := newFlagSet("drift", "report where the live database and the Go schema disagree")
	schemaPkg := fs.String("schema", "", "Go package that exports func Schema() *pg.Schema (required)")
	dsn := fs.String("dsn", "", "PostgreSQL connection string; defaults to $DROPS_PG_DSN or $DATABASE_URL")
	pgSchema := fs.String("pg-schema", "public", "PostgreSQL schema to introspect")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireDSN(*dsn); err != nil {
		return err
	}
	pkg, err := locateSchema(ctx, *schemaPkg)
	if err != nil {
		return err
	}
	repo, err := loadSnapshot(ctx, pkg)
	if err != nil {
		return err
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

	report := pg.DetectDrift(repo, live)
	if report.InSync {
		fmt.Println("in sync: the database matches the Go schema")
		return nil
	}
	if len(report.PendingMigrations) > 0 {
		fmt.Printf("%d statement(s) would bring the database up to the Go schema:\n", len(report.PendingMigrations))
		for _, s := range report.PendingMigrations {
			fmt.Println("  " + oneLine(s))
		}
	}
	if len(report.UnauthorizedChanges) > 0 {
		if len(report.PendingMigrations) > 0 {
			fmt.Println()
		}
		fmt.Printf("%d statement(s) would bring the Go schema up to the database:\n", len(report.UnauthorizedChanges))
		for _, s := range report.UnauthorizedChanges {
			fmt.Println("  " + oneLine(s))
		}
	}
	fmt.Println(`
Every difference shows up in both lists — once as the change and once
as its inverse — so the pair says which side to correct, not that there
are two problems.

Introspection reads tables, columns, keys, uniques, checks, indexes,
single-column foreign keys, enums, sequences, views, row-level security
and policies. Multi-column foreign keys are not read back, so a schema
declaring one reports it as missing on every run.

Expressions are compared as text and the database answers in
PostgreSQL's own spelling of them, not yours: a CHECK body, a partial
index's predicate, a policy's USING clause or a view's definition will
be listed on every run even when the database matches. Drift has no
server to ask. "drops push --dry-run" does, and respells the declared
side before comparing.`)
	return findingError{fmt.Errorf("the database and the Go schema disagree")}
}
