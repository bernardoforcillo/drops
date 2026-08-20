package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	var (
		file     = flag.String("file", "", "Go source file to scan for entities (bind/scan mode)")
		schema   = flag.String("schema", "", "Go source file to scan for //drops:schema structs (schema mode)")
		outName  = flag.String("o", "", "output file (bind/scan mode; default: <input>_drops_gen.go)")
		rows     = flag.String("rows", "", "Go package declaring a Schema() to generate row / insert structs from")
		snapshot = flag.String("snapshot", "", "drops snapshot JSON to introspect into Go structs")
		sqlDir   = flag.String("sql", "", "directory of .sql files to compile into typed Go funcs")
		outDir   = flag.String("out", "models", "output directory (introspect / sql modes)")
		pkg      = flag.String("pkg", "", "Go package name for generated files (default: dir basename)")
	)
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "dropsgen — generate schema declarations, row structs, bind/scan helpers, or typed queries")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Bind/scan mode:")
		fmt.Fprintln(os.Stderr, "  dropsgen -file users.go [-o out.go]")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Introspect mode:")
		fmt.Fprintln(os.Stderr, "  dropsgen -snapshot meta/0001_snapshot.json -out models/ [-pkg models]")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Schema mode (typed table + column declarations from a struct):")
		fmt.Fprintln(os.Stderr, "  dropsgen -schema users.go [-o out.go]")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Rows mode (row + insert structs from the table declaration):")
		fmt.Fprintln(os.Stderr, "  dropsgen -rows ./db/schema [-o rows.go]")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "SQL mode (typed sqlc-style queries):")
		fmt.Fprintln(os.Stderr, "  dropsgen -sql queries/ -out queries/ [-pkg queries]")
		flag.PrintDefaults()
	}
	flag.Parse()

	switch {
	case *schema != "":
		if err := runSchema(*schema, *outName); err != nil {
			fmt.Fprintf(os.Stderr, "dropsgen: %v\n", err)
			os.Exit(1)
		}
	case *rows != "":
		if err := runRows(*rows, *outName); err != nil {
			fmt.Fprintf(os.Stderr, "dropsgen: %v\n", err)
			os.Exit(1)
		}
	case *sqlDir != "":
		if err := runSQL(*sqlDir, *outDir, *pkg); err != nil {
			fmt.Fprintf(os.Stderr, "dropsgen: %v\n", err)
			os.Exit(1)
		}
	case *snapshot != "":
		if err := runIntrospect(*snapshot, *outDir, *pkg); err != nil {
			fmt.Fprintf(os.Stderr, "dropsgen: %v\n", err)
			os.Exit(1)
		}
	case *file != "":
		if err := run(*file, *outName); err != nil {
			fmt.Fprintf(os.Stderr, "dropsgen: %v\n", err)
			os.Exit(1)
		}
	default:
		flag.Usage()
		os.Exit(2)
	}
}

func run(in, out string) error {
	entities, pkg, err := parseFile(in)
	if err != nil {
		return err
	}
	if len(entities) == 0 {
		return fmt.Errorf("no `//drops:entity` directives found in %s", in)
	}
	src, err := emit(pkg, entities)
	if err != nil {
		return err
	}
	if out == "" {
		dir := filepath.Dir(in)
		base := strings.TrimSuffix(filepath.Base(in), ".go")
		out = filepath.Join(dir, base+"_drops_gen.go")
	}
	if err := os.WriteFile(out, src, 0o644); err != nil {
		return err
	}
	return nil
}
