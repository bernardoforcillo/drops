package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"strings"
)

// Renaming, on the command line.
//
// A diff cannot tell a rename from a drop and an add — see
// drops/pg/rename.go — so `generate` stops, and so does `push`, which
// reaches the same comparison against a live database. This file is
// what both stop with: four flags that state the answer up front, and,
// behind --interactive, a prompt that asks about each pair instead.
//
// `push` has a fifth answer these flags cannot carry, and it is the
// better one: RenamedFrom on the column or table in the schema, which
// outlives the terminal a flag is typed into. See pg/push.go.
//
// Prompting is opt-in rather than something drops works out for
// itself. It could ask whether stdin looks like a terminal, and the
// portable version of that question — is this a character device —
// says yes to /dev/null, which is what a program with no stdin at all
// is given. Getting that wrong in the direction of "somebody is there"
// means a scripted run reads EOF, and an EOF is not an answer. A flag
// is the same information without the guess.
//
// The flags matter more than the prompt anyway. CI has no terminal,
// and the answer drops would have to invent without one is the DROP
// COLUMN that loses the column. So a scripted run states its answers
// on the command line, once; drops writes them into the migration
// directory, and every run after that finds them there. A push has no
// such directory, which is the whole reason its lasting answer belongs
// in the schema rather than here.

// repeatedFlag is a string flag that can be given more than once.
type repeatedFlag []string

func (r *repeatedFlag) String() string { return strings.Join(*r, ",") }

func (r *repeatedFlag) Set(v string) error {
	*r = append(*r, v)
	return nil
}

// renameAnswers holds the rename decisions a command line carries.
type renameAnswers struct {
	renameColumns repeatedFlag
	renameTables  repeatedFlag
	dropColumns   repeatedFlag
	dropTables    repeatedFlag
}

// register attaches the four flags to a command's flag set.
func (a *renameAnswers) register(fs *flag.FlagSet) {
	fs.Var(&a.renameColumns, "rename-column",
		"answer a rename question: table.oldName=newName (repeatable)")
	fs.Var(&a.renameTables, "rename-table",
		"answer a rename question: oldName=newName (repeatable)")
	fs.Var(&a.dropColumns, "drop-column",
		"answer that a column really is being dropped: table.name (repeatable)")
	fs.Var(&a.dropTables, "drop-table",
		"answer that a table really is being dropped: name (repeatable)")
}

// decisions turns the flags into the answers the bridge carries.
func (a *renameAnswers) decisions() ([]bridgeRename, error) {
	var out []bridgeRename
	for _, spec := range a.renameColumns {
		lhs, rhs, ok := strings.Cut(spec, "=")
		if !ok {
			return nil, usagef("--rename-column %s: expected table.oldName=newName", spec)
		}
		table, from, ok := strings.Cut(lhs, ".")
		if !ok || table == "" || from == "" || rhs == "" {
			return nil, usagef("--rename-column %s: expected table.oldName=newName", spec)
		}
		out = append(out, bridgeRename{Kind: "column", Table: table, From: from, To: rhs, IsRename: true})
	}
	for _, spec := range a.renameTables {
		from, to, ok := strings.Cut(spec, "=")
		if !ok || from == "" || to == "" {
			return nil, usagef("--rename-table %s: expected oldName=newName", spec)
		}
		out = append(out, bridgeRename{Kind: "table", From: from, To: to, IsRename: true})
	}
	// A refusal names only the object that is going. What the diff
	// thought it might have become is drops's question, not the
	// operator's, so answering it does not require repeating it.
	for _, spec := range a.dropColumns {
		table, name, ok := strings.Cut(spec, ".")
		if !ok || table == "" || name == "" {
			return nil, usagef("--drop-column %s: expected table.name", spec)
		}
		out = append(out, bridgeRename{Kind: "column", Table: table, From: name})
	}
	for _, spec := range a.dropTables {
		if spec == "" {
			return nil, usagef("--drop-table: expected a table name")
		}
		out = append(out, bridgeRename{Kind: "table", From: spec})
	}
	return out, nil
}

// interactiveHint is added to a refusal, because a person reading one
// at a terminal wants the third option and the library that wrote the
// message does not know the CLI has one.
const interactiveHint = "\nor re-run with --interactive to be asked about each one"

// promptRenames asks about each candidate in turn and returns the
// answers. Anything that is not a yes is a no — the safe reading of a
// stray keystroke is "leave the column alone and drop it", which the
// migration then says out loud in its DROP COLUMN, rather than a rename
// nobody asked for.
//
// The reader is the caller's, not one made here, because answering can
// raise a second round of questions: a bufio.Reader built per round
// would read ahead into its own buffer and throw away the answers meant
// for the round after it.
func promptRenames(reader *bufio.Reader, out io.Writer, candidates []bridgeCandidate) ([]bridgeRename, error) {
	answers := make([]bridgeRename, 0, len(candidates))
	claimed := map[string]bool{}
	for _, c := range candidates {
		// An earlier yes already accounted for one of these names.
		if claimed[c.Kind+"\x00"+c.Table+"\x00"+c.From] || claimed[c.Kind+"\x00"+c.Table+"\x00"+c.To] {
			continue
		}
		if c.Kind == "column" {
			fmt.Fprintf(out, "is %s.%s a rename of %s.%s? [y/N] ", c.Table, c.To, c.Table, c.From)
		} else {
			fmt.Fprintf(out, "is table %s a rename of %s? [y/N] ", c.To, c.From)
		}
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			return nil, fmt.Errorf("reading the answer: %w", err)
		}
		yes := false
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			yes = true
		}
		answer := bridgeRename{Kind: c.Kind, Table: c.Table, From: c.From, To: c.To, IsRename: yes}
		if !yes {
			// Recorded against the old name alone, so the same answer
			// covers every pair the diff offered for it.
			answer.To = ""
		} else {
			claimed[c.Kind+"\x00"+c.Table+"\x00"+c.From] = true
			claimed[c.Kind+"\x00"+c.Table+"\x00"+c.To] = true
		}
		answers = append(answers, answer)
	}
	return answers, nil
}
