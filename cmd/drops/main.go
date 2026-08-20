package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
)

const version = "0.6.0"

// Exit codes. A CLI is scripted against, so they are part of its
// interface: 0 success, 1 failure, 2 the command line was wrong, 3 the
// command ran and the answer was "no" — drift found, changes refused.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
	exitFinding = 3
)

// usageError marks a failure caused by the command line rather than by
// the work, so main can exit 2 and print the flags.
type usageError struct{ err error }

func (u usageError) Error() string { return u.err.Error() }
func (u usageError) Unwrap() error { return u.err }

func usagef(format string, args ...any) error {
	return usageError{fmt.Errorf(format, args...)}
}

// findingError marks a successful run whose answer is negative: drift
// was detected, or a destructive statement was refused.
type findingError struct{ err error }

func (f findingError) Error() string { return f.err.Error() }
func (f findingError) Unwrap() error { return f.err }

func main() {
	if err := run(); err != nil {
		fail(err)
	}
}

// run does the work and returns; main is the only place that exits,
// so a deferred cleanup always gets to run first.
func run() error {
	if len(os.Args) < 2 {
		usage()
		return usageError{errFlagsReported}
	}
	// Ctrl-C has to reach the server: a cancelled context closes the
	// socket, which rolls back a migration in flight rather than
	// leaving it half-applied with no one waiting on it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "generate":
		return runGenerate(ctx, args)
	case "migrate":
		return runMigrate(ctx, args)
	case "push":
		return runPush(ctx, args)
	case "drift":
		return runDrift(ctx, args)
	case "pull":
		return runPull(ctx, args)
	case "baseline":
		return runBaseline(ctx, args)
	case "status":
		return runStatus(ctx, args)
	case "diagram":
		return runDiagram(args)
	case "lint":
		return runLint(ctx, args)
	case "version", "--version", "-v":
		fmt.Println("drops", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "drops: unknown subcommand %q\n\n", cmd)
		usage()
		return usageError{errFlagsReported}
	}
	return nil
}

func usage() {
	fmt.Fprintln(os.Stderr, `drops — declare the schema in Go, migrate the database to match

Usage:
  drops <command> [flags]

Schema commands (need --schema, a Go package that exports
"func Schema() *pg.Schema"):
  generate   Diff the Go schema against the last snapshot and write a migration
  push       Diff the Go schema against the live database and apply it directly
  drift      Report where the live database and the Go schema disagree

Database commands (need --dsn, or DROPS_PG_DSN / DATABASE_URL):
  migrate    Apply pending migrations; "drops migrate down" rolls the last one back
  status     Show what is applied, what is pending and what is unaccounted for
  baseline   Adopt an existing database: snapshot it, mark the history applied
  pull       Introspect a live database into a Go schema file

Offline:
  lint       Report query mistakes the type checker can see: "drops lint ./..."
  diagram    Emit a Mermaid ER diagram from a snapshot JSON
  version    Print the toolkit version

Exit codes:
  0 success   1 failure   2 bad command line   3 drift found, or changes refused

Run "drops <command> -h" for a command's flags.`)
}

func fail(err error) {
	if errors.Is(err, errFlagsReported) {
		os.Exit(exitUsage)
	}
	var u usageError
	if errors.As(err, &u) {
		fmt.Fprintf(os.Stderr, "drops: %v\n", err)
		os.Exit(exitUsage)
	}
	var f findingError
	if errors.As(err, &f) {
		fmt.Fprintf(os.Stderr, "drops: %v\n", err)
		os.Exit(exitFinding)
	}
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(exitUsage)
	}
	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "drops: interrupted")
		os.Exit(exitFailure)
	}
	fmt.Fprintf(os.Stderr, "drops: %v\n", err)
	os.Exit(exitFailure)
}

// newFlagSet returns a flag set that reports an error instead of
// calling os.Exit, so a missing flag prints the command's own usage
// and returns exit code 2 like every other command-line mistake.
func newFlagSet(name, summary string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "drops %s — %s\n\nFlags:\n", name, summary)
		fs.PrintDefaults()
	}
	return fs
}

// errFlagsReported stands in for a flag error the flag package has
// already printed, together with the command's usage. Returning the
// error itself would print the same line twice.
var errFlagsReported = errors.New("the command line was rejected")

// parseFlags parses args, mapping any failure to a usage error.
func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usageError{errFlagsReported}
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return usagef("unexpected argument %q", fs.Arg(0))
	}
	return nil
}
