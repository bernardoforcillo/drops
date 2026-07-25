package sqlite

// The Snapshot, TableSnapshot, ColumnSnapshot, ForeignKeySnapshot,
// UniqueSnapshot and CompositePKSnapshot types referenced here are
// defined in snapshot.go (same package). This file only reads a live
// SQLite database via its catalog and populates those types; it does
// not declare them.

import (
	"context"
	"sort"
	"strings"
)

// Introspect queries a live SQLite database and returns a Snapshot
// describing its current state — tables, columns (declared type,
// NOT NULL, default, primary key), single- and multi-column foreign
// keys with referential actions, composite primary keys, and UNIQUE
// constraints/indexes.
//
// It mirrors pg.Introspect but reads SQLite's catalog: sqlite_master
// for the table list and the PRAGMA table_info / foreign_key_list /
// index_list / index_info functions for per-table detail. The
// migration-history table (_drops_sqlite_migrations) and SQLite's
// internal sqlite_* tables are skipped.
func Introspect(ctx context.Context, db *DB) (*Snapshot, error) {
	snap := &Snapshot{Tables: map[string]*TableSnapshot{}}

	tables, err := introspectTableNames(ctx, db)
	if err != nil {
		return nil, err
	}

	for _, name := range tables {
		ts := &TableSnapshot{
			Name:                 name,
			Columns:              map[string]*ColumnSnapshot{},
			ForeignKeys:          map[string]*ForeignKeySnapshot{},
			CompositePrimaryKeys: map[string]*CompositePKSnapshot{},
			UniqueConstraints:    map[string]*UniqueSnapshot{},
		}
		if err := introspectColumns(ctx, db, ts); err != nil {
			return nil, err
		}
		if err := introspectForeignKeys(ctx, db, ts); err != nil {
			return nil, err
		}
		if err := introspectUniques(ctx, db, ts); err != nil {
			return nil, err
		}
		snap.Tables[name] = ts
	}
	return snap, nil
}

// introspectTableNames lists user tables, skipping SQLite internals and
// the drops migration-history table.
func introspectTableNames(ctx context.Context, db *DB) ([]string, error) {
	rows, err := queryRows(ctx, db,
		`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rows {
		name := asString(r["name"])
		if name == "" || name == "_drops_sqlite_migrations" {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

// introspectColumns fills Columns (and any composite primary key) on ts
// using PRAGMA table_info.
func introspectColumns(ctx context.Context, db *DB, ts *TableSnapshot) error {
	rows, err := queryRows(ctx, db, `PRAGMA table_info(`+quoteLiteral(ts.Name)+`)`)
	if err != nil {
		return err
	}

	// Track PK columns in declared order (pk is the 1-based position of
	// the column within the primary key, 0 when not part of it).
	type pkCol struct {
		name string
		pos  int64
	}
	var pks []pkCol

	for _, r := range rows {
		name := asString(r["name"])
		cs := &ColumnSnapshot{
			Name:    name,
			Type:    asString(r["type"]),
			NotNull: asInt64(r["notnull"]) != 0,
			Default: asNullableString(r["dflt_value"]),
		}
		pk := asInt64(r["pk"])
		if pk > 0 {
			pks = append(pks, pkCol{name: name, pos: pk})
		}
		ts.Columns[name] = cs
	}

	switch len(pks) {
	case 0:
		// no primary key
	case 1:
		if c, ok := ts.Columns[pks[0].name]; ok {
			c.PrimaryKey = true
		}
	default:
		sort.Slice(pks, func(i, j int) bool { return pks[i].pos < pks[j].pos })
		cols := make([]string, len(pks))
		for i, p := range pks {
			cols[i] = p.name
		}
		name := ts.Name + "_pk"
		ts.CompositePrimaryKeys[name] = &CompositePKSnapshot{
			Name:    name,
			Columns: cols,
		}
	}
	return nil
}

// introspectForeignKeys assembles single- and multi-column foreign keys
// from PRAGMA foreign_key_list, grouping rows by the FK id.
func introspectForeignKeys(ctx context.Context, db *DB, ts *TableSnapshot) error {
	rows, err := queryRows(ctx, db, `PRAGMA foreign_key_list(`+quoteLiteral(ts.Name)+`)`)
	if err != nil {
		return err
	}

	type fkRow struct {
		seq          int64
		from, to     string
		table        string
		onDel, onUpd string
	}
	groups := map[int64][]fkRow{}
	var ids []int64
	for _, r := range rows {
		id := asInt64(r["id"])
		if _, seen := groups[id]; !seen {
			ids = append(ids, id)
		}
		groups[id] = append(groups[id], fkRow{
			seq:   asInt64(r["seq"]),
			from:  asString(r["from"]),
			to:    asString(r["to"]),
			table: asString(r["table"]),
			onDel: asString(r["on_delete"]),
			onUpd: asString(r["on_update"]),
		})
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	for _, id := range ids {
		grp := groups[id]
		sort.Slice(grp, func(i, j int) bool { return grp[i].seq < grp[j].seq })
		from := make([]string, len(grp))
		to := make([]string, len(grp))
		for i, g := range grp {
			from[i] = g.from
			to[i] = g.to
		}
		tableTo := grp[0].table
		name := fkConstraintName(ts.Name, from, tableTo, to)
		ts.ForeignKeys[name] = &ForeignKeySnapshot{
			Name:        name,
			TableFrom:   ts.Name,
			ColumnsFrom: from,
			TableTo:     tableTo,
			ColumnsTo:   to,
			OnDelete:    normaliseFKAction(grp[0].onDel),
			OnUpdate:    normaliseFKAction(grp[0].onUpd),
		}
	}
	return nil
}

// introspectUniques records UNIQUE constraints/indexes from
// PRAGMA index_list + index_info. Primary-key indexes (origin "pk") are
// ignored — they are captured as PrimaryKey / CompositePrimaryKeys.
func introspectUniques(ctx context.Context, db *DB, ts *TableSnapshot) error {
	idxRows, err := queryRows(ctx, db, `PRAGMA index_list(`+quoteLiteral(ts.Name)+`)`)
	if err != nil {
		return err
	}
	for _, r := range idxRows {
		if asInt64(r["unique"]) == 0 {
			continue
		}
		if strings.EqualFold(asString(r["origin"]), "pk") {
			continue
		}
		idxName := asString(r["name"])
		if idxName == "" {
			continue
		}
		infoRows, err := queryRows(ctx, db, `PRAGMA index_info(`+quoteLiteral(idxName)+`)`)
		if err != nil {
			return err
		}
		type colPos struct {
			name string
			seq  int64
		}
		var cols []colPos
		for _, ir := range infoRows {
			cols = append(cols, colPos{name: asString(ir["name"]), seq: asInt64(ir["seqno"])})
		}
		sort.Slice(cols, func(i, j int) bool { return cols[i].seq < cols[j].seq })
		names := make([]string, 0, len(cols))
		for _, c := range cols {
			if c.name != "" {
				names = append(names, c.name)
			}
		}
		if len(names) == 0 {
			continue
		}
		ts.UniqueConstraints[idxName] = &UniqueSnapshot{
			Name:    idxName,
			Columns: names,
		}
	}
	return nil
}

// queryRows runs sql and returns every row as a column-name→value map.
// Values are scanned into *any so the caller can coerce defensively;
// PRAGMA columns arrive as int64, string, []byte or nil depending on
// the driver.
func queryRows(ctx context.Context, db *DB, sql string) ([]map[string]any, error) {
	rows, err := db.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	var out []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		dest := make([]any, len(cols))
		for i := range vals {
			dest[i] = &vals[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		m := make(map[string]any, len(cols))
		for i, c := range cols {
			m[c] = vals[i]
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// --- value coercion --------------------------------------------------

func asInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case int32:
		return int64(n)
	case bool:
		if n {
			return 1
		}
		return 0
	case float64:
		return int64(n)
	}
	return 0
}

func asString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		return string(s)
	}
	return ""
}

// asNullableString returns nil for a SQL NULL and a pointer to the
// string form otherwise.
func asNullableString(v any) *string {
	switch s := v.(type) {
	case nil:
		return nil
	case string:
		return &s
	case []byte:
		str := string(s)
		return &str
	}
	return nil
}

// --- naming helpers --------------------------------------------------

// quoteLiteral wraps s in double quotes for use inside a PRAGMA target,
// escaping embedded quotes.
func quoteLiteral(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// fkConstraintName builds a deterministic FK name in the camelCase form
// pg's introspection uses: <tableFrom><ColFrom...><TableTo><ColTo...>Fk.
func fkConstraintName(tableFrom string, cols []string, tableTo string, targetCols []string) string {
	var b strings.Builder
	b.WriteString(tableFrom)
	for _, c := range cols {
		b.WriteString(upperFirst(c))
	}
	b.WriteString(upperFirst(tableTo))
	for _, c := range targetCols {
		b.WriteString(upperFirst(c))
	}
	b.WriteString("Fk")
	return b.String()
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	first := s[0]
	if first >= 'a' && first <= 'z' {
		first -= 'a' - 'A'
	}
	return string(first) + s[1:]
}

// normaliseFKAction lowercases a referential action, mapping the empty
// / "NO ACTION" default to "no action" as drizzle-kit records it.
func normaliseFKAction(a string) string {
	a = strings.TrimSpace(a)
	if a == "" {
		return "no action"
	}
	return strings.ToLower(a)
}
