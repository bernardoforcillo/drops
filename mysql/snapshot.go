package mysql

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Schema is a registered set of tables — the input to snapshotting,
// diffing and migration generation.
//
// MySQL has no namespace above the table other than the database
// itself, and the database is what the connection is already pointed
// at, so a Schema is a slice of *Table and nothing else. The pg
// equivalent also carries enums, sequences and views; MySQL's ENUM is
// a column type rather than an object ([Enum]), it has no sequences,
// and views are not declarable through this DSL.
//
// A table declared with [NewDatabaseTable] is snapshotted under its
// bare name and its migration SQL is rendered unqualified, so it lands
// in whichever database the connection is pointed at — which is also
// the one [Introspect] reads unless IntrospectOptions.Database says
// otherwise. A schema spanning two databases needs one Schema and one
// connection per database.
type Schema struct {
	tables []*Table
}

// NewSchema returns a Schema containing the supplied tables.
func NewSchema(tables ...*Table) *Schema {
	return &Schema{tables: append([]*Table(nil), tables...)}
}

// Add appends a table and returns the schema for chaining.
func (s *Schema) Add(t *Table) *Schema {
	s.tables = append(s.tables, t)
	return s
}

// Tables returns the registered tables in declaration order.
func (s *Schema) Tables() []*Table { return s.tables }

// sortedTables returns the tables sorted by name so snapshot and diff
// output is deterministic.
func (s *Schema) sortedTables() []*Table {
	out := append([]*Table(nil), s.tables...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// AddIndex registers a secondary index with the table so [BuildSnapshot]
// carries it and [Push] creates it. The index must have been built
// against this table:
//
//	mysql.Add(Posts, mysql.Varchar("slug", 190))
//	Posts.AddIndex(mysql.NewIndex("idx_posts_slug", Posts, PostSlug).Unique())
//
// A UNIQUE index is how MySQL spells a unique constraint — there is no
// separate constraint object, which is why this one method covers both
// what pg calls an index and what it calls a UNIQUE constraint.
func (t *Table) AddIndex(idx *Index) *Table {
	if idx.table != t {
		panic(fmt.Sprintf("drops/mysql: index %q was built against table %q and cannot be added to %q",
			idx.name, idx.table.Name(), t.name))
	}
	t.indexes = append(t.indexes, idx)
	return t
}

// Indexes returns the registered secondary indexes in declaration
// order. The index MySQL creates behind a PRIMARY KEY or a FOREIGN KEY
// is not among them; those belong to the constraint that owns them.
func (t *Table) Indexes() []*Index { return t.indexes }

// AddCheck registers a named CHECK constraint.
//
// The expression is raw SQL, as the server will parse it. Note that it
// is not the spelling you get back: both servers re-render a stored
// CHECK from their own parse tree, so `age >= 0` reads back as
// "`age` >= 0". Push asks the server to respell the declared side
// before it diffs — see probeCheckExpressions — and a caller diffing
// [Introspect] against [BuildSnapshot] by hand does not get that.
//
// MySQL enforces CHECK constraints from 8.0.16 and MariaDB from 10.2;
// 5.7 parses and ignores them, so a constraint declared here is a
// comment on that server and nothing more.
func (t *Table) AddCheck(name, expr string) *Table {
	mustIdent("check constraint", name)
	if t.checks == nil {
		t.checks = map[string]string{}
	}
	t.checks[name] = expr
	return t
}

// Checks returns the declared CHECK constraints keyed by name.
func (t *Table) Checks() map[string]string { return t.checks }

// Snapshot captures the shape of a MySQL schema so two revisions can be
// diffed into migration SQL.
//
// It is MySQL-flavoured rather than a copy of drops/pg's drizzle-kit v7
// format, because three of that format's assumptions are wrong here:
// tables are keyed by bare name (a MySQL database is the namespace, and
// the connection already picked it), a UNIQUE constraint is not an
// object distinct from its index, and the primary key is always called
// PRIMARY whatever the schema named it.
type Snapshot struct {
	ID      string                    `json:"id"`
	PrevID  string                    `json:"prevId"`
	Version string                    `json:"version"`
	Dialect string                    `json:"dialect"`
	Tables  map[string]*TableSnapshot `json:"tables"`
}

// TableSnapshot is one entry in Snapshot.Tables.
//
// Engine, Charset and Collation are recorded so a CREATE TABLE can
// carry them, but they take no part in the diff: changing any of them
// on a populated table is a full copy, and the schema layer leaves
// them empty far more often than it sets them, so comparing them would
// mean rewriting every table on the first push. Push reports the
// difference as a notice instead.
type TableSnapshot struct {
	Name             string                         `json:"name"`
	Columns          map[string]*ColumnSnapshot     `json:"columns"`
	PrimaryKey       *PrimaryKeySnapshot            `json:"primaryKey,omitempty"`
	Indexes          map[string]*IndexSnapshot      `json:"indexes"`
	ForeignKeys      map[string]*ForeignKeySnapshot `json:"foreignKeys"`
	CheckConstraints map[string]*CheckSnapshot      `json:"checkConstraints"`
	Engine           string                         `json:"engine,omitempty"`
	Charset          string                         `json:"charset,omitempty"`
	Collation        string                         `json:"collation,omitempty"`
}

// ColumnSnapshot is one entry in TableSnapshot.Columns.
//
// Type is normalised: lower-cased, with the display width MariaDB
// still reports on integers stripped, so that the declared side and
// the introspected side of a diff can be compared as text. See
// normaliseType.
//
// There is no PrimaryKey field. MySQL names every primary key PRIMARY
// and permits exactly one, so the key lives on the table whether it
// spans one column or four — unlike drops/pg, where a single-column
// key rides on the column definition.
type ColumnSnapshot struct {
	Name          string  `json:"name"`
	Type          string  `json:"type"`
	NotNull       bool    `json:"notNull"`
	AutoIncrement bool    `json:"autoIncrement,omitempty"`
	Default       *string `json:"default,omitempty"`
	OnUpdate      string  `json:"onUpdate,omitempty"`
	Comment       string  `json:"comment,omitempty"`
}

// PrimaryKeySnapshot describes a table's PRIMARY KEY. Name is always
// "PRIMARY": MySQL ignores whatever name a CONSTRAINT clause gives it
// and reports that one back, so anything else here would be a name the
// catalogue can never confirm.
type PrimaryKeySnapshot struct {
	Name     string   `json:"name"`
	Columns  []string `json:"columns"`
	Prefixes []int    `json:"prefixes,omitempty"`
}

// IndexSnapshot is one entry in TableSnapshot.Indexes.
//
// Prefixes runs parallel to Columns, holding the prefix length of each
// key part and 0 where the whole column is indexed. It is nil when
// every entry would be 0. A TEXT or BLOB column cannot be indexed
// without one, so this is not an optional refinement — it is part of
// whether the index can be created at all.
type IndexSnapshot struct {
	Name     string   `json:"name"`
	Columns  []string `json:"columns"`
	Prefixes []int    `json:"prefixes,omitempty"`
	IsUnique bool     `json:"isUnique"`
	Method   string   `json:"method,omitempty"`
	Comment  string   `json:"comment,omitempty"`
}

// ForeignKeySnapshot is one entry in TableSnapshot.ForeignKeys.
type ForeignKeySnapshot struct {
	Name        string   `json:"name"`
	TableFrom   string   `json:"tableFrom"`
	ColumnsFrom []string `json:"columnsFrom"`
	TableTo     string   `json:"tableTo"`
	ColumnsTo   []string `json:"columnsTo"`
	OnDelete    string   `json:"onDelete"`
	OnUpdate    string   `json:"onUpdate"`
}

// CheckSnapshot is one entry in TableSnapshot.CheckConstraints. Value
// is the SQL expression inside CHECK (...).
type CheckSnapshot struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// zeroUUID is the PrevID of the very first snapshot.
const zeroUUID = "00000000-0000-0000-0000-000000000000"

// EmptySnapshot returns a snapshot with no tables — the "previous"
// side of the first migration.
func EmptySnapshot() *Snapshot {
	return &Snapshot{
		ID:      zeroUUID,
		PrevID:  zeroUUID,
		Version: "5",
		Dialect: "mysql",
		Tables:  map[string]*TableSnapshot{},
	}
}

// newTableSnapshot returns a table snapshot with every map allocated,
// so a declared snapshot and an introspected one serialise the same
// shape and neither side has to check for nil.
func newTableSnapshot(name string) *TableSnapshot {
	return &TableSnapshot{
		Name:             name,
		Columns:          map[string]*ColumnSnapshot{},
		Indexes:          map[string]*IndexSnapshot{},
		ForeignKeys:      map[string]*ForeignKeySnapshot{},
		CheckConstraints: map[string]*CheckSnapshot{},
	}
}

// BuildSnapshot constructs a snapshot from a Go schema definition. The
// ID is a fresh UUID v4; PrevID defaults to zeroUUID and the caller
// overwrites it from the previous snapshot, if any.
func BuildSnapshot(schema *Schema) *Snapshot {
	s := EmptySnapshot()
	s.ID = newUUID()
	for _, t := range schema.sortedTables() {
		ts := newTableSnapshot(t.Name())
		ts.Engine = t.engine
		ts.Charset = t.charset
		ts.Collation = t.collation

		for _, c := range t.Columns() {
			cs := &ColumnSnapshot{
				Name: c.Name(),
				Type: normaliseType(c.Type().TypeSQL()),
				// A key column is NOT NULL whether or not the schema
				// said so: MySQL silently converts a nullable column
				// to NOT NULL when it joins the primary key, and
				// Introspect reads it back that way, so recording it
				// nullable would have every push emit a MODIFY the
				// next push undoes.
				NotNull:       c.IsNotNull() || c.IsPrimaryKey(),
				AutoIncrement: c.IsAutoIncrement(),
				OnUpdate:      canonicalDefault(c.onUpdate),
				Comment:       c.Comment(),
			}
			if c.HasDefault() {
				if d := canonicalDefault(c.DefaultSQL()); d != "" {
					cs.Default = &d
				}
			}
			ts.Columns[c.Name()] = cs
		}

		if keys := t.PrimaryKeyColumns(); len(keys) > 0 {
			pk := &PrimaryKeySnapshot{Name: primaryKeyName}
			for _, c := range keys {
				pk.Columns = append(pk.Columns, c.Name())
				pk.Prefixes = append(pk.Prefixes, c.KeyPrefixLength())
			}
			pk.Prefixes = trimPrefixes(pk.Prefixes)
			ts.PrimaryKey = pk
		}

		// A column marked Unique renders as a table-level UNIQUE KEY in
		// CreateTable under this name; the migration layer has to agree
		// with it, or a table created by one and evolved by the other
		// grows a second index over the same column.
		for _, c := range t.Columns() {
			if !c.IsUnique() || c.IsPrimaryKey() {
				continue
			}
			name := constraintName("uq_", t.Name(), c.Name())
			ts.Indexes[name] = &IndexSnapshot{
				Name:     name,
				Columns:  []string{c.Name()},
				Prefixes: trimPrefixes([]int{c.KeyPrefixLength()}),
				IsUnique: true,
			}
		}
		for _, idx := range t.Indexes() {
			ts.Indexes[idx.name] = indexSnapshotOf(idx)
		}

		for _, c := range t.Columns() {
			fk := c.ForeignKey()
			if fk == nil {
				continue
			}
			target := fk.Target
			name := constraintName("fk_", t.Name(), c.Name())
			ts.ForeignKeys[name] = &ForeignKeySnapshot{
				Name:        name,
				TableFrom:   t.Name(),
				ColumnsFrom: []string{c.Name()},
				TableTo:     target.table.Name(),
				ColumnsTo:   []string{target.Name()},
				OnDelete:    normaliseAction(fk.OnDelete),
				OnUpdate:    normaliseAction(fk.OnUpdate),
			}
		}

		for name, expr := range t.Checks() {
			ts.CheckConstraints[name] = &CheckSnapshot{Name: name, Value: expr}
		}

		s.Tables[t.Name()] = ts
	}
	return s
}

// indexSnapshotOf extracts the snapshot form of a declared index.
func indexSnapshotOf(idx *Index) *IndexSnapshot {
	is := &IndexSnapshot{
		Name:     idx.name,
		IsUnique: idx.unique,
		Method:   strings.ToUpper(idx.using),
		Comment:  idx.comment,
	}
	for _, c := range idx.cols {
		col := c.col()
		is.Columns = append(is.Columns, col.name)
		is.Prefixes = append(is.Prefixes, idx.prefix[col.name])
	}
	is.Prefixes = trimPrefixes(is.Prefixes)
	return is
}

// trimPrefixes returns nil when no key part has a prefix length, so
// that a declared index and an introspected one — where the catalogue
// reports SUB_PART as NULL — hold the same value rather than one
// holding a slice of zeros.
func trimPrefixes(in []int) []int {
	for _, n := range in {
		if n > 0 {
			return in
		}
	}
	return nil
}

// Marshal returns the snapshot as 2-space-indented JSON with a
// trailing newline.
func (s *Snapshot) Marshal() ([]byte, error) {
	body, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

// UnmarshalSnapshot parses a snapshot from JSON, restoring the maps a
// hand-written or trimmed file may have left out.
func UnmarshalSnapshot(data []byte) (*Snapshot, error) {
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("drops/mysql: parse snapshot: %w", err)
	}
	if s.Tables == nil {
		s.Tables = map[string]*TableSnapshot{}
	}
	for _, t := range s.Tables {
		if t.Columns == nil {
			t.Columns = map[string]*ColumnSnapshot{}
		}
		if t.Indexes == nil {
			t.Indexes = map[string]*IndexSnapshot{}
		}
		if t.ForeignKeys == nil {
			t.ForeignKeys = map[string]*ForeignKeySnapshot{}
		}
		if t.CheckConstraints == nil {
			t.CheckConstraints = map[string]*CheckSnapshot{}
		}
	}
	return &s, nil
}

// ----------------------------------------------------------------------
// Normalisation
//
// Both sides of a diff have to be spelled the same way or every push
// emits work that is not work. The declared side is spelled by a Go
// program and the live side by whichever server answered, and the two
// disagree about more than case — see normaliseType and
// canonicalDefault for what each of them reconciles.
// ----------------------------------------------------------------------

const primaryKeyName = "PRIMARY"

var (
	// reDisplayWidth matches the width MariaDB still reports on
	// integer types — int(11), bigint(20) unsigned — which MySQL 8.0.19
	// deprecated and 8.4 no longer reports at all.
	reDisplayWidth = regexp.MustCompile(`^(tinyint|smallint|mediumint|int|integer|bigint)\((\d+)\)(.*)$`)
	// reCurrentTimestamp matches every spelling of the one default
	// value that is a function call: MySQL writes CURRENT_TIMESTAMP,
	// MariaDB writes current_timestamp(), and both accept now().
	reCurrentTimestamp = regexp.MustCompile(`^(?i)(current_timestamp|localtime|localtimestamp|now)(\(\s*(\d*)\s*\))?$`)
)

// normaliseType rewrites a column type into the one spelling both
// sides of a diff can be compared in.
//
// What it reconciles, in the order the rules apply:
//
//   - case and spacing *outside a quoted literal*, so VARCHAR(255) and
//     varchar(255) meet, and ENUM('a', 'b') meets enum('a','b'). What
//     is inside the quotes is left exactly as written, because an ENUM
//     or SET carries its members in its type and those members are
//     data: ENUM('Draft') and ENUM('draft') are different columns, and
//     a CREATE TABLE rendered from the second stores "draft" where the
//     schema said "Draft". See foldTypeText;
//   - the integer display width, because MariaDB 10.11 reports int(11)
//     for the INT this package renders and MySQL 8.4 reports int.
//     TINYINT(1) is left alone: it is not a width, it is how both
//     servers spell BOOLEAN;
//   - the type aliases the servers resolve on the way in — BOOLEAN to
//     tinyint(1), DOUBLE PRECISION to double, NUMERIC to decimal —
//     which the catalogue only ever reports in their resolved form.
//
// It does not reconcile JSON, which MariaDB stores as LONGTEXT. That
// one is not a spelling difference but a different type with the same
// meaning, and collapsing it here would have a CREATE TABLE emit
// LONGTEXT and lose the json_valid constraint MariaDB attaches to a
// real JSON column. typeEqual handles it at comparison time instead.
func normaliseType(t string) string {
	out := foldTypeText(t)
	switch out {
	case "bool", "boolean":
		return "tinyint(1)"
	case "double precision", "real":
		return "double"
	case "numeric", "dec", "fixed", "decimal":
		return "decimal(10,0)"
	case "integer":
		return "int"
	}
	if rest, ok := strings.CutPrefix(out, "numeric("); ok {
		out = "decimal(" + rest
	}
	if m := reDisplayWidth.FindStringSubmatch(out); m != nil {
		if !(m[1] == "tinyint" && m[2] == "1") {
			base := m[1]
			if base == "integer" {
				base = "int"
			}
			out = base + m[3]
		}
	}
	return out
}

// foldTypeText lower-cases a type and squeezes its whitespace — a run
// of it becomes one space, and a space after a comma goes — while
// copying anything inside a quoted run through untouched.
//
// The quoting is what makes this more than a tidy-up. Everywhere else
// a type is keywords and numbers, where case is noise; inside ENUM(…)
// and SET(…) it is the column's values, and folding those rewrites the
// data model rather than its spelling. Both sides of a diff would
// still agree — each was folded the same way — so the churn a
// normalisation bug usually announces itself with never appears, and
// the only symptom is that the migration built a column the schema did
// not describe.
func foldTypeText(t string) string {
	s := strings.TrimSpace(t)
	out := make([]byte, 0, len(s))
	pendingSpace := false
	for i := 0; i < len(s); {
		c := s[i]
		if c == '\'' || c == '"' {
			if pendingSpace && len(out) > 0 && out[len(out)-1] != ',' {
				out = append(out, ' ')
			}
			pendingSpace = false
			end := scanQuoted(s, i, c)
			out = append(out, s[i:end]...)
			i = end
			continue
		}
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			pendingSpace = true
			i++
			continue
		}
		if pendingSpace && len(out) > 0 && out[len(out)-1] != ',' {
			out = append(out, ' ')
		}
		pendingSpace = false
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out = append(out, c)
		i++
	}
	return string(out)
}

// typeEqual reports whether two normalised types describe the same
// column on this server.
//
// MariaDB has no native JSON type: JSON is an alias for LONGTEXT plus a
// generated json_valid() CHECK, and that is what the catalogue reports.
// So a column declared JSON reads back longtext there and the two have
// to compare equal, or every push against MariaDB rewrites every JSON
// column. On MySQL they are genuinely different types and must not.
func typeEqual(a, b string, server ServerInfo) bool {
	if a == b {
		return true
	}
	if server.MariaDB {
		return jsonAlias(a) == jsonAlias(b)
	}
	return false
}

func jsonAlias(t string) string {
	if t == "json" {
		return "longtext"
	}
	return t
}

// canonicalDefault rewrites a DEFAULT expression into the one spelling
// both sides can be compared in.
//
// The literal cases are the servers disagreeing with themselves:
// MariaDB reports a string default quoted and MySQL 8 reports it bare
// (introspect re-quotes it, since only the catalogue knows the column
// was a string); both accept TRUE and FALSE and store 1 and 0; and the
// clock has four names — CURRENT_TIMESTAMP, now(), localtime,
// localtimestamp — of which MariaDB reports current_timestamp() and
// MySQL reports CURRENT_TIMESTAMP.
//
// An empty result means "no default at all", which is also what a bare
// NULL means: MySQL treats DEFAULT NULL on a nullable column as no
// default, and MariaDB goes further and reports the four-character
// string NULL in COLUMN_DEFAULT for a column that never had one.
func canonicalDefault(s string) string {
	out := strings.TrimSpace(s)
	if out == "" || strings.EqualFold(out, "null") {
		return ""
	}
	if strings.EqualFold(out, "true") {
		return "1"
	}
	if strings.EqualFold(out, "false") {
		return "0"
	}
	if m := reCurrentTimestamp.FindStringSubmatch(out); m != nil {
		if m[3] != "" {
			return "CURRENT_TIMESTAMP(" + m[3] + ")"
		}
		return "CURRENT_TIMESTAMP"
	}
	return out
}

// normaliseAction converts a referential action to the lowercase form
// the snapshot stores.
//
// RESTRICT and NO ACTION collapse into one. They are the same
// behaviour in InnoDB — it has no deferred constraints, so there is
// nothing for NO ACTION to defer — and the two servers disagree about
// which of them to report for a foreign key declared with neither:
// MariaDB says RESTRICT, MySQL says NO ACTION. Keeping them apart
// would mean a foreign key dropped and recreated on every push against
// whichever server the schema was not written on.
func normaliseAction(a string) string {
	switch strings.ToLower(strings.TrimSpace(a)) {
	case "", "restrict", "no action":
		return "no action"
	default:
		return strings.ToLower(strings.TrimSpace(a))
	}
}

// ----------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------

// newUUID returns a UUID v4 in the canonical 8-4-4-4-12 form.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return zeroUUID
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// sortedKeys returns a map's keys in ascending order, so the diff and
// the SQL it emits are deterministic.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sameStrings reports whether two ordered identifier lists match.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sameInts reports whether two prefix-length lists match, treating a
// nil list as all zeros so trimPrefixes' output compares equal to a
// hand-written slice of zeros.
func sameInts(a, b []int) bool {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if intAt(a, i) != intAt(b, i) {
			return false
		}
	}
	return true
}

func intAt(s []int, i int) int {
	if i < len(s) {
		return s[i]
	}
	return 0
}

func sameStringPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
