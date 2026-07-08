package sqlite

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sort"
)

// Schema is a registered set of tables — the input to snapshotting and
// diffing. SQLite has no schemas (namespaces), so a Schema is just a
// thin wrapper around a slice of *Table.
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

// sortedTables returns the tables sorted by name for deterministic
// snapshot/diff output.
func (s *Schema) sortedTables() []*Table {
	out := append([]*Table(nil), s.tables...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Snapshot captures the shape of a SQLite schema — tables, columns and
// every table-level constraint — so two revisions can be diffed into
// migration SQL. Unlike the drops/pg snapshot (which mirrors
// drizzle-kit's PostgreSQL v7 format), this is SQLite-flavoured: type
// strings are affinity keywords (INTEGER/TEXT/REAL/BLOB/NUMERIC),
// columns carry an autoincrement flag, and single-column foreign keys
// and uniques live inline on the column while composite ones are
// table-level constraints.
type Snapshot struct {
	ID      string                    `json:"id"`
	PrevID  string                    `json:"prevId"`
	Version string                    `json:"version"`
	Dialect string                    `json:"dialect"`
	Tables  map[string]*TableSnapshot `json:"tables"`
}

// TableSnapshot is one entry in Snapshot.Tables.
type TableSnapshot struct {
	Name                 string                          `json:"name"`
	Columns              map[string]*ColumnSnapshot      `json:"columns"`
	ForeignKeys          map[string]*ForeignKeySnapshot  `json:"foreignKeys"`
	CompositePrimaryKeys map[string]*CompositePKSnapshot `json:"compositePrimaryKeys"`
	UniqueConstraints    map[string]*UniqueSnapshot      `json:"uniqueConstraints"`
	CheckConstraints     map[string]*CheckSnapshot       `json:"checkConstraints"`
}

// ColumnSnapshot is one entry in TableSnapshot.Columns. Type is the
// SQLite affinity keyword. Unique / PrimaryKey record single-column
// constraints that render inline in CREATE TABLE; AutoIncrement is only
// meaningful on an INTEGER PRIMARY KEY.
type ColumnSnapshot struct {
	Name          string  `json:"name"`
	Type          string  `json:"type"`
	PrimaryKey    bool    `json:"primaryKey"`
	NotNull       bool    `json:"notNull"`
	Unique        bool    `json:"unique"`
	AutoIncrement bool    `json:"autoincrement"`
	Default       *string `json:"default,omitempty"`
}

// ForeignKeySnapshot is one entry in TableSnapshot.ForeignKeys. A
// single-column FK (len(ColumnsFrom)==1) renders inline on its column; a
// composite FK renders as a table-level CONSTRAINT. OnDelete/OnUpdate
// hold the raw referential action ("CASCADE", "SET NULL", …) or "" for
// the SQLite default.
type ForeignKeySnapshot struct {
	Name        string   `json:"name"`
	TableFrom   string   `json:"tableFrom"`
	ColumnsFrom []string `json:"columnsFrom"`
	TableTo     string   `json:"tableTo"`
	ColumnsTo   []string `json:"columnsTo"`
	OnDelete    string   `json:"onDelete"`
	OnUpdate    string   `json:"onUpdate"`
}

// CompositePKSnapshot is one entry in TableSnapshot.CompositePrimaryKeys.
type CompositePKSnapshot struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
}

// UniqueSnapshot is one entry in TableSnapshot.UniqueConstraints
// (multi-column / named UNIQUE declared via Table.AddUnique).
type UniqueSnapshot struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
}

// CheckSnapshot is one entry in TableSnapshot.CheckConstraints. Value is
// the SQL expression inside CHECK (...).
type CheckSnapshot struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// zeroUUID is used as PrevID for the very first snapshot.
const zeroUUID = "00000000-0000-0000-0000-000000000000"

// EmptySnapshot returns a fresh snapshot with no tables. Useful as the
// "previous" snapshot when diffing the first migration.
func EmptySnapshot() *Snapshot {
	return &Snapshot{
		ID:      zeroUUID,
		PrevID:  zeroUUID,
		Version: "6",
		Dialect: "sqlite",
		Tables:  map[string]*TableSnapshot{},
	}
}

// BuildSnapshot constructs a snapshot from a Go schema definition. The
// resulting ID is a fresh UUID v4; PrevID defaults to zeroUUID and the
// caller should overwrite it from the previous snapshot, if any.
func BuildSnapshot(schema *Schema) *Snapshot {
	s := EmptySnapshot()
	s.ID = newUUID()
	for _, t := range schema.sortedTables() {
		ts := &TableSnapshot{
			Name:                 t.Name(),
			Columns:              map[string]*ColumnSnapshot{},
			ForeignKeys:          map[string]*ForeignKeySnapshot{},
			CompositePrimaryKeys: map[string]*CompositePKSnapshot{},
			UniqueConstraints:    map[string]*UniqueSnapshot{},
			CheckConstraints:     map[string]*CheckSnapshot{},
		}
		// Composite primary key.
		if pk := t.CompositePrimaryKey(); len(pk) > 0 {
			cols := make([]string, len(pk))
			for i, c := range pk {
				cols[i] = c.Name()
			}
			name := compositePKName(t.Name(), cols)
			ts.CompositePrimaryKeys[name] = &CompositePKSnapshot{Name: name, Columns: cols}
		}
		// Named composite UNIQUE constraints (Table.AddUnique).
		for name, cols := range t.CompositeUniques() {
			names := make([]string, len(cols))
			for i, c := range cols {
				names[i] = c.Name()
			}
			ts.UniqueConstraints[name] = &UniqueSnapshot{Name: name, Columns: names}
		}
		// CHECK constraints.
		for name, expr := range t.Checks() {
			ts.CheckConstraints[name] = &CheckSnapshot{Name: name, Value: expr}
		}
		// Composite (multi-column) foreign keys (Table.ForeignKeyN).
		for _, cfk := range t.CompositeForeignKeys() {
			from := make([]string, len(cfk.Columns))
			for i, c := range cfk.Columns {
				from[i] = c.Name()
			}
			to := make([]string, len(cfk.TargetColumns))
			for i, c := range cfk.TargetColumns {
				to[i] = c.Name()
			}
			targetName := ""
			if cfk.Target != nil {
				targetName = cfk.Target.Name()
			}
			ts.ForeignKeys[cfk.Name] = &ForeignKeySnapshot{
				Name:        cfk.Name,
				TableFrom:   t.Name(),
				ColumnsFrom: from,
				TableTo:     targetName,
				ColumnsTo:   to,
				OnDelete:    cfk.OnDelete,
				OnUpdate:    cfk.OnUpdate,
			}
		}
		// Columns and their single-column constraints.
		for _, c := range t.Columns() {
			cs := &ColumnSnapshot{
				Name:          c.Name(),
				Type:          c.Type().TypeSQL(),
				PrimaryKey:    c.IsPrimaryKey(),
				NotNull:       c.IsNotNull(),
				Unique:        c.IsUnique() && !c.IsPrimaryKey(),
				AutoIncrement: c.IsAutoIncrement(),
			}
			if c.HasDefault() {
				d := c.DefaultSQL()
				cs.Default = &d
			}
			ts.Columns[c.Name()] = cs

			if fk := c.ForeignKey(); fk != nil && fk.Target != nil {
				targetTable := fk.Target.Table()
				targetName := ""
				if targetTable != nil {
					targetName = targetTable.Name()
				}
				name := fkName(t.Name(), []string{c.Name()}, targetName, []string{fk.Target.Name()})
				ts.ForeignKeys[name] = &ForeignKeySnapshot{
					Name:        name,
					TableFrom:   t.Name(),
					ColumnsFrom: []string{c.Name()},
					TableTo:     targetName,
					ColumnsTo:   []string{fk.Target.Name()},
					OnDelete:    fk.OnDelete,
					OnUpdate:    fk.OnUpdate,
				}
			}
		}
		s.Tables[t.Name()] = ts
	}
	return s
}

// Marshal returns the snapshot as 2-space-indented JSON.
func (s *Snapshot) Marshal() ([]byte, error) {
	body, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

// UnmarshalSnapshot parses a snapshot from JSON, restoring zero-valued
// maps for any nil collections so subsequent diffs don't have to check
// for nil.
func UnmarshalSnapshot(data []byte) (*Snapshot, error) {
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("drops/sqlite: parse snapshot: %w", err)
	}
	if s.Tables == nil {
		s.Tables = map[string]*TableSnapshot{}
	}
	for _, t := range s.Tables {
		if t.Columns == nil {
			t.Columns = map[string]*ColumnSnapshot{}
		}
		if t.ForeignKeys == nil {
			t.ForeignKeys = map[string]*ForeignKeySnapshot{}
		}
		if t.CompositePrimaryKeys == nil {
			t.CompositePrimaryKeys = map[string]*CompositePKSnapshot{}
		}
		if t.UniqueConstraints == nil {
			t.UniqueConstraints = map[string]*UniqueSnapshot{}
		}
		if t.CheckConstraints == nil {
			t.CheckConstraints = map[string]*CheckSnapshot{}
		}
	}
	return &s, nil
}

// Naming helpers ------------------------------------------------------

// compositePKName builds the composite-primary-key name <table><Col...>Pk.
func compositePKName(table string, cols []string) string {
	out := table
	for _, c := range cols {
		out += titleFirst(c)
	}
	return out + "Pk"
}

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

// sortedMapKeys returns the keys of a map in ascending order, so diff
// and SQL emission are deterministic.
func sortedMapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
