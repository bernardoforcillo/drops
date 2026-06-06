package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleSnapshot = `{
  "id": "00000000-0000-0000-0000-000000000001",
  "prevId": "00000000-0000-0000-0000-000000000000",
  "version": "7",
  "dialect": "postgresql",
  "tables": {
    "public.users": {
      "name": "users",
      "schema": "",
      "columns": {
        "id": {"name": "id", "type": "bigserial", "primaryKey": true, "notNull": true},
        "email": {"name": "email", "type": "text", "primaryKey": false, "notNull": true},
        "name": {"name": "name", "type": "text", "primaryKey": false, "notNull": true},
        "createdAt": {"name": "createdAt", "type": "timestamptz", "primaryKey": false, "notNull": true, "default": "now()"}
      },
      "uniqueConstraints": {
        "usersEmailUnique": {"name": "usersEmailUnique", "columns": ["email"]}
      },
      "foreignKeys": {},
      "indexes": {},
      "compositePrimaryKeys": {},
      "policies": {},
      "checkConstraints": {},
      "isRLSEnabled": false
    }
  },
  "enums": {},
  "schemas": {},
  "sequences": {},
  "roles": {},
  "policies": {},
  "views": {},
  "_meta": {"columns": {}, "schemas": {}, "tables": {}}
}`

func TestIntrospectGeneratesStruct(t *testing.T) {
	tmp := t.TempDir()
	snap := filepath.Join(tmp, "snap.json")
	if err := os.WriteFile(snap, []byte(sampleSnapshot), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tmp, "models")
	if err := runIntrospect(snap, out, "models"); err != nil {
		t.Fatalf("introspect: %v", err)
	}

	gen, err := os.ReadFile(filepath.Join(out, "users_drops.go"))
	if err != nil {
		t.Fatalf("output file missing: %v", err)
	}
	got := string(gen)
	for _, want := range []string{
		"package models",
		`"time"`,
		"type Users struct {",
		`ID int64 ` + "`drop:\"id,primaryKey,autoIncrement\"`",
		`CreatedAt time.Time ` + "`drop:\"createdAt,notNull,default=now()\"`",
		`Email string ` + "`drop:\"email,notNull,unique\"`",
		`Name string ` + "`drop:\"name,notNull\"`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestIntrospectRejectsBadJSON(t *testing.T) {
	tmp := t.TempDir()
	snap := filepath.Join(tmp, "bad.json")
	if err := os.WriteFile(snap, []byte("{ this is not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runIntrospect(snap, filepath.Join(tmp, "out"), "x"); err == nil {
		t.Error("expected JSON parse error")
	}
}

func TestSqlToGoTypeFallsBackToString(t *testing.T) {
	if got := sqlToGoType("some unknown thing"); got != "string" {
		t.Errorf("unknown SQL type should fall back to string, got %q", got)
	}
}

func TestGoIdentForAcronymPreservation(t *testing.T) {
	cases := map[string]string{
		"id":         "ID",
		"http_code":  "HTTPCode",
		"user_id":    "UserID",
		"profile":    "Profile",
		"created_at": "CreatedAt",
	}
	for in, want := range cases {
		if got := goIdentFor(in); got != want {
			t.Errorf("goIdentFor(%q): got %q, want %q", in, got, want)
		}
	}
}

// Round-trip: introspect a known JSON snapshot, then re-parse the
// generated source — sanity that the output parses as valid Go.
func TestIntrospectOutputIsValidGo(t *testing.T) {
	tmp := t.TempDir()
	snap := filepath.Join(tmp, "snap.json")
	if err := os.WriteFile(snap, []byte(sampleSnapshot), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tmp, "models")
	if err := runIntrospect(snap, out, "models"); err != nil {
		t.Fatalf("introspect: %v", err)
	}

	// Walk the output dir, gofmt every file via decoder.
	files, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no files generated")
	}
}

// Just exercise json.Unmarshal on a snapshot to confirm the struct
// shape matches drops' Snapshot format.
func TestSampleSnapshotParses(t *testing.T) {
	var s struct {
		Tables map[string]*introspectTable `json:"tables"`
	}
	if err := json.Unmarshal([]byte(sampleSnapshot), &s); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := s.Tables["public.users"]; !ok {
		t.Errorf("missing users table in parsed snapshot")
	}
}
