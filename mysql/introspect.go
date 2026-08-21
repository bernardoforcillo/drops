package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// ServerInfo identifies the server on the other end of a connection.
//
// It exists because "MySQL" is two products. MariaDB forked before
// CHECK constraints, functional indexes, instant DDL and the JSON type
// existed, implemented most of them, and spells several of them
// differently; the migration layer has to know which one it is talking
// to before it can tell a real schema difference from a difference of
// dialect.
type ServerInfo struct {
	// Version is the raw VERSION() string, e.g. "8.4.0" or
	// "10.11.14-MariaDB-0ubuntu0.24.04.1".
	Version string
	// MariaDB is true when the server is MariaDB rather than MySQL.
	MariaDB bool
	// Major, Minor and Patch are the leading numeric components of
	// Version, zero when it could not be parsed.
	Major, Minor, Patch int
}

// String renders the server as "MariaDB 10.11.14" / "MySQL 8.4.0".
func (s ServerInfo) String() string {
	name := "MySQL"
	if s.MariaDB {
		name = "MariaDB"
	}
	return fmt.Sprintf("%s %d.%d.%d", name, s.Major, s.Minor, s.Patch)
}

// AtLeast reports whether the server is at or past a version. It is
// only meaningful within one product — 10.2 means one thing on MariaDB
// and does not exist on MySQL — so callers pair it with MariaDB.
func (s ServerInfo) AtLeast(major, minor, patch int) bool {
	if s.Major != major {
		return s.Major > major
	}
	if s.Minor != minor {
		return s.Minor > minor
	}
	return s.Patch >= patch
}

// SupportsCheckConstraints reports whether the server enforces CHECK
// constraints rather than parsing and discarding them.
//
// MySQL 5.7 accepts the syntax and ignores it entirely — the
// constraint is not stored, not enforced and not visible in
// information_schema — so a schema declaring one against 5.7 gets a
// comment. MySQL enforces from 8.0.16 and MariaDB from 10.2.1.
func (s ServerInfo) SupportsCheckConstraints() bool {
	if s.MariaDB {
		return s.AtLeast(10, 2, 1)
	}
	return s.AtLeast(8, 0, 16)
}

// SupportsDropConstraint reports whether ALTER TABLE … DROP CONSTRAINT
// names a CHECK constraint. MySQL 8.0.16 through 8.0.18 spell it DROP
// CHECK, which MariaDB in turn rejects; from 8.0.19 the generic form
// works on both, so drops emits only that one.
func (s ServerInfo) SupportsDropConstraint() bool {
	if s.MariaDB {
		return s.AtLeast(10, 2, 1)
	}
	return s.AtLeast(8, 0, 19)
}

// SupportsRenameColumn reports whether ALTER TABLE … RENAME COLUMN old
// TO new is understood. MySQL learned it in 8.0 and MariaDB in 10.5.2;
// before that a rename has to go through CHANGE COLUMN, which restates
// the whole definition and can copy the table to do it.
//
// A zero ServerInfo is a server of unknown version and answers false,
// so a migration generated without one carries the spelling every
// server accepts.
func (s ServerInfo) SupportsRenameColumn() bool {
	if s.MariaDB {
		return s.AtLeast(10, 5, 2)
	}
	return s.AtLeast(8, 0, 0)
}

// ServerVersion asks the server what it is.
func ServerVersion(ctx context.Context, db *DB) (ServerInfo, error) {
	rows, err := db.Query(ctx, "SELECT VERSION()")
	if err != nil {
		return ServerInfo{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return ServerInfo{}, err
		}
		return ServerInfo{}, fmt.Errorf("drops/mysql: VERSION() returned no row")
	}
	var v string
	if err := rows.Scan(&v); err != nil {
		return ServerInfo{}, err
	}
	return parseServerVersion(v), rows.Err()
}

// parseServerVersion reads the leading dotted number out of a VERSION()
// string and looks for the MariaDB marker, which that server appends to
// every build ("10.11.14-MariaDB-…") and which is the only reliable way
// to tell the two apart from one string.
func parseServerVersion(v string) ServerInfo {
	info := ServerInfo{
		Version: v,
		MariaDB: strings.Contains(strings.ToLower(v), "mariadb"),
	}
	head := v
	if i := strings.IndexAny(head, "-+ "); i >= 0 {
		head = head[:i]
	}
	parts := strings.Split(head, ".")
	dst := []*int{&info.Major, &info.Minor, &info.Patch}
	for i := 0; i < len(parts) && i < 3; i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			break
		}
		*dst[i] = n
	}
	return info
}

// IntrospectOptions tunes which database Introspect reads.
type IntrospectOptions struct {
	// Database restricts the introspection to one database. Empty
	// means the connection's current one, which is what a DSN names
	// and what unqualified DDL lands in.
	Database string
}

// Introspect queries the live server and returns a Snapshot of the
// database's current state: tables, columns, the primary key, the
// secondary indexes (unique ones included, since that is all a MySQL
// UNIQUE constraint is), single-column foreign keys with their
// referential actions, and CHECK constraints where the server has any.
//
// The result is in the same format as BuildSnapshot's output and can be
// diffed against it. Everything the schema layer can declare has to be
// read back here, or Diff sees the difference between "absent" and "not
// looked at" as work to do and Push stops being idempotent.
//
// Three things it deliberately does not report, each of which would
// otherwise be dropped on the next push:
//
//   - the index MySQL creates behind a foreign key. It has the
//     constraint's name and is not separable from it — DROP INDEX on
//     one answers error 1553 — so it belongs to the foreign key, not to
//     the schema;
//   - a multi-column foreign key, which the schema layer cannot
//     declare;
//   - on MariaDB, a column-level CHECK. That is where MariaDB keeps
//     the json_valid() constraint it attaches to every JSON column, an
//     artefact of JSON being LONGTEXT there, and the schema layer
//     declares only table-level checks.
//
// Expressions come back in the server's own spelling, not the one a Go
// program wrote: `age >= 0` reads back as "`age` >= 0". Comparing the
// two needs the declared side respelled by the server first; see
// probeCheckExpressions.
func Introspect(ctx context.Context, db *DB, opts ...IntrospectOptions) (*Snapshot, error) {
	var opt IntrospectOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	server, err := ServerVersion(ctx, db)
	if err != nil {
		return nil, err
	}

	snap := EmptySnapshot()
	snap.ID = newUUID()

	if err := readIntrospectTables(ctx, db, opt.Database, snap); err != nil {
		return nil, err
	}
	if err := readIntrospectColumns(ctx, db, opt.Database, snap); err != nil {
		return nil, err
	}
	// Foreign keys before indexes: the index reader needs the
	// constraint names to know which indexes are owned by one.
	fkIndexes, err := readIntrospectForeignKeys(ctx, db, opt.Database, snap)
	if err != nil {
		return nil, err
	}
	if err := readIntrospectIndexes(ctx, db, opt.Database, snap, fkIndexes); err != nil {
		return nil, err
	}
	if server.SupportsCheckConstraints() {
		if err := readIntrospectChecks(ctx, db, opt.Database, snap, server); err != nil {
			return nil, err
		}
	}
	return snap, nil
}

// schemaPredicate returns the WHERE fragment naming the database and
// the argument it binds. An empty name means the session's current
// database, which has no literal to bind — DATABASE() is the only way
// to say it.
func schemaPredicate(column, database string) (string, []any) {
	if database == "" {
		return column + " = DATABASE()", nil
	}
	return column + " = ?", []any{database}
}

func readIntrospectTables(ctx context.Context, db *DB, database string, snap *Snapshot) error {
	pred, args := schemaPredicate("TABLE_SCHEMA", database)
	rows, err := db.Query(ctx, `
		SELECT TABLE_NAME, ENGINE, TABLE_COLLATION
		FROM information_schema.TABLES
		WHERE `+pred+` AND TABLE_TYPE = 'BASE TABLE'
		ORDER BY TABLE_NAME`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var engine, collation sql.NullString
		if err := rows.Scan(&name, &engine, &collation); err != nil {
			return err
		}
		ts := newTableSnapshot(name)
		ts.Engine = engine.String
		ts.Collation = collation.String
		// information_schema reports the collation but not the
		// character set; the collation's first component is the
		// charset it belongs to, which is how the server derives one
		// from the other too.
		if i := strings.IndexByte(ts.Collation, '_'); i > 0 {
			ts.Charset = ts.Collation[:i]
		}
		snap.Tables[name] = ts
	}
	return rows.Err()
}

func readIntrospectColumns(ctx context.Context, db *DB, database string, snap *Snapshot) error {
	pred, args := schemaPredicate("TABLE_SCHEMA", database)
	rows, err := db.Query(ctx, `
		SELECT TABLE_NAME, COLUMN_NAME, COLUMN_TYPE, DATA_TYPE,
			IS_NULLABLE, COLUMN_DEFAULT, EXTRA, COLUMN_COMMENT
		FROM information_schema.COLUMNS
		WHERE `+pred+`
		ORDER BY TABLE_NAME, ORDINAL_POSITION`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var table, name, colType, dataType, nullable, extra, comment string
		var def sql.NullString
		if err := rows.Scan(&table, &name, &colType, &dataType, &nullable, &def, &extra, &comment); err != nil {
			return err
		}
		ts, ok := snap.Tables[table]
		if !ok {
			continue
		}
		lowerExtra := strings.ToLower(extra)
		cs := &ColumnSnapshot{
			Name:          name,
			Type:          normaliseType(colType),
			NotNull:       nullable == "NO",
			AutoIncrement: strings.Contains(lowerExtra, "auto_increment"),
			OnUpdate:      onUpdateFromExtra(lowerExtra),
			Comment:       comment,
		}
		if d := introspectedDefault(def, dataType, lowerExtra); d != "" {
			cs.Default = &d
		}
		ts.Columns[name] = cs
	}
	return rows.Err()
}

// onUpdateFromExtra pulls the per-column ON UPDATE clause out of
// information_schema's EXTRA, which is where both servers keep it —
// there is no column of its own.
func onUpdateFromExtra(extra string) string {
	const marker = "on update "
	i := strings.Index(extra, marker)
	if i < 0 {
		return ""
	}
	return canonicalDefault(strings.TrimSpace(extra[i+len(marker):]))
}

// introspectedDefault converts COLUMN_DEFAULT into the spelling a Go
// schema would have written, or "" when the column has no default.
//
// Two server-specific traps live here. MariaDB reports the string
// "NULL" — four characters, not SQL NULL — for a column that never had
// a default, which canonicalDefault folds away. And MySQL 8 reports a
// string default with its quotes stripped, so `DEFAULT 'draft'` comes
// back as draft and has to be re-quoted; only the catalogue knows the
// column was a string, which is why DATA_TYPE is consulted here rather
// than in canonicalDefault. An expression default is exempt: MySQL
// marks it DEFAULT_GENERATED and reports it as SQL, quotes and all.
func introspectedDefault(def sql.NullString, dataType, extra string) string {
	if !def.Valid {
		return ""
	}
	raw := def.String
	if isQuotableStringType(dataType) &&
		!strings.Contains(extra, "default_generated") &&
		!strings.HasPrefix(raw, "'") &&
		!strings.EqualFold(strings.TrimSpace(raw), "null") {
		return "'" + quoteLiteral(raw) + "'"
	}
	return canonicalDefault(raw)
}

func isQuotableStringType(dataType string) bool {
	switch strings.ToLower(dataType) {
	case "char", "varchar", "tinytext", "text", "mediumtext", "longtext",
		"enum", "set", "binary", "varbinary":
		return true
	}
	return false
}

// fkIndexKey identifies the index a foreign key owns.
type fkIndexKey struct{ table, index string }

// readIntrospectForeignKeys pulls single-column FOREIGN KEY
// constraints and returns the set of indexes they own.
//
// Composite keys are skipped rather than reported: the schema layer
// declares foreign keys one column at a time, so a composite one is by
// construction somebody else's, and reporting it would have Diff drop
// it on the first push.
func readIntrospectForeignKeys(ctx context.Context, db *DB, database string, snap *Snapshot) (map[fkIndexKey]bool, error) {
	pred, args := schemaPredicate("k.TABLE_SCHEMA", database)
	rows, err := db.Query(ctx, `
		SELECT k.TABLE_NAME, k.CONSTRAINT_NAME, k.COLUMN_NAME,
			k.REFERENCED_TABLE_NAME, k.REFERENCED_COLUMN_NAME,
			r.DELETE_RULE, r.UPDATE_RULE
		FROM information_schema.KEY_COLUMN_USAGE k
		JOIN information_schema.REFERENTIAL_CONSTRAINTS r
			ON r.CONSTRAINT_SCHEMA = k.CONSTRAINT_SCHEMA
			AND r.CONSTRAINT_NAME = k.CONSTRAINT_NAME
			AND r.TABLE_NAME = k.TABLE_NAME
		WHERE `+pred+` AND k.REFERENCED_TABLE_NAME IS NOT NULL
		ORDER BY k.TABLE_NAME, k.CONSTRAINT_NAME, k.ORDINAL_POSITION`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type fkRow struct {
		table, name          string
		from, toTable, toCol []string
		onDelete, onUpdate   string
	}
	grouped := map[fkIndexKey]*fkRow{}
	owned := map[fkIndexKey]bool{}
	for rows.Next() {
		var table, name, col, refTable, refCol, del, upd string
		if err := rows.Scan(&table, &name, &col, &refTable, &refCol, &del, &upd); err != nil {
			return nil, err
		}
		k := fkIndexKey{table, name}
		owned[k] = true
		r := grouped[k]
		if r == nil {
			r = &fkRow{table: table, name: name, onDelete: normaliseAction(del), onUpdate: normaliseAction(upd)}
			grouped[k] = r
		}
		r.from = append(r.from, col)
		r.toTable = append(r.toTable, refTable)
		r.toCol = append(r.toCol, refCol)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for k, r := range grouped {
		if len(r.from) != 1 {
			continue
		}
		ts, ok := snap.Tables[k.table]
		if !ok {
			continue
		}
		ts.ForeignKeys[r.name] = &ForeignKeySnapshot{
			Name:        r.name,
			TableFrom:   r.table,
			ColumnsFrom: r.from,
			TableTo:     r.toTable[0],
			ColumnsTo:   r.toCol,
			OnDelete:    r.onDelete,
			OnUpdate:    r.onUpdate,
		}
	}
	return owned, nil
}

// readIntrospectIndexes fills in the primary key and the secondary
// indexes.
//
// information_schema.STATISTICS reports one row per key part rather
// than one per index, ordered by SEQ_IN_INDEX, so an index is
// assembled across rows and a column list is only complete once the
// name changes. The index named PRIMARY is the primary key however the
// schema spelled it — MySQL renames it — and is lifted out rather than
// left among the secondary indexes.
//
// A key part whose COLUMN_NAME is NULL is a functional key part, which
// MySQL 8.0.13 added and the snapshot cannot describe. One of them
// makes the whole index unrepresentable, and it is recorded with no
// columns — the shape Diff skips and Push reports — rather than as a
// narrower index that would agree with itself for ever after.
func readIntrospectIndexes(ctx context.Context, db *DB, database string, snap *Snapshot, fkOwned map[fkIndexKey]bool) error {
	pred, args := schemaPredicate("TABLE_SCHEMA", database)
	rows, err := db.Query(ctx, `
		SELECT TABLE_NAME, INDEX_NAME, NON_UNIQUE, COLUMN_NAME,
			SUB_PART, INDEX_TYPE, INDEX_COMMENT
		FROM information_schema.STATISTICS
		WHERE `+pred+`
		ORDER BY TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	unrepresentable := map[*IndexSnapshot]bool{}
	for rows.Next() {
		var table, index, indexType, comment string
		var nonUnique int
		var column sql.NullString
		var subPart sql.NullInt64
		if err := rows.Scan(&table, &index, &nonUnique, &column, &subPart, &indexType, &comment); err != nil {
			return err
		}
		ts, ok := snap.Tables[table]
		if !ok {
			continue
		}
		if fkOwned[fkIndexKey{table, index}] {
			continue
		}
		if index == primaryKeyName {
			if ts.PrimaryKey == nil {
				ts.PrimaryKey = &PrimaryKeySnapshot{Name: primaryKeyName}
			}
			ts.PrimaryKey.Columns = append(ts.PrimaryKey.Columns, column.String)
			ts.PrimaryKey.Prefixes = append(ts.PrimaryKey.Prefixes, int(subPart.Int64))
			continue
		}
		is := ts.Indexes[index]
		if is == nil {
			is = &IndexSnapshot{
				Name:     index,
				IsUnique: nonUnique == 0,
				Method:   normaliseIndexMethod(indexType),
				Comment:  comment,
			}
			ts.Indexes[index] = is
		}
		if !column.Valid {
			unrepresentable[is] = true
			continue
		}
		is.Columns = append(is.Columns, column.String)
		is.Prefixes = append(is.Prefixes, int(subPart.Int64))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, ts := range snap.Tables {
		if ts.PrimaryKey != nil {
			ts.PrimaryKey.Prefixes = trimPrefixes(ts.PrimaryKey.Prefixes)
		}
		for _, is := range ts.Indexes {
			if unrepresentable[is] {
				is.Columns = nil
				is.Prefixes = nil
				continue
			}
			is.Prefixes = trimPrefixes(is.Prefixes)
		}
	}
	return nil
}

// normaliseIndexMethod folds the default access method to the empty
// string, which is what a schema that never called Using holds.
func normaliseIndexMethod(m string) string {
	if strings.EqualFold(m, "BTREE") {
		return ""
	}
	return strings.ToUpper(m)
}

// readIntrospectChecks pulls table-level CHECK constraints.
//
// The two servers put the same information in different places.
// MariaDB's information_schema.CHECK_CONSTRAINTS carries TABLE_NAME
// and LEVEL, so it can be read alone and column-level checks — where
// the generated json_valid() constraint of a JSON column lives — can be
// excluded. MySQL's carries neither, because it scopes a check name to
// the schema rather than the table, so the table has to come from a
// join against TABLE_CONSTRAINTS. Joining that way on MariaDB would
// duplicate every check whose name is reused on another table, which
// MariaDB permits and MySQL does not.
func readIntrospectChecks(ctx context.Context, db *DB, database string, snap *Snapshot, server ServerInfo) error {
	var query string
	var args []any
	if server.MariaDB {
		pred, a := schemaPredicate("CONSTRAINT_SCHEMA", database)
		args = a
		query = `
			SELECT TABLE_NAME, CONSTRAINT_NAME, CHECK_CLAUSE
			FROM information_schema.CHECK_CONSTRAINTS
			WHERE ` + pred + ` AND LEVEL = 'Table'
			ORDER BY TABLE_NAME, CONSTRAINT_NAME`
	} else {
		pred, a := schemaPredicate("tc.TABLE_SCHEMA", database)
		args = a
		query = `
			SELECT tc.TABLE_NAME, tc.CONSTRAINT_NAME, cc.CHECK_CLAUSE
			FROM information_schema.TABLE_CONSTRAINTS tc
			JOIN information_schema.CHECK_CONSTRAINTS cc
				ON cc.CONSTRAINT_SCHEMA = tc.CONSTRAINT_SCHEMA
				AND cc.CONSTRAINT_NAME = tc.CONSTRAINT_NAME
			WHERE ` + pred + ` AND tc.CONSTRAINT_TYPE = 'CHECK'
			ORDER BY tc.TABLE_NAME, tc.CONSTRAINT_NAME`
	}
	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var table, name, clause string
		if err := rows.Scan(&table, &name, &clause); err != nil {
			return err
		}
		ts, ok := snap.Tables[table]
		if !ok {
			continue
		}
		ts.CheckConstraints[name] = &CheckSnapshot{Name: name, Value: unwrapCheckClause(clause)}
	}
	return rows.Err()
}

// unwrapCheckClause strips the parentheses MySQL wraps a stored CHECK
// in. MySQL 8 reports "(`age` >= 0)" where MariaDB reports
// "`age` >= 0"; the probe respells the declared side through the same
// server, so both sides pass through here and meet.
func unwrapCheckClause(clause string) string {
	out := strings.TrimSpace(clause)
	if wrappedInParens(out) {
		out = strings.TrimSpace(out[1 : len(out)-1])
	}
	return out
}

// wrappedInParens reports whether s is one parenthesised group — that
// is, whether its leading "(" is closed by its trailing ")" rather than
// by something in the middle, as in "(a) and (b)".
func wrappedInParens(s string) bool {
	if len(s) < 2 || s[0] != '(' || s[len(s)-1] != ')' {
		return false
	}
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'', '"', '`':
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
// start, honouring the doubled-quote escape MySQL's string and
// identifier literals share. A parenthesis inside one is text, not
// structure.
func scanQuoted(s string, start int, quote byte) int {
	for i := start + 1; i < len(s); i++ {
		if s[i] == '\\' && quote != '`' {
			i++
			continue
		}
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
