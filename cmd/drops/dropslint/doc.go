// Package dropslint holds the static checks behind "drops lint".
//
// The rules exist because the same three mistakes keep reaching
// production, and every one of them is visible in the source: a DELETE
// that forgot its WHERE, a read of a whole table, a relation eager-
// loaded once per iteration of a loop. drops already answers all three
// at run time — [pg.AnalyzeMigration] for the migration, [pg.Budget]
// for the row count, [pg.WithN1Detector] for the loop — and every one
// of those answers arrives after the query has run. These are the same
// questions asked of the source instead.
//
// # Why go/analysis
//
// The equivalent ESLint plugin has to guess from a method name: any
// object with a .delete() and no .where() looks the same to it. A
// go/analysis pass is handed the type checker's answers, so it knows
// the value is a *pg.DeleteBuilder and not a *os.File, and it knows
// which package-level *pg.Table the statement targets. It can then
// carry what it learned about that table across package boundaries as
// an analysis fact.
//
// # The rules
//
//   - unfilteredwrite — a DELETE or UPDATE executed with nothing to
//     bound it: no Where, and no Limit where the dialect offers one on
//     a write.
//   - unboundedread — an entity read of a whole table: no Where, no
//     Limit, and no Budget.MaxRows to cap it.
//   - loopload — a query that eager-loads a relation and executes
//     inside a loop, which is an N+1 by construction.
//
// Each is a separate [analysis.Analyzer], so a caller can run one
// without the others; [All] returns them together.
//
// # How far a value is followed
//
// All three rules follow a builder within one function body and no
// further. Inside that body the analysis is flow-insensitive: a Where
// called anywhere on the value counts, even under an if the value may
// not take. That is deliberately the quiet direction — it will miss
//
//	q := db.Delete(Users)
//	if narrow {
//	    q = q.Where(pred)
//	}
//	q.Exec(ctx)
//
// which is a real bug, in exchange for never mis-reading the same
// shape written correctly. The rules also go silent the moment a
// builder leaves the function — returned, passed to a call, stored in
// a struct, aliased to a second variable — because what happens to it
// there is not visible here. A helper that returns a half-built
// *pg.DeleteBuilder is invisible to unfilteredwrite, and that is the
// documented boundary rather than an oversight.
//
// A statement is only ever reported where it is *executed*: Exec, All,
// One, Stream. Rendering one with ToSQL is not running it, so the
// dialect packages' own tests — thousands of ToSQL calls on unfiltered
// deletes — are not findings.
//
// # Comment directives
//
// A deliberate full-table write is a real thing; it needs to say so:
//
//	//drops:lint ignore unfilteredwrite
//	db.Delete(Sessions).Exec(ctx)
//
// The directive is honoured on the reported line, on the line above
// it, and in the enclosing function's doc comment. Naming no rule
//
//	//drops:lint ignore
//
// silences every rule at that position. The alternative — a method on
// the builder — was considered and rejected: it would put a linter's
// vocabulary into the runtime API of four dialect packages, and a
// value that already exists (a comment) reads the same to a human.
//
// One directive is not a suppression. On a table's declaration
//
//	//drops:lint lookup
//	var Currencies = pg.NewTable("currencies")
//
// records that the table is a small lookup table, so reading all of it
// is fine. It travels as an analysis fact, so packages that import the
// schema see it too.
package dropslint
