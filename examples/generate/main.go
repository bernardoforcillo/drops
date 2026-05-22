// generate demonstrates drops's drizzle-kit-compatible migration
// generator. It builds two snapshots of a Go schema (an initial one
// and one after a column is added) and prints the SQL drops emits for
// each — exactly the bytes that would be written to disk as part of a
// real drizzle/ directory.
//
// Run with:
//
//	go run ./examples/generate
package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing/fstest"

	"github.com/bernardoforcillo/drops/pg"
)

// Initial schema.
var (
	Users    = pg.NewTable("users")
	UserID   = pg.Add(Users, pg.BigSerial("id").PrimaryKey())
	UserName = pg.Add(Users, pg.Text("name").NotNull())
	UserMail = pg.Add(Users, pg.Text("email").NotNull().Unique())

	Posts      = pg.NewTable("posts")
	PostID     = pg.Add(Posts, pg.BigSerial("id").PrimaryKey())
	PostUserID = pg.Add(Posts, pg.BigInt("user_id").NotNull().References(UserID, pg.OnDelete("CASCADE")))
	PostTitle  = pg.Add(Posts, pg.Text("title").NotNull())
)

func main() {
	// First migration — schema goes from empty to the declared tables.
	files := captureWrites{}
	mig1, err := pg.GenerateMigration(pg.GenerateOptions{
		Schema: pg.NewSchema(Users, Posts),
		Dir:    "drizzle",
		Name:   "init",
		FS:     fstest.MapFS{},
		Write:  files.write,
		Now:    func() int64 { return 1700000000000 },
	})
	if err != nil {
		panic(err)
	}
	report(mig1, files)

	// Second migration — pretend the developer adds an age column and a
	// new column on posts, then runs generate again. We seed the FS with
	// what the first run wrote so the diff is computed against it.
	mig1FS := files.asFS("drizzle")

	pg.Add(Users, pg.Integer("age"))
	pg.Add(Posts, pg.Timestamp("published_at", true).Default("now()"))

	files2 := captureWrites{}
	mig2, err := pg.GenerateMigration(pg.GenerateOptions{
		Schema: pg.NewSchema(Users, Posts),
		Dir:    "drizzle",
		Name:   "add_age_and_published",
		FS:     mig1FS,
		Write:  files2.write,
		Now:    func() int64 { return 1700000100000 },
	})
	if err != nil {
		panic(err)
	}
	report(mig2, files2)

	// Third "migration" — no schema change, should be a no-op.
	mig3FS := merge(mig1FS, files2.asFS("drizzle"))
	files3 := captureWrites{}
	mig3, err := pg.GenerateMigration(pg.GenerateOptions{
		Schema: pg.NewSchema(Users, Posts),
		Dir:    "drizzle",
		FS:     mig3FS,
		Write:  files3.write,
	})
	if err != nil {
		panic(err)
	}
	if mig3.NoOp {
		fmt.Println(strings.Repeat("=", 60))
		fmt.Println("Third run: no schema change → NoOp, nothing written.")
	}
}

// captureWrites is a minimal in-memory sink for GenerateOptions.Write.
type captureWrites map[string][]byte

func (c captureWrites) write(rel string, data []byte) error {
	c[rel] = append([]byte(nil), data...)
	return nil
}

func (c captureWrites) asFS(dir string) fstest.MapFS {
	out := fstest.MapFS{}
	for rel, data := range c {
		out[dir+"/"+rel] = &fstest.MapFile{Data: data}
	}
	return out
}

func merge(a, b fstest.MapFS) fstest.MapFS {
	out := fstest.MapFS{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func report(r *pg.GenerateResult, files captureWrites) {
	fmt.Println(strings.Repeat("=", 60))
	fmt.Printf("Migration: %s (idx %d)\n", r.Tag, r.Idx)
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println()
	fmt.Println("--- " + r.Tag + ".sql ---")
	fmt.Println(string(files[r.Tag+".sql"]))
	fmt.Println("--- meta/_journal.json ---")
	fmt.Println(string(files["meta/_journal.json"]))
	fmt.Printf("--- meta/%04d_snapshot.json (truncated) ---\n", r.Idx)
	var compact any
	_ = json.Unmarshal(files[fmt.Sprintf("meta/%04d_snapshot.json", r.Idx)], &compact)
	out, _ := json.MarshalIndent(compact, "", "  ")
	const max = 600
	s := string(out)
	if len(s) > max {
		s = s[:max] + "\n  ..."
	}
	fmt.Println(s)
	fmt.Println()
}
