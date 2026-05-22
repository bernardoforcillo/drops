package pg

import "sort"

// Schema is a registered set of tables — the input to snapshotting,
// diffing and migration generation. It is just a thin wrapper around a
// slice of *Table; the table declarations themselves are unaffected.
type Schema struct {
	tables []*Table
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

// Tables returns the registered tables.
func (s *Schema) Tables() []*Table { return s.tables }

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
