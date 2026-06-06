// dropsgen generates zero-reflection bind / scan helpers for structs
// tagged as drops entities.
//
// Usage:
//
//	go install github.com/bernardoforcillo/drops/cmd/dropsgen@latest
//
//	//go:generate dropsgen -file users.go
//
// dropsgen scans the input file for struct types whose doc comment
// contains a `//drops:entity` directive. For each match it emits
// `<input>_drops_gen.go` next to the source, containing:
//
//   - bind<Type>(*Type) []any — extracts field values in declared
//     column order, ready to pass to an INSERT or UPDATE.
//   - scan<Type>(drops.Rows, *Type) error — scans the next row into
//     the struct without reflection.
//   - cols<Type>() []string — the column names in the order bind /
//     scan use.
//
// The directive accepts a "table" key naming the *pg.Table variable
// the entity is bound to. The generator does not resolve column
// metadata at parse time — it derives it from struct tags (`drop:`).
// At runtime the generated code references the table variable so
// imports and identifiers stay correct.
//
//	//drops:entity table=Users
//	type User struct {
//	    ID    int64  `drop:"id"`
//	    Name  string `drop:"name"`
//	    Email string `drop:"email"`
//	}
//
// Polymorphism, relations, and validation are intentionally out of
// scope for the first iteration — the generator focuses on the hot
// row-binding path that benefits most from skipping reflection.
package main
