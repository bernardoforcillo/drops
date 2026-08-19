package sqlite

import (
	"strings"
	"testing"
	"testing/fstest"
)

// identsMentioned decides the wording of the warning drops prints when
// a rebuild drops a column a trigger's body names. It has to see a
// quoted reference and a qualified one, and it has to not see a longer
// name that merely contains the dropped one — a table with both "org"
// and "org_id" would otherwise warn about every trigger it has.
func TestIdentsMentionedMatchesWholeIdentifiers(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
		want bool
	}{
		{"bare", `UPDATE t SET legacy = 1`, true},
		{"qualified", `... WHERE NEW.legacy IS NULL`, true},
		{"quoted", `UPDATE t SET "legacy" = 1`, true},
		{"bracketed", `UPDATE t SET [legacy] = 1`, true},
		{"different case", `UPDATE t SET LEGACY = 1`, true},
		{"longer name containing it", `UPDATE t SET legacy_id = 1`, false},
		{"name containing it as a suffix", `UPDATE t SET old_legacy = 1`, false},
		{"absent", `UPDATE t SET body = 1`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := len(identsMentioned(tc.sql, []string{"legacy"})) > 0
			if got != tc.want {
				t.Errorf("identsMentioned(%q) = %v, want %v", tc.sql, got, tc.want)
			}
		})
	}
}

// snapshot fixture: a table with one index and one trigger, in the shape
// Introspect produces.
func rebuildFixture() (prev, cur *TableSnapshot) {
	prev = &TableSnapshot{
		Name: "t",
		Columns: map[string]*ColumnSnapshot{
			"id":     {Name: "id", Type: "INTEGER", PrimaryKey: true},
			"body":   {Name: "body", Type: "TEXT"},
			"legacy": {Name: "legacy", Type: "TEXT"},
		},
		Indexes: map[string]*IndexSnapshot{
			"t_body_idx":   {Name: "t_body_idx", Table: "t", Columns: []string{"body"}, SQL: `CREATE INDEX t_body_idx ON t (body)`},
			"t_legacy_idx": {Name: "t_legacy_idx", Table: "t", Columns: []string{"legacy"}, SQL: `CREATE INDEX t_legacy_idx ON t (legacy)`},
		},
		Triggers: map[string]*TriggerSnapshot{
			"t_touch": {Name: "t_touch", Table: "t", SQL: `CREATE TRIGGER t_touch AFTER INSERT ON t BEGIN UPDATE t SET body = 'x'; END`},
		},
	}
	cur = &TableSnapshot{
		Name: "t",
		Columns: map[string]*ColumnSnapshot{
			"id":   {Name: "id", Type: "INTEGER", PrimaryKey: true},
			"body": {Name: "body", Type: "TEXT"},
		},
		Indexes:  map[string]*IndexSnapshot{},
		Triggers: map[string]*TriggerSnapshot{},
	}
	return prev, cur
}

// The replay has to come after the RENAME — the old names are only free
// once DROP TABLE has taken the old objects with it.
func TestReplayComesAfterTheRename(t *testing.T) {
	prev, cur := rebuildFixture()
	stmts := rebuildTable(prev, cur, "test")

	rename, index := -1, -1
	for i, s := range stmts {
		switch {
		case strings.HasPrefix(s, "ALTER TABLE") && strings.Contains(s, "RENAME TO"):
			rename = i
		case strings.HasPrefix(s, "CREATE INDEX t_body_idx"):
			index = i
		}
	}
	if rename < 0 || index < 0 {
		t.Fatalf("expected both a RENAME and the index replay:\n%s", strings.Join(stmts, "\n"))
	}
	if index < rename {
		t.Errorf("the index is re-created before the rename, when its name is still taken:\n%s",
			strings.Join(stmts, "\n"))
	}
}

// A rebuild that drops a column keeps the indexes that do not touch it
// and records the one it cannot keep.
func TestReplaySkipsAnIndexOnADroppedColumn(t *testing.T) {
	prev, cur := rebuildFixture()
	out := strings.Join(replayObjectsSQL(prev, cur), "\n")

	if !strings.Contains(out, `CREATE INDEX t_body_idx ON t (body);`) {
		t.Errorf("an index unaffected by the drop was not replayed:\n%s", out)
	}
	if strings.Contains(out, "CREATE INDEX t_legacy_idx") {
		t.Errorf("an index on the dropped column was replayed, which the engine will refuse:\n%s", out)
	}
	if !strings.Contains(out, `-- index "t_legacy_idx" dropped with column "legacy"`) {
		t.Errorf("the index was dropped without a word about it:\n%s", out)
	}
}

// Nothing is lost when the rebuild only widens the table.
func TestReplayKeepsEverythingWhenNoColumnGoes(t *testing.T) {
	prev, cur := rebuildFixture()
	cur.Columns["legacy"] = prev.Columns["legacy"]
	cur.Columns["extra"] = &ColumnSnapshot{Name: "extra", Type: "TEXT"}

	out := strings.Join(replayObjectsSQL(prev, cur), "\n")
	for _, want := range []string{"CREATE INDEX t_body_idx", "CREATE INDEX t_legacy_idx", "CREATE TRIGGER t_touch"} {
		if !strings.Contains(out, want) {
			t.Errorf("%s was not replayed:\n%s", want, out)
		}
	}
	if strings.Contains(out, "--") {
		t.Errorf("a rebuild that loses nothing should have nothing to report:\n%s", out)
	}
}

// A trigger is replayed whatever it says, because SQLite does not
// resolve the names in a trigger body until it fires — so the only
// thing drops can do about one that names a dropped column is say so.
func TestReplayWarnsButStillReplaysATrigger(t *testing.T) {
	prev, cur := rebuildFixture()
	prev.Triggers["t_touch"].SQL = `CREATE TRIGGER t_touch AFTER INSERT ON t BEGIN UPDATE t SET legacy = 'x'; END`

	out := replayObjectsSQL(prev, cur)
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, `-- trigger "t_touch" names dropped column "legacy"`) {
		t.Errorf("no warning for a trigger that can no longer work:\n%s", joined)
	}
	if !strings.Contains(joined, "CREATE TRIGGER t_touch") {
		t.Errorf("the trigger was dropped rather than replayed:\n%s", joined)
	}
	// The warning has to precede the statement it is about.
	warn, create := -1, -1
	for i, s := range out {
		switch {
		case strings.HasPrefix(s, `-- trigger "t_touch"`):
			warn = i
		case strings.HasPrefix(s, "CREATE TRIGGER t_touch"):
			create = i
		}
	}
	if warn > create {
		t.Errorf("the warning comes after the statement it warns about:\n%s", joined)
	}
}

// Diff must never emit an index change of its own: a declared snapshot
// carries no indexes, so every index in a database looks undeclared and
// diffing them would drop the lot.
func TestDiffNeverDropsAnIndex(t *testing.T) {
	live := EmptySnapshot()
	live.Tables["t"] = &TableSnapshot{
		Name:              "t",
		Columns:           map[string]*ColumnSnapshot{"id": {Name: "id", Type: "INTEGER", PrimaryKey: true}},
		UniqueConstraints: map[string]*UniqueSnapshot{},
		Indexes: map[string]*IndexSnapshot{
			"t_hot_idx": {Name: "t_hot_idx", Table: "t", Columns: []string{"id"}, SQL: `CREATE INDEX t_hot_idx ON t (id)`},
		},
		Triggers: map[string]*TriggerSnapshot{},
	}
	declared := EmptySnapshot()
	declared.Tables["t"] = &TableSnapshot{
		Name:    "t",
		Columns: map[string]*ColumnSnapshot{"id": {Name: "id", Type: "INTEGER", PrimaryKey: true}},
	}

	if stmts := Diff(live, declared); len(stmts) != 0 {
		t.Errorf("an index drops was never told about produced a migration:\n%s", strings.Join(stmts, "\n"))
	}
}

// SQLite does not store a UNIQUE constraint's name, so the three ways
// of writing the same constraint have to compare equal.
func TestUniqueKeysIgnoreNameAndSyntax(t *testing.T) {
	inline := &TableSnapshot{
		Columns: map[string]*ColumnSnapshot{"email": {Name: "email", Unique: true}},
	}
	named := &TableSnapshot{
		Columns:           map[string]*ColumnSnapshot{"email": {Name: "email"}},
		UniqueConstraints: map[string]*UniqueSnapshot{"users_email_uq": {Name: "users_email_uq", Columns: []string{"email"}}},
	}
	autoNamed := &TableSnapshot{
		Columns:           map[string]*ColumnSnapshot{"email": {Name: "email"}},
		UniqueConstraints: map[string]*UniqueSnapshot{"sqlite_autoindex_users_1": {Name: "sqlite_autoindex_users_1", Columns: []string{"email"}}},
	}
	widened := &TableSnapshot{
		Columns:           map[string]*ColumnSnapshot{"email": {Name: "email"}},
		UniqueConstraints: map[string]*UniqueSnapshot{"users_email_uq": {Name: "users_email_uq", Columns: []string{"org", "email"}}},
	}

	if !sameUniqueKeys(inline, named) {
		t.Error("an inline UNIQUE and a named one over the same column compared unequal")
	}
	if !sameUniqueKeys(named, autoNamed) {
		t.Error("a UNIQUE constraint compared unequal to its own introspected form")
	}
	if sameUniqueKeys(named, widened) {
		t.Error("widening a UNIQUE constraint went unnoticed")
	}
}

// The analyser is where drops states, in the tool a reviewer actually
// reads, what a rebuild could not preserve.
func TestAnalyserClassifiesWhatARebuildLoses(t *testing.T) {
	prev, cur := rebuildFixture()
	prev.Triggers["t_touch"].SQL = `CREATE TRIGGER t_touch AFTER INSERT ON t BEGIN UPDATE t SET legacy = 'x'; END`

	rules := map[string]SafetySeverity{}
	for _, w := range AnalyzeStatements(rebuildTable(prev, cur, "drop column \"legacy\"")) {
		rules[w.Rule] = w.Severity
	}
	if sev, ok := rules["rebuild-drops-index"]; !ok {
		t.Error("the analyser said nothing about the index the rebuild dropped")
	} else if sev != SeverityWarn {
		t.Errorf("rebuild-drops-index severity = %v, want warn", sev)
	}
	if sev, ok := rules["rebuild-stale-trigger"]; !ok {
		t.Error("the analyser said nothing about the trigger left naming a dropped column")
	} else if sev != SeverityError {
		t.Errorf("rebuild-stale-trigger severity = %v, want error", sev)
	}
}

// And it must stay quiet about a rebuild that loses nothing, or the
// warning means nothing.
func TestAnalyserIsQuietWhenARebuildLosesNothing(t *testing.T) {
	prev, cur := rebuildFixture()
	cur.Columns["legacy"] = prev.Columns["legacy"]

	for _, w := range AnalyzeStatements(rebuildTable(prev, cur, "constraint change")) {
		if w.Rule == "rebuild-drops-index" || w.Rule == "rebuild-stale-trigger" {
			t.Errorf("a rebuild that preserved everything still reported %s", w.Rule)
		}
	}
}

// A generated migration is the one rebuild drops cannot repair. Push
// replays what Introspect read out of sqlite_master; GenerateMigration
// has only snapshot files, and BuildSnapshot records no index and no
// trigger because the schema DSL cannot declare either. The rebuild
// therefore destroys them and puts nothing back — so the migration has
// to say so, in the file the reviewer reads.
func TestAGeneratedRebuildSaysItCannotKeepTheIndexes(t *testing.T) {
	before := NewTable("accounts")
	Add(before, BigInt("id").PrimaryKey())
	Add(before, Text("email").NotNull())
	Add(before, Text("legacy"))

	after := NewTable("accounts")
	Add(after, BigInt("id").PrimaryKey())
	Add(after, Text("email").NotNull())

	files := map[string][]byte{}
	gen := func(schema *Schema, name string) *GenerateResult {
		t.Helper()
		fsys := fstest.MapFS{}
		for k, v := range files {
			fsys["m/"+k] = &fstest.MapFile{Data: v}
		}
		res, err := GenerateMigration(GenerateOptions{
			Schema: NewSchema(schema.Tables()...), Dir: "m", Name: name,
			FS: fsys, Now: func() int64 { return 1 },
			Write: func(rel string, data []byte) error { files[rel] = data; return nil },
		})
		if err != nil {
			t.Fatalf("GenerateMigration: %v", err)
		}
		return res
	}

	if res := gen(NewSchema(before), "init"); res.NoOp {
		t.Fatal("the first migration produced nothing")
	}
	res := gen(NewSchema(after), "drop_legacy")
	if !strings.Contains(res.SQL, "-- rebuild") {
		t.Fatalf("dropping a column was supposed to be a rebuild:\n%s", res.SQL)
	}
	if !strings.Contains(res.SQL, "every index and trigger on it") {
		t.Errorf("a generated rebuild destroys every index and trigger and said nothing:\n%s", res.SQL)
	}
	// The note has to come before the rebuild it is about, not after it.
	if note, rebuild := strings.Index(res.SQL, "every index and trigger on it"),
		strings.Index(res.SQL, "-- rebuild"); note > rebuild {
		t.Errorf("the note follows the rebuild it warns about:\n%s", res.SQL)
	}
	// And the analyser has to carry it, because a comment in a file is
	// the thing a reviewer scrolls past.
	var found bool
	for _, w := range AnalyzeMigration(res.SQL) {
		if w.Rule == "rebuild-loses-indexes" {
			found = true
			if w.Severity != SeverityWarn {
				t.Errorf("rebuild-loses-indexes severity = %v, want warn", w.Severity)
			}
		}
	}
	if !found {
		t.Errorf("the analyser said nothing about a rebuild that cannot restore its indexes:\n%s", res.SQL)
	}
}

// The note belongs to the generated path only. Push replays the real
// objects, so telling its reviewer to go and re-create them by hand
// would be false — and a warning that fires when nothing is wrong is
// how a warning stops being read.
func TestPushsRebuildCarriesNoSuchNote(t *testing.T) {
	prev, cur := rebuildFixture()
	for _, s := range rebuildTable(prev, cur, "drop column \"legacy\"") {
		if strings.Contains(s, "every index and trigger on it") {
			t.Errorf("the rebuild Push runs claims it cannot restore indexes, but it just did:\n%s", s)
		}
	}
}
