package pg

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sort"
)

// Snapshot is the on-disk representation of a database schema as written
// to drizzle-kit's meta/<idx>_snapshot.json. The JSON keys match
// drizzle-kit's PostgreSQL snapshot v7 format so a Snapshot produced by
// drops is round-trippable through drizzle-kit, and vice versa.
type Snapshot struct {
	ID        string                    `json:"id"`
	PrevID    string                    `json:"prevId"`
	Version   string                    `json:"version"`
	Dialect   string                    `json:"dialect"`
	Tables    map[string]*TableSnapshot `json:"tables"`
	Enums     map[string]any            `json:"enums"`
	Schemas   map[string]any            `json:"schemas"`
	Sequences map[string]any            `json:"sequences"`
	Roles     map[string]any            `json:"roles"`
	Policies  map[string]any            `json:"policies"`
	Views     map[string]any            `json:"views"`
	Meta      SnapshotMeta              `json:"_meta"`
}

// SnapshotMeta carries rename-tracking annotations. drops never sets
// these; the field is present for drizzle-kit compatibility.
type SnapshotMeta struct {
	Columns map[string]any `json:"columns"`
	Schemas map[string]any `json:"schemas"`
	Tables  map[string]any `json:"tables"`
}

// TableSnapshot is one entry in Snapshot.Tables.
type TableSnapshot struct {
	Name                 string                         `json:"name"`
	Schema               string                         `json:"schema"`
	Columns              map[string]*ColumnSnapshot     `json:"columns"`
	Indexes              map[string]any                 `json:"indexes"`
	ForeignKeys          map[string]*ForeignKeySnapshot `json:"foreignKeys"`
	CompositePrimaryKeys map[string]any                 `json:"compositePrimaryKeys"`
	UniqueConstraints    map[string]*UniqueSnapshot     `json:"uniqueConstraints"`
	Policies             map[string]any                 `json:"policies"`
	CheckConstraints     map[string]any                 `json:"checkConstraints"`
	IsRLSEnabled         bool                           `json:"isRLSEnabled"`
}

// ColumnSnapshot is one entry in TableSnapshot.Columns.
type ColumnSnapshot struct {
	Name       string  `json:"name"`
	Type       string  `json:"type"`
	PrimaryKey bool    `json:"primaryKey"`
	NotNull    bool    `json:"notNull"`
	Default    *string `json:"default,omitempty"`
}

// ForeignKeySnapshot is one entry in TableSnapshot.ForeignKeys.
type ForeignKeySnapshot struct {
	Name        string   `json:"name"`
	TableFrom   string   `json:"tableFrom"`
	ColumnsFrom []string `json:"columnsFrom"`
	TableTo     string   `json:"tableTo"`
	SchemaTo    string   `json:"schemaTo"`
	ColumnsTo   []string `json:"columnsTo"`
	OnDelete    string   `json:"onDelete"`
	OnUpdate    string   `json:"onUpdate"`
}

// UniqueSnapshot is one entry in TableSnapshot.UniqueConstraints.
type UniqueSnapshot struct {
	Name             string   `json:"name"`
	NullsNotDistinct bool     `json:"nullsNotDistinct"`
	Columns          []string `json:"columns"`
}

// zeroUUID is used as PrevID for the very first snapshot.
const zeroUUID = "00000000-0000-0000-0000-000000000000"

// EmptySnapshot returns a fresh snapshot with no tables. Useful as the
// "previous" snapshot when diffing the first migration.
func EmptySnapshot() *Snapshot {
	return &Snapshot{
		ID:        zeroUUID,
		PrevID:    zeroUUID,
		Version:   "7",
		Dialect:   "postgresql",
		Tables:    map[string]*TableSnapshot{},
		Enums:     map[string]any{},
		Schemas:   map[string]any{},
		Sequences: map[string]any{},
		Roles:     map[string]any{},
		Policies:  map[string]any{},
		Views:     map[string]any{},
		Meta:      SnapshotMeta{Columns: map[string]any{}, Schemas: map[string]any{}, Tables: map[string]any{}},
	}
}

// BuildSnapshot constructs a snapshot from a Go schema definition.
// The resulting ID is a fresh UUID v4; PrevID defaults to zeroUUID and
// the caller should overwrite it from the previous snapshot, if any.
func BuildSnapshot(schema *Schema) *Snapshot {
	s := EmptySnapshot()
	s.ID = newUUID()
	for _, t := range schema.sortedTables() {
		ts := &TableSnapshot{
			Name:                 t.Name(),
			Schema:               t.Schema(),
			Columns:              map[string]*ColumnSnapshot{},
			Indexes:              map[string]any{},
			ForeignKeys:          map[string]*ForeignKeySnapshot{},
			CompositePrimaryKeys: map[string]any{},
			UniqueConstraints:    map[string]*UniqueSnapshot{},
			Policies:             map[string]any{},
			CheckConstraints:     map[string]any{},
			IsRLSEnabled:         false,
		}
		for _, c := range t.Columns() {
			cs := &ColumnSnapshot{
				Name:       c.Name(),
				Type:       c.Type().TypeSQL(),
				PrimaryKey: c.IsPrimaryKey(),
				NotNull:    c.IsNotNull(),
			}
			if c.HasDefault() {
				d := c.DefaultSQL()
				cs.Default = &d
			}
			ts.Columns[c.Name()] = cs

			if c.IsUnique() && !c.IsPrimaryKey() {
				name := uniqueName(t.Name(), []string{c.Name()})
				ts.UniqueConstraints[name] = &UniqueSnapshot{
					Name:             name,
					NullsNotDistinct: false,
					Columns:          []string{c.Name()},
				}
			}

			if fk := c.ForeignKey(); fk != nil {
				targetTable := fk.Target.Table()
				targetSchema := ""
				targetName := ""
				if targetTable != nil {
					targetSchema = targetTable.Schema()
					targetName = targetTable.Name()
				}
				name := fkName(t.Name(), []string{c.Name()}, targetName, []string{fk.Target.Name()})
				ts.ForeignKeys[name] = &ForeignKeySnapshot{
					Name:        name,
					TableFrom:   t.Name(),
					ColumnsFrom: []string{c.Name()},
					TableTo:     targetName,
					SchemaTo:    targetSchema,
					ColumnsTo:   []string{fk.Target.Name()},
					OnDelete:    normaliseAction(fk.OnDelete),
					OnUpdate:    normaliseAction(fk.OnUpdate),
				}
			}
		}
		s.Tables[tableKey(t)] = ts
	}
	return s
}

// Marshal returns the snapshot as the canonical 2-space-indented JSON
// drizzle-kit produces.
func (s *Snapshot) Marshal() ([]byte, error) {
	// We marshal manually to control field order and the map sort order
	// so output is byte-stable across runs.
	body, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	body = append(body, '\n')
	return body, nil
}

// UnmarshalSnapshot parses a snapshot from JSON, restoring zero-valued
// maps for any nil collections (so subsequent reads/diffs don't have
// to check for nil).
func UnmarshalSnapshot(data []byte) (*Snapshot, error) {
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("drops/pg: parse snapshot: %w", err)
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
		if t.UniqueConstraints == nil {
			t.UniqueConstraints = map[string]*UniqueSnapshot{}
		}
		if t.Indexes == nil {
			t.Indexes = map[string]any{}
		}
		if t.CompositePrimaryKeys == nil {
			t.CompositePrimaryKeys = map[string]any{}
		}
		if t.Policies == nil {
			t.Policies = map[string]any{}
		}
		if t.CheckConstraints == nil {
			t.CheckConstraints = map[string]any{}
		}
	}
	return &s, nil
}

// Naming helpers — drizzle-kit's conventions ---------------------------

// fkName builds drizzle-kit's foreign-key constraint name:
//
//	<tableFrom>_<colFrom...>_<tableTo>_<colTo...>_fk
func fkName(tableFrom string, cols []string, tableTo string, targetCols []string) string {
	out := tableFrom
	for _, c := range cols {
		out += "_" + c
	}
	out += "_" + tableTo
	for _, c := range targetCols {
		out += "_" + c
	}
	out += "_fk"
	return out
}

// uniqueName builds drizzle-kit's unique-constraint name:
//
//	<table>_<col...>_unique
func uniqueName(table string, cols []string) string {
	out := table
	for _, c := range cols {
		out += "_" + c
	}
	out += "_unique"
	return out
}

// normaliseAction converts a possibly-empty referential action ("CASCADE",
// "SET NULL", ...) to the lowercase form drizzle-kit writes ("cascade",
// "set null", or "no action" for the default).
func normaliseAction(a string) string {
	if a == "" {
		return "no action"
	}
	out := []byte(a)
	for i, b := range out {
		if b >= 'A' && b <= 'Z' {
			out[i] = b + 32
		}
	}
	return string(out)
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

// sortedKeys returns the keys of a map in ascending order. Used by the
// diff and SQL emission so output is deterministic.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
