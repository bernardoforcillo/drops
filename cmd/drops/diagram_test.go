package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureSnapshot = `{
  "tables": {
    "public.users": {
      "name": "users",
      "columns": {
        "id":    {"name": "id", "type": "bigserial", "primaryKey": true, "notNull": true},
        "email": {"name": "email", "type": "text",   "primaryKey": false, "notNull": true}
      },
      "foreignKeys": {}
    },
    "public.posts": {
      "name": "posts",
      "columns": {
        "id":     {"name": "id",     "type": "bigserial", "primaryKey": true, "notNull": true},
        "userId": {"name": "userId", "type": "bigint",    "primaryKey": false, "notNull": true},
        "title":  {"name": "title",  "type": "text",      "primaryKey": false, "notNull": true}
      },
      "foreignKeys": {
        "postsUserIdUsersIdFk": {
          "name": "postsUserIdUsersIdFk",
          "tableFrom": "posts",
          "columnsFrom": ["userId"],
          "tableTo": "users",
          "columnsTo": ["id"]
        }
      }
    }
  }
}`

func TestRenderMermaidStructure(t *testing.T) {
	tmp := t.TempDir()
	snap := filepath.Join(tmp, "snap.json")
	if err := os.WriteFile(snap, []byte(fixtureSnapshot), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := renderMermaid(snap)
	if err != nil {
		t.Fatalf("renderMermaid: %v", err)
	}
	got := string(out)
	for _, want := range []string{
		"erDiagram",
		"USERS {",
		"bigserial id PK",
		"POSTS {",
		"USERS ||--o{ POSTS : postsUserIdUsersIdFk",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestRenderMermaidRejectsBadJSON(t *testing.T) {
	tmp := t.TempDir()
	snap := filepath.Join(tmp, "bad.json")
	if err := os.WriteFile(snap, []byte("{ broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := renderMermaid(snap); err == nil {
		t.Error("expected parse error")
	}
}

func TestRenderMermaidMissingFile(t *testing.T) {
	if _, err := renderMermaid("/no/such/path/snap.json"); err == nil {
		t.Error("expected file error")
	}
}

func TestRunDiagramWritesOutput(t *testing.T) {
	tmp := t.TempDir()
	snap := filepath.Join(tmp, "snap.json")
	out := filepath.Join(tmp, "diagram.mmd")
	if err := os.WriteFile(snap, []byte(fixtureSnapshot), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runDiagram([]string{"--snapshot", snap, "--out", out}); err != nil {
		t.Fatalf("runDiagram: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !strings.Contains(string(b), "erDiagram") {
		t.Errorf("output missing erDiagram, got:\n%s", b)
	}
}

// Types straight out of pg.BuildSnapshot: pg.DoublePrecision writes
// "double precision" and pg.Numeric writes "numeric(10,2)". Mermaid
// reads an attribute line as `<type> <name> [key] [comment]`, so a
// space or a comma in the type silently turns one column into two
// tokens and the whole diagram stops rendering.
const spacedTypeSnapshot = `{
  "tables": {
    "public.orders": {
      "name": "orders",
      "columns": {
        "id":     {"name": "id",     "type": "bigserial",      "primaryKey": true},
        "amount": {"name": "amount", "type": "double precision"},
        "price":  {"name": "price",  "type": "numeric(10,2)"},
        "note":   {"name": "note",   "type": "timestamp with time zone"}
      },
      "foreignKeys": {}
    }
  }
}`

func TestRenderMermaidKeepsAttributesToOneWordEach(t *testing.T) {
	tmp := t.TempDir()
	snap := filepath.Join(tmp, "snap.json")
	if err := os.WriteFile(snap, []byte(spacedTypeSnapshot), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := renderMermaid(snap)
	if err != nil {
		t.Fatalf("renderMermaid: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "    ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[2] == "PK" {
			continue
		}
		if len(fields) != 2 {
			t.Errorf("attribute line is not `<type> <name>[ PK]`: %q", line)
		}
	}
	if !strings.Contains(string(out), "double_precision amount") {
		t.Errorf("expected the spaced type to collapse to one word:\n%s", out)
	}
}
