package pg

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// IntrospectOptions tunes which schemas Introspect inspects.
type IntrospectOptions struct {
	// Schemas restricts the introspection to these schema names. Empty
	// means just "public".
	Schemas []string
}

// Introspect queries the live database and returns a Snapshot describing
// its current state — tables, columns (with type normalisation matching
// drizzle-kit's conventions: bigserial / serial / smallserial detected
// from int + nextval default), primary keys, single-column unique
// constraints, and single-column foreign keys with referential actions.
//
// The returned snapshot is in the same format as BuildSnapshot's output
// and can be diffed against a Go-schema snapshot via Diff. It deliberately
// leaves indexes, composite keys, enums, sequences and views empty —
// those features aren't yet representable in drops's schema layer.
func Introspect(ctx context.Context, db *DB, opts ...IntrospectOptions) (*Snapshot, error) {
	var opt IntrospectOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	if len(opt.Schemas) == 0 {
		opt.Schemas = []string{"public"}
	}

	snap := EmptySnapshot()
	snap.ID = newUUID()

	tables, err := readIntrospectTables(ctx, db, opt.Schemas)
	if err != nil {
		return nil, err
	}
	for _, t := range tables {
		snap.Tables[introspectTableKey(t.Schema, t.Name)] = t
	}

	if err := readIntrospectColumns(ctx, db, opt.Schemas, snap.Tables); err != nil {
		return nil, err
	}
	if err := readIntrospectPrimaryKeys(ctx, db, opt.Schemas, snap.Tables); err != nil {
		return nil, err
	}
	if err := readIntrospectUniques(ctx, db, opt.Schemas, snap.Tables); err != nil {
		return nil, err
	}
	if err := readIntrospectForeignKeys(ctx, db, opt.Schemas, snap.Tables); err != nil {
		return nil, err
	}
	return snap, nil
}

func introspectTableKey(schema, name string) string {
	if schema == "" {
		schema = "public"
	}
	return schema + "." + name
}

// readIntrospectTables lists all base tables in the requested schemas.
func readIntrospectTables(ctx context.Context, db *DB, schemas []string) ([]*TableSnapshot, error) {
	rows, err := db.Query(ctx, fmt.Sprintf(`
		SELECT table_schema, table_name
		FROM information_schema.tables
		WHERE table_type = 'BASE TABLE' AND table_schema IN (%s)
		ORDER BY table_schema, table_name`,
		placeholderList(len(schemas), 1)), anySlice(schemas)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*TableSnapshot
	for rows.Next() {
		var schema, name string
		if err := rows.Scan(&schema, &name); err != nil {
			return nil, err
		}
		storedSchema := schema
		if storedSchema == "public" {
			storedSchema = ""
		}
		out = append(out, &TableSnapshot{
			Name:                 name,
			Schema:               storedSchema,
			Columns:              map[string]*ColumnSnapshot{},
			Indexes:              map[string]any{},
			ForeignKeys:          map[string]*ForeignKeySnapshot{},
			CompositePrimaryKeys: map[string]any{},
			UniqueConstraints:    map[string]*UniqueSnapshot{},
			Policies:             map[string]any{},
			CheckConstraints:     map[string]any{},
			IsRLSEnabled:         false,
		})
	}
	return out, rows.Err()
}

// readIntrospectColumns fills in Columns on each table snapshot.
func readIntrospectColumns(ctx context.Context, db *DB, schemas []string, tables map[string]*TableSnapshot) error {
	rows, err := db.Query(ctx, fmt.Sprintf(`
		SELECT
			table_schema, table_name, column_name,
			udt_name, character_maximum_length,
			numeric_precision, numeric_scale,
			is_nullable, column_default
		FROM information_schema.columns
		WHERE table_schema IN (%s)
		ORDER BY table_schema, table_name, ordinal_position`,
		placeholderList(len(schemas), 1)), anySlice(schemas)...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			schema, table, name, udt string
			charLen                  sql.NullInt64
			numPrec, numScale        sql.NullInt64
			isNullable               string
			colDefault               sql.NullString
		)
		if err := rows.Scan(&schema, &table, &name, &udt, &charLen, &numPrec, &numScale, &isNullable, &colDefault); err != nil {
			return err
		}
		ts, ok := tables[introspectTableKey(schema, table)]
		if !ok {
			continue
		}
		def := derefNullString(colDefault)
		typ := normalisePGType(udt, charLen, numPrec, numScale, def)
		if typ == "" {
			typ = udt
		}
		cs := &ColumnSnapshot{
			Name:    name,
			Type:    typ,
			NotNull: isNullable == "NO",
		}
		// Serial-style columns own the nextval default implicitly; we
		// hide it from the snapshot to match drizzle-kit.
		if !isSerialFamily(typ) && def != "" {
			d := def
			cs.Default = &d
		}
		ts.Columns[name] = cs
	}
	return rows.Err()
}

// isSerialFamily reports whether the type name is one of the serial
// pseudo-types whose default is owned by an implicit sequence.
func isSerialFamily(t string) bool {
	return t == "serial" || t == "bigserial" || t == "smallserial"
}

// normalisePGType converts an information_schema.columns row into the
// type string drizzle-kit uses in its snapshot.
func normalisePGType(udt string, charLen, numPrec, numScale sql.NullInt64, columnDefault string) string {
	switch udt {
	case "int2":
		if hasSequenceDefault(columnDefault) {
			return "smallserial"
		}
		return "smallint"
	case "int4":
		if hasSequenceDefault(columnDefault) {
			return "serial"
		}
		return "integer"
	case "int8":
		if hasSequenceDefault(columnDefault) {
			return "bigserial"
		}
		return "bigint"
	case "float4":
		return "real"
	case "float8":
		return "double precision"
	case "bool":
		return "boolean"
	case "varchar":
		if charLen.Valid {
			return fmt.Sprintf("varchar(%d)", charLen.Int64)
		}
		return "varchar"
	case "bpchar":
		if charLen.Valid {
			return fmt.Sprintf("char(%d)", charLen.Int64)
		}
		return "char"
	case "numeric":
		if numPrec.Valid && numScale.Valid {
			return fmt.Sprintf("numeric(%d,%d)", numPrec.Int64, numScale.Int64)
		}
		return "numeric"
	case "timestamp":
		return "timestamp"
	case "timestamptz":
		return "timestamptz"
	}
	return udt
}

// hasSequenceDefault reports whether the column default reads from a
// sequence — the marker for serial/bigserial/smallserial columns.
func hasSequenceDefault(def string) bool {
	return strings.HasPrefix(strings.TrimSpace(def), "nextval(")
}

// readIntrospectPrimaryKeys marks PK columns on each table.
func readIntrospectPrimaryKeys(ctx context.Context, db *DB, schemas []string, tables map[string]*TableSnapshot) error {
	rows, err := db.Query(ctx, fmt.Sprintf(`
		SELECT tc.table_schema, tc.table_name, kcu.column_name
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
			ON kcu.constraint_schema = tc.constraint_schema
			AND kcu.constraint_name = tc.constraint_name
		WHERE tc.constraint_type = 'PRIMARY KEY'
			AND tc.table_schema IN (%s)
		ORDER BY tc.table_schema, tc.table_name, kcu.ordinal_position`,
		placeholderList(len(schemas), 1)), anySlice(schemas)...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var schema, table, column string
		if err := rows.Scan(&schema, &table, &column); err != nil {
			return err
		}
		ts, ok := tables[introspectTableKey(schema, table)]
		if !ok {
			continue
		}
		if col, ok := ts.Columns[column]; ok {
			col.PrimaryKey = true
			col.NotNull = true
		}
	}
	return rows.Err()
}

// readIntrospectUniques pulls UNIQUE constraints. Composite uniques are
// skipped (we don't model them).
func readIntrospectUniques(ctx context.Context, db *DB, schemas []string, tables map[string]*TableSnapshot) error {
	rows, err := db.Query(ctx, fmt.Sprintf(`
		SELECT tc.table_schema, tc.table_name, tc.constraint_name, kcu.column_name
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
			ON kcu.constraint_schema = tc.constraint_schema
			AND kcu.constraint_name = tc.constraint_name
		WHERE tc.constraint_type = 'UNIQUE'
			AND tc.table_schema IN (%s)
		ORDER BY tc.table_schema, tc.table_name, tc.constraint_name, kcu.ordinal_position`,
		placeholderList(len(schemas), 1)), anySlice(schemas)...)
	if err != nil {
		return err
	}
	defer rows.Close()

	type key struct {
		schema, table, name string
	}
	cols := map[key][]string{}
	for rows.Next() {
		var schema, table, name, column string
		if err := rows.Scan(&schema, &table, &name, &column); err != nil {
			return err
		}
		k := key{schema, table, name}
		cols[k] = append(cols[k], column)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for k, columns := range cols {
		ts, ok := tables[introspectTableKey(k.schema, k.table)]
		if !ok {
			continue
		}
		if len(columns) != 1 {
			continue // skip composite uniques
		}
		ts.UniqueConstraints[k.name] = &UniqueSnapshot{
			Name:             k.name,
			NullsNotDistinct: false,
			Columns:          columns,
		}
	}
	return nil
}

// readIntrospectForeignKeys pulls single-column FOREIGN KEY constraints.
func readIntrospectForeignKeys(ctx context.Context, db *DB, schemas []string, tables map[string]*TableSnapshot) error {
	rows, err := db.Query(ctx, fmt.Sprintf(`
		SELECT
			tc.table_schema,
			tc.table_name,
			tc.constraint_name,
			kcu.column_name,
			ccu.table_schema  AS target_schema,
			ccu.table_name    AS target_table,
			ccu.column_name   AS target_column,
			rc.delete_rule,
			rc.update_rule
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
			ON kcu.constraint_schema = tc.constraint_schema
			AND kcu.constraint_name = tc.constraint_name
		JOIN information_schema.referential_constraints rc
			ON rc.constraint_schema = tc.constraint_schema
			AND rc.constraint_name = tc.constraint_name
		JOIN information_schema.constraint_column_usage ccu
			ON ccu.constraint_schema = rc.unique_constraint_schema
			AND ccu.constraint_name = rc.unique_constraint_name
		WHERE tc.constraint_type = 'FOREIGN KEY'
			AND tc.table_schema IN (%s)
		ORDER BY tc.table_schema, tc.table_name, tc.constraint_name, kcu.ordinal_position`,
		placeholderList(len(schemas), 1)), anySlice(schemas)...)
	if err != nil {
		return err
	}
	defer rows.Close()

	type fkKey struct {
		schema, table, name string
	}
	type fkRow struct {
		col, targetCol, targetTable, targetSchema, onDelete, onUpdate string
	}
	groups := map[fkKey][]fkRow{}
	for rows.Next() {
		var schema, table, name, col, tSchema, tTable, tCol, del, upd string
		if err := rows.Scan(&schema, &table, &name, &col, &tSchema, &tTable, &tCol, &del, &upd); err != nil {
			return err
		}
		groups[fkKey{schema, table, name}] = append(
			groups[fkKey{schema, table, name}],
			fkRow{col, tCol, tTable, tSchema, del, upd},
		)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for k, rs := range groups {
		if len(rs) != 1 {
			continue // skip composite FKs
		}
		ts, ok := tables[introspectTableKey(k.schema, k.table)]
		if !ok {
			continue
		}
		r := rs[0]
		schemaTo := r.targetSchema
		if schemaTo == "public" {
			schemaTo = ""
		}
		ts.ForeignKeys[k.name] = &ForeignKeySnapshot{
			Name:        k.name,
			TableFrom:   k.table,
			ColumnsFrom: []string{r.col},
			TableTo:     r.targetTable,
			SchemaTo:    schemaTo,
			ColumnsTo:   []string{r.targetCol},
			OnDelete:    strings.ToLower(r.onDelete),
			OnUpdate:    strings.ToLower(r.onUpdate),
		}
	}
	return nil
}

// placeholderList returns "$start, $start+1, ..., $start+count-1".
func placeholderList(count, start int) string {
	var b strings.Builder
	for i := 0; i < count; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "$%d", start+i)
	}
	return b.String()
}

// anySlice converts a []string to []any for variadic arg passing.
func anySlice(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

func derefNullString(s sql.NullString) string {
	if !s.Valid {
		return ""
	}
	return s.String
}
