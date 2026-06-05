package pg

import "sort"

// Schema is a registered set of tables — the input to snapshotting,
// diffing and migration generation. It is just a thin wrapper around a
// slice of *Table plus optional top-level objects (enums, sequences,
// views); the table declarations themselves are unaffected.
type Schema struct {
	tables    []*Table
	enums     []*PgEnum
	sequences []*PgSequence
	views     []*PgView
}

// NewSchema returns a Schema containing the supplied tables.
func NewSchema(tables ...*Table) *Schema {
	return &Schema{tables: append([]*Table(nil), tables...)}
}

// Add appends a table.
func (s *Schema) Add(t *Table) *Schema {
	s.tables = append(s.tables, t)
	return s
}

// AddEnum registers a top-level enum type with the schema so the
// snapshot / diff generator emits the matching CREATE TYPE.
func (s *Schema) AddEnum(e *PgEnum) *Schema {
	s.enums = append(s.enums, e)
	return s
}

// AddSequence registers a top-level sequence with the schema so the
// snapshot / diff generator emits the matching CREATE SEQUENCE.
func (s *Schema) AddSequence(seq *PgSequence) *Schema {
	s.sequences = append(s.sequences, seq)
	return s
}

// AddView registers a view with the schema so the snapshot /
// diff generator emits the matching CREATE VIEW.
func (s *Schema) AddView(v *PgView) *Schema {
	s.views = append(s.views, v)
	return s
}

// Tables returns the registered tables.
func (s *Schema) Tables() []*Table { return s.tables }

// Enums returns the registered enum types in declaration order.
func (s *Schema) Enums() []*PgEnum { return s.enums }

// Sequences returns the registered sequences.
func (s *Schema) Sequences() []*PgSequence { return s.sequences }

// Views returns the registered views.
func (s *Schema) Views() []*PgView { return s.views }

// sortedTables returns the tables sorted by schema-qualified name for
// deterministic snapshot/diff output.
func (s *Schema) sortedTables() []*Table {
	out := append([]*Table(nil), s.tables...)
	sort.Slice(out, func(i, j int) bool {
		return tableKey(out[i]) < tableKey(out[j])
	})
	return out
}

// tableKey returns the schema-qualified table name used as the
// map key in snapshot.json. Empty schema is normalised to "public".
func tableKey(t *Table) string {
	schema := t.Schema()
	if schema == "" {
		schema = "public"
	}
	return schema + "." + t.Name()
}
