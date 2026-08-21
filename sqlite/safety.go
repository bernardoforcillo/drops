package sqlite

import (
	"regexp"
	"strconv"
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

// ----------------------------------------------------------------------
// What the statements cannot say
// ----------------------------------------------------------------------

// Everything above reads SQL, and on this dialect reading SQL is not
// enough.
//
// A destructive change in PostgreSQL is a destructive statement — DROP
// COLUMN, DROP TABLE, TRUNCATE — so an analyser that classifies DDL
// text finds every one of them, and drops/pg's does. SQLite has no
// ALTER COLUMN and drops emits no ALTER TABLE ... DROP COLUMN, so the
// same change arrives as a table rebuild: CREATE "t_new", INSERT ...
// SELECT, DROP TABLE, ALTER TABLE ... RENAME TO. All four statements
// appear unchanged in a rebuild that loses nothing whatsoever, and the
// column that went missing is missing from the INSERT's column list —
// which is to say no statement names it at all.
//
// Diff does write a `-- rebuild "t": drop column "c"` comment above the
// sequence, and that is not a way round this. The comment is there
// because Diff read the diff; the fact travels out of the diff and into
// the text, never the other way. So this is where it is read: this
// column is in the previous snapshot and not in the next one.
// DestructiveChanges reports it from there, and Push carries the answer
// out rather than trying to reconstruct it from what it emitted.

// DestructiveChange is one thing evolving a schema would destroy, named
// from the diff rather than from the statements.
//
// A rebuild is the whole of SQLite's schema-change vocabulary, so the
// same four statements carry both the safe changes and these; nothing
// downstream can tell them apart. That is why this is a value the diff
// produces rather than a verdict on SQL somebody else can re-derive.
type DestructiveChange struct {
	// Rule is a stable identifier for the kind of change. The names are
	// shared with the statement analyser above and with drops/pg
	// wherever the meaning is the same, so drop-column means here what
	// it means there: "drop-table", "drop-column",
	// "alter-column-type", "alter-column-set-not-null",
	// "add-unique-constraint", "add-check-constraint",
	// "rebuild-drops-index", "rebuild-stale-trigger".
	Rule string

	// Table is the table the change is on, under the name it has after
	// any stated rename.
	Table string

	// Object is the column, index or trigger the change is about, empty
	// when the whole table is going.
	Object string

	// Message says what would be destroyed and how.
	Message string

	// Suggestion is how to keep it, or how to say the loss is intended.
	Suggestion string
}

// String renders the change as "rule: message".
func (c DestructiveChange) String() string { return c.Rule + ": " + c.Message }

// DestructiveChanges reports what evolving prev into cur would destroy.
//
// It is the diff read for loss rather than for statements, and it takes
// the same DiffOptions for the same reason Diff does: a stated rename
// is not a drop, so the renames are applied to prev before anything is
// compared. Pass exactly what you pass Diff and the two agree on what
// the migration means.
//
// Three rules are ones whose answer this function cannot finish:
// alter-column-set-not-null, add-unique-constraint and
// add-check-constraint. Whether a column that gains NOT NULL has any
// NULL to carry, whether one that gains UNIQUE holds a value twice,
// whether a new CHECK has a row that breaks it — each is a fact about
// the rows and not about either schema. The rules report every such
// candidate and leave confirming it to the caller that holds a
// database — see Push, which asks and discards the ones the data
// acquits.
func DestructiveChanges(prev, cur *Snapshot, opts ...DiffOptions) []DestructiveChange {
	var opt DiffOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	if prev == nil {
		prev = EmptySnapshot()
	}
	if cur == nil {
		cur = EmptySnapshot()
	}
	prev = applyRenames(prev, opt.Renames)

	var out []DestructiveChange
	for _, key := range sortedMapKeys(prev.Tables) {
		prevT := prev.Tables[key]
		curT, ok := cur.Tables[key]
		if !ok {
			out = append(out, DestructiveChange{
				Rule:       "drop-table",
				Table:      prevT.Name,
				Message:    "table " + quoteIdent(prevT.Name) + " is in the database and not in the schema; the table and every row in it go.",
				Suggestion: "If the table is really going, say so with PushOptions.AllowDestructive. Otherwise rename it aside and drop it in a later migration, once a retention window has passed.",
			})
			continue
		}
		out = append(out, tableLosses(prevT, curT)...)
	}
	return out
}

// tableLosses reports what rebuilding prev into cur costs the table.
//
// The index and trigger rules mirror replayObjectsSQL exactly, because
// they are describing what it does: an index keyed on a column that is
// going is not replayed, and a trigger whose body names one is replayed
// and will fail when it fires. Both only arise where a column is going,
// which is why they are computed under that condition — a rebuild that
// only widens a table gets all of them back untouched and has lost
// nothing.
func tableLosses(prev, cur *TableSnapshot) []DestructiveChange {
	var out []DestructiveChange
	var dropped []string
	gone := map[string]bool{}
	for _, k := range sortedMapKeys(prev.Columns) {
		if _, ok := cur.Columns[k]; ok {
			continue
		}
		dropped = append(dropped, k)
		gone[k] = true
		out = append(out, DestructiveChange{
			Rule:    "drop-column",
			Table:   cur.Name,
			Object:  k,
			Message: "column " + quoteIdent(k) + " of " + quoteIdent(cur.Name) + " is in the database and not in the schema; the rebuild copies the columns both sides name, so the column's data goes with no DROP COLUMN anywhere in the statement list to say so.",
			Suggestion: "If the column was renamed rather than dropped, state the rename — sqlite.Text(\"newName\").RenamedFrom(" + strconv.Quote(k) +
				") — and the data comes with it. If it really is going, back it up and set PushOptions.AllowDestructive.",
		})
	}
	for _, k := range sortedMapKeys(cur.Columns) {
		prevC, ok := prev.Columns[k]
		if !ok {
			continue
		}
		curC := cur.Columns[k]
		if narrowsAffinity(prevC.Type, curC.Type) {
			out = append(out, DestructiveChange{
				Rule:   "alter-column-type",
				Table:  cur.Name,
				Object: k,
				Message: "column " + quoteIdent(k) + " of " + quoteIdent(cur.Name) + " changes declared type " + prevC.Type + " -> " + curC.Type +
					", from TEXT affinity to " + typeAffinity(curC.Type) + "; the rebuild's INSERT ... SELECT applies the new affinity as it copies, so stored text that reads as a number is converted to one — '007' comes back as 7.",
				Suggestion: "Add a second column of the new type, copy into it and check what the conversion did, then drop the old one. If the conversion is what you want, set PushOptions.AllowDestructive.",
			})
		}
		if !prevC.NotNull && curC.NotNull {
			out = append(out, DestructiveChange{
				Rule:       "alter-column-set-not-null",
				Table:      cur.Name,
				Object:     k,
				Message:    "column " + quoteIdent(k) + " of " + quoteIdent(cur.Name) + " gains NOT NULL; the rebuild's INSERT ... SELECT carries every existing NULL into a column that rejects it, so the push fails partway rather than converging.",
				Suggestion: "Backfill the NULLs first — UPDATE " + quoteIdent(cur.Name) + " SET " + quoteIdent(k) + " = ... WHERE " + quoteIdent(k) + " IS NULL — and push after.",
			})
		}
	}
	out = append(out, tightenings(prev, cur)...)
	if len(dropped) == 0 {
		return out
	}
	for _, name := range sortedMapKeys(prev.Indexes) {
		idx := prev.Indexes[name]
		if idx.SQL == "" {
			continue
		}
		var lost []string
		for _, c := range idx.Columns {
			if gone[c] {
				lost = append(lost, quoteIdent(c))
			}
		}
		if len(lost) == 0 {
			continue
		}
		out = append(out, DestructiveChange{
			Rule:       "rebuild-drops-index",
			Table:      cur.Name,
			Object:     name,
			Message:    "index " + quoteIdent(name) + " keys column " + strings.Join(lost, ", ") + ", which this change removes; the rebuild drops the table and does not put the index back.",
			Suggestion: "If the index is still wanted over the columns that remain, declare it by hand and create it after the push. To lose it, set PushOptions.AllowDestructive.",
		})
	}
	for _, name := range sortedMapKeys(prev.Triggers) {
		trg := prev.Triggers[name]
		if trg.SQL == "" {
			continue
		}
		named := identsMentioned(trg.SQL, dropped)
		if len(named) == 0 {
			continue
		}
		out = append(out, DestructiveChange{
			Rule:       "rebuild-stale-trigger",
			Table:      cur.Name,
			Object:     name,
			Message:    "trigger " + quoteIdent(name) + " names column " + strings.Join(quoteIdentList(named), ", ") + ", which this change removes; the rebuild re-creates the trigger as it was stored, SQLite accepts it without resolving the body, and it fails the first time it fires.",
			Suggestion: "Rewrite the trigger against the new shape, or drop it, before pushing. To leave it broken, set PushOptions.AllowDestructive.",
		})
	}
	return out
}

// tightenings reports the constraints cur adds that the rows already in
// the table may not satisfy: a UNIQUE over values that repeat, a CHECK
// over rows that break it.
//
// They are the same shape as alter-column-set-not-null and they fail
// the same way. Nothing is destroyed — SQLite refuses the rebuild's
// INSERT ... SELECT and the transaction rolls back whole — but the
// push dies partway through a rebuild on a constraint message, which
// is precisely the failure the NOT NULL rule exists to turn into a
// readable refusal. Leaving these two out left the rule set covering
// less than what a rebuild costs while the documentation said it
// covered it.
//
// Like that rule, these are candidates rather than findings: whether
// the data actually contradicts the constraint is not in either
// snapshot. Push puts each one to the database and drops the ones the
// rows acquit, so declaring a column unique that has held distinct
// values all along is not a refusal demanding the override by reflex.
func tightenings(prev, cur *TableSnapshot) []DestructiveChange {
	var out []DestructiveChange
	// A single-column UNIQUE renders inline on the column, a
	// multi-column one as a table-level constraint, and the two are
	// the same constraint as far as the rows are concerned — so both
	// arrive under one rule, named by the column and by the constraint
	// respectively, and both are matched against the same set.
	//
	// That set is column tuples rather than names, for the reason
	// sameUniqueKeys gives: SQLite stores a UNIQUE constraint as an
	// anonymous index and reports it under a generated name, so a
	// snapshot read out of a database never carries the name the schema
	// gave it. Matching by name would call a constraint that has been
	// enforced all along a new one, on every push.
	was := map[string]bool{}
	for _, tuple := range uniqueKeyTuples(prev) {
		was[tuple] = true
	}
	for _, k := range sortedMapKeys(cur.Columns) {
		c := cur.Columns[k]
		if !c.Unique || c.PrimaryKey || was[k] {
			continue
		}
		out = append(out, uniqueTightening(cur.Name, k, []string{k}))
	}
	for _, name := range sortedMapKeys(cur.UniqueConstraints) {
		u := cur.UniqueConstraints[name]
		if was[strings.Join(u.Columns, "\x00")] {
			continue
		}
		out = append(out, uniqueTightening(cur.Name, name, u.Columns))
	}
	// A CHECK has no such trouble with names, and a different trouble
	// instead: Introspect does not read SQLite's catalogue for CHECK
	// constraints at all, so against a live database prev never has
	// one and every declared CHECK arrives here as a candidate on every
	// push. The probe settles it — a constraint the table already
	// enforces has no row that breaks it — at the cost of one count
	// query per push.
	for _, name := range sortedMapKeys(cur.CheckConstraints) {
		c := cur.CheckConstraints[name]
		if prevC, ok := prev.CheckConstraints[name]; ok && prevC.Value == c.Value {
			continue
		}
		out = append(out, DestructiveChange{
			Rule:   "add-check-constraint",
			Table:  cur.Name,
			Object: name,
			Message: "CHECK " + quoteIdent(name) + " on " + quoteIdent(cur.Name) + " is new; the rebuild's INSERT ... SELECT " +
				"carries every existing row through it, so a row that breaks it fails the push partway rather than converging.",
			Suggestion: "Find the rows first — SELECT * FROM " + quoteIdent(cur.Name) + " WHERE NOT (" + c.Value + ") — and fix or delete them before pushing.",
		})
	}
	return out
}

func uniqueTightening(table, object string, columns []string) DestructiveChange {
	spans := quoteIdent(columns[0])
	if len(columns) > 1 {
		spans = "(" + strings.Join(quoteIdentList(columns), ", ") + ")"
	}
	return DestructiveChange{
		Rule:   "add-unique-constraint",
		Table:  table,
		Object: object,
		Message: "UNIQUE " + quoteIdent(object) + " on " + quoteIdent(table) + " is new over " + spans +
			"; the rebuild's INSERT ... SELECT carries every existing row through it, so a value already held twice fails the push partway rather than converging.",
		Suggestion: "Find the duplicates first — SELECT " + strings.Join(quoteIdentList(columns), ", ") + ", count(*) FROM " + quoteIdent(table) +
			" GROUP BY " + strings.Join(quoteIdentList(columns), ", ") + " HAVING count(*) > 1 — and resolve them before pushing.",
	}
}

// narrowsAffinity reports whether re-declaring a column from to to
// changes what the engine will store.
//
// The test is deliberately narrower than "the type changed", because
// most type changes on SQLite cost nothing: the engine is dynamically
// typed, a column's declared type only sets an affinity, and an
// affinity only converts a value when the conversion is one it considers
// faithful. A REAL 1.5 copied into an INTEGER column stays 1.5, and a
// BLOB copied anywhere stays a BLOB — the engine says so, and the
// integration suite asks it.
//
// One direction does convert: text into a numeric affinity. A column
// declared TEXT holding '007' becomes the integer 7 the moment the
// rebuild copies it into a column with INTEGER, REAL or NUMERIC
// affinity, and nothing about the migration says it happened. That is
// the case worth stopping for, and the only one this reports.
//
// Note that NUMERIC is where SQLite's rules put every declared type it
// does not otherwise recognise, DATE, DATETIME and BOOLEAN included, so
// re-declaring a Text column as a Timestamp lands here too. It should:
// that conversion is the same conversion.
func narrowsAffinity(from, to string) bool {
	if typeAffinity(from) != "TEXT" {
		return false
	}
	switch typeAffinity(to) {
	case "INTEGER", "REAL", "NUMERIC":
		return true
	}
	return false
}

// typeAffinity applies SQLite's declared-type rules, in their order,
// to work out which of the five affinities a declared type carries.
// See https://sqlite.org/datatype3.html#determination_of_column_affinity.
func typeAffinity(declared string) string {
	t := strings.ToUpper(strings.TrimSpace(declared))
	switch {
	case strings.Contains(t, "INT"):
		return "INTEGER"
	case strings.Contains(t, "CHAR"), strings.Contains(t, "CLOB"), strings.Contains(t, "TEXT"):
		return "TEXT"
	case t == "", strings.Contains(t, "BLOB"):
		return "BLOB"
	case strings.Contains(t, "REAL"), strings.Contains(t, "FLOA"), strings.Contains(t, "DOUB"):
		return "REAL"
	}
	return "NUMERIC"
}
