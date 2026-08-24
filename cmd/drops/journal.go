package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bernardoforcillo/drops/pg"
)

// Working with a drizzle-kit migration directory.
//
// The layout is drizzle-kit's, and so is the protocol for applying it:
// meta/_journal.json lists the migrations in order, each <tag>.sql is
// hashed with SHA-256, and the hashes of the applied ones live in
// drizzle.__drizzle_migrations. pg.DrizzleMigrator implements exactly
// that, which is why `drops migrate` can apply a set drizzle-kit
// generated and drizzle-orm can apply one drops generated. The CLI
// drives it rather than reimplementing it; what lives here is only the
// bookkeeping the migrator does not expose — the rollback direction,
// which drizzle-kit has no concept of, and the hashes in the database
// that no file accounts for.

// migratorFor returns a migrator reading from dir.
func migratorFor(db *pg.DB, dir string) *pg.DrizzleMigrator {
	return pg.NewDrizzleMigrator(db, os.DirFS(dir), ".")
}

// requireMigrationDir fails with a remedy when dir holds no journal.
func requireMigrationDir(dir string) error {
	journal := dir + "/meta/_journal.json"
	if _, err := os.Stat(journal); err != nil {
		return fmt.Errorf("no migration journal at %s: run `drops generate --schema <pkg> --dir %s` first, or `drops baseline` to adopt a database that already has the tables", journal, dir)
	}
	return nil
}

// splitMigration breaks migration SQL at the statement-breakpoint
// delimiter drizzle-kit writes, dropping empty fragments.
func splitMigration(sql string) []string {
	var out []string
	for _, part := range strings.Split(sql, pg.StatementBreakpoint) {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// unaccountedHashes returns the migration hashes recorded in the
// database that no file in the journal produces.
//
// One of those means the history and the directory have diverged: a
// migration file was edited after it was applied, or it was applied
// from a branch this checkout does not have. Either way the next
// migration runs against a database that is not the one the snapshot
// describes, so it is worth a line of output.
func unaccountedHashes(ctx context.Context, db *pg.DB, m *pg.DrizzleMigrator) ([]string, error) {
	known := map[string]bool{}
	entries, err := m.LoadEntries()
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		known[e.Hash] = true
	}
	rows, err := db.Query(ctx, fmt.Sprintf("SELECT hash FROM %s.%s ORDER BY id",
		quoteIdent(pg.DrizzleSchema), quoteIdent(pg.DrizzleTable)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		if !known[h] {
			out = append(out, h)
		}
	}
	return out, rows.Err()
}

// forgetMigration removes one applied migration's row from the
// drizzle history table.
func forgetMigration(ctx context.Context, db *pg.DB, hash string) error {
	_, err := db.Exec(ctx, fmt.Sprintf("DELETE FROM %s.%s WHERE hash = $1",
		quoteIdent(pg.DrizzleSchema), quoteIdent(pg.DrizzleTable)), hash)
	return err
}

// recordMigration marks one migration as applied without running it —
// what baseline does to a database that already has the tables.
func recordMigration(ctx context.Context, db *pg.DB, hash string, when int64) error {
	_, err := db.Exec(ctx, fmt.Sprintf("INSERT INTO %s.%s (hash, created_at) VALUES ($1, $2)",
		quoteIdent(pg.DrizzleSchema), quoteIdent(pg.DrizzleTable)), hash, when)
	return err
}

// relativeDir renders a migration directory the way
// pg.GenerateMigration needs it.
//
// The generator reads the existing history through os.DirFS("."), and
// io/fs rejects both an absolute path and one containing "..", so the
// directory has to be expressed relative to the working directory the
// generated program runs in — which is this process's.
func relativeDir(dir string) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("--dir must name a migration directory")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(cwd, abs)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("--dir %s is outside %s: the migration directory has to be inside the directory drops runs in, because the generator reads the previous snapshot relative to it", dir, cwd)
	}
	return filepath.ToSlash(rel), nil
}

// requirePublicSchema refuses a --pg-schema the DDL side cannot honour.
//
// pg.Diff writes unqualified identifiers, so every statement it
// produces lands wherever search_path points — public, in practice.
// A command that introspects "reporting" and then applies SQL naming
// no schema at all would report success and put the tables somewhere
// else, and the run after it would wedge on "relation already
// exists". Refusing is the only answer that is not a lie; when Diff
// learns to qualify, this goes away.
func requirePublicSchema(command, schema string) error {
	if schema == "public" {
		return nil
	}
	return usagef("%s cannot target schema %q: the DDL drops generates carries no schema qualifier, so it lands wherever search_path points — public, unless you have changed it — and not in %[2]q; leave --pg-schema at public, or connect with a DSN whose search_path is already the schema you mean", command, schema)
}

// requireDSN reports a missing connection string as the command-line
// mistake it is, before a command does any other work.
func requireDSN(flagValue string) error {
	if _, err := resolveDSN(flagValue); err != nil {
		return usageError{err}
	}
	return nil
}

// quoteIdent quotes a single SQL identifier.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
