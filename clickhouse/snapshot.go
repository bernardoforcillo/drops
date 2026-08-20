package clickhouse

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/bernardoforcillo/drops"
)

// Schema is a registered set of tables — the input to snapshotting,
// diffing and [Push].
//
// ClickHouse's namespace above the table is the database, and a table
// carries its own (see [NewDatabaseTable]), so a Schema is a slice of
// *Table and nothing else. Views, dictionaries and materialized views
// are not declarable through this DSL and so are never part of one;
// [Introspect] reads them back but [Push] leaves them alone because
// the schema never names them.
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

// sortedTables returns the tables sorted by their snapshot key so
// snapshot and diff output is deterministic.
func (s *Schema) sortedTables() []*Table {
	out := append([]*Table(nil), s.tables...)
	sort.Slice(out, func(i, j int) bool {
		return SnapshotKey(out[i].database, out[i].name) < SnapshotKey(out[j].database, out[j].name)
	})
	return out
}

// Snapshot captures the shape of a ClickHouse schema so a declaration
// and a live server can be diffed.
//
// It is ClickHouse-flavoured rather than a copy of drops/pg's format,
// because most of what decides whether two ClickHouse tables are the
// same table is not a column at all: the engine and its parameters,
// the sorting key, the primary key, the partition key. Those live on
// [TableSnapshot] and are the fields [Diff] refuses on rather than
// emits statements for — see [Refusal].
type Snapshot struct {
	ID      string                    `json:"id"`
	PrevID  string                    `json:"prevId"`
	Version string                    `json:"version"`
	Dialect string                    `json:"dialect"`
	Tables  map[string]*TableSnapshot `json:"tables"`
}

// TableSnapshot is one entry in [Snapshot.Tables], keyed by
// [SnapshotKey].
//
// Engine, SortingKey, PartitionKey and PrimaryKey are not decoration.
// ClickHouse has no ALTER that changes any of them on a populated
// table, so each is compared and then reported as a [Refusal]. For a
// ReplacingMergeTree the sorting key is load-bearing beyond layout:
// the engine collapses rows that share it, so changing it changes
// what "the same row" means, and every row the old key merged into
// one reappears as several.
type TableSnapshot struct {
	// Database is the database the table lives in, empty when the
	// declaration left it to the connection. It is part of
	// [SnapshotKey] and decides whether a statement renders qualified.
	Database string `json:"database,omitempty"`
	Name     string `json:"name"`

	Columns map[string]*ColumnSnapshot `json:"columns"`

	// Engine is the engine name alone ("MergeTree",
	// "ReplacingMergeTree"), matching system.tables.engine.
	Engine string `json:"engine,omitempty"`

	// EngineParams are the engine's arguments in declaration order —
	// the version column of a ReplacingMergeTree, the ZooKeeper path
	// and replica of a ReplicatedMergeTree. They are as significant as
	// the engine name: ReplacingMergeTree() and
	// ReplacingMergeTree(version) resolve a duplicate differently.
	EngineParams []string `json:"engineParams,omitempty"`

	// SortingKey, PrimaryKey, PartitionKey and SampleBy are the key
	// expressions as text, normalised by [NormalizeKeyExpr] so the
	// server's spelling and the declaration's compare equal.
	//
	// PrimaryKey is filled in from the sorting key when a declaration
	// leaves it out, because that is what ClickHouse itself does: a
	// MergeTree declared with ORDER BY and no PRIMARY KEY reports the
	// sorting key in system.tables.primary_key, and a snapshot that
	// left it empty would read that back as drift on every push.
	SortingKey   string `json:"sortingKey,omitempty"`
	PrimaryKey   string `json:"primaryKey,omitempty"`
	PartitionKey string `json:"partitionKey,omitempty"`
	SampleBy     string `json:"sampleBy,omitempty"`

	// TTL is the table-wide TTL expression. Introspection reads it out
	// of system.tables.engine_full, which is where ClickHouse renders
	// it — there is no column of its own for it.
	TTL string `json:"ttl,omitempty"`

	// Settings are the SETTINGS pairs. Only the ones a declaration
	// names take part in the diff; see [Diff] for why a live setting
	// with no declared counterpart is left alone.
	Settings map[string]string `json:"settings,omitempty"`
}

// ColumnSnapshot is one entry in [TableSnapshot.Columns].
//
// Type is normalised by [NormalizeType]: ClickHouse echoes a declared
// "Decimal(10,2)" back as "Decimal(10, 2)", and a diff that treated
// the server's own formatting as drift would rewrite the column on
// every push.
//
// The three key flags come from system.columns and are what makes a
// column's type change possible or not: ClickHouse refuses both MODIFY
// COLUMN and DROP COLUMN on a column that is part of the sorting,
// primary or partition key, because those keys are the on-disk layout.
type ColumnSnapshot struct {
	Name string `json:"name"`
	Type string `json:"type"`

	// Position is the column's 1-based ordinal. It decides where an
	// ADD COLUMN puts a new column — ClickHouse appends unless told
	// AFTER or FIRST — and takes no part in the comparison itself:
	// reordering existing columns is not something an ALTER can do.
	Position int `json:"position"`

	// Default is the DEFAULT expression, empty when there is none.
	// Only a plain DEFAULT is represented; MATERIALIZED, ALIAS and
	// EPHEMERAL columns are read back with DefaultKind set and are
	// left out of the diff — the DSL cannot declare them, so a diff
	// that compared them would propose dropping every one.
	Default     string `json:"default,omitempty"`
	DefaultKind string `json:"defaultKind,omitempty"`

	Codec   string `json:"codec,omitempty"`
	Comment string `json:"comment,omitempty"`

	// TTL is the per-column TTL as declared. It is always empty on an
	// introspected snapshot — system.columns does not report it — so
	// it is excluded from the comparison and reported as a notice
	// instead. See [Push].
	TTL string `json:"ttl,omitempty"`

	InSortingKey   bool `json:"inSortingKey,omitempty"`
	InPrimaryKey   bool `json:"inPrimaryKey,omitempty"`
	InPartitionKey bool `json:"inPartitionKey,omitempty"`
	InSamplingKey  bool `json:"inSamplingKey,omitempty"`
}

// InKey reports whether the column takes part in any of the keys that
// make up the on-disk layout. It is the condition under which
// ClickHouse refuses to alter or drop the column at all.
func (c *ColumnSnapshot) InKey() bool {
	return c.InSortingKey || c.InPrimaryKey || c.InPartitionKey
}

// zeroUUID is the PrevID of the very first snapshot.
const zeroUUID = "00000000-0000-0000-0000-000000000000"

// EmptySnapshot returns a snapshot with no tables — the "previous"
// side of the first diff.
func EmptySnapshot() *Snapshot {
	return &Snapshot{
		ID:      zeroUUID,
		PrevID:  zeroUUID,
		Version: "1",
		Dialect: "clickhouse",
		Tables:  map[string]*TableSnapshot{},
	}
}

// SnapshotKey is how a table is addressed inside [Snapshot.Tables]:
// the bare name when the declaration named no database, and
// "database.name" when it did.
//
// The two are deliberately different keys. A table declared with
// [NewDatabaseTable] renders every statement qualified and one
// declared with [NewTable] renders none, so they are not
// interchangeable even when they name the same table on the same
// server — which is why [Push] insists that [Introspect] be pointed at
// the database the declaration names.
func SnapshotKey(database, name string) string {
	if database == "" {
		return name
	}
	return database + "." + name
}

// newTableSnapshot returns a table snapshot with every map allocated,
// so a declared snapshot and an introspected one serialise the same
// shape and neither side has to check for nil.
func newTableSnapshot(database, name string) *TableSnapshot {
	return &TableSnapshot{
		Database: database,
		Name:     name,
		Columns:  map[string]*ColumnSnapshot{},
		Settings: map[string]string{},
	}
}

// BuildSnapshot constructs a snapshot from a Go schema declaration —
// the "desired" side of a diff, the shape [Introspect] is compared
// against.
//
// Everything the declaration can say that [Introspect] can read back
// is recorded here in the same normalised spelling, because a field
// one side carries and the other does not is drift the diff can never
// settle: it re-emits the same statement on every push.
func BuildSnapshot(schema *Schema) *Snapshot {
	s := EmptySnapshot()
	s.ID = newUUID()
	if schema == nil {
		return s
	}
	for _, t := range schema.sortedTables() {
		s.Tables[SnapshotKey(t.database, t.name)] = buildTableSnapshot(t)
	}
	return s
}

// buildTableSnapshot is BuildSnapshot for one table.
func buildTableSnapshot(t *Table) *TableSnapshot {
	ts := newTableSnapshot(t.database, t.name)

	inSorting := map[string]bool{}
	for _, c := range t.orderBy {
		inSorting[c.col().Name()] = true
	}
	inPrimary := map[string]bool{}
	for _, c := range t.primaryKey {
		inPrimary[c.col().Name()] = true
	}
	// A MergeTree declared with ORDER BY and no PRIMARY KEY has the
	// sorting key as its primary key, and system.columns flags every
	// one of its columns is_in_primary_key. Mirroring that here keeps
	// the declared and the introspected flags equal.
	if len(t.primaryKey) == 0 {
		inPrimary = inSorting
	}
	inPartition := map[string]bool{}
	inSampling := map[string]bool{}
	for _, e := range t.partition {
		for _, name := range columnsNamedBy(t, e) {
			inPartition[name] = true
		}
	}
	if t.sampleBy != nil {
		for _, name := range columnsNamedBy(t, t.sampleBy) {
			inSampling[name] = true
		}
	}

	for i, c := range t.columns {
		cs := &ColumnSnapshot{
			Name:           c.name,
			Type:           NormalizeType(c.typ.TypeSQL()),
			Position:       i + 1,
			Codec:          c.codec,
			Comment:        c.comment,
			TTL:            c.ttl,
			InSortingKey:   inSorting[c.name],
			InPrimaryKey:   inPrimary[c.name],
			InPartitionKey: inPartition[c.name],
			InSamplingKey:  inSampling[c.name],
		}
		if c.hasDef {
			cs.Default = c.defSQL
			cs.DefaultKind = "DEFAULT"
		}
		ts.Columns[c.name] = cs
	}

	ts.Engine, ts.EngineParams = engineParts(t.engine)
	ts.SortingKey = NormalizeKeyExpr(renderKeyList(t.orderBy))
	ts.PrimaryKey = NormalizeKeyExpr(renderKeyList(t.primaryKey))
	if ts.PrimaryKey == "" {
		ts.PrimaryKey = ts.SortingKey
	}
	ts.PartitionKey = NormalizeKeyExpr(renderExprList(t.partition))
	if t.sampleBy != nil {
		ts.SampleBy = NormalizeKeyExpr(renderBare(t.sampleBy))
	}
	ts.TTL = strings.TrimSpace(t.ttl)
	for _, pair := range t.settings {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			// Setting joins the two halves with " = ", so a pair with
			// no "=" cannot come from it. Record it under itself
			// rather than dropping it: an unparseable setting that
			// vanishes from the snapshot is a difference the diff can
			// never see.
			ts.Settings[strings.TrimSpace(pair)] = ""
			continue
		}
		ts.Settings[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return ts
}

// columnsNamedBy returns the names of t's columns that e references.
//
// system.columns reports which columns a partition or sampling
// expression names, not the expression, so the declared side has to be
// reduced the same way for the two to be comparable. Rendering the
// expression and looking for each column's name in it is crude, and it
// is the same crudeness the server's own flag has: toYYYYMM(ts) and
// toYear(ts) both flag ts and nothing distinguishes them here either.
// The expression itself is compared separately and exactly, as
// [TableSnapshot.PartitionKey].
func columnsNamedBy(t *Table, e drops.Expression) []string {
	rendered := renderBare(e)
	var out []string
	for _, c := range t.columns {
		if strings.Contains(rendered, c.name) {
			out = append(out, c.name)
		}
	}
	return out
}

// renderKeyList renders a column list the way a key expression reads —
// comma separated, no wrapping parentheses, which is how
// system.tables.sorting_key reports it.
func renderKeyList(cols []ColRef) string {
	if len(cols) == 0 {
		return ""
	}
	parts := make([]string, 0, len(cols))
	for _, c := range cols {
		parts = append(parts, c.col().Name())
	}
	return strings.Join(parts, ", ")
}

// renderExprList renders a list of expressions the same way.
func renderExprList(exprs []drops.Expression) string {
	if len(exprs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(exprs))
	for _, e := range exprs {
		parts = append(parts, renderBare(e))
	}
	return strings.Join(parts, ", ")
}

// renderBare renders an expression with unqualified column names,
// which is the only form valid in a clause that defines the table it
// names — and also the form ClickHouse echoes back.
func renderBare(e drops.Expression) string {
	b := drops.NewBuilder(drops.WithDialect(Dialect))
	b.SetBareIdents(true)
	b.Append(e)
	sql, _ := b.SQL()
	return sql
}

// engineParts splits an engine into its name and its arguments.
//
// The engine is only reachable through [Engine.WriteEngine], so it is
// rendered and then read back with the same parser [Introspect] uses
// on system.tables.engine_full. One parser for both sides is the point:
// a declared ReplacingMergeTree("version") and a live
// ReplacingMergeTree(version) have to reduce to the same two fields or
// every push proposes rebuilding the table.
func engineParts(e Engine) (name string, params []string) {
	if e == nil {
		return "", nil
	}
	return parseEngine(renderBare(drops.ExprFunc(e.WriteEngine)))
}

// parseEngine reads "Name" or "Name(arg, arg)" into its two halves.
// Arguments are split at top-level commas so a nested call — the
// rand() of a Distributed engine — survives intact, and each is
// normalised so quoting differences do not read as drift.
func parseEngine(text string) (name string, params []string) {
	text = strings.TrimSpace(text)
	open := strings.IndexByte(text, '(')
	if open < 0 {
		return text, nil
	}
	name = strings.TrimSpace(text[:open])
	inner, ok := balancedInner(text[open:])
	if !ok {
		return name, nil
	}
	for _, arg := range splitTopLevel(inner, ',') {
		arg = strings.TrimSpace(arg)
		if arg == "" {
			continue
		}
		params = append(params, NormalizeKeyExpr(arg))
	}
	return name, params
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
		return nil, fmt.Errorf("drops/clickhouse: parse snapshot: %w", err)
	}
	if s.Tables == nil {
		s.Tables = map[string]*TableSnapshot{}
	}
	for _, t := range s.Tables {
		if t.Columns == nil {
			t.Columns = map[string]*ColumnSnapshot{}
		}
		if t.Settings == nil {
			t.Settings = map[string]string{}
		}
	}
	return &s, nil
}

// ----------------------------------------------------------------------
// Normalisation
// ----------------------------------------------------------------------

// NormalizeType strips the whitespace ClickHouse adds when it echoes a
// type back — "Decimal(10, 2)" for a column declared "Decimal(10,2)",
// "Enum8('a' = 1)" for "Enum8('a'=1)" — so that a comparison is about
// the type rather than about the server's formatting.
//
// Text inside single quotes is left alone, because a time zone name or
// an enum label is a value and not syntax: DateTime('Europe/Rome')
// must not lose anything, and neither must an enum label that contains
// a space.
func NormalizeType(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inQuote := false
	for _, r := range s {
		if r == '\'' {
			inQuote = !inQuote
		} else if !inQuote && (r == ' ' || r == '\t' || r == '\n' || r == '\r') {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// NormalizeKeyExpr reduces a key expression to a form both sides of a
// diff can be compared in.
//
// Two spellings of one key have to compare equal. drops renders
// identifiers quoted, because it renders every identifier quoted;
// ClickHouse re-renders the expression from its own parse tree and
// quotes an identifier only when it has to. So "ts", "id" and ts, id
// are the same sorting key, and toYYYYMM("ts") and toYYYYMM(ts) are
// the same partition key.
//
// Unquoting is limited to identifiers that need no quotes — a leading
// letter or underscore followed by letters, digits and underscores.
// Anything else keeps its quotes, since dropping them would change
// what the expression means rather than how it is spelled. Single
// quotes are left alone throughout: they delimit values.
func NormalizeKeyExpr(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch r {
		case ' ', '\t', '\n', '\r':
			continue
		case '\'':
			// A string literal: copy it through, closing quote
			// included, doubled quotes and all.
			b.WriteRune(r)
			for i++; i < len(runes); i++ {
				b.WriteRune(runes[i])
				if runes[i] == '\'' {
					break
				}
			}
		case '"', '`':
			ident, next, ok := readQuotedIdent(runes, i, r)
			if !ok {
				b.WriteRune(r)
				continue
			}
			if isBareIdent(ident) {
				b.WriteString(ident)
			} else {
				b.WriteRune(r)
				b.WriteString(strings.ReplaceAll(ident, string(r), string(r)+string(r)))
				b.WriteRune(r)
			}
			i = next
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// readQuotedIdent reads a quoted identifier starting at runes[start],
// returning its contents with the doubled quote escape undone and the
// index of its closing quote.
func readQuotedIdent(runes []rune, start int, quote rune) (ident string, end int, ok bool) {
	var b strings.Builder
	for i := start + 1; i < len(runes); i++ {
		if runes[i] != quote {
			b.WriteRune(runes[i])
			continue
		}
		if i+1 < len(runes) && runes[i+1] == quote {
			b.WriteRune(quote)
			i++
			continue
		}
		return b.String(), i, true
	}
	return "", start, false
}

// isBareIdent reports whether name is spelled the way ClickHouse
// writes an identifier that needs no quoting.
func isBareIdent(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// balancedInner returns the contents of the parenthesised group text
// starts with, honouring nesting and quoted text.
func balancedInner(text string) (string, bool) {
	if len(text) == 0 || text[0] != '(' {
		return "", false
	}
	depth := 0
	inQuote := false
	for i, r := range text {
		switch {
		case inQuote:
			if r == '\'' {
				inQuote = false
			}
		case r == '\'':
			inQuote = true
		case r == '(':
			depth++
		case r == ')':
			depth--
			if depth == 0 {
				return text[1:i], true
			}
		}
	}
	return "", false
}

// splitTopLevel splits text at sep, ignoring separators inside
// parentheses or single quotes.
func splitTopLevel(text string, sep rune) []string {
	var (
		out     []string
		start   int
		depth   int
		inQuote bool
	)
	for i, r := range text {
		switch {
		case inQuote:
			if r == '\'' {
				inQuote = false
			}
		case r == '\'':
			inQuote = true
		case r == '(':
			depth++
		case r == ')':
			depth--
		case r == sep && depth == 0:
			out = append(out, text[start:i])
			start = i + len(string(r))
		}
	}
	return append(out, text[start:])
}

// sortedKeys returns a map's keys in sorted order, so every walk over
// a snapshot produces the same statement order.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// newUUID returns a random v4 UUID for [Snapshot.ID].
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
