// drops is the schema toolkit CLI. It complements cmd/dropsgen
// (codegen) with operations on the schema snapshot itself —
// rendering diagrams, comparing snapshots, exporting summaries.
//
// Subcommands:
//
//	drops diagram   Emit a Mermaid ER diagram from a snapshot JSON
//	drops version   Print the toolkit version
//
// Install: go install github.com/bernardoforcillo/drops/cmd/drops@latest
//
// All subcommands read drops snapshot files (the JSON format used by
// pg.Snapshot and the migration meta directory), so the CLI works
// fully offline — no DB connection required.
package main
