// drops is the front end to the declarative migration loop: declare
// the schema in Go, diff it against the database, apply the
// difference, and notice when the two have parted company.
//
//	drops generate   Diff the Go schema against the last snapshot and write a migration
//	drops migrate    Apply pending migrations; "drops migrate down" rolls the last one back
//	drops push       Diff the Go schema against the live database and apply it directly
//	drops drift      Report where the live database and the Go schema disagree
//	drops pull       Introspect a live database into a Go schema file
//	drops baseline   Adopt an existing database: snapshot it, mark the history applied
//	drops status     Show what is applied, what is pending and what is unaccounted for
//	drops diagram    Emit a Mermaid ER diagram from a snapshot JSON
//	drops version    Print the toolkit version
//
// Install: go install github.com/bernardoforcillo/drops/cmd/drops@latest
//
// # Reading a Go schema
//
// A schema built from pg.NewTable and pg.Add is a Go value, and the
// only thing that can evaluate one is Go. So the commands that need a
// schema — generate, push, drift — write a small program into the
// module that declares it, run it with `go run`, and read the answer
// back as JSON. See schema.go: the convention is a single exported
// function,
//
//	func Schema() *pg.Schema
//
// and a package without one is reported as such, with the function to
// add. Those three commands therefore need a Go toolchain and have to
// be run from inside the module, as does status when it is given
// --schema. The rest — migrate, baseline, pull, diagram, and status
// without --schema — read the database and the migration directory
// only, and run anywhere.
//
// # Connecting
//
// --dsn, else $DROPS_PG_DSN, else $DATABASE_URL. The binary speaks the
// PostgreSQL v3 wire protocol itself (cmd/drops/pgwire) because drops
// has no dependencies and so cannot link pgx.
//
// # Exit codes
//
//	0  success
//	1  failure
//	2  the command line was wrong
//	3  the command ran and the answer was no — drift found, or a
//	   destructive change refused
//
// Every command that applies anything routes its statements through
// pg.AnalyzeMigration first and refuses the destructive ones unless
// --allow-destructive is passed, printing what it refused.
//
// Codegen helpers (bind/scan, struct introspection) live in the
// sibling `dropsgen` binary.
package main
