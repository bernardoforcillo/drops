package sqlite

import (
	"regexp"
	"strings"
)

// Pre-flight safety analysis for SQLite migration SQL. The rules are
// tuned to SQLite's specific foot-guns, which differ from PostgreSQL's:
// SQLite's ALTER TABLE only supports RENAME, ADD COLUMN and DROP COLUMN,
// and it rejects some ADD COLUMN shapes outright.
//
//	for _, w := range sqlite.AnalyzeMigration(sql) {
//	    log.Printf("%s [%s] %s", w.Severity, w.Rule, w.Message)
//	}

// SafetySeverity ranks a warning's urgency.
type SafetySeverity int

const (
	// SeverityInfo is a heads-up — usually fine.
	SeverityInfo SafetySeverity = iota
	// SeverityWarn flags a likely behaviour change or a rewrite.
	SeverityWarn
	// SeverityError flags a statement that destroys data or that SQLite
	// will reject at execution time.
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
	Severity   SafetySeverity
	Rule       string
	Statement  string
	Message    string
	Suggestion string
}

// SafetyOptions tunes the analyser — currently rule suppression.
type SafetyOptions struct {
	// Ignore lists rule IDs to skip (known-safe migrations).
	Ignore []string
}

// AnalyzeMigration splits a migration script on the drizzle-kit
// "--> statement-breakpoint" boundary and analyses each statement.
func AnalyzeMigration(sql string, opts ...SafetyOptions) []SafetyWarning {
	return AnalyzeStatements(splitStatements(sql), opts...)
}

// AnalyzeStatements runs the safety rules against each statement in
// order, preserving statement order in the output.
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
// "statement-breakpoint" boundary, stripping trailing semicolons.
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

// safetyRules is the rule set in declaration order.
var safetyRules = []func(stmt string) (SafetyWarning, bool){
	ruleDropTable,
	ruleDropColumn,
	ruleRenameTable,
	ruleRenameColumn,
	ruleAddColumnNotNullNoDefault,
	ruleAddColumnDynamicDefault,
	ruleDeleteWithoutWhere,
	ruleUpdateWithoutWhere,
	ruleRebuildDropsIndex,
	ruleRebuildStaleTrigger,
	ruleRebuildLosesIndexes,
}

var (
	reDropTable      = regexp.MustCompile(`(?i)\bDROP\s+TABLE\b`)
	reDropColumn     = regexp.MustCompile(`(?i)\bALTER\s+TABLE\b.*\bDROP\s+COLUMN\b`)
	reRenameTable    = regexp.MustCompile(`(?i)\bALTER\s+TABLE\b.*\bRENAME\s+TO\b`)
	reRenameColumn   = regexp.MustCompile(`(?i)\bALTER\s+TABLE\b.*\bRENAME\s+COLUMN\b`)
	reAddColumn      = regexp.MustCompile(`(?i)\bALTER\s+TABLE\b.*\bADD\s+COLUMN\b`)
	reHasNotNull     = regexp.MustCompile(`(?i)\bNOT\s+NULL\b`)
	reHasDefault     = regexp.MustCompile(`(?i)\bDEFAULT\b`)
	reDynamicDefault = regexp.MustCompile(`(?i)\bDEFAULT\s+(CURRENT_TIME|CURRENT_DATE|CURRENT_TIMESTAMP|\()`)
	reIndexDropped   = regexp.MustCompile(`(?i)^--\s*index\s+".*"\s+dropped with column\b`)
	reTriggerStale   = regexp.MustCompile(`(?i)^--\s*trigger\s+".*"\s+names dropped column\b`)
	reBlindRebuild   = regexp.MustCompile(`(?i)^--\s*this rebuild drops the table, and with it every index and trigger\b`)
	reDelete         = regexp.MustCompile(`(?i)^\s*DELETE\s+FROM\b`)
	reUpdate         = regexp.MustCompile(`(?i)^\s*UPDATE\b`)
	reHasWhere       = regexp.MustCompile(`(?i)\bWHERE\b`)
)

func ruleDropTable(stmt string) (SafetyWarning, bool) {
	if !reDropTable.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityError,
		Rule:       "drop-table",
		Statement:  stmt,
		Message:    "DROP TABLE destroys data irreversibly.",
		Suggestion: "Rename the table aside (ALTER TABLE ... RENAME TO _archived_x) and drop it in a later migration after a retention window.",
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
		Message:    "DROP COLUMN is irreversible and breaks any code still referencing the column (and requires SQLite 3.35+).",
		Suggestion: "Stop writing to the column, deploy, then drop it in a follow-up migration — and back up the data if it matters.",
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
		Message:    "RENAME TABLE breaks code referring to the old name during a rolling deploy.",
		Suggestion: "Create a view with the old name over the new table, deploy, then drop the view later.",
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
		Message:    "RENAME COLUMN breaks code referring to the old name during a rolling deploy.",
		Suggestion: "Add a new column, dual-write, switch reads, then drop the old column — avoid renames across deploy boundaries.",
	}, true
}

func ruleAddColumnNotNullNoDefault(stmt string) (SafetyWarning, bool) {
	if !reAddColumn.MatchString(stmt) || !reHasNotNull.MatchString(stmt) || reHasDefault.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityError,
		Rule:       "add-not-null-column-without-default",
		Statement:  stmt,
		Message:    "SQLite rejects ADD COLUMN ... NOT NULL without a DEFAULT unless the table is empty — the migration will fail on a populated table.",
		Suggestion: "Add the column nullable, backfill it, then rebuild the table to enforce NOT NULL — or add it NOT NULL with a constant DEFAULT.",
	}, true
}

func ruleAddColumnDynamicDefault(stmt string) (SafetyWarning, bool) {
	if !reAddColumn.MatchString(stmt) || !reDynamicDefault.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityError,
		Rule:       "add-column-dynamic-default",
		Statement:  stmt,
		Message:    "SQLite forbids ADD COLUMN with a non-constant DEFAULT (CURRENT_TIMESTAMP, a parenthesised expression, etc.) — the statement will be rejected.",
		Suggestion: "Add the column with a constant DEFAULT (or nullable), then backfill the dynamic value with an UPDATE.",
	}, true
}

// ruleRebuildDropsIndex surfaces the index a table rebuild could not
// keep. Diff already records it as a comment in the migration, but a
// comment is easy to scroll past, and an index that silently stops
// existing is the difference between a query plan and a table scan.
func ruleRebuildDropsIndex(stmt string) (SafetyWarning, bool) {
	if !reIndexDropped.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityWarn,
		Rule:       "rebuild-drops-index",
		Statement:  stmt,
		Message:    "The table rebuild drops this index along with the column it keys, and will not re-create it.",
		Suggestion: "If the index is still wanted over the remaining columns, add the CREATE INDEX to this migration by hand.",
	}, true
}

// ruleRebuildStaleTrigger surfaces a trigger a rebuild put back even
// though it names a column that is now gone. SQLite accepts the CREATE
// TRIGGER regardless — it does not resolve the body until the trigger
// fires — so nothing else in the pipeline can catch this.
func ruleRebuildStaleTrigger(stmt string) (SafetyWarning, bool) {
	if !reTriggerStale.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityError,
		Rule:       "rebuild-stale-trigger",
		Statement:  stmt,
		Message:    "This trigger is re-created naming a column the rebuild removed; SQLite accepts it now and fails when it fires.",
		Suggestion: "Rewrite the trigger against the new shape in this migration, or drop it.",
	}, true
}

// ruleRebuildLosesIndexes surfaces a rebuild in a generated migration,
// where drops has only snapshot files to work from and those record no
// index and no trigger. Push replays what Introspect read; this path
// has nothing to replay, and the reviewer is the only one who can fill
// the gap in.
func ruleRebuildLosesIndexes(stmt string) (SafetyWarning, bool) {
	if !reBlindRebuild.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityWarn,
		Rule:       "rebuild-loses-indexes",
		Statement:  stmt,
		Message:    "This rebuild destroys every index and trigger on the table, and a generated migration cannot re-create them.",
		Suggestion: "Read the table's indexes and triggers out of sqlite_master on the target database and append their CREATE statements to this migration.",
	}, true
}

func ruleDeleteWithoutWhere(stmt string) (SafetyWarning, bool) {
	if !reDelete.MatchString(stmt) || reHasWhere.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityError,
		Rule:       "delete-without-where",
		Statement:  stmt,
		Message:    "DELETE without WHERE removes every row in the table.",
		Suggestion: "Add a WHERE clause, or if a full wipe is intended, accept this warning via SafetyOptions.Ignore.",
	}, true
}

func ruleUpdateWithoutWhere(stmt string) (SafetyWarning, bool) {
	if !reUpdate.MatchString(stmt) || reHasWhere.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityWarn,
		Rule:       "update-without-where",
		Statement:  stmt,
		Message:    "UPDATE without WHERE rewrites every row in the table.",
		Suggestion: "Add a WHERE clause, or accept this warning via SafetyOptions.Ignore if a table-wide update is intended.",
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
// The column rule reaches less far here than it does on the other
// dialects, and it is worth saying which part. SQLite has no ALTER
// COLUMN and drops never emits ALTER TABLE ... DROP COLUMN, so a lost
// column rename in a migration Diff wrote is a rebuild that copies
// every column except the one that mattered — the drop and the add are
// inside a CREATE TABLE and an INSERT … SELECT, where a statement
// matcher cannot see them as a pair. The rule still earns its place:
// AnalyzeStatements takes any SQL, and hand-written ALTER TABLE ...
// DROP COLUMN is what SQLite 3.35 exists for.
//
// Loudness is the whole design question here. The obvious move is to
// grade the pair an error, and it is the wrong one: DROP TABLE is
// already an error here and DROP COLUMN already a warning, so a second
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
