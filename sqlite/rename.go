package sqlite

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// Renames.
//
// A structural diff compares two snapshots and can say what is in one
// and not the other. It cannot say why. A column called "email" in the
// old snapshot and one called "emailAddress" in the new one is either a
// rename — the same column, the same data, a different name — or a drop
// and an add, and the two produce migrations that differ by the whole
// contents of the column. Nothing in either snapshot distinguishes
// them, so no amount of care in Diff can either.
//
// So drops does not guess. DetectRenames finds the ambiguous pairs,
// GenerateMigration refuses while any of them is unanswered, and the
// answers are recorded in meta/_renames.json so the question is asked
// once and replayed afterwards.
//
// SQLite is the dialect where this matters most and shows least. Most
// column changes here go through a table rebuild — create the new
// shape, copy, drop, rename — and a rebuild that has not been told
// about a rename copies nothing into the new column. The old table is
// dropped in the same breath, so the data is gone with no DROP COLUMN
// anywhere in the migration to warn anybody. Stating the rename puts an
// ALTER TABLE ... RENAME COLUMN in front of the rebuild, and the copy
// then finds the column under the name it is looking for.

// RenameKind says which kind of object a Rename is about.
type RenameKind string

const (
	// RenameTable is ALTER TABLE ... RENAME TO.
	RenameTable RenameKind = "table"
	// RenameColumn is ALTER TABLE ... RENAME COLUMN ... TO, which
	// SQLite has had since 3.25.
	RenameColumn RenameKind = "column"
)

// Rename states that the object named To in the new schema is the
// object named From in the old one.
//
// For a column rename, Table is the table holding it. Table renames are
// applied before column renames, so a column rename inside a table that
// is itself being renamed names the table by its new name.
type Rename struct {
	Kind  RenameKind `json:"kind"`
	Table string     `json:"table,omitempty"`
	From  string     `json:"from"`
	To    string     `json:"to"`
}

// String renders the rename the way the CLI flag that states it is
// spelled: "users.email=emailAddress" or "users=people".
func (r Rename) String() string {
	if r.Kind == RenameColumn {
		return fmt.Sprintf("%s.%s=%s", r.Table, r.From, r.To)
	}
	return fmt.Sprintf("%s=%s", r.From, r.To)
}

// key identifies the question a Rename answers, so a recorded decision
// can be looked up by the candidate that provoked it.
func (r Rename) key() string {
	return string(r.Kind) + "\x00" + r.Table + "\x00" + r.From + "\x00" + r.To
}

// RenameCandidate is a (drop, add) pair that Diff cannot tell apart
// from a rename: same table, compatible types, one name going and one
// arriving.
type RenameCandidate struct {
	Rename
	FromType string `json:"fromType,omitempty"`
	ToType   string `json:"toType,omitempty"`
}

// RenameDecision is an answer to one candidate. IsRename false is a
// real answer — "no, that column really is being dropped" — and is
// recorded so the question is not asked again.
//
// A declining decision may leave To empty, and then it answers every
// candidate that proposed the object named by From, whatever the
// candidate proposed it became. That is the shape the answer has when
// it is given: "users.email really is going" is one fact about one
// column, and making a person enumerate the columns it might have been
// mistaken for would be asking them to answer a question they were
// never asked.
type RenameDecision struct {
	Rename
	IsRename bool `json:"rename"`
}

// RenameAmbiguityError is what GenerateMigration returns instead of a
// migration when a rename candidate has no recorded answer.
type RenameAmbiguityError struct {
	// Candidates are the unanswered pairs, in a deterministic order.
	Candidates []RenameCandidate

	// Advice replaces everything the message says about how to answer
	// — the per-candidate flag lines and the closing note about the
	// rename log alike — for a caller neither of them fits. Push is
	// the one: it compares a Go schema against a live database, there
	// is no migration directory anywhere in that, and the flags belong
	// to a command that generates migrations. Sending a person to
	// either would send them somewhere that does not exist.
	//
	// The candidates are still named the same way. It is only the two
	// ways out that differ, and a message that names the wrong two is
	// worse than one that names none.
	//
	// Empty means the generator's own wording.
	Advice string
}

// Error names each ambiguity and both ways out of it.
func (e *RenameAmbiguityError) Error() string {
	var b strings.Builder
	b.WriteString("drops/sqlite: this schema change could be a rename or a drop-and-add and drops will not guess:\n")
	for _, c := range e.Candidates {
		if c.Kind == RenameColumn {
			fmt.Fprintf(&b, "  column %q on table %q is gone and %q (%s) has appeared\n",
				c.From, c.Table, c.To, c.ToType)
			if e.Advice == "" {
				fmt.Fprintf(&b, "    rename it:      --rename-column %s.%s=%s\n", c.Table, c.From, c.To)
				fmt.Fprintf(&b, "    or drop it:     --drop-column %s.%s\n", c.Table, c.From)
			}
			continue
		}
		fmt.Fprintf(&b, "  table %q is gone and %q has appeared with much the same columns\n", c.From, c.To)
		if e.Advice == "" {
			fmt.Fprintf(&b, "    rename it:      --rename-table %s=%s\n", c.From, c.To)
			fmt.Fprintf(&b, "    or drop it:     --drop-table %s\n", c.From)
		}
	}
	if e.Advice != "" {
		b.WriteString(e.Advice)
		return b.String()
	}
	b.WriteString("the answer is written to meta/_renames.json, so it is given once and replayed after that")
	return b.String()
}

// DetectRenames returns every (drop, add) pair in the move from prev to
// cur that could be a rename.
//
// A column pair qualifies when the two declared types belong to the same
// family: identical would miss a rename that retyped the column in the
// same step, and no test at all would ask about a dropped BLOB and an
// added INTEGER. A table pair qualifies when the two agree on at least
// half their columns — see tablesLookAlike.
//
// A column inside a table that is itself a rename candidate is not
// reported here, because until the table question is answered there is
// no table to ask it about: the two snapshots file that table under
// different names. ResolveRenames asks the second question once the
// first has an answer.
//
// The result is sorted, so two runs over the same pair of snapshots ask
// the same questions in the same order..
func DetectRenames(prev, cur *Snapshot) []RenameCandidate {
	if prev == nil {
		prev = EmptySnapshot()
	}
	if cur == nil {
		cur = EmptySnapshot()
	}
	out := detectTableRenames(prev, cur)
	out = append(out, detectColumnRenames(prev, cur)...)
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

// detectTableRenames pairs each table prev has and cur does not with
// each table cur has and prev does not, when the two look alike.
func detectTableRenames(prev, cur *Snapshot) []RenameCandidate {
	var out []RenameCandidate
	var goneTables, newTables []string
	for _, k := range sortedMapKeys(prev.Tables) {
		if _, ok := cur.Tables[k]; !ok {
			goneTables = append(goneTables, k)
		}
	}
	for _, k := range sortedMapKeys(cur.Tables) {
		if _, ok := prev.Tables[k]; !ok {
			newTables = append(newTables, k)
		}
	}
	for _, gk := range goneTables {
		for _, nk := range newTables {
			if !tablesLookAlike(prev.Tables[gk], cur.Tables[nk]) {
				continue
			}
			out = append(out, RenameCandidate{Rename: Rename{
				Kind: RenameTable,
				From: prev.Tables[gk].Name,
				To:   cur.Tables[nk].Name,
			}})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

// detectColumnRenames pairs a gone column with an arrived one, within
// each table both snapshots name.
func detectColumnRenames(prev, cur *Snapshot) []RenameCandidate {
	var out []RenameCandidate
	for _, k := range sortedMapKeys(cur.Tables) {
		prevT, ok := prev.Tables[k]
		if !ok {
			continue
		}
		curT := cur.Tables[k]
		for _, from := range sortedMapKeys(prevT.Columns) {
			if _, still := curT.Columns[from]; still {
				continue
			}
			for _, to := range sortedMapKeys(curT.Columns) {
				if _, had := prevT.Columns[to]; had {
					continue
				}
				fromC, toC := prevT.Columns[from], curT.Columns[to]
				if !renameTypeCompatible(fromC.Type, toC.Type) {
					continue
				}
				out = append(out, RenameCandidate{
					Rename:   Rename{Kind: RenameColumn, Table: curT.Name, From: from, To: to},
					FromType: fromC.Type,
					ToType:   toC.Type,
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

// tablesLookAlike reports whether two tables resemble each other enough
// for one to be the other under a new name: at least half the columns of
// the wider of the two appear in both, under the same name and with a
// compatible type.
//
// A majority rather than an equality, because the alternative to asking
// is dropping the table. Demanding the two carry exactly the same
// columns meant a table renamed in the same commit that added a column
// to it — or renamed a column inside it — resembled nothing, so nothing
// was asked, and the diff dropped the table and built a new one with the
// rows left behind. A question nobody needed costs one --drop-table; a
// DROP TABLE nobody was asked about costs the table.
//
// A table whose every column was also renamed still resembles nothing,
// and drops still cannot tell that from an unrelated drop and add.
func tablesLookAlike(a, b *TableSnapshot) bool {
	wider := len(a.Columns)
	if len(b.Columns) > wider {
		wider = len(b.Columns)
	}
	if wider == 0 {
		return false
	}
	shared := 0
	for name, ac := range a.Columns {
		if bc, ok := b.Columns[name]; ok && renameTypeCompatible(ac.Type, bc.Type) {
			shared++
		}
	}
	return shared > 0 && shared*2 >= wider
}

// renameTypeCompatible reports whether a column of type a could
// plausibly be the same column as one of type b. See typeFamily.
func renameTypeCompatible(a, b string) bool {
	return strings.EqualFold(a, b) || typeFamily(a) == typeFamily(b)
}

// typeFamily reduces a declared SQLite type to the family its values
// live in.
//
// This is deliberately not SQLite's own affinity rule. That rule folds
// BOOLEAN, DATE, DATETIME and NUMERIC into one affinity, which is true
// of how the engine stores them and useless as a test of whether two
// columns could be the same one: it would offer a dropped flag and an
// added timestamp as a rename. The declared type is what the schema
// meant, so the declared type is what is compared.
func typeFamily(t string) string {
	t = strings.ToUpper(strings.TrimSpace(t))
	if i := strings.IndexByte(t, '('); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	switch t {
	case "TEXT", "VARCHAR", "CHAR", "CHARACTER", "CLOB", "NVARCHAR", "NCHAR", "VARYING CHARACTER":
		return "TEXT"
	case "INTEGER", "INT", "TINYINT", "SMALLINT", "MEDIUMINT", "BIGINT", "INT2", "INT8":
		return "INTEGER"
	case "REAL", "FLOAT", "DOUBLE", "DOUBLE PRECISION":
		return "REAL"
	case "NUMERIC", "DECIMAL":
		return "NUMERIC"
	case "BOOLEAN", "BOOL":
		return "BOOLEAN"
	case "DATETIME", "TIMESTAMP":
		return "DATETIME"
	case "BLOB", "":
		return "BLOB"
	}
	return t
}

// DeclaredRenames reads the renames a schema states about itself —
// every (*Col[T]).RenamedFrom and every (*Table).RenamedFrom — in the
// shape ResolveRenames settles questions with.
//
// It exists because Push has nowhere else to look. GenerateMigration
// keeps its answers in meta/_renames.json, beside the snapshots that
// raised the questions; Push compares a Go schema against a live
// database, and there is no migration directory anywhere in that. The
// schema is the only thing both sides of a push have in common, and a
// rename is a fact about the schema's history rather than about one
// operator's terminal, so the schema is the honest place to keep it:
// stated once, it answers the same question against every database the
// schema is ever pushed to.
//
// GenerateMigration reads them as well, so a schema that states a
// rename does not raise the question again on the generate path. What
// it does not do is write them into the log — the log records answers
// somebody gave, and this is not one.
//
// A declared rename that no longer describes a pending change is inert;
// see renameStillPending.
func DeclaredRenames(schema *Schema) []RenameDecision {
	if schema == nil {
		return nil
	}
	var out []RenameDecision
	for _, t := range schema.Tables() {
		if t == nil {
			continue
		}
		if prev := t.PreviousName(); prev != "" {
			out = append(out, RenameDecision{
				Rename:   Rename{Kind: RenameTable, From: prev, To: t.Name()},
				IsRename: true,
			})
		}
		for _, c := range t.Columns() {
			prev := c.PreviousName()
			if prev == "" {
				continue
			}
			// The table is named as the schema names it now, because a
			// column rename is applied after the table rename in front
			// of it has already run.
			out = append(out, RenameDecision{
				Rename:   Rename{Kind: RenameColumn, Table: t.Name(), From: prev, To: c.Name()},
				IsRename: true,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

// ResolveRenames matches recorded decisions against the candidates a
// pair of snapshots produces and returns the renames to apply, plus the
// candidates still unanswered.
//
// The two kinds are settled in turn, tables first, because a column
// rename inside a renamed table is not a visible question until the
// table has an answer: while the two snapshots file the table under
// different names, nothing in it pairs with anything, and the diff that
// followed dropped the column and added the new one with no question
// asked. So the column pass runs against a previous snapshot in which
// the accepted table renames have already happened. A migration that
// renames both therefore asks twice — once about the table, and once,
// after that answer is recorded, about the column inside it.
//
// A decision that names a pair no longer in question is ignored rather
// than rejected: the file accumulates answers, and an answer about a
// migration that has already been generated describes a schema change
// that has already happened.
func ResolveRenames(prev, cur *Snapshot, decisions []RenameDecision) (applied []Rename, unresolved []RenameCandidate) {
	if prev == nil {
		prev = EmptySnapshot()
	}
	if cur == nil {
		cur = EmptySnapshot()
	}
	tables, tablesOpen := resolveOneKind(prev, cur, decisions, RenameTable, detectTableRenames(prev, cur))
	after := applyRenames(prev, tables)
	columns, columnsOpen := resolveOneKind(after, cur, decisions, RenameColumn, detectColumnRenames(after, cur))

	applied = tables
	applied = append(applied, columns...)
	unresolved = tablesOpen
	unresolved = append(unresolved, columnsOpen...)
	return applied, unresolved
}

// resolveOneKind settles the decisions of a single kind against the
// candidates of that kind.
func resolveOneKind(prev, cur *Snapshot, decisions []RenameDecision, kind RenameKind, candidates []RenameCandidate) (applied []Rename, unresolved []RenameCandidate) {
	offered := map[string]bool{}
	for _, c := range candidates {
		offered[c.key()] = true
	}
	// The refusals are read first, because a refusal outranks a rename
	// naming the same object. The two answers are not the same shape: a
	// rename names a pair, a refusal names only the object that is
	// going. So an answer given for this run cannot outrank the
	// schema's standing declaration by carrying the same key — the two
	// keys coincide only when the two agree — and outranks it by naming
	// the same column instead. Without this, "that column really is
	// going" was inaudible against a RenamedFrom saying otherwise, and
	// there was nothing a caller could say to be heard.
	declined := map[string]bool{}
	for _, d := range decisions {
		if d.Kind == kind && !d.IsRename && d.To == "" {
			declined[d.Kind.side(d.Table, d.From)] = true
		}
	}
	answered := map[string]bool{}
	for _, d := range decisions {
		if d.Kind != kind {
			continue
		}
		if !d.IsRename && d.To == "" {
			continue
		}
		if declined[d.Kind.side(d.Table, d.From)] {
			continue
		}
		answered[d.key()] = d.IsRename
		// A rename nobody was asked about is still a rename. The
		// detector decides what to ask, not what is true, and somebody
		// renaming a column and changing its type past what the type
		// test accepts is telling drops something it could not have
		// worked out. Only a rename that still describes a pending
		// change is applied, which is what keeps a recorded answer from
		// an earlier migration from being replayed against a schema
		// that has already moved past it.
		if d.IsRename && !offered[d.key()] && renameStillPending(prev, cur, d.Rename) {
			applied = append(applied, d.Rename)
		}
	}
	// A candidate whose From or To has been claimed by an accepted
	// rename is answered by that: saying "email became emailAddress"
	// says everything there is to say about email.
	claimed := map[string]bool{}
	for _, r := range applied {
		claimed[r.Kind.side(r.Table, r.From)] = true
		claimed[r.Kind.side(r.Table, r.To)] = true
	}
	for _, c := range candidates {
		if answered[c.key()] {
			applied = append(applied, c.Rename)
			claimed[c.Kind.side(c.Table, c.From)] = true
			claimed[c.Kind.side(c.Table, c.To)] = true
		}
	}
	for _, c := range candidates {
		if _, ok := answered[c.key()]; ok {
			continue
		}
		if declined[c.Kind.side(c.Table, c.From)] {
			continue
		}
		if claimed[c.Kind.side(c.Table, c.From)] || claimed[c.Kind.side(c.Table, c.To)] {
			continue
		}
		unresolved = append(unresolved, c)
	}
	return applied, unresolved
}

// renameStillPending reports whether a rename describes a change this
// diff has yet to make: the old name on the previous side, the new name
// on the new side, and the old name gone from it.
//
// This is the test a recorded answer has to pass before it is replayed.
// The log accumulates, so it still holds the answer to a question that
// was settled three migrations ago; applying that one again would emit
// an ALTER TABLE naming a column no database has had since.
func renameStillPending(prev, cur *Snapshot, r Rename) bool {
	switch r.Kind {
	case RenameTable:
		return prev.Tables[r.From] != nil &&
			cur.Tables[r.To] != nil &&
			cur.Tables[r.From] == nil
	case RenameColumn:
		pt, ct := prev.Tables[r.Table], cur.Tables[r.Table]
		if pt == nil || ct == nil {
			return false
		}
		_, had := pt.Columns[r.From]
		_, gained := ct.Columns[r.To]
		_, kept := ct.Columns[r.From]
		return had && gained && !kept
	}
	return false
}

// side names one end of a candidate, so two candidates about the same
// column can be recognised as the same question answered.
func (k RenameKind) side(table, name string) string {
	return string(k) + "\x00" + table + "\x00" + name
}

// validateRenames checks that every rename names objects that exist on
// the side they should, that no name is renamed twice, and that the name
// each rename is moving to is free.
func validateRenames(prev, cur *Snapshot, renames []Rename) error {
	seenFrom := map[string]bool{}
	seenTo := map[string]bool{}
	for _, r := range renames {
		if r.Kind != RenameTable && r.Kind != RenameColumn {
			return fmt.Errorf("drops/sqlite: unknown rename kind %q", r.Kind)
		}
		if seenFrom[r.Kind.side(r.Table, r.From)] {
			return fmt.Errorf("drops/sqlite: %s is renamed twice", r)
		}
		if seenTo[r.Kind.side(r.Table, r.To)] {
			return fmt.Errorf("drops/sqlite: two renames both produce %q", r.To)
		}
		seenFrom[r.Kind.side(r.Table, r.From)] = true
		seenTo[r.Kind.side(r.Table, r.To)] = true
	}

	// Tables first, and against prev rather than against the rewritten
	// copy below, because the rewrite is what an occupied name corrupts:
	// moving a table onto a name already in use overwrites the entry
	// standing there, and the diff then says nothing at all about the
	// table it displaced.
	for _, r := range renames {
		if r.Kind != RenameTable {
			continue
		}
		if prev.Tables[r.From] == nil {
			return fmt.Errorf("drops/sqlite: rename %s: no table %q in the previous snapshot", r, r.From)
		}
		if cur.Tables[r.To] == nil {
			return fmt.Errorf("drops/sqlite: rename %s: no table %q in the new schema", r, r.To)
		}
		if prev.Tables[r.To] != nil {
			return fmt.Errorf("drops/sqlite: rename %s: the previous snapshot already holds a table %q, "+
				"and no server will rename one onto the other; drop or rename that table in a migration of its own first", r, r.To)
		}
	}

	// Table renames land first, so a column rename is checked against
	// the table names cur uses.
	after := applyRenames(prev, tablesOnly(renames))
	for _, r := range renames {
		if r.Kind != RenameColumn {
			continue
		}
		pt := after.Tables[r.Table]
		if pt == nil {
			return fmt.Errorf("drops/sqlite: rename %s: no table %q in the previous snapshot", r, r.Table)
		}
		if _, ok := pt.Columns[r.From]; !ok {
			return fmt.Errorf("drops/sqlite: rename %s: table %q has no column %q in the previous snapshot", r, r.Table, r.From)
		}
		if _, ok := pt.Columns[r.To]; ok {
			return fmt.Errorf("drops/sqlite: rename %s: table %q already has a column %q, "+
				"and no server will rename one onto the other; drop that column in a migration of its own first", r, r.Table, r.To)
		}
		ct := cur.Tables[r.Table]
		if ct == nil {
			return fmt.Errorf("drops/sqlite: rename %s: no table %q in the new schema", r, r.Table)
		}
		if _, ok := ct.Columns[r.To]; !ok {
			return fmt.Errorf("drops/sqlite: rename %s: table %q has no column %q in the new schema", r, r.Table, r.To)
		}
	}
	return nil
}

func tablesOnly(renames []Rename) []Rename {
	var out []Rename
	for _, r := range renames {
		if r.Kind == RenameTable {
			out = append(out, r)
		}
	}
	return out
}

// renameStatements renders the RENAME DDL, tables before columns.
//
// Both spellings are ALTER TABLE forms SQLite supports without a
// rebuild — RENAME TO from the beginning, RENAME COLUMN since 3.25 —
// and both go out in front of whatever else the table needs, rebuild
// included. A rebuild that follows one copies the column across under
// the name the RENAME has just given it.
func renameStatements(renames []Rename) []string {
	var out []string
	for _, r := range renames {
		if r.Kind != RenameTable {
			continue
		}
		out = append(out, fmt.Sprintf("ALTER TABLE %s RENAME TO %s;",
			quoteIdent(r.From), quoteIdent(r.To)))
	}
	for _, r := range renames {
		if r.Kind != RenameColumn {
			continue
		}
		out = append(out, fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s;",
			quoteIdent(r.Table), quoteIdent(r.From), quoteIdent(r.To)))
	}
	return out
}

// invertRenames reverses a set of renames, for the down direction of a
// generated migration.
//
// A column rename names its table by the table's new name, because the
// table rename in front of it has already run. That holds in both
// directions, so inverting one has to move the column's table name back
// as well: the inverse of "users became people, and people.email became
// people.emailAddress" is "people became users, and users.emailAddress
// became users.email". Leaving the column pointed at "people" would
// emit an ALTER TABLE naming a table the statement before it had just
// renamed away.
func invertRenames(renames []Rename) []Rename {
	priorName := map[string]string{}
	for _, r := range renames {
		if r.Kind == RenameTable {
			priorName[r.To] = r.From
		}
	}
	out := make([]Rename, 0, len(renames))
	for _, r := range renames {
		if r.Kind == RenameTable {
			out = append(out, Rename{Kind: RenameTable, From: r.To, To: r.From})
		}
	}
	for _, r := range renames {
		if r.Kind != RenameColumn {
			continue
		}
		table := r.Table
		if prior, ok := priorName[table]; ok {
			table = prior
		}
		out = append(out, Rename{Kind: RenameColumn, Table: table, From: r.To, To: r.From})
	}
	return out
}

// applyRenames returns a copy of s as it would be once the renames had
// run — the table and the column under their new names, and every key,
// index and foreign key that named either following them.
//
// This is what lets Diff stay one function. The renames go out in
// front, and what follows is diffed against a previous schema in which
// they have already happened. On this dialect that is worth more than
// tidiness: the rebuild's INSERT ... SELECT copies the columns prev and
// cur agree on, so a rename the rebuild has not been told about is a
// column it does not copy.
func applyRenames(s *Snapshot, renames []Rename) *Snapshot {
	if len(renames) == 0 {
		return s
	}
	out := cloneSnapshot(s)
	for _, r := range renames {
		if r.Kind != RenameTable {
			continue
		}
		renameTableIn(out, r.From, r.To)
	}
	for _, r := range renames {
		if r.Kind != RenameColumn {
			continue
		}
		renameColumnIn(out, r.Table, r.From, r.To)
	}
	return out
}

// renameTableIn moves a table to its new name, repoints every foreign
// key that named the old one, and rewrites the stored DDL of the
// indexes and triggers on it.
func renameTableIn(s *Snapshot, from, to string) {
	t, ok := s.Tables[from]
	if !ok {
		return
	}
	t.Name = to
	delete(s.Tables, from)
	s.Tables[to] = t
	for _, idx := range t.Indexes {
		idx.Table = to
		idx.SQL = renameIdentInSQL(idx.SQL, from, to)
	}
	for _, trg := range t.Triggers {
		trg.SQL = renameIdentInSQL(trg.SQL, from, to)
	}
	for _, k := range sortedMapKeys(s.Tables) {
		for _, fk := range s.Tables[k].ForeignKeys {
			if fk.TableTo == from {
				fk.TableTo = to
			}
			if fk.TableFrom == from {
				fk.TableFrom = to
			}
		}
	}
}

// renameColumnIn moves a column to its new name within one table, and
// follows it into the keys, indexes, foreign keys and stored DDL that
// name it.
func renameColumnIn(s *Snapshot, table, from, to string) {
	t, ok := s.Tables[table]
	if !ok {
		return
	}
	c, ok := t.Columns[from]
	if !ok {
		return
	}
	c.Name = to
	delete(t.Columns, from)
	t.Columns[to] = c

	for _, pk := range t.CompositePrimaryKeys {
		pk.Columns = replaceName(pk.Columns, from, to)
	}
	for _, u := range t.UniqueConstraints {
		u.Columns = replaceName(u.Columns, from, to)
	}
	for _, idx := range t.Indexes {
		idx.Columns = replaceName(idx.Columns, from, to)
		idx.SQL = renameIdentInSQL(idx.SQL, from, to)
	}
	for _, trg := range t.Triggers {
		trg.SQL = renameIdentInSQL(trg.SQL, from, to)
	}
	for _, fk := range t.ForeignKeys {
		fk.ColumnsFrom = replaceName(fk.ColumnsFrom, from, to)
	}
	for _, k := range sortedMapKeys(s.Tables) {
		for _, fk := range s.Tables[k].ForeignKeys {
			if fk.TableTo == table {
				fk.ColumnsTo = replaceName(fk.ColumnsTo, from, to)
			}
		}
	}
}

func replaceName(names []string, from, to string) []string {
	for i, n := range names {
		if n == from {
			names[i] = to
		}
	}
	return names
}

// renameIdentInSQL rewrites whole occurrences of one identifier in
// stored DDL, keeping whatever quoting each occurrence wore.
//
// This has to happen, and it is a text scan rather than a parse for the
// same reason identsMentioned is: drops has no SQL parser and the DDL
// in question is whatever the user wrote. What is at stake is a
// rebuild. SQLite rewrites an index's and a trigger's definition itself
// when a RENAME COLUMN runs, so the live database is correct — but a
// rebuild then drops the table and replays the DDL from the snapshot,
// which was recorded before the rename, and a CREATE INDEX naming a
// column that no longer exists fails and takes the migration with it.
//
// The scan matches only whole identifiers, so "email" does not match
// inside "emailAddress"; it cannot tell one table's "id" from
// another's, and it does not know a string literal from an identifier.
// A wrong substitution inside a trigger body is the failure mode, and
// it is the price of replaying DDL drops did not write.
func renameIdentInSQL(sql, from, to string) string {
	if sql == "" || from == "" || from == to {
		return sql
	}
	var b strings.Builder
	lower, target := strings.ToLower(sql), strings.ToLower(from)
	for i := 0; i < len(sql); {
		j := strings.Index(lower[i:], target)
		if j < 0 {
			b.WriteString(sql[i:])
			break
		}
		at := i + j
		end := at + len(target)
		b.WriteString(sql[i:at])
		if isIdentByte(byteAt(lower, at-1)) || isIdentByte(byteAt(lower, end)) {
			b.WriteString(sql[at:end])
		} else {
			b.WriteString(to)
		}
		i = end
	}
	return b.String()
}

// cloneSnapshot deep-copies a snapshot through its own JSON form.
//
// A hand-written clone would be faster and would silently stop copying
// any field added to the snapshot after it was written; the round trip
// cannot, because the same tags drive both directions. Nothing here is
// on a hot path — it happens once per migration.
func cloneSnapshot(s *Snapshot) *Snapshot {
	body, err := s.Marshal()
	if err != nil {
		return s
	}
	out, err := UnmarshalSnapshot(body)
	if err != nil {
		return s
	}
	return out
}

// The decision log ------------------------------------------------------

// RenameLogFile is where GenerateMigration records the answers, relative
// to the migration directory.
//
// It sits inside meta/ next to the snapshots and the journal, because
// the decision belongs to the migration history and not to one
// developer's terminal: a colleague generating from the same directory
// has to get the same answer, and CI, which can answer nothing, has to
// find one already there.
//
// The file accumulates, and it is meant to be read. An answer only
// applies while it still describes a pending change — see
// renameStillPending — so a settled one is inert, but a *declined* one
// is recorded against the old name alone and would answer the same
// question again if that name ever came back. A column deleted and, a
// year later, reintroduced is the case; delete the line if it happens.
const RenameLogFile = "meta/_renames.json"

// renameLog is the on-disk form of RenameLogFile.
type renameLog struct {
	Version   string           `json:"version"`
	Dialect   string           `json:"dialect"`
	Decisions []RenameDecision `json:"decisions"`
}

// loadRenameLog reads the recorded decisions. A directory without a log
// has answered nothing, which is not an error.
func loadRenameLog(fsys fs.FS, dir string) ([]RenameDecision, error) {
	body, err := fs.ReadFile(fsys, path.Join(dir, RenameLogFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("drops/sqlite: read rename log: %w", err)
	}
	var l renameLog
	if err := json.Unmarshal(body, &l); err != nil {
		return nil, fmt.Errorf("drops/sqlite: parse %s: %w", RenameLogFile, err)
	}
	return l.Decisions, nil
}

// marshalRenameLog renders the log in the same 2-space-indented JSON the
// journal and the snapshots use.
func marshalRenameLog(decisions []RenameDecision) ([]byte, error) {
	body, err := json.MarshalIndent(renameLog{
		Version:   "1",
		Dialect:   "sqlite",
		Decisions: decisions,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

// mergeDecisions layers answers given for this run over the recorded
// ones and returns the result in a stable order. An answer given now
// wins: it is the one a person just typed.
func mergeDecisions(recorded, given []RenameDecision) []RenameDecision {
	byKey := map[string]RenameDecision{}
	for _, d := range recorded {
		byKey[d.key()] = d
	}
	for _, d := range given {
		byKey[d.key()] = d
	}
	out := make([]RenameDecision, 0, len(byKey))
	for _, k := range sortedMapKeys(byKey) {
		out = append(out, byKey[k])
	}
	return out
}
