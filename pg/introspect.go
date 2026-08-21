package pg

import (
	"context"
	"database/sql"
	"fmt"
	"math"
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
// from int + nextval default), primary keys, unique constraints, CHECK
// constraints, indexes, and single-column foreign keys with referential
// actions.
//
// Alongside the tables it reads the schema-level objects: enum types
// with their labels in catalogue order, standalone sequences, views and
// materialised views, and per table whether row-level security is
// enabled and forced together with the policies attached to it.
//
// Enums, sequences and views are keyed by bare name, the way
// BuildSnapshot keys them and the snapshot format holds them, so
// introspecting two schemas that each carry a type of the same name
// reports only one of the two. Tables are keyed schema-qualified and
// have no such limit.
//
// The returned snapshot is in the same format as BuildSnapshot's output
// and can be diffed against a Go-schema snapshot via Diff. Everything
// the schema layer can declare has to be read back here, or Diff sees
// the difference between "absent" and "not looked at" as work to do and
// Push stops being idempotent. What remains unread is listed under
// "What Push cannot see" in Push's doc comment.
//
// # What belongs to somebody else
//
// An object created by an extension is not reported at all. PostGIS
// installs a table, two views and a fistful of types; a serial column
// owns a sequence and an identity column owns another. None of them
// appear in any Go schema, so Diff — which compares two declarations
// and takes an absence for a removal — would emit a DROP for every one
// of them. They are recognised by their pg_depend entry: an extension
// member (deptype 'e') or a sequence owned by a column ('a' or 'i').
//
// An object a person created outside the Go schema cannot be told from
// one by any catalogue column, so it *is* reported, and Push is where
// the drop is withheld and named — see "An object the schema never
// declared" in Push's doc comment.
//
// The expressions read back here — a CHECK's body, a partial index's
// predicate, a policy's USING clause, a view's body — are PostgreSQL's
// own spelling of them, not the one a Go schema wrote. Comparing the
// two needs the declared side respelled by the server first; see
// renormaliseExpressions.
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
	if err := readIntrospectChecks(ctx, db, opt.Schemas, snap.Tables); err != nil {
		return nil, err
	}
	if err := readIntrospectIndexes(ctx, db, opt.Schemas, snap.Tables); err != nil {
		return nil, err
	}
	if err := readIntrospectForeignKeys(ctx, db, opt.Schemas, snap.Tables); err != nil {
		return nil, err
	}
	if err := readIntrospectPolicies(ctx, db, opt.Schemas, snap.Tables); err != nil {
		return nil, err
	}
	if err := readIntrospectEnums(ctx, db, opt.Schemas, snap); err != nil {
		return nil, err
	}
	if err := readIntrospectSequences(ctx, db, opt.Schemas, snap); err != nil {
		return nil, err
	}
	if err := readIntrospectViews(ctx, db, opt.Schemas, snap); err != nil {
		return nil, err
	}
	return snap, nil
}

// notExtensionOwned is the WHERE fragment that hides an object an
// extension installed. classid names the catalogue the object lives
// in; alias is the alias its oid is available under.
//
// deptype 'e' is extension membership. For a sequence, 'a' and 'i'
// matter as well: an owned sequence — a serial column's, an identity
// column's — is created and dropped with the column, appears in no Go
// declaration, and cannot be dropped on its own.
func notExtensionOwned(classid, alias string, deptypes string) string {
	return fmt.Sprintf(
		`NOT EXISTS (SELECT 1 FROM pg_depend d
			WHERE d.classid = '%s'::regclass AND d.objid = %s.oid
				AND d.deptype IN (%s))`, classid, alias, deptypes)
}

func introspectTableKey(schema, name string) string {
	if schema == "" {
		schema = "public"
	}
	return schema + "." + name
}

// readIntrospectTables lists all base tables in the requested schemas,
// with their row-level security flags.
//
// The source is pg_class rather than information_schema.tables: the
// two agree on which relations are base tables ('r' ordinary and 'p'
// partitioned), but relrowsecurity and relforcerowsecurity have no
// information_schema counterpart, and neither does the extension
// membership that hides PostGIS's spatial_ref_sys from a Diff that
// would otherwise DROP TABLE it.
func readIntrospectTables(ctx context.Context, db *DB, schemas []string) ([]*TableSnapshot, error) {
	rows, err := db.Query(ctx, fmt.Sprintf(`
		SELECT n.nspname, c.relname, c.relrowsecurity, c.relforcerowsecurity
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'p') AND n.nspname IN (%s)
			AND %s
		ORDER BY n.nspname, c.relname`,
		placeholderList(len(schemas)),
		notExtensionOwned("pg_class", "c", "'e'")), anySlice(schemas)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*TableSnapshot
	for rows.Next() {
		var schema, name string
		var rls, forced bool
		if err := rows.Scan(&schema, &name, &rls, &forced); err != nil {
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
			Indexes:              map[string]*IndexSnapshot{},
			ForeignKeys:          map[string]*ForeignKeySnapshot{},
			CompositePrimaryKeys: map[string]*CompositePKSnapshot{},
			UniqueConstraints:    map[string]*UniqueSnapshot{},
			Policies:             map[string]*PolicySnapshot{},
			CheckConstraints:     map[string]*CheckSnapshot{},
			IsRLSEnabled:         rls,
			IsRLSForced:          forced,
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
		placeholderList(len(schemas))), anySlice(schemas)...)
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

// readIntrospectPrimaryKeys records each table's PRIMARY KEY.
//
// Where the key lands mirrors BuildSnapshot: in CompositePrimaryKeys
// under the name the catalogue holds, whatever its arity, and
// additionally as PrimaryKey=true on the column when it spans exactly
// one — the spelling drizzle-kit uses and `drops pull` reads.
// Marking every column of a two-column key PrimaryKey=true instead
// would describe a table PostgreSQL cannot have: two inline primary
// keys.
//
// The column list is what Diff compares, so a one-column key has to
// arrive as a list too. Recording it only as a bool left the width of
// the key unsayable, and a key that narrowed from two columns to one
// was dropped and never re-added.
//
// Key columns are NOT NULL either way; PostgreSQL sets attnotnull when
// the key is created.
func readIntrospectPrimaryKeys(ctx context.Context, db *DB, schemas []string, tables map[string]*TableSnapshot) error {
	rows, err := db.Query(ctx, fmt.Sprintf(`
		SELECT tc.table_schema, tc.table_name, tc.constraint_name, kcu.column_name
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
			ON kcu.constraint_schema = tc.constraint_schema
			AND kcu.constraint_name = tc.constraint_name
		WHERE tc.constraint_type = 'PRIMARY KEY'
			AND tc.table_schema IN (%s)
		ORDER BY tc.table_schema, tc.table_name, kcu.ordinal_position`,
		placeholderList(len(schemas))), anySlice(schemas)...)
	if err != nil {
		return err
	}
	defer rows.Close()

	type pkKey struct {
		schema, table, name string
	}
	var order []pkKey
	cols := map[pkKey][]string{}
	for rows.Next() {
		var schema, table, name, column string
		if err := rows.Scan(&schema, &table, &name, &column); err != nil {
			return err
		}
		k := pkKey{schema, table, name}
		if _, seen := cols[k]; !seen {
			order = append(order, k)
		}
		cols[k] = append(cols[k], column)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, k := range order {
		ts, ok := tables[introspectTableKey(k.schema, k.table)]
		if !ok {
			continue
		}
		columns := cols[k]
		for _, name := range columns {
			if col, ok := ts.Columns[name]; ok {
				col.NotNull = true
				col.PrimaryKey = len(columns) == 1
			}
		}
		ts.CompositePrimaryKeys[k.name] = &CompositePKSnapshot{
			Name:    k.name,
			Columns: columns,
		}
	}
	return nil
}

// readIntrospectUniques pulls UNIQUE constraints, single- and
// multi-column alike.
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
		placeholderList(len(schemas))), anySlice(schemas)...)
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
		ts.UniqueConstraints[k.name] = &UniqueSnapshot{
			Name:             k.name,
			NullsNotDistinct: false,
			Columns:          columns,
		}
	}
	return nil
}

// checkExprOf reduces a pg_get_constraintdef result to the expression
// a CHECK clause wraps.
//
// The definition is always "CHECK (" + the deparsed expression + ")",
// with NOT VALID or NO INHERIT appended when they apply; peeling that
// wrapper off leaves the same spelling pg_get_expr gives for an index
// predicate, which is what lets one probe answer for both.
func checkExprOf(def string) string {
	out := strings.TrimSpace(def)
	for _, suffix := range []string{" NOT VALID", " NO INHERIT"} {
		out = strings.TrimSuffix(out, suffix)
	}
	out = strings.TrimSpace(strings.TrimPrefix(out, "CHECK "))
	if wrappedInParens(out) {
		out = out[1 : len(out)-1]
	}
	return out
}

// wrappedInParens reports whether s is one parenthesised group — that
// is, whether its leading "(" is closed by its trailing ")" rather
// than by something in the middle, as in "(a) AND (b)".
func wrappedInParens(s string) bool {
	if len(s) < 2 || s[0] != '(' || s[len(s)-1] != ')' {
		return false
	}
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'', '"':
			i = scanQuoted(s, i, s[i]) - 1
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 && i != len(s)-1 {
				return false
			}
		}
	}
	return depth == 0
}

// scanQuoted returns the index just past the quoted run starting at
// start, honouring the doubled-quote escape both kinds of SQL literal
// use. A parenthesis inside a literal is text, not structure.
func scanQuoted(s string, start int, quote byte) int {
	for i := start + 1; i < len(s); i++ {
		if s[i] != quote {
			continue
		}
		if i+1 < len(s) && s[i+1] == quote {
			i++
			continue
		}
		return i + 1
	}
	return len(s)
}

// readIntrospectChecks pulls CHECK constraints.
//
// The source is pg_constraint rather than
// information_schema.check_constraints, which also lists one synthetic
// "%_not_null" row per NOT NULL column — those are attnotnull, already
// carried on the column snapshot, and reporting them as constraints
// would have Diff try to drop them.
func readIntrospectChecks(ctx context.Context, db *DB, schemas []string, tables map[string]*TableSnapshot) error {
	rows, err := db.Query(ctx, fmt.Sprintf(`
		SELECT n.nspname, rel.relname, con.conname, pg_get_constraintdef(con.oid)
		FROM pg_constraint con
		JOIN pg_class rel ON rel.oid = con.conrelid
		JOIN pg_namespace n ON n.oid = rel.relnamespace
		WHERE con.contype = 'c'
			AND n.nspname IN (%s)
		ORDER BY n.nspname, rel.relname, con.conname`,
		placeholderList(len(schemas))), anySlice(schemas)...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var schema, table, name, def string
		if err := rows.Scan(&schema, &table, &name, &def); err != nil {
			return err
		}
		ts, ok := tables[introspectTableKey(schema, table)]
		if !ok {
			continue
		}
		ts.CheckConstraints[name] = &CheckSnapshot{
			Name:  name,
			Value: checkExprOf(def),
		}
	}
	return rows.Err()
}

// readIntrospectIndexes pulls the standalone indexes on each table.
//
// Indexes that back a constraint are excluded: a primary key or a
// UNIQUE constraint owns its index, the constraint readers already
// report it, and DROP INDEX on one is refused (SQLSTATE 2BP01).
//
// indkey lists the key columns and the INCLUDE columns end to end;
// indnkeyatts is where the first ends and the second begins. Reading
// them as one list made a covering index look like a wider plain one,
// so a declared INCLUDE never matched what the catalogue held.
//
// An expression element has no pg_attribute row behind it (indkey
// carries 0), and the snapshot has no way to describe one. Such an
// index is recorded with no columns at all, the same shape
// BuildSnapshot gives a declared functional index, so the two sides
// still compare equal instead of the catalogue reporting a narrower
// index than the schema declared and Diff dropping it every push.
func readIntrospectIndexes(ctx context.Context, db *DB, schemas []string, tables map[string]*TableSnapshot) error {
	rows, err := db.Query(ctx, fmt.Sprintf(`
		SELECT n.nspname, rel.relname, idx.relname, ix.indisunique, am.amname,
			coalesce(pg_get_expr(ix.indpred, ix.indrelid), ''),
			coalesce(a.attname, ''), k.ord <= ix.indnkeyatts
		FROM pg_index ix
		JOIN pg_class idx ON idx.oid = ix.indexrelid
		JOIN pg_class rel ON rel.oid = ix.indrelid
		JOIN pg_namespace n ON n.oid = rel.relnamespace
		JOIN pg_am am ON am.oid = idx.relam
		JOIN unnest(ix.indkey) WITH ORDINALITY AS k(attnum, ord) ON true
		LEFT JOIN pg_attribute a ON a.attrelid = ix.indrelid AND a.attnum = k.attnum
		WHERE n.nspname IN (%s)
			AND NOT ix.indisprimary
			AND NOT EXISTS (SELECT 1 FROM pg_constraint con WHERE con.conindid = idx.oid)
		ORDER BY n.nspname, rel.relname, idx.relname, k.ord`,
		placeholderList(len(schemas))), anySlice(schemas)...)
	if err != nil {
		return err
	}
	defer rows.Close()

	// Indexes with an element the snapshot cannot describe, by
	// pointer: the rows of one index arrive together but the verdict
	// is only known once they all have.
	unrepresentable := map[*IndexSnapshot]bool{}
	for rows.Next() {
		var schema, table, name, method, where, column string
		var unique, isKey bool
		if err := rows.Scan(&schema, &table, &name, &unique, &method, &where, &column, &isKey); err != nil {
			return err
		}
		ts, ok := tables[introspectTableKey(schema, table)]
		if !ok {
			continue
		}
		is := ts.Indexes[name]
		if is == nil {
			is = &IndexSnapshot{
				Name:     name,
				IsUnique: unique,
				Method:   method,
				Where:    where,
				With:     map[string]any{},
			}
			ts.Indexes[name] = is
		}
		if column == "" {
			unrepresentable[is] = true
			continue
		}
		if isKey {
			is.Columns = append(is.Columns, column)
		} else {
			is.Include = append(is.Include, column)
		}
	}
	for is := range unrepresentable {
		is.Columns = nil
	}
	return rows.Err()
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
		placeholderList(len(schemas))), anySlice(schemas)...)
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

// readIntrospectPolicies pulls the row-level security policies
// attached to each table.
//
// The USING and WITH CHECK expressions come back through pg_get_expr,
// which prints a parse tree rather than echoing what CREATE POLICY was
// given — the same gap CHECK constraints have, closed the same way by
// Push. The parenthesised form pg_get_expr returns is kept verbatim,
// because that is also what checkExprOf leaves behind when the probe
// respells the declared side.
//
// polroles holds oid 0 for PUBLIC, which CREATE POLICY spells by
// omitting the TO clause entirely; it is dropped here so a policy the
// Go schema declared without roles compares equal to the one the
// server holds.
func readIntrospectPolicies(ctx context.Context, db *DB, schemas []string, tables map[string]*TableSnapshot) error {
	rows, err := db.Query(ctx, fmt.Sprintf(`
		SELECT n.nspname, c.relname, pol.polname, pol.polcmd, pol.polpermissive,
			array_to_string(ARRAY(
				SELECT pg_get_userbyid(r) FROM unnest(pol.polroles) AS r WHERE r <> 0
			), ','),
			coalesce(pg_get_expr(pol.polqual, pol.polrelid), ''),
			coalesce(pg_get_expr(pol.polwithcheck, pol.polrelid), '')
		FROM pg_policy pol
		JOIN pg_class c ON c.oid = pol.polrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname IN (%s)
		ORDER BY n.nspname, c.relname, pol.polname`,
		placeholderList(len(schemas))), anySlice(schemas)...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var schema, table, name, cmd, roles, using, withCheck string
		var permissive bool
		if err := rows.Scan(&schema, &table, &name, &cmd, &permissive, &roles, &using, &withCheck); err != nil {
			return err
		}
		ts, ok := tables[introspectTableKey(schema, table)]
		if !ok {
			continue
		}
		as := "RESTRICTIVE"
		if permissive {
			as = "PERMISSIVE"
		}
		var to []string
		if roles != "" {
			to = strings.Split(roles, ",")
		}
		ts.Policies[name] = &PolicySnapshot{
			Name:      name,
			As:        as,
			For:       policyCommandOf(cmd),
			To:        to,
			Using:     using,
			WithCheck: withCheck,
		}
	}
	return rows.Err()
}

// policyCommandOf expands pg_policy.polcmd to the keyword CREATE
// POLICY's FOR clause uses, which is what the schema layer declares.
func policyCommandOf(cmd string) string {
	switch cmd {
	case "r":
		return "SELECT"
	case "a":
		return "INSERT"
	case "w":
		return "UPDATE"
	case "d":
		return "DELETE"
	}
	return "ALL"
}

// readIntrospectEnums pulls the enum types declared in the requested
// schemas.
//
// Labels are ordered by enumsortorder, not by oid: ALTER TYPE ... ADD
// VALUE BEFORE inserts a label whose oid is the highest of the set and
// whose position is not, and the position is the part that is the
// type. Two enums with the same labels in a different order are
// different types, and PostgreSQL cannot turn one into the other, so
// the order has to survive the read for Push to be able to say so.
func readIntrospectEnums(ctx context.Context, db *DB, schemas []string, snap *Snapshot) error {
	rows, err := db.Query(ctx, fmt.Sprintf(`
		SELECT n.nspname, t.typname, e.enumlabel
		FROM pg_type t
		JOIN pg_namespace n ON n.oid = t.typnamespace
		JOIN pg_enum e ON e.enumtypid = t.oid
		WHERE t.typtype = 'e' AND n.nspname IN (%s)
			AND %s
		ORDER BY n.nspname, t.typname, e.enumsortorder`,
		placeholderList(len(schemas)),
		notExtensionOwned("pg_type", "t", "'e'")), anySlice(schemas)...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var schema, name, label string
		if err := rows.Scan(&schema, &name, &label); err != nil {
			return err
		}
		e := snap.Enums[name]
		if e == nil {
			e = &EnumSnapshot{Name: name, Schema: schema}
			snap.Enums[name] = e
		}
		e.Values = append(e.Values, label)
	}
	return rows.Err()
}

// readIntrospectSequences pulls the standalone sequences.
//
// An attribute the declaration left to PostgreSQL is read back unset
// rather than as the value PostgreSQL chose. NewSequence("orderSeq")
// names none of them, so recording the START WITH 1, MINVALUE 1 and
// MAXVALUE 9223372036854775807 the server picked would leave the two
// sides unequal for every sequence anybody ever declared plainly.
func readIntrospectSequences(ctx context.Context, db *DB, schemas []string, snap *Snapshot) error {
	rows, err := db.Query(ctx, fmt.Sprintf(`
		SELECT n.nspname, c.relname,
			s.seqstart, s.seqincrement, s.seqmin, s.seqmax, s.seqcache, s.seqcycle
		FROM pg_sequence s
		JOIN pg_class c ON c.oid = s.seqrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname IN (%s)
			AND %s
		ORDER BY n.nspname, c.relname`,
		placeholderList(len(schemas)),
		notExtensionOwned("pg_class", "c", "'e', 'a', 'i'")), anySlice(schemas)...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var schema, name string
		var start, increment, minValue, maxValue, cache int64
		var cycle bool
		if err := rows.Scan(&schema, &name, &start, &increment, &minValue, &maxValue, &cache, &cycle); err != nil {
			return err
		}
		ss := &SequenceSnapshot{Name: name, Schema: schema, Cycle: cycle}
		d := sequenceDefaults(increment)
		ss.Increment = nonDefaultInt64(increment, d.increment)
		ss.Start = nonDefaultInt64(start, d.start)
		ss.MinValue = nonDefaultInt64(minValue, d.minValue)
		ss.MaxValue = nonDefaultInt64(maxValue, d.maxValue)
		ss.Cache = nonDefaultInt64(cache, d.cache)
		snap.Sequences[name] = ss
	}
	return rows.Err()
}

// seqDefaults are the values CREATE SEQUENCE picks for the attributes
// a statement does not name.
type seqDefaults struct {
	start, increment, minValue, maxValue, cache int64
}

// sequenceDefaults returns the defaults for a sequence with the given
// increment, which is what decides the direction: a descending
// sequence starts at -1 and runs to the bottom of the range, an
// ascending one starts at 1 and runs to the top.
//
// The bounds are bigint's. A sequence declared over a narrower type
// has narrower ones and reads back with them spelled out, which is the
// truth about it — SequenceOptions cannot name a type, so nothing
// declared through drops lands there.
func sequenceDefaults(increment int64) seqDefaults {
	if increment < 0 {
		return seqDefaults{start: -1, increment: 1, minValue: math.MinInt64, maxValue: -1, cache: 1}
	}
	return seqDefaults{start: 1, increment: 1, minValue: 1, maxValue: math.MaxInt64, cache: 1}
}

// nonDefaultInt64 returns a pointer to v, or nil when v is what the
// server would have chosen anyway.
func nonDefaultInt64(v, def int64) *int64 {
	if v == def {
		return nil
	}
	out := v
	return &out
}

// readIntrospectViews pulls views and materialised views.
//
// pg_get_viewdef returns the rewritten query, indented and terminated
// with a semicolon; the semicolon is stripped because the snapshot
// holds the body a CREATE VIEW ... AS would be given, and createViewSQL
// appends its own. The body is PostgreSQL's spelling of the query, not
// the Go schema's — see renormaliseExpressions for how the declared
// side is brought into the same one.
func readIntrospectViews(ctx context.Context, db *DB, schemas []string, snap *Snapshot) error {
	rows, err := db.Query(ctx, fmt.Sprintf(`
		SELECT n.nspname, c.relname, c.relkind, pg_get_viewdef(c.oid, true)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('v', 'm') AND n.nspname IN (%s)
			AND %s
		ORDER BY n.nspname, c.relname`,
		placeholderList(len(schemas)),
		notExtensionOwned("pg_class", "c", "'e'")), anySlice(schemas)...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var schema, name, kind, def string
		if err := rows.Scan(&schema, &name, &kind, &def); err != nil {
			return err
		}
		snap.Views[name] = &ViewSnapshot{
			Name:         name,
			Schema:       schema,
			Definition:   viewBodyOf(def),
			Materialized: kind == "m",
		}
	}
	return rows.Err()
}

// viewBodyOf trims pg_get_viewdef's output down to the query itself.
func viewBodyOf(def string) string {
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(def), ";"))
}

// placeholderList returns "$1, $2, ..., $count".
func placeholderList(count int) string {
	var b strings.Builder
	for i := 0; i < count; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "$%d", 1+i)
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
