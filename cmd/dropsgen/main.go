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
		file    = flag.String("file", "", "Go source file to scan for entities")
		outName = flag.String("o", "", "output file (default: <input>_drops_gen.go)")
	)
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "dropsgen — generate zero-reflection bind/scan helpers for drops entities")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Usage:")
		fmt.Fprintln(os.Stderr, "  dropsgen -file <path>")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *file == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*file, *outName); err != nil {
		fmt.Fprintf(os.Stderr, "dropsgen: %v\n", err)
		os.Exit(1)
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
	if err := os.WriteFile(out, src, 0644); err != nil {
		return err
	}
	return nil
}
