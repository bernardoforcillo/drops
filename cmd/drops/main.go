package main

import (
	"flag"
	"fmt"
	"os"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "diagram":
		if err := runDiagram(args); err != nil {
			fail(err)
		}
	case "version", "--version", "-v":
		fmt.Println("drops", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "drops: unknown subcommand %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "drops — schema toolkit for the drops Go ORM")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  drops <command> [flags]")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Commands:")
	fmt.Fprintln(os.Stderr, "  diagram   Emit a Mermaid ER diagram from a snapshot JSON")
	fmt.Fprintln(os.Stderr, "  version   Print the toolkit version")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Codegen helpers (bind/scan, struct introspection) live in")
	fmt.Fprintln(os.Stderr, "the sibling `dropsgen` binary.")
}

func runDiagram(args []string) error {
	fs := flag.NewFlagSet("diagram", flag.ExitOnError)
	snapshot := fs.String("snapshot", "", "path to a drops snapshot JSON (required)")
	out := fs.String("out", "", "output file (default: stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *snapshot == "" {
		fs.Usage()
		return fmt.Errorf("--snapshot is required")
	}
	src, err := renderMermaid(*snapshot)
	if err != nil {
		return err
	}
	if *out == "" {
		_, err = os.Stdout.Write(src)
		return err
	}
	return os.WriteFile(*out, src, 0o644)
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "drops: %v\n", err)
	os.Exit(1)
}
