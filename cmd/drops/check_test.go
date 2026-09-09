package main

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/pg"
)

// The rules read a snapshot and nothing else, so they are tested
// against one built by hand. That is also the shape a caller's schema
// arrives in — loadSnapshot compiles and runs a bridge program whose
// whole output is this document.

func snap(tables ...*pg.TableSnapshot) *pg.Snapshot {
	s := pg.EmptySnapshot()
	for _, t := range tables {
		s.Tables[t.Name] = t
	}
	return s
}

func tbl(name string) *pg.TableSnapshot {
	return &pg.TableSnapshot{
		Name:                 name,
		Columns:              map[string]*pg.ColumnSnapshot{},
		Indexes:              map[string]*pg.IndexSnapshot{},
		ForeignKeys:          map[string]*pg.ForeignKeySnapshot{},
		CompositePrimaryKeys: map[string]*pg.CompositePKSnapshot{},
		UniqueConstraints:    map[string]*pg.UniqueSnapshot{},
		Policies:             map[string]*pg.PolicySnapshot{},
		CheckConstraints:     map[string]*pg.CheckSnapshot{},
	}
}

func withCol(t *pg.TableSnapshot, name, typ string, pk, notNull bool) *pg.TableSnapshot {
	t.Columns[name] = &pg.ColumnSnapshot{Name: name, Type: typ, PrimaryKey: pk, NotNull: notNull}
	return t
}

func withIndex(t *pg.TableSnapshot, name string, cols ...string) *pg.TableSnapshot {
	t.Indexes[name] = &pg.IndexSnapshot{Name: name, Columns: cols}
	return t
}

func withFK(t *pg.TableSnapshot, name string, from []string, to string, toCols []string, onDelete string) *pg.TableSnapshot {
	t.ForeignKeys[name] = &pg.ForeignKeySnapshot{
		Name: name, TableFrom: t.Name, ColumnsFrom: from,
		TableTo: to, ColumnsTo: toCols, OnDelete: onDelete,
	}
	return t
}

// rules returns the findings of one named rule, so a test says which
// rule it is about rather than filtering a whole report.
func rules(t *testing.T, name string, s *pg.Snapshot) []CheckFinding {
	t.Helper()
	for _, r := range checkRules {
		if r.name == name {
			return r.check(s)
		}
	}
	t.Fatalf("no rule named %q", name)
	return nil
}

func only(t *testing.T, f []CheckFinding) CheckFinding {
	t.Helper()
	if len(f) != 1 {
		t.Fatalf("findings = %+v, want exactly one", f)
	}
	return f[0]
}

// ----------------------------------------------------------------------

// PostgreSQL indexes the REFERENCED side of a foreign key for you and
// never the referencing side — which is the side every delete of a
// parent row consults.
func TestCheckReportsAForeignKeyWithNoIndexOnItsOwnSide(t *testing.T) {
	users := withCol(tbl("users"), "id", "bigint", true, true)
	orders := withCol(withCol(tbl("orders"), "id", "bigint", true, true),
		"userId", "bigint", false, true)
	withFK(orders, "ordersUserIdFk", []string{"userId"}, "users", []string{"id"}, "")

	got := only(t, rules(t, "unindexed-foreign-key", snap(users, orders)))
	if got.Table != "orders" || got.Object != "ordersUserIdFk" {
		t.Errorf("finding = %+v", got)
	}
	if !strings.Contains(got.Message, "scans orders") {
		t.Errorf("the message does not say what it costs: %s", got.Message)
	}
	if !strings.Contains(got.Fix, "pg.NewIndex") {
		t.Errorf("the fix is not in the schema's vocabulary: %s", got.Fix)
	}
}

// A leading prefix serves the key; an index that merely contains the
// column does not, and a partial index does not either — the check has
// to see every row that could reference the parent.
func TestCheckKnowsWhichIndexesServeAForeignKey(t *testing.T) {
	build := func(idx func(*pg.TableSnapshot)) *pg.Snapshot {
		users := withCol(tbl("users"), "id", "bigint", true, true)
		orders := withCol(withCol(withCol(tbl("orders"),
			"id", "bigint", true, true),
			"userId", "bigint", false, true),
			"createdAt", "timestamptz", false, true)
		withFK(orders, "fk", []string{"userId"}, "users", []string{"id"}, "")
		idx(orders)
		return snap(users, orders)
	}

	tests := []struct {
		name  string
		idx   func(*pg.TableSnapshot)
		clean bool
	}{
		{"exactly the column", func(t *pg.TableSnapshot) { withIndex(t, "i", "userId") }, true},
		{"leading it", func(t *pg.TableSnapshot) { withIndex(t, "i", "userId", "createdAt") }, true},
		{"trailing it", func(t *pg.TableSnapshot) { withIndex(t, "i", "createdAt", "userId") }, false},
		{"partial", func(t *pg.TableSnapshot) {
			t.Indexes["i"] = &pg.IndexSnapshot{Name: "i", Columns: []string{"userId"}, Where: `"createdAt" IS NULL`}
		}, false},
		{"a unique constraint over it", func(t *pg.TableSnapshot) {
			t.UniqueConstraints["u"] = &pg.UniqueSnapshot{Name: "u", Columns: []string{"userId"}}
		}, true},
		{"none at all", func(*pg.TableSnapshot) {}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rules(t, "unindexed-foreign-key", build(tt.idx))
			if tt.clean && len(got) != 0 {
				t.Errorf("reported a served foreign key: %+v", got)
			}
			if !tt.clean && len(got) != 1 {
				t.Errorf("findings = %+v, want one", got)
			}
		})
	}
}

// A cascade is the same missing index with a different sentence: the
// scan is to FIND the rows to delete rather than to prove there are
// none.
func TestCheckNamesTheCascadeWhenThereIsOne(t *testing.T) {
	users := withCol(tbl("users"), "id", "bigint", true, true)
	orders := withCol(withCol(tbl("orders"), "id", "bigint", true, true), "userId", "bigint", false, true)
	withFK(orders, "fk", []string{"userId"}, "users", []string{"id"}, "cascade")

	got := only(t, rules(t, "unindexed-foreign-key", snap(users, orders)))
	if !strings.Contains(got.Message, "cascade") {
		t.Errorf("a cascading key reads like a restricting one: %s", got.Message)
	}
}

func TestCheckReportsATableWithNoKey(t *testing.T) {
	keyless := withCol(tbl("events"), "payload", "jsonb", false, true)
	got := only(t, rules(t, "no-primary-key", snap(keyless)))
	if !strings.Contains(got.Message, "logical replication") {
		t.Errorf("the message names only the loud failure: %s", got.Message)
	}

	// A composite key is a key.
	composite := withCol(withCol(tbl("memberships"), "userId", "bigint", false, true),
		"orgId", "bigint", false, true)
	composite.CompositePrimaryKeys["pk"] = &pg.CompositePKSnapshot{
		Name: "pk", Columns: []string{"userId", "orgId"},
	}
	if got := rules(t, "no-primary-key", snap(composite)); len(got) != 0 {
		t.Errorf("a composite key was read as none: %+v", got)
	}
}

// The two halves of row-level security, each of which fails silently in
// the opposite direction.
func TestCheckReportsRowLevelSecurityThatIsHalfDeclared(t *testing.T) {
	enabled := withCol(tbl("secrets"), "id", "bigint", true, true)
	enabled.IsRLSEnabled = true
	got := only(t, rules(t, "rls-without-policy", snap(enabled)))
	if !strings.Contains(got.Message, "no rows") {
		t.Errorf("the message does not say the table goes empty: %s", got.Message)
	}
	// FORCE means the owner too, and the message has to say so.
	enabled.IsRLSForced = true
	if got := only(t, rules(t, "rls-without-policy", snap(enabled))); !strings.Contains(got.Message, "owner") {
		t.Errorf("FORCE is not named: %s", got.Message)
	}

	inert := withCol(tbl("notes"), "id", "bigint", true, true)
	inert.Policies["byTenant"] = &pg.PolicySnapshot{Name: "byTenant", Using: "true"}
	got = only(t, rules(t, "policy-without-rls", snap(inert)))
	if !strings.Contains(got.Message, "inert") || !strings.Contains(got.Message, "visible to everybody") {
		t.Errorf("the message does not say what it costs: %s", got.Message)
	}

	// Both declared is the correct schema and reports nothing.
	both := withCol(tbl("ok"), "id", "bigint", true, true)
	both.IsRLSEnabled = true
	both.Policies["p"] = &pg.PolicySnapshot{Name: "p", Using: "true"}
	if got := rules(t, "rls-without-policy", snap(both)); len(got) != 0 {
		t.Errorf("a correct table was reported: %+v", got)
	}
	if got := rules(t, "policy-without-rls", snap(both)); len(got) != 0 {
		t.Errorf("a correct table was reported: %+v", got)
	}
}

// Two NULLs are distinct in PostgreSQL, so a unique constraint over a
// nullable column does not stop a second row with NULL there.
func TestCheckReportsAUniqueConstraintNullsDoNotViolate(t *testing.T) {
	subs := withCol(withCol(withCol(tbl("subscriptions"),
		"id", "bigint", true, true),
		"userId", "bigint", false, true),
		"cancelledAt", "timestamptz", false, false)
	subs.UniqueConstraints["oneActive"] = &pg.UniqueSnapshot{
		Name: "oneActive", Columns: []string{"userId", "cancelledAt"},
	}
	got := only(t, rules(t, "nullable-unique", snap(subs)))
	if !strings.Contains(got.Message, "cancelledAt") {
		t.Errorf("the finding does not name the nullable column: %s", got.Message)
	}

	// NULLS NOT DISTINCT is the answer, and having given it the
	// declaration is not reported.
	subs.UniqueConstraints["oneActive"].NullsNotDistinct = true
	if got := rules(t, "nullable-unique", snap(subs)); len(got) != 0 {
		t.Errorf("NULLS NOT DISTINCT was still reported: %+v", got)
	}
}

func TestCheckReportsAnIndexAnotherAlreadyAnswers(t *testing.T) {
	posts := withCol(withCol(withCol(tbl("posts"),
		"id", "bigint", true, true),
		"authorId", "bigint", false, true),
		"createdAt", "timestamptz", false, true)
	withIndex(posts, "narrow", "authorId")
	withIndex(posts, "wide", "authorId", "createdAt")

	got := only(t, rules(t, "redundant-index", snap(posts)))
	if got.Object != "narrow" {
		t.Errorf("the wider index was reported instead: %+v", got)
	}
	if !strings.Contains(got.Message, "every write") {
		t.Errorf("the message does not say what the second tree costs: %s", got.Message)
	}

	// A unique index is a constraint before it is an index, and
	// dropping it changes what the table accepts.
	posts.Indexes["narrow"].IsUnique = true
	if got := rules(t, "redundant-index", snap(posts)); len(got) != 0 {
		t.Errorf("a unique index was called redundant: %+v", got)
	}
	posts.Indexes["narrow"].IsUnique = false

	// A partial index answers a different question whatever its
	// columns are.
	posts.Indexes["narrow"].Where = `"deletedAt" IS NULL`
	if got := rules(t, "redundant-index", snap(posts)); len(got) != 0 {
		t.Errorf("a partial index was called redundant: %+v", got)
	}
}

func TestCheckReportsAKeyThatReferencesAWiderColumn(t *testing.T) {
	users := withCol(tbl("users"), "id", "bigint", true, true)
	orders := withCol(withCol(tbl("orders"), "id", "bigint", true, true),
		"userId", "integer", false, true)
	withFK(orders, "fk", []string{"userId"}, "users", []string{"id"}, "")
	withIndex(orders, "i", "userId")

	got := only(t, rules(t, "foreign-key-type-mismatch", snap(users, orders)))
	if !strings.Contains(got.Message, "cast") {
		t.Errorf("the message does not say why it costs: %s", got.Message)
	}

	// A serial is its integer type — that is all serial ever was — so
	// bigserial referenced by bigint is one type and not a finding.
	users.Columns["id"].Type = "bigserial"
	orders.Columns["userId"].Type = "bigint"
	if got := rules(t, "foreign-key-type-mismatch", snap(users, orders)); len(got) != 0 {
		t.Errorf("bigserial and bigint were called different types: %+v", got)
	}
}

// --off is checked against the rule list, so a typo in CI fails rather
// than quietly turning nothing off.
func TestCheckRefusesAnUnknownRuleName(t *testing.T) {
	if _, err := enabledCheckRules("no-primary-key"); err != nil {
		t.Fatalf("a real rule was refused: %v", err)
	}
	_, err := enabledCheckRules("no-primary-kye")
	if err == nil || !strings.Contains(err.Error(), "no-primary-kye") {
		t.Fatalf("err = %v, want the typo named", err)
	}
}

// Every rule has a name, a line of documentation and a body, because
// the usage text prints all three and a rule nobody can read about is
// one nobody can decide to turn off.
func TestEveryCheckRuleIsDocumented(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range checkRules {
		if r.name == "" || r.doc == "" || r.check == nil {
			t.Errorf("rule %+v is incomplete", r.name)
		}
		if seen[r.name] {
			t.Errorf("two rules are called %q", r.name)
		}
		seen[r.name] = true
		if strings.ToLower(r.doc) != r.doc[:1]+r.doc[1:] && r.doc[0] >= 'A' && r.doc[0] <= 'Z' {
			t.Errorf("rule %q's doc line starts with a capital; the list reads as a sentence per line", r.name)
		}
	}
}

// Every finding says what it costs and what to do. A rule that reports
// a fact without either is taste, and taste is what gets a schema
// checker turned off wholesale.
func TestEveryFindingCarriesACostAndAFix(t *testing.T) {
	// One schema that trips every rule at once.
	users := withCol(tbl("users"), "id", "bigserial", true, true)
	users.IsRLSEnabled = true
	orders := withCol(withCol(withCol(tbl("orders"),
		"id", "bigint", true, true),
		"userId", "integer", false, true),
		"code", "text", false, false)
	withFK(orders, "fk", []string{"userId"}, "users", []string{"id"}, "cascade")
	orders.UniqueConstraints["u"] = &pg.UniqueSnapshot{Name: "u", Columns: []string{"code"}}
	keyless := withCol(tbl("events"), "payload", "jsonb", false, true)
	keyless.Policies["p"] = &pg.PolicySnapshot{Name: "p"}
	withIndex(keyless, "narrow", "payload")
	withIndex(keyless, "wide", "payload", "payload2")

	s := snap(users, orders, keyless)
	var all []CheckFinding
	for _, r := range checkRules {
		got := r.check(s)
		if len(got) == 0 {
			t.Errorf("rule %q found nothing in a schema built to trip it", r.name)
		}
		all = append(all, got...)
	}
	for _, f := range all {
		if f.Rule == "" || f.Table == "" {
			t.Errorf("finding %+v does not say where it is", f)
		}
		if len(f.Message) < 40 {
			t.Errorf("%s on %s: the message is too short to say what it costs: %q", f.Rule, f.Table, f.Message)
		}
		if f.Fix == "" {
			t.Errorf("%s on %s: no fix", f.Rule, f.Table)
		}
	}
}

// The report is stable: the same schema twice is the same bytes, so a
// diff between two runs is a change in the schema and never in a map's
// iteration order.
func TestTheCheckReportIsOrdered(t *testing.T) {
	a := withCol(tbl("aaa"), "x", "bigint", false, true)
	b := withCol(tbl("bbb"), "x", "bigint", false, true)
	c := withCol(tbl("ccc"), "x", "bigint", false, true)

	for i := 0; i < 20; i++ {
		var got []CheckFinding
		got = append(got, rules(t, "no-primary-key", snap(c, a, b))...)
		sortCheckFindings(got)
		if len(got) != 3 || got[0].Table != "aaa" || got[1].Table != "bbb" || got[2].Table != "ccc" {
			t.Fatalf("run %d: %+v", i, got)
		}
	}
}
