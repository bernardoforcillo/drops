package main

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/pg"
)

// pulled builds a snapshot shaped like one Introspect returns.
func pulled() *pg.Snapshot {
	snap := pg.EmptySnapshot()
	joined := "now()"
	snap.Tables["public.cli_authors"] = &pg.TableSnapshot{
		Name: "cli_authors",
		Columns: map[string]*pg.ColumnSnapshot{
			"id":        {Name: "id", Type: "bigserial", PrimaryKey: true, NotNull: true},
			"email":     {Name: "email", Type: "varchar(255)", NotNull: true},
			"joined_at": {Name: "joined_at", Type: "timestamptz", NotNull: true, Default: &joined},
			"rating":    {Name: "rating", Type: "numeric(4,2)"},
			"status":    {Name: "status", Type: "author_status"},
		},
		UniqueConstraints: map[string]*pg.UniqueSnapshot{
			"cli_authorsEmailUnique": {Name: "cli_authorsEmailUnique", Columns: []string{"email"}},
		},
		Indexes:              map[string]*pg.IndexSnapshot{},
		ForeignKeys:          map[string]*pg.ForeignKeySnapshot{},
		CompositePrimaryKeys: map[string]*pg.CompositePKSnapshot{},
		CheckConstraints:     map[string]*pg.CheckSnapshot{},
	}
	snap.Tables["public.cli_articles"] = &pg.TableSnapshot{
		Name: "cli_articles",
		Columns: map[string]*pg.ColumnSnapshot{
			"id":        {Name: "id", Type: "bigserial", PrimaryKey: true, NotNull: true},
			"author_id": {Name: "author_id", Type: "bigint", NotNull: true},
			"title":     {Name: "title", Type: "text", NotNull: true},
		},
		ForeignKeys: map[string]*pg.ForeignKeySnapshot{
			"cli_articlesAuthorIdFk": {
				Name: "cli_articlesAuthorIdFk", TableFrom: "cli_articles",
				ColumnsFrom: []string{"author_id"}, TableTo: "cli_authors",
				ColumnsTo: []string{"id"}, OnDelete: "cascade", OnUpdate: "no action",
			},
		},
		Indexes: map[string]*pg.IndexSnapshot{
			"cli_articles_author_idx": {
				Name: "cli_articles_author_idx", Columns: []string{"author_id"}, Method: "btree",
			},
			"cli_articles_recent_idx": {
				Name: "cli_articles_recent_idx", Columns: []string{"title"},
				Method: "btree", Where: "(title IS NOT NULL)",
			},
			"cli_articles_lower_idx": {Name: "cli_articles_lower_idx", Method: "btree"},
		},
		CheckConstraints: map[string]*pg.CheckSnapshot{
			"cli_articles_title_len": {Name: "cli_articles_title_len", Value: "length(title) > 0"},
		},
		UniqueConstraints:    map[string]*pg.UniqueSnapshot{},
		CompositePrimaryKeys: map[string]*pg.CompositePKSnapshot{},
	}
	return snap
}

func TestRenderGoSchema(t *testing.T) {
	src, err := renderGoSchema(pulled(), "demo")
	if err != nil {
		t.Fatalf("renderGoSchema: %v", err)
	}
	got := string(src)
	for _, want := range []string{
		"package demo",
		`CliAuthors = pg.NewTable("cli_authors")`,
		`CliAuthorsEmail = pg.Add(CliAuthors, pg.Varchar("email", 255).NotNull())`,
		`CliAuthorsId = pg.Add(CliAuthors, pg.BigSerial("id").PrimaryKey())`,
		`pg.Timestamp("joined_at", true).NotNull().Default("now()")`,
		`pg.Numeric("rating", 4, 2)`,
		`pg.Custom[string]("status", "author_status")`,
		`pg.BigInt("author_id").NotNull().References(CliAuthorsId, pg.OnDelete("CASCADE"))`,
		`CliAuthors.AddUnique("cli_authorsEmailUnique", CliAuthorsEmail)`,
		`CliArticles.AddCheck("cli_articles_title_len", "length(title) > 0")`,
		`CliArticles.AddIndex(pg.NewIndex("cli_articles_author_idx", CliArticles, CliArticlesAuthorId))`,
		"func Schema() *pg.Schema",
	} {
		if !containsNormalised(got, want) {
			t.Errorf("the pulled schema is missing %s\n---\n%s", want, got)
		}
	}
	// A primary key column carries PrimaryKey(), which already implies
	// NOT NULL; saying both would render the constraint twice.
	if strings.Contains(got, ".PrimaryKey().NotNull()") {
		t.Error("a primary key column was rendered NOT NULL as well as PRIMARY KEY")
	}
}

// An index that the snapshot cannot describe must not come back as a
// plainer index under the same name — that is a different index the
// catalogue would then agree with for ever.
func TestRenderGoSchemaRefusesToApproximateIndexes(t *testing.T) {
	got := string(mustRender(t))
	if strings.Contains(got, `pg.NewIndex("cli_articles_recent_idx"`) {
		t.Error("a partial index was rendered without its predicate")
	}
	if !strings.Contains(got, "cli_articles_recent_idx") {
		t.Error("the partial index was dropped silently instead of being reported")
	}
	if strings.Contains(got, `pg.NewIndex("cli_articles_lower_idx"`) {
		t.Error("an expression index was rendered as a column index")
	}
	if !strings.Contains(got, "cli_articles_lower_idx") {
		t.Error("the expression index was dropped silently instead of being reported")
	}
}

func mustRender(t *testing.T) []byte {
	t.Helper()
	src, err := renderGoSchema(pulled(), "demo")
	if err != nil {
		t.Fatalf("renderGoSchema: %v", err)
	}
	return src
}

// containsNormalised ignores the alignment gofmt adds inside a var
// block, which is a property of the formatter rather than of what was
// rendered.
func containsNormalised(haystack, needle string) bool {
	collapse := func(s string) string {
		return strings.Join(strings.Fields(s), " ")
	}
	for _, line := range strings.Split(haystack, "\n") {
		if strings.Contains(collapse(line), collapse(needle)) {
			return true
		}
	}
	return strings.Contains(collapse(haystack), collapse(needle))
}

func TestGoIdent(t *testing.T) {
	cases := map[string]string{
		"users":        "Users",
		"user_account": "UserAccount",
		"2fa_codes":    "T2faCodes",
		"__":           "Table",
		"Weird-Name":   "WeirdName",
	}
	for in, want := range cases {
		if got := goIdent(in); got != want {
			t.Errorf("goIdent(%q) = %q, want %q", in, got, want)
		}
	}
}

// Two SQL names can collapse to one Go identifier; the file still has
// to compile.
func TestRenderGoSchemaDisambiguatesIdentifiers(t *testing.T) {
	snap := pg.EmptySnapshot()
	for _, name := range []string{"user_account", "user account"} {
		snap.Tables["public."+name] = &pg.TableSnapshot{
			Name:                 name,
			Columns:              map[string]*pg.ColumnSnapshot{"id": {Name: "id", Type: "bigint", NotNull: true}},
			Indexes:              map[string]*pg.IndexSnapshot{},
			ForeignKeys:          map[string]*pg.ForeignKeySnapshot{},
			UniqueConstraints:    map[string]*pg.UniqueSnapshot{},
			CompositePrimaryKeys: map[string]*pg.CompositePKSnapshot{},
			CheckConstraints:     map[string]*pg.CheckSnapshot{},
		}
	}
	src, err := renderGoSchema(snap, "demo")
	if err != nil {
		t.Fatalf("renderGoSchema: %v", err)
	}
	if !containsNormalised(string(src), "UserAccount2 = pg.NewTable") {
		t.Errorf("colliding table names were not disambiguated:\n%s", src)
	}
}

func TestSplitTypeArgs(t *testing.T) {
	base, args := splitTypeArgs("numeric(10, 2)")
	if base != "numeric" || len(args) != 2 || args[0] != "10" || args[1] != "2" {
		t.Fatalf("splitTypeArgs = %q %v", base, args)
	}
	if base, args := splitTypeArgs("double precision"); base != "double precision" || args != nil {
		t.Fatalf("splitTypeArgs = %q %v", base, args)
	}
}
