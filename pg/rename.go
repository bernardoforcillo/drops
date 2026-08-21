package pg

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
// So drops does not guess. DetectRenames finds the pairs that are
// ambiguous, GenerateMigration refuses to write a migration while any
// of them is unanswered, and the answers are recorded in
// meta/_renames.json so the question is asked once and replayed
// afterwards — by the next run, and by whoever else generates from the
// same directory.
//
// The answer, once given, is applied by rewriting the previous snapshot
// as if the rename had already happened and diffing that: the RENAME
// statements go out in front, and everything after them is the residual
// change. That is why a rename combined with a type change produces a
// RENAME COLUMN and a SET DATA TYPE rather than a drop and an add.

// RenameKind says which kind of object a Rename is about.
type RenameKind string

const (
	// RenameTable is ALTER TABLE ... RENAME TO.
	RenameTable RenameKind = "table"
	// RenameColumn is ALTER TABLE ... RENAME COLUMN ... TO.
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
// arriving. FromType and ToType are the two type strings, which are
// equal in the common case and differ when a column was renamed and
// retyped in one step.
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
// migration when a rename candidate has no recorded answer. It is the
// safe default for a run nobody can answer: CI has no terminal, and the
// alternative to stopping is emitting the DROP COLUMN that loses the
// data.
type RenameAmbiguityError struct {
	// Candidates are the unanswered pairs, in a deterministic order.
	Candidates []RenameCandidate
}

// Error names each ambiguity and both ways out of it.
func (e *RenameAmbiguityError) Error() string {
	var b strings.Builder
	b.WriteString("drops/pg: this schema change could be a rename or a drop-and-add and drops will not guess:\n")
	for _, c := range e.Candidates {
		if c.Kind == RenameColumn {
			fmt.Fprintf(&b, "  column %q on table %q is gone and %q (%s) has appeared\n",
				c.From, c.Table, c.To, c.ToType)
			fmt.Fprintf(&b, "    rename it:      --rename-column %s.%s=%s\n", c.Table, c.From, c.To)
			fmt.Fprintf(&b, "    or drop it:     --drop-column %s.%s\n", c.Table, c.From)
			continue
		}
		fmt.Fprintf(&b, "  table %q is gone and %q has appeared with the same columns\n", c.From, c.To)
		fmt.Fprintf(&b, "    rename it:      --rename-table %s=%s\n", c.From, c.To)
		fmt.Fprintf(&b, "    or drop it:     --drop-table %s\n", c.From)
	}
	b.WriteString("the answer is written to meta/_renames.json, so it is given once and replayed after that")
	return b.String()
}

// DetectRenames returns every (drop, add) pair in the move from prev to
// cur that could be a rename.
//
// What counts as "could be":
//
//   - A column that prev's table has and cur's does not, paired with one
//     cur's table has and prev's does not, when the two types belong to
//     the same family. Requiring the types to be identical would miss a
//     rename that also widened the column, which people do in one step;
//     accepting any pair at all would ask about a dropped timestamp and
//     an added boolean, which nobody is going to say yes to.
//
//   - A table prev has and cur does not, paired with one cur has and
//     prev does not, when both carry the same column names with
//     compatible types. A table rename is normally the only thing in its
//     migration, so the strict shape test costs little and keeps a
//     migration that drops one table and adds an unrelated other from
//     asking a question about them.
//
// The result is sorted, so two runs over the same pair of snapshots ask
// the same questions in the same order.
func DetectRenames(prev, cur *Snapshot) []RenameCandidate {
	if prev == nil {
		prev = EmptySnapshot()
	}
	if cur == nil {
		cur = EmptySnapshot()
	}
	var out []RenameCandidate

	var goneTables, newTables []string
	for _, k := range sortedKeys(prev.Tables) {
		if _, ok := cur.Tables[k]; !ok {
			goneTables = append(goneTables, k)
		}
	}
	for _, k := range sortedKeys(cur.Tables) {
		if _, ok := prev.Tables[k]; !ok {
			newTables = append(newTables, k)
		}
	}
	for _, gk := range goneTables {
		for _, nk := range newTables {
			if !sameTableShape(prev.Tables[gk], cur.Tables[nk]) {
				continue
			}
			out = append(out, RenameCandidate{Rename: Rename{
				Kind: RenameTable,
				From: prev.Tables[gk].Name,
				To:   cur.Tables[nk].Name,
			}})
		}
	}

	for _, k := range sortedKeys(cur.Tables) {
		prevT, ok := prev.Tables[k]
		if !ok {
			continue
		}
		curT := cur.Tables[k]
		for _, from := range sortedKeys(prevT.Columns) {
			if _, still := curT.Columns[from]; still {
				continue
			}
			for _, to := range sortedKeys(curT.Columns) {
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

// sameTableShape reports whether two tables carry the same column names
// with compatible types — the test a table-rename candidate has to pass.
func sameTableShape(a, b *TableSnapshot) bool {
	if len(a.Columns) == 0 || len(a.Columns) != len(b.Columns) {
		return false
	}
	for name, ac := range a.Columns {
		bc, ok := b.Columns[name]
		if !ok || !renameTypeCompatible(ac.Type, bc.Type) {
			return false
		}
	}
	return true
}

// renameTypeCompatible reports whether a column of type a could plausibly
// be the same column as one of type b.
//
// The test is family membership, not equality: text and varchar(120) are
// the same family, so widening a column while renaming it is still
// recognised as a rename, and text and integer are not, so an unrelated
// drop and add on one table is not offered as a question. A type drops
// does not know — a user-defined enum, a PostGIS geometry — is its own
// family and matches only itself, which is the conservative reading.
func renameTypeCompatible(a, b string) bool {
	return a == b || typeFamily(a) == typeFamily(b)
}

// typeFamily reduces a PostgreSQL type name to the family its values
// live in. Unrecognised names come back as themselves, normalised.
func typeFamily(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	// An array is the family of its element type, kept distinct from
	// that element type: text and text[] are not interchangeable.
	suffix := ""
	for strings.HasSuffix(t, "[]") {
		t = strings.TrimSuffix(t, "[]")
		suffix += "[]"
	}
	// Drop a precision, length or scale — varchar(120) and varchar(255)
	// are the same family, and the diff still emits the SET DATA TYPE
	// that changes one into the other.
	if i := strings.IndexByte(t, '('); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	switch t {
	case "text", "varchar", "character varying", "char", "character", "bpchar", "citext", "name":
		return "text" + suffix
	case "smallint", "int2", "integer", "int", "int4", "bigint", "int8",
		"smallserial", "serial2", "serial", "serial4", "bigserial", "serial8":
		return "integer" + suffix
	case "real", "float4", "double precision", "float8":
		return "float" + suffix
	case "numeric", "decimal", "money":
		return "numeric" + suffix
	case "boolean", "bool":
		return "boolean" + suffix
	case "date":
		return "date" + suffix
	case "time", "timetz", "time with time zone", "time without time zone":
		return "time" + suffix
	case "timestamp", "timestamptz", "timestamp with time zone", "timestamp without time zone":
		return "timestamp" + suffix
	case "json", "jsonb":
		return "json" + suffix
	case "bytea":
		return "bytea" + suffix
	case "uuid":
		return "uuid" + suffix
	}
	return t + suffix
}

// ResolveRenames matches recorded decisions against the candidates a
// pair of snapshots produces and returns the renames to apply, plus the
// candidates still unanswered.
//
// A decision that names a pair no longer in question is ignored rather
// than rejected: the file accumulates answers, and an answer about a
// migration that has already been generated describes a schema change
// that has already happened.
func ResolveRenames(prev, cur *Snapshot, decisions []RenameDecision) (applied []Rename, unresolved []RenameCandidate) {
	candidates := DetectRenames(prev, cur)
	offered := map[string]bool{}
	for _, c := range candidates {
		offered[c.key()] = true
	}
	answered := map[string]bool{}
	declined := map[string]bool{}
	var stated []Rename
	for _, d := range decisions {
		if !d.IsRename && d.To == "" {
			declined[d.Kind.side(d.Table, d.From)] = true
			continue
		}
		answered[d.key()] = d.IsRename
		if d.IsRename && !offered[d.key()] {
			stated = append(stated, d.Rename)
		}
	}
	// A rename nobody was asked about is still a rename. The detector's
	// type test decides what to ask, not what is true, and somebody
	// renaming a column and changing its type past what that test
	// accepts is telling drops something it could not have worked out.
	// Only a rename that still describes a pending change is applied,
	// which is what keeps a recorded answer from an earlier migration
	// from being replayed against a schema that has already moved past
	// it.
	var statedTables []Rename
	for _, r := range stated {
		if r.Kind == RenameTable && renameStillPending(prev, cur, r) {
			statedTables = append(statedTables, r)
			applied = append(applied, r)
		}
	}
	after := applyRenames(prev, statedTables)
	for _, r := range stated {
		if r.Kind == RenameColumn && renameStillPending(after, cur, r) {
			applied = append(applied, r)
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
		return findTable(prev, r.From) != nil &&
			findTable(cur, r.To) != nil &&
			findTable(cur, r.From) == nil
	case RenameColumn:
		pt, ct := findTable(prev, r.Table), findTable(cur, r.Table)
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
// the side they should, and that no name is renamed twice.
func validateRenames(prev, cur *Snapshot, renames []Rename) error {
	seenFrom := map[string]bool{}
	seenTo := map[string]bool{}
	// Table renames land first, so a column rename is checked against
	// the table names cur uses.
	after := applyRenames(prev, tablesOnly(renames))
	for _, r := range renames {
		if seenFrom[r.Kind.side(r.Table, r.From)] {
			return fmt.Errorf("drops/pg: %s is renamed twice", r)
		}
		if seenTo[r.Kind.side(r.Table, r.To)] {
			return fmt.Errorf("drops/pg: two renames both produce %q", r.To)
		}
		seenFrom[r.Kind.side(r.Table, r.From)] = true
		seenTo[r.Kind.side(r.Table, r.To)] = true

		switch r.Kind {
		case RenameTable:
			if findTable(prev, r.From) == nil {
				return fmt.Errorf("drops/pg: rename %s: no table %q in the previous snapshot", r, r.From)
			}
			if findTable(cur, r.To) == nil {
				return fmt.Errorf("drops/pg: rename %s: no table %q in the new schema", r, r.To)
			}
		case RenameColumn:
			pt := findTable(after, r.Table)
			if pt == nil {
				return fmt.Errorf("drops/pg: rename %s: no table %q in the previous snapshot", r, r.Table)
			}
			if _, ok := pt.Columns[r.From]; !ok {
				return fmt.Errorf("drops/pg: rename %s: table %q has no column %q in the previous snapshot", r, r.Table, r.From)
			}
			ct := findTable(cur, r.Table)
			if ct == nil {
				return fmt.Errorf("drops/pg: rename %s: no table %q in the new schema", r, r.Table)
			}
			if _, ok := ct.Columns[r.To]; !ok {
				return fmt.Errorf("drops/pg: rename %s: table %q has no column %q in the new schema", r, r.Table, r.To)
			}
		default:
			return fmt.Errorf("drops/pg: unknown rename kind %q", r.Kind)
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

// findTable looks a table up by its bare name, whatever schema it is
// in. Snapshot keys are schema-qualified; a rename names the table the
// way the schema and the command line do.
func findTable(s *Snapshot, name string) *TableSnapshot {
	for _, k := range sortedKeys(s.Tables) {
		if s.Tables[k].Name == name {
			return s.Tables[k]
		}
	}
	return nil
}

// renameStatements renders the RENAME DDL, tables before columns. A
// column rename names its table by the table's new name, which is the
// name the ALTER TABLE ... RENAME TO in front of it has just created.
func renameStatements(renames []Rename) []string {
	var out []string
	for _, r := range renames {
		if r.Kind != RenameTable {
			continue
		}
		out = append(out, fmt.Sprintf(`ALTER TABLE %s RENAME TO %s;`,
			quoteIdent(r.From), quoteIdent(r.To)))
	}
	for _, r := range renames {
		if r.Kind != RenameColumn {
			continue
		}
		out = append(out, fmt.Sprintf(`ALTER TABLE %s RENAME COLUMN %s TO %s;`,
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
// run — the table under its new name, the column under its new name,
// and every reference to either from a key, an index or a foreign key
// following it.
//
// This is what lets Diff stay one function. The renames are emitted in
// front, and what follows is diffed against a previous schema in which
// they have already happened, so a column that was renamed and widened
// produces the RENAME and the SET DATA TYPE and nothing else.
//
// Two things deliberately do not follow the name. A constraint or index
// keeps the name it was created under, because PostgreSQL does not
// rename either when a column moves; and a CHECK expression keeps its
// text, because rewriting SQL is not something drops does by pattern
// match. Both mean the diff after a rename may drop and re-create a
// constraint whose generated name is built from the column's — which is
// correct, if noisier than it looks.
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

// renameTableIn moves a table to its new name and repoints every
// foreign key that named the old one.
func renameTableIn(s *Snapshot, from, to string) {
	for _, k := range sortedKeys(s.Tables) {
		t := s.Tables[k]
		if t.Name != from {
			continue
		}
		t.Name = to
		delete(s.Tables, k)
		newKey := to
		if i := strings.LastIndex(k, "."); i >= 0 {
			newKey = k[:i+1] + to
		}
		s.Tables[newKey] = t
		break
	}
	for _, k := range sortedKeys(s.Tables) {
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
// follows it into the keys, indexes and foreign keys that name it.
func renameColumnIn(s *Snapshot, table, from, to string) {
	t := findTable(s, table)
	if t == nil {
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
		idx.Include = replaceName(idx.Include, from, to)
	}
	for _, fk := range t.ForeignKeys {
		fk.ColumnsFrom = replaceName(fk.ColumnsFrom, from, to)
	}
	// A foreign key on another table pointing at this column.
	for _, k := range sortedKeys(s.Tables) {
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

// cloneSnapshot deep-copies a snapshot through its own JSON form.
//
// A hand-written clone would be faster and would silently stop copying
// any field added to the snapshot after it was written; the round trip
// cannot, because the same tags drive both directions. Nothing here is
// on a hot path — it happens once per migration — and a failure to
// marshal a snapshot that was just built is not a case worth inventing
// a return value for, so the original comes back and the caller diffs
// what it already had.
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
// year later, reintroduced is the case; delete the line if it happens. drizzle-kit reads meta/_journal.json and
// meta/<idx>_snapshot.json by name and ignores everything else, so a
// directory the two tools share stays readable by both.
//
// The snapshot's own _meta field is not used for this. drizzle-kit
// writes its rename bookkeeping there in a format that is its own
// business, and drops writing a second dialect of it into the same key
// would be the one change most likely to break the interoperability the
// format exists for.
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
		return nil, fmt.Errorf("drops/pg: read rename log: %w", err)
	}
	var l renameLog
	if err := json.Unmarshal(body, &l); err != nil {
		return nil, fmt.Errorf("drops/pg: parse %s: %w", RenameLogFile, err)
	}
	return l.Decisions, nil
}

// marshalRenameLog renders the log in the same 2-space-indented JSON the
// journal and the snapshots use.
func marshalRenameLog(decisions []RenameDecision) ([]byte, error) {
	body, err := json.MarshalIndent(renameLog{
		Version:   "1",
		Dialect:   "postgresql",
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
	for _, k := range sortedKeys(byKey) {
		out = append(out, byKey[k])
	}
	return out
}
