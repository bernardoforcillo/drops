package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// renderMermaid parses a drops snapshot JSON and emits a Mermaid
// erDiagram block. Self-contained — no DB connection, no Go AST.
// The JSON shape matches pg.Snapshot's PostgreSQL v7 format.
func renderMermaid(snapshotPath string) ([]byte, error) {
	raw, err := os.ReadFile(snapshotPath)
	if err != nil {
		return nil, err
	}
	var snap diagSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, fmt.Errorf("parse snapshot: %w", err)
	}

	tables := make([]*diagTable, 0, len(snap.Tables))
	for _, t := range snap.Tables {
		tables = append(tables, t)
	}
	sort.Slice(tables, func(i, j int) bool { return tables[i].Name < tables[j].Name })

	var b strings.Builder
	b.WriteString("erDiagram\n")
	for _, t := range tables {
		writeTable(&b, t)
	}
	writeRelations(&b, tables)
	return []byte(b.String()), nil
}

type diagSnapshot struct {
	Tables map[string]*diagTable `json:"tables"`
}

type diagTable struct {
	Name        string                     `json:"name"`
	Schema      string                     `json:"schema"`
	Columns     map[string]*diagColumn     `json:"columns"`
	ForeignKeys map[string]*diagForeignKey `json:"foreignKeys"`
}

type diagColumn struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	PrimaryKey bool   `json:"primaryKey"`
	NotNull    bool   `json:"notNull"`
}

type diagForeignKey struct {
	Name        string   `json:"name"`
	TableFrom   string   `json:"tableFrom"`
	ColumnsFrom []string `json:"columnsFrom"`
	TableTo     string   `json:"tableTo"`
	ColumnsTo   []string `json:"columnsTo"`
}

func writeTable(b *strings.Builder, t *diagTable) {
	fmt.Fprintf(b, "  %s {\n", strings.ToUpper(mermaidToken(t.Name)))
	cols := make([]*diagColumn, 0, len(t.Columns))
	for _, c := range t.Columns {
		cols = append(cols, c)
	}
	sort.Slice(cols, func(i, j int) bool {
		if cols[i].PrimaryKey != cols[j].PrimaryKey {
			return cols[i].PrimaryKey
		}
		return cols[i].Name < cols[j].Name
	})
	for _, c := range cols {
		marker := ""
		if c.PrimaryKey {
			marker = " PK"
		}
		fmt.Fprintf(b, "    %s %s%s\n", mermaidToken(c.Type), mermaidToken(c.Name), marker)
	}
	b.WriteString("  }\n")
}

// writeRelations emits one edge per FOREIGN KEY found across the
// tables. Direction is parent ||--o{ child : <fk>; cardinality is
// kept simple — Mermaid renders the same shape regardless of
// composite vs single-column keys.
func writeRelations(b *strings.Builder, tables []*diagTable) {
	var lines []string
	seen := map[string]bool{}
	for _, t := range tables {
		fkKeys := make([]string, 0, len(t.ForeignKeys))
		for k := range t.ForeignKeys {
			fkKeys = append(fkKeys, k)
		}
		sort.Strings(fkKeys)
		for _, k := range fkKeys {
			fk := t.ForeignKeys[k]
			line := fmt.Sprintf("  %s ||--o{ %s : %s",
				strings.ToUpper(mermaidToken(fk.TableTo)),
				strings.ToUpper(mermaidToken(fk.TableFrom)),
				mermaidToken(fk.Name))
			if !seen[line] {
				seen[line] = true
				lines = append(lines, line)
			}
		}
	}
	sort.Strings(lines)
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
}

// mermaidToken makes s safe to drop into a Mermaid erDiagram, where
// entity names, attribute names and attribute types are all single
// words. PostgreSQL type names are not: "double precision" and
// "numeric(10,2)" are what pg.DoublePrecision and pg.Numeric put in a
// snapshot, and either one turns the attribute line into three tokens
// that Mermaid refuses to parse. Everything outside the word grammar
// collapses to a single underscore.
func mermaidToken(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevFill := false
	for _, r := range s {
		if mermaidWordRune(r) {
			b.WriteRune(r)
			prevFill = false
			continue
		}
		if !prevFill {
			b.WriteByte('_')
			prevFill = true
		}
	}
	return b.String()
}

func mermaidWordRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	switch r {
	case '-', '_', '[', ']', '(', ')':
		return true
	}
	return false
}

// runDiagram renders a snapshot as a Mermaid ER diagram. It is the one
// command that needs neither a database nor a Go schema: the snapshot
// on disk is the whole input.
func runDiagram(args []string) error {
	fs := newFlagSet("diagram", "emit a Mermaid ER diagram from a snapshot JSON")
	snapshot := fs.String("snapshot", "", "path to a drops snapshot JSON, such as drizzle/meta/0000_snapshot.json (required)")
	out := fs.String("out", "", "file to write; stdout when empty")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *snapshot == "" {
		fs.Usage()
		return usagef("--snapshot is required")
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
