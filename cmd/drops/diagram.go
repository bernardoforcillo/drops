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
	Name        string                       `json:"name"`
	Schema      string                       `json:"schema"`
	Columns     map[string]*diagColumn       `json:"columns"`
	ForeignKeys map[string]*diagForeignKey   `json:"foreignKeys"`
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
	fmt.Fprintf(b, "  %s {\n", strings.ToUpper(t.Name))
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
		fmt.Fprintf(b, "    %s %s%s\n", c.Type, c.Name, marker)
	}
	b.WriteString("  }\n")
}

// writeRelations emits one edge per FOREIGN KEY found across the
// tables. Direction is parent ||--o{ child : <fk>; cardinality is
// kept simple — Mermaid renders the same shape regardless of
// composite vs single-column keys.
func writeRelations(b *strings.Builder, tables []*diagTable) {
	type edge struct{ line string }
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
				strings.ToUpper(fk.TableTo),
				strings.ToUpper(fk.TableFrom),
				fk.Name)
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
