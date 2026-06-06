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
