package pg

import (
	"regexp"
	"strings"
)

// Pre-flight safety analysis for migration SQL. Catches the most
// common foot-guns before they hit production:
//
//	r := pg.GenerateMigration(opts)
//	for _, w := range pg.AnalyzeMigration(r.SQL) {
//	    log.Printf("%s [%s] %s", w.Severity, w.Rule, w.Message)
//	}
//
// The rules are intentionally conservative — most catch correctness
// issues (locks held too long, data loss, plan invalidation), not
// stylistic preferences. False positives are better than the
// production incident they prevent.

// SafetySeverity ranks a warning's urgency.
type SafetySeverity int

const (
	// SeverityInfo is a heads-up — usually fine, occasionally
	// worth a second look.
	SeverityInfo SafetySeverity = iota
	// SeverityWarn flags a statement that will likely cause
	// downtime or visible behaviour change on a non-trivial
	// table. Reviewable.
	SeverityWarn
	// SeverityError flags a statement that almost certainly
	// breaks production at any reasonable table size — full
	// table rewrites, exclusive locks held indefinitely,
	// unrecoverable data loss.
	SeverityError
)

// String renders the level as "info" / "warn" / "error".
func (s SafetySeverity) String() string {
	switch s {
	case SeverityInfo:
		return "info"
	case SeverityWarn:
		return "warn"
	case SeverityError:
		return "error"
	}
	return "unknown"
}

// SafetyWarning is one finding from the migration analyser.
type SafetyWarning struct {
	// Severity ranks the urgency — info / warn / error.
	Severity SafetySeverity

	// Rule is a stable identifier for the check (e.g.
	// "add-not-null-column"). Use it to suppress a known-
	// acceptable warning via SafetyOptions.Ignore.
	Rule string

	// Statement is the offending SQL fragment, trimmed of
	// surrounding whitespace and statement breakpoints.
	Statement string

	// Message describes the problem in plain language.
	Message string

	// Suggestion is a short hint on how to fix the issue —
	// typically a safer migration shape.
	Suggestion string
}

// SafetyOptions tunes the analyser — currently used for rule
// suppression. Add to it as the rule set grows.
type SafetyOptions struct {
	// Ignore lists rule IDs to skip. Useful when a particular
	// migration is known-safe (e.g. small table, scheduled
	// downtime).
	Ignore []string
}

// AnalyzeMigration splits a migration script on the drizzle-kit
// "--> statement-breakpoint" boundary and runs the per-statement
// analyser on each piece. Pass the SQL field of a GenerateResult.
func AnalyzeMigration(sql string, opts ...SafetyOptions) []SafetyWarning {
	parts := splitStatements(sql)
	return AnalyzeStatements(parts, opts...)
}

// AnalyzeStatements runs the safety rules against each statement
// in order. The output preserves statement order so callers can
// align warnings with their migration text.
func AnalyzeStatements(stmts []string, opts ...SafetyOptions) []SafetyWarning {
	var ignore map[string]bool
	for _, o := range opts {
		for _, r := range o.Ignore {
			if ignore == nil {
				ignore = map[string]bool{}
			}
			ignore[r] = true
		}
	}
	var out []SafetyWarning
	var kept []string
	for _, s := range stmts {
		trim := strings.TrimSpace(s)
		if trim == "" {
			continue
		}
		kept = append(kept, trim)
		for _, rule := range safetyRules {
			if w, ok := rule(trim); ok {
				if ignore[w.Rule] {
					continue
				}
				out = append(out, w)
			}
		}
	}
	for _, w := range analyzePairs(kept) {
		if ignore[w.Rule] {
			continue
		}
		out = append(out, w)
	}
	return out
}

// splitStatements breaks a migration up at the drizzle-kit
// "statement-breakpoint" boundary used by the diff generator.
// Trailing semicolons are stripped so the per-rule matchers see
// the raw statement.
func splitStatements(sql string) []string {
	parts := strings.Split(sql, "--> statement-breakpoint")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.TrimSuffix(p, ";")
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// safetyRules is the rule set in declaration order. Each rule is a
// closure that inspects one statement and returns a SafetyWarning
// when the statement matches its pattern.
var safetyRules = []func(stmt string) (SafetyWarning, bool){
	ruleAddColumnNotNullNoDefault,
	ruleAddColumnNotNullVolatileDefault,
	ruleAlterColumnType,
	ruleAlterColumnSetNotNull,
	ruleCreateIndexNotConcurrent,
	ruleDropIndex,
	ruleDropTable,
	ruleDropColumn,
	ruleAlterTypeDropValue,
	ruleRenameColumn,
	ruleRenameTable,
	ruleTruncate,
}

// ----------------------------------------------------------------------
// Rules
// ----------------------------------------------------------------------

var (
	reAddColumn      = regexp.MustCompile(`(?i)\bALTER\s+TABLE\b.*\bADD\s+COLUMN\b`)
	reHasNotNull     = regexp.MustCompile(`(?i)\bNOT\s+NULL\b`)
	reHasDefault     = regexp.MustCompile(`(?i)\bDEFAULT\b`)
	reVolatileDef    = regexp.MustCompile(`(?i)\bDEFAULT\s+(now\(\)|current_timestamp|gen_random_uuid\(\)|uuid_generate_v4\(\)|random\(\))`)
	reAlterColType   = regexp.MustCompile(`(?i)\bALTER\s+TABLE\b.*\bALTER\s+COLUMN\b.*\b(SET\s+DATA\s+TYPE|TYPE)\b`)
	reAlterColSetNN  = regexp.MustCompile(`(?i)\bALTER\s+TABLE\b.*\bALTER\s+COLUMN\b.*\bSET\s+NOT\s+NULL\b`)
	reCreateIndex    = regexp.MustCompile(`(?i)\bCREATE\s+(UNIQUE\s+)?INDEX\b`)
	reConcurrently   = regexp.MustCompile(`(?i)\bCONCURRENTLY\b`)
	reDropIndex      = regexp.MustCompile(`(?i)\bDROP\s+INDEX\b`)
	reDropTable      = regexp.MustCompile(`(?i)\bDROP\s+TABLE\b`)
	reDropColumn     = regexp.MustCompile(`(?i)\bALTER\s+TABLE\b.*\bDROP\s+COLUMN\b`)
	reAlterTypeDrop  = regexp.MustCompile(`(?i)\bALTER\s+TYPE\b.*\bDROP\s+VALUE\b`)
	reRenameColumn   = regexp.MustCompile(`(?i)\bALTER\s+TABLE\b.*\bRENAME\s+COLUMN\b`)
	reRenameTable    = regexp.MustCompile(`(?i)\bALTER\s+TABLE\b.*\bRENAME\s+TO\b`)
	reTruncate       = regexp.MustCompile(`(?i)\bTRUNCATE\b`)
	reCreateTableNew = regexp.MustCompile(`(?i)\bCREATE\s+TABLE\b`)
)

func ruleAddColumnNotNullNoDefault(stmt string) (SafetyWarning, bool) {
	if !reAddColumn.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	if !reHasNotNull.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	if reHasDefault.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityError,
		Rule:       "add-not-null-column-without-default",
		Statement:  stmt,
		Message:    "ADD COLUMN NOT NULL without DEFAULT requires every existing row to satisfy the constraint and locks the table while PG validates it.",
		Suggestion: "Add the column nullable, backfill in batches, then SET NOT NULL in a follow-up migration — or add it NOT NULL with a constant DEFAULT (PG 11+ skips the rewrite).",
	}, true
}

func ruleAddColumnNotNullVolatileDefault(stmt string) (SafetyWarning, bool) {
	if !reAddColumn.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	if !reHasNotNull.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	if !reVolatileDef.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityError,
		Rule:       "add-not-null-column-with-volatile-default",
		Statement:  stmt,
		Message:    "ADD COLUMN NOT NULL with a volatile DEFAULT (now(), gen_random_uuid(), random()) forces PG to rewrite every row — exclusive lock for the duration.",
		Suggestion: "Add the column nullable with the volatile default, backfill existing rows in batches, then SET NOT NULL.",
	}, true
}

func ruleAlterColumnType(stmt string) (SafetyWarning, bool) {
	if !reAlterColType.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityWarn,
		Rule:       "alter-column-type",
		Statement:  stmt,
		Message:    "ALTER COLUMN TYPE often rewrites the whole table (and any dependent indexes / constraints).",
		Suggestion: "Add a new column with the target type, dual-write from the application, backfill, then drop the old column in a follow-up migration.",
	}, true
}

func ruleAlterColumnSetNotNull(stmt string) (SafetyWarning, bool) {
	if !reAlterColSetNN.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityWarn,
		Rule:       "alter-column-set-not-null",
		Statement:  stmt,
		Message:    "SET NOT NULL takes ACCESS EXCLUSIVE while PG scans every row to validate the constraint.",
		Suggestion: "Backfill nulls first, add a CHECK (col IS NOT NULL) NOT VALID, VALIDATE CONSTRAINT, then SET NOT NULL — the constraint stays validated so the SET NOT NULL is metadata-only on PG 12+.",
	}, true
}

func ruleCreateIndexNotConcurrent(stmt string) (SafetyWarning, bool) {
	if !reCreateIndex.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	if reConcurrently.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	if reCreateTableNew.MatchString(stmt) {
		// CREATE TABLE often carries inline CREATE INDEX-like
		// fragments via reserved keywords — skip when it's a
		// brand-new table (no live traffic yet).
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityWarn,
		Rule:       "create-index-not-concurrent",
		Statement:  stmt,
		Message:    "CREATE INDEX without CONCURRENTLY blocks writes against the table while the index builds.",
		Suggestion: "Append CONCURRENTLY (note: cannot run inside a transaction; emit the index DDL as a standalone migration).",
	}, true
}

// ruleDropIndex flags DROP INDEX.
//
// It is here because Push can emit one for an index nobody declared —
// a DBA's, an extension's, another migration tool's. The statement
// itself is metadata-only and instant, so the severity is not about
// the drop; it is about the rebuild that follows once someone notices
// the plan that regressed, on a table large enough to have wanted the
// index in the first place. CONCURRENTLY is not an escape either: it
// only avoids the lock, not the loss.
func ruleDropIndex(stmt string) (SafetyWarning, bool) {
	if !reDropIndex.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityWarn,
		Rule:       "drop-index",
		Statement:  stmt,
		Message:    "DROP INDEX is not reversible in any useful sense: rebuilding it means a full index build on a table that was big enough to need it.",
		Suggestion: "Confirm the index is unused (pg_stat_user_indexes.idx_scan) before dropping it, and keep the CREATE INDEX CONCURRENTLY that recreates it to hand.",
	}, true
}

func ruleDropTable(stmt string) (SafetyWarning, bool) {
	if !reDropTable.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityError,
		Rule:       "drop-table",
		Statement:  stmt,
		Message:    "DROP TABLE destroys data irreversibly.",
		Suggestion: "Rename the table aside (ALTER TABLE ... RENAME TO _archived_xxx) and drop in a follow-up migration after a retention window.",
	}, true
}

func ruleDropColumn(stmt string) (SafetyWarning, bool) {
	if !reDropColumn.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityWarn,
		Rule:       "drop-column",
		Statement:  stmt,
		Message:    "DROP COLUMN is irreversible and breaks any application code that still references the column.",
		Suggestion: "Stop writing to the column first, deploy, wait, then drop — and keep a backup if the data matters.",
	}, true
}

func ruleAlterTypeDropValue(stmt string) (SafetyWarning, bool) {
	if !reAlterTypeDrop.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityError,
		Rule:       "alter-type-drop-value",
		Statement:  stmt,
		Message:    "PostgreSQL cannot drop enum values — the statement will fail.",
		Suggestion: "Replace the enum with a text + CHECK constraint, or accept that old values stay listed and stop emitting them from the application.",
	}, true
}

func ruleRenameColumn(stmt string) (SafetyWarning, bool) {
	if !reRenameColumn.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityWarn,
		Rule:       "rename-column",
		Statement:  stmt,
		Message:    "RENAME COLUMN breaks any application code referring to the old name during a rolling deploy.",
		Suggestion: "Add a new column, dual-write, switch reads, drop the old column — never rename across deploy boundaries.",
	}, true
}

func ruleRenameTable(stmt string) (SafetyWarning, bool) {
	if !reRenameTable.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityWarn,
		Rule:       "rename-table",
		Statement:  stmt,
		Message:    "RENAME TABLE breaks any application code referring to the old name during a rolling deploy.",
		Suggestion: "Create a view with the old name pointing at the new table, deploy, drop the view in a follow-up migration.",
	}, true
}

func ruleTruncate(stmt string) (SafetyWarning, bool) {
	if !reTruncate.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityError,
		Rule:       "truncate-table",
		Statement:  stmt,
		Message:    "TRUNCATE removes every row irreversibly and bypasses ON DELETE triggers.",
		Suggestion: "If this is intentional, accept the warning via SafetyOptions.Ignore; otherwise drop the statement.",
	}, true
}

// ----------------------------------------------------------------------
// Cross-statement rules
// ----------------------------------------------------------------------

// The rename work established that an ambiguity is refused rather than
// guessed. DetectRenames does the refusing, but it only sees the
// ambiguities it is built to see: a table rename is a candidate only
// when the two tables still agree on half their column names, and a
// column rename only when the two types belong to one family. A table
// whose every column was renamed with it, and a column renamed across
// a type family, fall through both — and come out of the generator as
// the destructive pair, silently.
//
// Nothing can tell those apart from a genuine drop-and-add; that is
// what makes them ambiguous. What the analyser can do is say what
// shape the migration has, and let the reader — who knows which it was
// — decide.
//
// Loudness is the whole design question here. The obvious move is to
// grade the pair an error, and it is the wrong one: DROP TABLE is
// already an error and DROP COLUMN already a warning, so a second
// finding at that level on the same statements adds urgency to nothing
// and becomes the first rule people put in Ignore. These are graded
// info, and they earn even that by needing *both* halves — a lone drop
// says nothing, because a warning that fires on every genuine drop is
// one people learn to skip.

// analyzePairs runs the rules that read the migration as a whole
// rather than one statement at a time.
func analyzePairs(stmts []string) []SafetyWarning {
	var out []SafetyWarning
	out = append(out, pairTableRename(stmts)...)
	out = append(out, pairColumnRename(stmts)...)
	return out
}

// ident matches one SQL identifier, quoted or bare, optionally
// schema-qualified. Good enough for the names drops itself emits, and
// a name it fails to parse simply produces no finding.
const identPat = `(?:"[^"]*"|[A-Za-z_][\w$]*)(?:\.(?:"[^"]*"|[A-Za-z_][\w$]*))?`

var (
	reDropTableName   = regexp.MustCompile(`(?i)\bDROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?(` + identPat + `)`)
	reCreateTableName = regexp.MustCompile(`(?i)\bCREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(` + identPat + `)`)
	reRenameToNames   = regexp.MustCompile(`(?i)\bALTER\s+TABLE\s+(?:ONLY\s+)?(` + identPat + `)\s+RENAME\s+TO\s+(` + identPat + `)`)
	reDropColumnName  = regexp.MustCompile(`(?i)\bALTER\s+TABLE\s+(?:ONLY\s+)?(` + identPat + `)\s+DROP\s+COLUMN\s+(?:IF\s+EXISTS\s+)?(` + identPat + `)`)
	reAddColumnName   = regexp.MustCompile(`(?i)\bALTER\s+TABLE\s+(?:ONLY\s+)?(` + identPat + `)\s+ADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?(` + identPat + `)`)
)

// unquoteIdent strips the quoting from an identifier so two spellings
// of one name compare equal, and so a message reads as prose.
func unquoteIdent(s string) string {
	return strings.ReplaceAll(s, `"`, "")
}

// pairTableRename reports each DROP TABLE that shares its migration
// with a CREATE TABLE of some other name.
//
// A drop and a create of the *same* name is a rebuild, not a rename:
// nothing moved, so there is nothing to have lost.
//
// So is the longer form, and it is the one that matters — SQLite's
// table rebuild is literally CREATE "t_new", copy, DROP "t", RENAME
// "t_new" TO "t", which every column type change produces. A rule that
// fired there would fire on most migrations that dialect writes and be
// suppressed within the week. A create whose table is renamed away, and
// a drop whose name something is renamed into, are both accounted for
// by a rename the migration states out loud, so neither is a lost one.
func pairTableRename(stmts []string) []SafetyWarning {
	renamedFrom := map[string]bool{}
	renamedTo := map[string]bool{}
	for _, s := range stmts {
		if m := reRenameToNames.FindStringSubmatch(s); m != nil {
			renamedFrom[unquoteIdent(m[1])] = true
			renamedTo[unquoteIdent(m[2])] = true
		}
	}
	var created []string
	for _, s := range stmts {
		if m := reCreateTableName.FindStringSubmatch(s); m != nil {
			if name := unquoteIdent(m[1]); !renamedFrom[name] {
				created = append(created, name)
			}
		}
	}
	if len(created) == 0 {
		return nil
	}
	var out []SafetyWarning
	for _, s := range stmts {
		m := reDropTableName.FindStringSubmatch(s)
		if m == nil {
			continue
		}
		dropped := unquoteIdent(m[1])
		if renamedTo[dropped] {
			continue
		}
		others := excluding(created, dropped)
		if len(others) == 0 {
			continue
		}
		out = append(out, SafetyWarning{
			Severity:  SeverityInfo,
			Rule:      "unstated-table-rename",
			Statement: s,
			Message: "this migration drops " + dropped + " and creates " + humanList(others) +
				". That is the shape a rename makes when nobody stated it: a stated rename carries the rows over, a drop and a create does not.",
			Suggestion: "If one of the new tables is " + dropped + " under a new name, state the rename (--rename-table " + dropped +
				"=NEW, or answer the generator's prompt) so the migration renames instead of destroying. If it really is a drop, silence this with SafetyOptions{Ignore: []string{\"unstated-table-rename\"}}.",
		})
	}
	return out
}

// pairColumnRename reports each table that loses a column and gains one
// in the same migration.
func pairColumnRename(stmts []string) []SafetyWarning {
	type pair struct {
		dropped, added []string
		stmt           string
	}
	byTable := map[string]*pair{}
	var order []string
	get := func(t string) *pair {
		p, ok := byTable[t]
		if !ok {
			p = &pair{}
			byTable[t] = p
			order = append(order, t)
		}
		return p
	}
	for _, s := range stmts {
		if m := reDropColumnName.FindStringSubmatch(s); m != nil {
			p := get(unquoteIdent(m[1]))
			p.dropped = append(p.dropped, unquoteIdent(m[2]))
			if p.stmt == "" {
				p.stmt = s
			}
			continue
		}
		if m := reAddColumnName.FindStringSubmatch(s); m != nil {
			p := get(unquoteIdent(m[1]))
			p.added = append(p.added, unquoteIdent(m[2]))
		}
	}
	var out []SafetyWarning
	for _, t := range order {
		p := byTable[t]
		if len(p.dropped) == 0 || len(p.added) == 0 {
			continue
		}
		out = append(out, SafetyWarning{
			Severity:  SeverityInfo,
			Rule:      "unstated-column-rename",
			Statement: p.stmt,
			Message: "this migration drops " + humanList(p.dropped) + " from " + t + " and adds " + humanList(p.added) +
				" to it. That is the shape a rename makes when nobody stated it — and a rename detector that pairs columns by type family will not have asked about a pair whose types are in different families.",
			Suggestion: "If one of the added columns is a dropped one under a new name, state the rename (--rename-column " + t + "." + p.dropped[0] +
				"=NEW, or answer the generator's prompt) so the data comes with it. If they really are different columns, silence this with SafetyOptions{Ignore: []string{\"unstated-column-rename\"}}.",
		})
	}
	return out
}

// excluding returns the entries of xs that are not name.
func excluding(xs []string, name string) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if x != name {
			out = append(out, x)
		}
	}
	return out
}

// humanList renders names as "a", "a and b", "a, b and c".
func humanList(xs []string) string {
	switch len(xs) {
	case 0:
		return ""
	case 1:
		return xs[0]
	case 2:
		return xs[0] + " and " + xs[1]
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
}
