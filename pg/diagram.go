package pg

import (
	"fmt"
	"sort"
	"strings"
)

// MermaidDiagram renders the schema as a Mermaid `erDiagram` block.
// Pipe it through any Mermaid-aware renderer (web preview tools,
// Markdown viewers, IDE previews) for an up-to-date ER diagram
// driven directly by the table declarations.
//
//	fmt.Println(pg.MermaidDiagram(pg.NewSchema(Users, Posts)))
//	// erDiagram
//	//   USERS {
//	//     bigserial id PK
//	//     text name
//	//   }
//	//   POSTS {
//	//     bigserial id PK
//	//     bigint userId FK
//	//   }
//	//   USERS ||--o{ POSTS : posts
//
// Cardinality marks come from the registered Relation metadata:
//
//	HasMany     →  ||--o{
//	HasOne      →  ||--o|
//	BelongsTo   →  rendered from the parent side as the inverse
//	ManyToMany  →  }o--o{   (with the junction omitted for clarity)
//	MorphTo     →  rendered as one edge per registered morph entry
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

// writeMermaidRelations emits one edge per declared Relation. Avoids
// duplicating the inverse direction by only rendering from the
// owning side (HasMany / HasOne / ManyToMany / MorphMany).
func writeMermaidRelations(b *strings.Builder, tables []*Table) {
	type edge struct{ line string }
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
		// relations is unexported; access via Relation lookups by
		// iterating known names.
		for _, name := range relationNamesOf(t) {
			rel := t.Relation(name)
			if rel == nil {
				continue
			}
			switch rel.Kind {
			case HasManyKind, MorphManyKind:
				emit(fmt.Sprintf("  %s ||--o{ %s : %s",
					mermaidLabel(t.Name()), mermaidLabel(rel.To.Name()), name))
			case HasOneKind:
				emit(fmt.Sprintf("  %s ||--o| %s : %s",
					mermaidLabel(t.Name()), mermaidLabel(rel.To.Name()), name))
			case ManyToManyKind:
				emit(fmt.Sprintf("  %s }o--o{ %s : %s",
					mermaidLabel(t.Name()), mermaidLabel(rel.To.Name()), name))
			case MorphToKind:
				if rel.MorphMap != nil {
					for value, ent := range rel.MorphMap.entries {
						emit(fmt.Sprintf("  %s }o--|| %s : %s_%s",
							mermaidLabel(t.Name()), mermaidLabel(ent.table.Name()),
							name, value))
					}
				}
			case BelongsToKind:
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
// order. The Table's relations map is unexported so we expose this
// helper for the diagram renderer (and for future tooling).
func relationNamesOf(t *Table) []string {
	out := make([]string, 0, len(t.relations))
	for k := range t.relations {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// mermaidLabel returns the upper-cased identifier Mermaid prefers
// for entity labels. Keeps the original column casing intact —
// Mermaid renders the body verbatim.
func mermaidLabel(name string) string {
	return strings.ToUpper(name)
}
