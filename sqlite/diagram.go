package sqlite

import (
	"fmt"
	"sort"
	"strings"
)

// MermaidDiagram renders the schema as a Mermaid `erDiagram` block, driven
// directly by the table declarations and their registered relations.
//
//	fmt.Println(sqlite.MermaidDiagram(sqlite.NewSchema(Users, Posts)))
//	// erDiagram
//	//   USERS {
//	//     INTEGER id PK
//	//     TEXT name
//	//   }
//	//   USERS ||--o{ POSTS : posts
//
// Cardinality marks come from the Relation metadata:
//
//	HasMany     →  ||--o{
//	HasOne      →  ||--o|
//	ManyToMany  →  }o--o{   (junction omitted for clarity)
//	BelongsTo   →  rendered from the parent side as the inverse
func MermaidDiagram(s *Schema) string {
	if s == nil {
		return ""
	}
	tables := s.sortedTables()

	var b strings.Builder
	b.WriteString("erDiagram\n")
	for _, t := range tables {
		writeMermaidTable(&b, t)
	}
	writeMermaidRelations(&b, tables)
	return b.String()
}

func writeMermaidTable(b *strings.Builder, t *Table) {
	fmt.Fprintf(b, "  %s {\n", mermaidLabel(t.Name()))
	for _, c := range t.Columns() {
		marker := ""
		switch {
		case c.IsPrimaryKey():
			marker = " PK"
		case c.ForeignKey() != nil:
			marker = " FK"
		}
		fmt.Fprintf(b, "    %s %s%s\n", c.Type().TypeSQL(), c.Name(), marker)
	}
	b.WriteString("  }\n")
}

// writeMermaidRelations emits one edge per declared Relation, rendering
// only from the owning side (HasMany / HasOne / ManyToMany) to avoid
// duplicating the inverse.
func writeMermaidRelations(b *strings.Builder, tables []*Table) {
	var lines []string
	seen := map[string]bool{}
	emit := func(s string) {
		if seen[s] {
			return
		}
		seen[s] = true
		lines = append(lines, s)
	}

	for _, t := range tables {
		for _, name := range relationNamesOf(t) {
			rel := t.Relation(name)
			if rel == nil {
				continue
			}
			switch rel.Kind {
			case HasMany:
				emit(fmt.Sprintf("  %s ||--o{ %s : %s",
					mermaidLabel(t.Name()), mermaidLabel(rel.Target.Name()), name))
			case HasOne:
				emit(fmt.Sprintf("  %s ||--o| %s : %s",
					mermaidLabel(t.Name()), mermaidLabel(rel.Target.Name()), name))
			case ManyToMany:
				emit(fmt.Sprintf("  %s }o--o{ %s : %s",
					mermaidLabel(t.Name()), mermaidLabel(rel.Target.Name()), name))
			case BelongsTo:
				// rendered from the parent side; skip here.
			}
		}
	}
	sort.Strings(lines)
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
}

// relationNamesOf returns the declared relation names on t in stable
// order.
func relationNamesOf(t *Table) []string {
	out := make([]string, 0, len(t.relations))
	for k := range t.relations {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// mermaidLabel upper-cases the identifier for Mermaid entity labels.
func mermaidLabel(name string) string { return strings.ToUpper(name) }
