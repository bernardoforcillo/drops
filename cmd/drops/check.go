package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/bernardoforcillo/drops/pg"
)

// drops check — the schema read against itself.
//
// The CLI already had two ways of being told something is wrong, and
// neither covers the declaration on its own:
//
//   - drops lint reads the Go SOURCE and reports query mistakes. It
//     knows nothing about the shape of the tables.
//   - drops drift reads a live DATABASE and reports where it and the
//     schema disagree. It says nothing about a schema both sides agree
//     on and that is going to hurt anyway.
//
// This is the third: no database, no query source, just the tables as
// declared — and the declarations that are legal SQL, that Push will
// apply without a word, and that a production workload then pays for.
//
// Every rule here answers "what does this COST", not "is this how I
// would have written it". A schema check that reports taste gets turned
// off wholesale within a week, and takes the rules that mattered with
// it. Each finding says what happens at scale and what to do; a rule
// that could not say either is not in the list.
//
// It exits 3 when it finds something, which is drift's answer: the
// command ran, and the answer was no.
func runCheck(ctx context.Context, args []string) error {
	fs := newFlagSet("check", "report schema declarations that cost at scale")
	schemaPkg := fs.String("schema", "", "Go package that exports func Schema() *pg.Schema (required)")
	off := fs.String("off", "", "comma-separated rules to skip (see the list below)")
	asJSON := fs.Bool("json", false, "emit findings as JSON instead of text")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `drops check — report schema declarations that cost at scale

Usage:
  drops check --schema ./schema [flags]

It reads the Go schema and nothing else: no database is opened and no
query source is parsed. Every rule is about the declaration.

Rules:
`)
		for _, r := range checkRules {
			fmt.Fprintf(os.Stderr, "  %-24s %s\n", r.name, r.doc)
		}
		fmt.Fprint(os.Stderr, "\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	rules, err := enabledCheckRules(*off)
	if err != nil {
		return err
	}
	pkg, err := locateSchema(ctx, *schemaPkg)
	if err != nil {
		return err
	}
	snap, err := loadSnapshot(ctx, pkg)
	if err != nil {
		return err
	}

	var found []CheckFinding
	for _, r := range rules {
		found = append(found, r.check(snap)...)
	}
	sortCheckFindings(found)

	if *asJSON {
		if err := emitCheckJSON(found); err != nil {
			return err
		}
	} else {
		emitCheckText(found)
	}
	if len(found) > 0 {
		return findingError{fmt.Errorf("%s", pluralFindings(len(found)))}
	}
	return nil
}

// CheckFinding is one thing the schema declares that will cost.
type CheckFinding struct {
	// Rule is the stable identifier, the one --off names.
	Rule string `json:"rule"`
	// Table is the table the finding is about.
	Table string `json:"table"`
	// Object is the index, constraint or column within it, empty when
	// the finding is about the table itself.
	Object string `json:"object,omitempty"`
	// Message says what is declared and what it costs.
	Message string `json:"message"`
	// Fix is the change that answers it, in the schema's own
	// vocabulary rather than in SQL: this command reads a Go schema,
	// so the edit it asks for is a Go one.
	Fix string `json:"fix"`
}

// checkRule is one rule, its one-line documentation, and its body.
type checkRule struct {
	name  string
	doc   string
	check func(*pg.Snapshot) []CheckFinding
}

// checkRules is the list, in the order the usage text prints them.
//
// It is a slice rather than a map so the order is the author's: the
// rules that cost the most read first, which is also the order somebody
// working through a report should fix them in.
var checkRules = []checkRule{
	{
		name:  "unindexed-foreign-key",
		doc:   "a foreign key whose own columns no index leads with",
		check: checkUnindexedForeignKeys,
	},
	{
		name:  "no-primary-key",
		doc:   "a table with neither a column key nor a composite one",
		check: checkNoPrimaryKey,
	},
	{
		name:  "rls-without-policy",
		doc:   "row-level security enabled on a table that declares no policy",
		check: checkRLSWithoutPolicy,
	},
	{
		name:  "policy-without-rls",
		doc:   "a policy on a table where row-level security is off, so it is inert",
		check: checkPolicyWithoutRLS,
	},
	{
		name:  "nullable-unique",
		doc:   "a unique constraint over a nullable column, which NULLs do not violate",
		check: checkNullableUnique,
	},
	{
		name:  "redundant-index",
		doc:   "an index whose columns are a leading prefix of another's",
		check: checkRedundantIndex,
	},
	{
		name:  "foreign-key-type-mismatch",
		doc:   "a foreign key whose column type differs from the one it references",
		check: checkForeignKeyTypes,
	},
}

// enabledCheckRules resolves the --off list. An unknown rule name is a
// usage error rather than a silent no-op, exactly as it is in lint: a
// typo in CI that quietly turns nothing off is worse than a failed
// build.
func enabledCheckRules(off string) ([]checkRule, error) {
	skip := map[string]bool{}
	for _, name := range strings.Split(off, ",") {
		if name = strings.TrimSpace(name); name != "" {
			skip[name] = true
		}
	}
	var out []checkRule
	for _, r := range checkRules {
		if skip[r.name] {
			delete(skip, r.name)
			continue
		}
		out = append(out, r)
	}
	if len(skip) > 0 {
		names := make([]string, 0, len(skip))
		for n := range skip {
			names = append(names, n)
		}
		sort.Strings(names)
		return nil, usagef("unknown rule(s) in --off: %s", strings.Join(names, ", "))
	}
	return out, nil
}

// ----------------------------------------------------------------------
// The rules
// ----------------------------------------------------------------------

// checkUnindexedForeignKeys reports a foreign key with no index on its
// own side.
//
// PostgreSQL creates an index for the REFERENCED side automatically —
// it has to, since the key it points at is unique. It creates nothing
// on the referencing side, and that is the side every delete of a
// parent row consults: the server must prove no child references the
// row, and with no index it proves it by scanning the child table.
//
// The cost is not linear in what the statement touches. Deleting one
// customer scans every order; a cascade over three levels scans all
// three. It stays invisible in development, where the child tables are
// small, and it is a common cause of a delete that took milliseconds in
// staging locking a table for minutes in production.
//
// A leading PREFIX is what counts, not an exact match: an index on
// (customerId, createdAt) serves a foreign key on customerId, and one
// on (createdAt, customerId) does not.
func checkUnindexedForeignKeys(s *pg.Snapshot) []CheckFinding {
	var out []CheckFinding
	for _, t := range sortedTables(s) {
		for _, fk := range sortedFKs(t) {
			if len(fk.ColumnsFrom) == 0 || indexLeadsWith(t, fk.ColumnsFrom) {
				continue
			}
			cols := strings.Join(fk.ColumnsFrom, ", ")
			cost := "every delete or key update of a %s row scans %s to prove no row here references it"
			if strings.EqualFold(fk.OnDelete, "cascade") {
				cost = "every delete of a %s row scans %s to find the rows to cascade to"
			}
			out = append(out, CheckFinding{
				Rule:   "unindexed-foreign-key",
				Table:  t.Name,
				Object: fk.Name,
				Message: fmt.Sprintf("the foreign key on (%s) has no index leading with those columns, so "+
					cost, cols, fk.TableTo, t.Name),
				Fix: fmt.Sprintf("pg.NewIndex(%q, %s, %s) on the schema — PostgreSQL indexes the referenced side for you and never this one",
					t.Name+"FkIdx", t.Name, cols),
			})
		}
	}
	return out
}

// checkNoPrimaryKey reports a table with no key of any kind.
//
// Three things need one, and the third is the one that surprises
// people. An entity cannot address a row without it — NewEntity panics
// at declaration time, so that failure is loud. A row cannot be
// updated by key from a query builder either. And logical replication
// cannot represent an UPDATE or a DELETE of the row at all: with
// REPLICA IDENTITY DEFAULT and no primary key the server refuses the
// statement outright once the table is in a publication, which is a
// write path failing long after the schema was declared.
func checkNoPrimaryKey(s *pg.Snapshot) []CheckFinding {
	var out []CheckFinding
	for _, t := range sortedTables(s) {
		if len(t.CompositePrimaryKeys) > 0 {
			continue
		}
		hasPK := false
		for _, c := range t.Columns {
			if c.PrimaryKey {
				hasPK = true
				break
			}
		}
		if hasPK {
			continue
		}
		out = append(out, CheckFinding{
			Rule:  "no-primary-key",
			Table: t.Name,
			Message: "the table declares no primary key, so no entity can address a row in it and logical " +
				"replication cannot represent an UPDATE or DELETE of one",
			Fix: "mark a column .PrimaryKey(), or declare a composite key with " + t.Name + ".PrimaryKey(cols...)",
		})
	}
	return out
}

// checkRLSWithoutPolicy reports a table with row-level security on and
// nothing granted.
//
// An RLS-enabled table with no policy is not "restrictive by default"
// in a useful sense: it denies EVERY row to every role the security
// applies to. Ordinary users read an empty table — no error, no denial,
// just no rows — and with FORCE ROW LEVEL SECURITY the owner reads one
// too. It is the shape somebody reaches while hardening, applies, and
// discovers from a support ticket rather than from a failure.
func checkRLSWithoutPolicy(s *pg.Snapshot) []CheckFinding {
	var out []CheckFinding
	for _, t := range sortedTables(s) {
		if !t.IsRLSEnabled || len(t.Policies) > 0 {
			continue
		}
		who := "every role but the owner"
		if t.IsRLSForced {
			who = "every role including the owner, since FORCE is declared"
		}
		out = append(out, CheckFinding{
			Rule:  "rls-without-policy",
			Table: t.Name,
			Message: "row-level security is enabled and no policy is declared, so the table returns no rows to " +
				who + " — an empty result rather than a denial",
			Fix: "declare a policy with " + t.Name + ".AddPolicy(pg.NewPolicy(...)), or drop the EnableRLS",
		})
	}
	return out
}

// checkPolicyWithoutRLS reports the mirror image: policies declared on
// a table where the security they belong to is off.
//
// A policy is inert until EnableRLS, which Table.AddPolicy documents.
// The failure mode is the opposite of the rule above and worse: the
// schema READS as though the rows are restricted, review passes, and
// every row is visible to everybody.
func checkPolicyWithoutRLS(s *pg.Snapshot) []CheckFinding {
	var out []CheckFinding
	for _, t := range sortedTables(s) {
		if len(t.Policies) == 0 || t.IsRLSEnabled {
			continue
		}
		names := make([]string, 0, len(t.Policies))
		for n := range t.Policies {
			names = append(names, n)
		}
		sort.Strings(names)
		out = append(out, CheckFinding{
			Rule:   "policy-without-rls",
			Table:  t.Name,
			Object: strings.Join(names, ", "),
			Message: "the table declares policies and row-level security is not enabled, so they are inert: " +
				"the schema reads as though the rows are restricted and every row is visible to everybody",
			Fix: "call " + t.Name + ".EnableRLS(), or remove the policies rather than leaving them to be read as protection",
		})
	}
	return out
}

// checkNullableUnique reports a unique constraint over a column that
// admits NULL.
//
// In PostgreSQL two NULLs are distinct, so a UNIQUE constraint over a
// nullable column does not stop a second row with NULL there, or a
// third. That is the standard's rule and almost never what the
// declaration was reaching for: "at most one row per user has an active
// subscription" written as UNIQUE (userId, cancelledAt) admits any
// number of rows whose cancelledAt is NULL, which is precisely the set
// the constraint was about.
//
// NULLS NOT DISTINCT is the answer where the intent really is "one NULL
// too"; a partial unique index is the answer where the intent is "one
// row among the ones that matter".
func checkNullableUnique(s *pg.Snapshot) []CheckFinding {
	var out []CheckFinding
	for _, t := range sortedTables(s) {
		names := make([]string, 0, len(t.UniqueConstraints))
		for n := range t.UniqueConstraints {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			u := t.UniqueConstraints[n]
			var nullable []string
			for _, col := range u.Columns {
				if c := t.Columns[col]; c != nil && !c.NotNull && !c.PrimaryKey {
					nullable = append(nullable, col)
				}
			}
			if len(nullable) == 0 || u.NullsNotDistinct {
				continue
			}
			out = append(out, CheckFinding{
				Rule:   "nullable-unique",
				Table:  t.Name,
				Object: u.Name,
				Message: fmt.Sprintf("the unique constraint covers (%s), and %s admits NULL — two NULLs are "+
					"distinct in PostgreSQL, so any number of rows may repeat the rest of the key",
					strings.Join(u.Columns, ", "), strings.Join(nullable, " and ")),
				Fix: "make the column NOT NULL, declare the constraint NULLS NOT DISTINCT, or use a partial " +
					"unique index over the rows the rule is about",
			})
		}
	}
	return out
}

// checkRedundantIndex reports an index whose columns are a leading
// prefix of another index on the same table.
//
// The narrower one answers no query the wider one does not: a B-tree on
// (a, b) serves every lookup an index on (a) serves, at the same depth.
// What the extra index costs is paid on every write — one more tree to
// maintain per INSERT, UPDATE and DELETE — and in the space and the
// vacuum time to keep it.
//
// A unique index is never reported as redundant: it is a constraint
// before it is an index, and dropping it changes what the table
// accepts.
func checkRedundantIndex(s *pg.Snapshot) []CheckFinding {
	var out []CheckFinding
	for _, t := range sortedTables(s) {
		idx := sortedIndexes(t)
		for _, narrow := range idx {
			if narrow.IsUnique || len(narrow.Columns) == 0 {
				continue
			}
			for _, wide := range idx {
				if wide.Name == narrow.Name || len(wide.Columns) <= len(narrow.Columns) {
					continue
				}
				// A partial index answers a different question from a
				// total one, whatever its columns are.
				if narrow.Where != wide.Where || narrow.Method != wide.Method {
					continue
				}
				if !hasPrefix(wide.Columns, narrow.Columns) {
					continue
				}
				out = append(out, CheckFinding{
					Rule:   "redundant-index",
					Table:  t.Name,
					Object: narrow.Name,
					Message: fmt.Sprintf("(%s) is a leading prefix of %q on (%s), which answers every lookup it "+
						"answers — the second tree is maintained on every write and read by nothing",
						strings.Join(narrow.Columns, ", "), wide.Name, strings.Join(wide.Columns, ", ")),
					Fix: "drop the narrower index from the schema",
				})
				break
			}
		}
	}
	return out
}

// checkForeignKeyTypes reports a foreign key whose column type differs
// from the type it references.
//
// PostgreSQL accepts the declaration when the two types are comparable
// — int and bigint are — and then compares them with a cast on every
// check. A cast on the indexed side is what stops the index being used,
// so the referential check that should be one index probe becomes a
// scan, on exactly the path checkUnindexedForeignKeys is about.
//
// It is also how a table outgrows its parent: a child keyed to an
// integer while the parent is a bigint stops being able to reference
// half its rows the day the parent passes two billion, with an
// out-of-range error on the INSERT rather than anything about the key.
func checkForeignKeyTypes(s *pg.Snapshot) []CheckFinding {
	var out []CheckFinding
	for _, t := range sortedTables(s) {
		for _, fk := range sortedFKs(t) {
			target := s.Tables[fk.TableTo]
			if target == nil {
				target = findTableByName(s, fk.TableTo)
			}
			if target == nil || len(fk.ColumnsFrom) != len(fk.ColumnsTo) {
				continue
			}
			for i, from := range fk.ColumnsFrom {
				fc, tc := t.Columns[from], target.Columns[fk.ColumnsTo[i]]
				if fc == nil || tc == nil || sameKeyType(fc.Type, tc.Type) {
					continue
				}
				out = append(out, CheckFinding{
					Rule:   "foreign-key-type-mismatch",
					Table:  t.Name,
					Object: fk.Name,
					Message: fmt.Sprintf("%s.%s is %s and references %s.%s, which is %s — the check compares them "+
						"through a cast, which stops the index on the referenced side being used",
						t.Name, from, fc.Type, target.Name, fk.ColumnsTo[i], tc.Type),
					Fix: fmt.Sprintf("declare %s.%s with the type %s has", t.Name, from, target.Name+"."+fk.ColumnsTo[i]),
				})
			}
		}
	}
	return out
}

// ----------------------------------------------------------------------
// Reading a snapshot
// ----------------------------------------------------------------------

// sortedTables returns the schema's tables in name order, so a report
// and a JSON document read the same way twice.
func sortedTables(s *pg.Snapshot) []*pg.TableSnapshot {
	keys := make([]string, 0, len(s.Tables))
	for k := range s.Tables {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*pg.TableSnapshot, 0, len(keys))
	for _, k := range keys {
		if t := s.Tables[k]; t != nil {
			out = append(out, t)
		}
	}
	return out
}

func sortedFKs(t *pg.TableSnapshot) []*pg.ForeignKeySnapshot {
	keys := make([]string, 0, len(t.ForeignKeys))
	for k := range t.ForeignKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*pg.ForeignKeySnapshot, 0, len(keys))
	for _, k := range keys {
		if fk := t.ForeignKeys[k]; fk != nil {
			out = append(out, fk)
		}
	}
	return out
}

func sortedIndexes(t *pg.TableSnapshot) []*pg.IndexSnapshot {
	keys := make([]string, 0, len(t.Indexes))
	for k := range t.Indexes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*pg.IndexSnapshot, 0, len(keys))
	for _, k := range keys {
		if i := t.Indexes[k]; i != nil {
			out = append(out, i)
		}
	}
	return out
}

// findTableByName finds a table by its bare name when the snapshot keys
// it some other way — a schema-qualified key, which is what a table
// declared through NewSchemaTable has.
func findTableByName(s *pg.Snapshot, name string) *pg.TableSnapshot {
	for _, t := range sortedTables(s) {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// indexLeadsWith reports whether any of the table's indexes, unique
// constraints or primary keys begins with exactly these columns.
//
// Leading rather than containing, because that is what a B-tree can
// use: an index on (a, b) serves a lookup on a, and an index on (b, a)
// does not. Order within the key matters too — a foreign key on
// (a, b) is served by an index on (a, b) and not by one on (b, a),
// since the referential check probes the whole key at once.
func indexLeadsWith(t *pg.TableSnapshot, cols []string) bool {
	for _, idx := range t.Indexes {
		// A partial index does not serve the check: it holds only the
		// rows its predicate admits, and a referential check has to
		// see every row that could reference the parent.
		if idx.Where == "" && hasPrefix(idx.Columns, cols) {
			return true
		}
	}
	for _, u := range t.UniqueConstraints {
		if hasPrefix(u.Columns, cols) {
			return true
		}
	}
	for _, pk := range t.CompositePrimaryKeys {
		if hasPrefix(pk.Columns, cols) {
			return true
		}
	}
	// A single-column primary key is its own index.
	if len(cols) == 1 {
		if c := t.Columns[cols[0]]; c != nil && c.PrimaryKey {
			return true
		}
	}
	return false
}

// hasPrefix reports whether have begins with want, comparing column
// names case-insensitively — a schema and an index may spell the same
// column two ways and PostgreSQL folds an unquoted one.
func hasPrefix(have, want []string) bool {
	if len(want) == 0 || len(have) < len(want) {
		return false
	}
	for i, w := range want {
		if !strings.EqualFold(have[i], w) {
			return false
		}
	}
	return true
}

// sameKeyType reports whether two column types compare without a cast.
//
// The comparison is on the type NAME with its modifiers stripped,
// because a length or precision does not change how two values of the
// same base type are compared: varchar(190) and varchar(255) reference
// each other through the same operator. A serial is its integer type —
// that is all "serial" ever was, a default and a sequence over an
// integer column — so bigserial and bigint are one type here, and a
// table keyed with BigSerial referenced by a BigInt is not reported.
func sameKeyType(a, b string) bool {
	return normaliseKeyType(a) == normaliseKeyType(b)
}

func normaliseKeyType(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	if i := strings.IndexByte(t, '('); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	switch t {
	case "bigserial":
		return "bigint"
	case "serial":
		return "integer"
	case "smallserial":
		return "smallint"
	case "int":
		return "integer"
	case "int2":
		return "smallint"
	case "int4":
		return "integer"
	case "int8":
		return "bigint"
	}
	return t
}

// ----------------------------------------------------------------------
// Reporting
// ----------------------------------------------------------------------

// sortCheckFindings orders the report by table, then rule, then object,
// so a schema is read table by table rather than rule by rule: somebody
// fixing this works through one table at a time.
func sortCheckFindings(f []CheckFinding) {
	sort.SliceStable(f, func(i, j int) bool {
		if f[i].Table != f[j].Table {
			return f[i].Table < f[j].Table
		}
		if f[i].Rule != f[j].Rule {
			return f[i].Rule < f[j].Rule
		}
		return f[i].Object < f[j].Object
	})
}

func pluralFindings(n int) string {
	if n == 1 {
		return "1 finding"
	}
	return fmt.Sprintf("%d findings", n)
}

// emitCheckJSON writes the findings as a JSON array — always an array,
// never null, so a consumer can index it without checking.
func emitCheckJSON(f []CheckFinding) error {
	if f == nil {
		f = []CheckFinding{}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(f)
}

// emitCheckText writes the human report, grouped by table.
//
// The fix is printed under every finding rather than under the first of
// its kind. Repetition is the price of a report somebody can act on by
// reading one entry, and the alternative — a rule reference to look up
// — is how a report becomes something to scroll past.
func emitCheckText(f []CheckFinding) {
	if len(f) == 0 {
		fmt.Println("drops check: nothing to report")
		return
	}
	table := ""
	for _, x := range f {
		if x.Table != table {
			if table != "" {
				fmt.Println()
			}
			fmt.Printf("%s\n", x.Table)
			table = x.Table
		}
		where := x.Rule
		if x.Object != "" {
			where += " (" + x.Object + ")"
		}
		fmt.Printf("  %s\n    %s\n    fix: %s\n", where, x.Message, x.Fix)
	}
	fmt.Printf("\n%s\n", pluralFindings(len(f)))
}
