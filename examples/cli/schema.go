// Package cli is a schema package as the `drops` CLI expects to find
// one: a set of tables declared with drops/pg, plus a Schema function
// that hands them over.
//
//	drops generate --schema ./examples/cli --dir drizzle
//	drops push     --schema ./examples/cli --dsn "$DATABASE_URL"
//	drops drift    --schema ./examples/cli --dsn "$DATABASE_URL"
//
// The CLI cannot read a Go variable, so it writes a small program that
// imports this package, calls Schema, and prints the snapshot. That is
// why the function has to exist and has to be called Schema: it is the
// one name the generated program knows.
//
// The tables are prefixed cli_ because this package is pushed against
// the same database the rest of the test suite uses.
package cli

import "github.com/bernardoforcillo/drops/pg"

// Authors is the cli_authors table.
var (
	Authors      = pg.NewTable("cli_authors")
	AuthorID     = pg.Add(Authors, pg.BigSerial("id").PrimaryKey())
	AuthorEmail  = pg.Add(Authors, pg.Text("email").NotNull().Unique())
	AuthorName   = pg.Add(Authors, pg.Text("name").NotNull())
	AuthorJoined = pg.Add(Authors, pg.Timestamp("joined_at", true).NotNull().Default("now()"))
)

// Articles is the cli_articles table.
var (
	Articles         = pg.NewTable("cli_articles")
	ArticleID        = pg.Add(Articles, pg.BigSerial("id").PrimaryKey())
	ArticleAuthorID  = pg.Add(Articles, pg.BigInt("author_id").NotNull().References(AuthorID, pg.OnDelete("CASCADE")))
	ArticleTitle     = pg.Add(Articles, pg.Text("title").NotNull())
	ArticleWordCount = pg.Add(Articles, pg.Integer("word_count").NotNull().Default("0"))
)

func init() {
	Articles.AddCheck("cli_articles_word_count_positive", "word_count >= 0")
	Articles.AddIndex(pg.NewIndex("cli_articles_author_idx", Articles, ArticleAuthorID))
}

// Schema is the set of tables drops manages — the entry point every
// schema-aware subcommand calls.
func Schema() *pg.Schema {
	return pg.NewSchema(Authors, Articles)
}
