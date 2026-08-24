package clickhouse

import (
	"context"
	"fmt"
	"strings"
)

// IntrospectOptions tunes which database [Introspect] reads.
type IntrospectOptions struct {
	// Database restricts the introspection to one database. Empty
	// means currentDatabase() — the one the DSN named, and the one
	// unqualified DDL lands in.
	//
	// It also decides how the resulting snapshot is keyed and how
	// [Diff] renders its statements: a snapshot read with a database
	// set carries it on every [TableSnapshot], and the statements come
	// out qualified. Point it at whatever the Go declarations say —
	// [NewDatabaseTable] for a qualified one, [NewTable] for not —
	// or [Diff] compares two tables that are not the same table.
	Database string

	// Tables narrows the read to these table names. Empty reads every
	// table in the database.
	Tables []string
}

// Introspect reads the live shape of a ClickHouse database out of
// system.tables and system.columns: every table's columns and types,
// its engine and the engine's parameters, its sorting key, primary
// key, partition key, sampling key, TTL and SETTINGS, and which
// columns take part in which key.
//
// The result is in the same format as [BuildSnapshot]'s and can be
// diffed against it. Anything the declaration can say has to be read
// back here in the same normalised spelling, or [Diff] sees the
// difference between "absent" and "not looked at" as work to do and
// [Push] stops being idempotent.
//
// # Where each field comes from
//
// system.tables answers the table itself. Its engine column holds the
// engine name alone, so the parameters — the version column of a
// ReplacingMergeTree, the ZooKeeper path of a Replicated one — are
// read out of engine_full, which is the whole storage clause as
// ClickHouse re-renders it: the engine with its arguments, then
// PARTITION BY, PRIMARY KEY, ORDER BY, SAMPLE BY, TTL and SETTINGS in
// that fixed order. The keys are taken from the columns that report
// them directly (partition_key, sorting_key, primary_key,
// sampling_key); TTL and SETTINGS have no column of their own and are
// parsed out of engine_full.
//
// system.columns answers the columns: name, type, position,
// default_kind and default_expression, comment, compression_codec, and
// the four is_in_*_key flags that decide whether a column can be
// altered or dropped at all.
//
// # What it does not read
//
//   - A per-column TTL. system.columns does not report one, and
//     parsing it out of the CREATE TABLE query would be guessing at a
//     formatter's output. [Diff] therefore leaves column TTLs out of
//     the comparison entirely rather than proposing the same statement
//     on every push; [Push] reports a declared one as a notice.
//   - A column's SETTINGS or STATISTICS clause, and MATERIALIZED,
//     ALIAS and EPHEMERAL columns beyond the fact of their
//     [ColumnSnapshot.DefaultKind]. The DSL declares none of them.
//   - Projections, skipping indexes, row policies and quotas.
//
// Views, materialized views and dictionaries come back like any other
// entry in system.tables, carrying their engine ("View",
// "MaterializedView", "Dictionary") and no keys. [Push] never touches
// them, because a [Schema] cannot declare one and Push acts only on
// the tables a schema names.
func Introspect(ctx context.Context, db *DB, opts ...IntrospectOptions) (*Snapshot, error) {
	if db == nil {
		return nil, fmt.Errorf("drops/clickhouse: Introspect needs a db")
	}
	var opt IntrospectOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	snap := EmptySnapshot()
	snap.ID = newUUID()
	if err := readIntrospectTables(ctx, db, opt, snap); err != nil {
		return nil, err
	}
	if err := readIntrospectColumns(ctx, db, opt, snap); err != nil {
		return nil, err
	}
	return snap, nil
}

// IntrospectTable reads one table, returning nil when the server has
// no such table. It is the read [Push] and an evolution both start
// from, and the cheap way to ask "does this exist and what shape is
// it".
func IntrospectTable(ctx context.Context, db *DB, database, name string) (*TableSnapshot, error) {
	snap, err := Introspect(ctx, db, IntrospectOptions{Database: database, Tables: []string{name}})
	if err != nil {
		return nil, err
	}
	return snap.Tables[SnapshotKey(database, name)], nil
}

// databasePredicate returns the WHERE fragment naming the database and
// the argument it binds. An empty name means the session's own
// database, which has no literal to bind — currentDatabase() is the
// only way to say it.
func databasePredicate(database string) (string, []any) {
	if database == "" {
		return "database = currentDatabase()", nil
	}
	return "database = ?", []any{database}
}

// tablePredicate narrows a read to a set of table names, or to nothing
// when the caller named none. The column differs between the two
// system tables — system.tables calls it "name" and system.columns
// calls it "table" — so the caller names it.
func tablePredicate(column string, tables []string) (string, []any) {
	if len(tables) == 0 {
		return "", nil
	}
	args := make([]any, 0, len(tables))
	marks := make([]string, 0, len(tables))
	for _, t := range tables {
		marks = append(marks, "?")
		args = append(args, t)
	}
	return " AND " + column + " IN (" + strings.Join(marks, ", ") + ")", args
}

// readIntrospectTables fills in one TableSnapshot per row of
// system.tables.
func readIntrospectTables(ctx context.Context, db *DB, opt IntrospectOptions, snap *Snapshot) error {
	where, args := databasePredicate(opt.Database)
	narrow, narrowArgs := tablePredicate("name", opt.Tables)
	args = append(args, narrowArgs...)

	sql := "SELECT name, engine, engine_full, partition_key, sorting_key, primary_key, sampling_key " +
		"FROM system.tables WHERE " + where + narrow + " ORDER BY name"
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("drops/clickhouse: reading system.tables: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var name, engine, engineFull, partitionKey, sortingKey, primaryKey, samplingKey string
		if err := rows.Scan(&name, &engine, &engineFull, &partitionKey, &sortingKey, &primaryKey, &samplingKey); err != nil {
			return fmt.Errorf("drops/clickhouse: scanning system.tables: %w", err)
		}
		ts := newTableSnapshot(opt.Database, name)
		ts.Engine = engine
		_, ts.EngineParams = parseEngine(engineTerm(engineFull))
		ts.PartitionKey = NormalizeKeyExpr(partitionKey)
		ts.SortingKey = NormalizeKeyExpr(sortingKey)
		ts.PrimaryKey = NormalizeKeyExpr(primaryKey)
		ts.SampleBy = NormalizeKeyExpr(samplingKey)
		ts.TTL, ts.Settings = storageTailOf(engineFull)
		snap.Tables[SnapshotKey(opt.Database, name)] = ts
	}
	return rows.Err()
}

// readIntrospectColumns fills in the columns of the tables already
// read.
//
// A column of a table the first pass did not see is dropped rather
// than inventing a table for it: the two reads are not one snapshot of
// the server, and a table created between them would otherwise arrive
// with columns and no engine, which is a shape [Diff] would read as a
// table needing to be created.
func readIntrospectColumns(ctx context.Context, db *DB, opt IntrospectOptions, snap *Snapshot) error {
	where, args := databasePredicate(opt.Database)
	narrow, narrowArgs := tablePredicate("table", opt.Tables)
	args = append(args, narrowArgs...)

	sql := "SELECT table, name, type, position, default_kind, default_expression, comment, " +
		"compression_codec, is_in_sorting_key, is_in_primary_key, is_in_partition_key, is_in_sampling_key " +
		"FROM system.columns WHERE " + where + narrow + " ORDER BY table, position"
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("drops/clickhouse: reading system.columns: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			table, name, typ                    string
			position                            uint64
			defaultKind, defaultExpr            string
			comment, codec                      string
			sorting, primary, partition, sample uint8
		)
		if err := rows.Scan(&table, &name, &typ, &position, &defaultKind, &defaultExpr,
			&comment, &codec, &sorting, &primary, &partition, &sample); err != nil {
			return fmt.Errorf("drops/clickhouse: scanning system.columns: %w", err)
		}
		ts := snap.Tables[SnapshotKey(opt.Database, table)]
		if ts == nil {
			continue
		}
		ts.Columns[name] = &ColumnSnapshot{
			Name:           name,
			Type:           NormalizeType(typ),
			Position:       int(position),
			Default:        defaultExpr,
			DefaultKind:    defaultKind,
			Comment:        comment,
			Codec:          unwrapCodec(codec),
			InSortingKey:   sorting != 0,
			InPrimaryKey:   primary != 0,
			InPartitionKey: partition != 0,
			InSamplingKey:  sample != 0,
		}
	}
	return rows.Err()
}

// unwrapCodec turns the "CODEC(ZSTD(3))" that
// system.columns.compression_codec reports into the "ZSTD(3)" that
// [Col.Codec] takes, so the declared and the live spelling are the
// same string. A column with no codec reports the empty string and
// keeps it.
func unwrapCodec(codec string) string {
	codec = strings.TrimSpace(codec)
	if !strings.HasPrefix(codec, "CODEC(") {
		return codec
	}
	inner, ok := balancedInner(codec[len("CODEC"):])
	if !ok {
		return codec
	}
	return strings.TrimSpace(inner)
}

// storageKeywords are the clauses ClickHouse renders after the engine
// in engine_full, in the order its formatter writes them.
var storageKeywords = []string{"PARTITION BY", "PRIMARY KEY", "ORDER BY", "SAMPLE BY", "TTL", "SETTINGS"}

// engineTerm returns the engine and its arguments from the front of an
// engine_full string, stopping at the first storage clause.
//
// engine_full is "ReplacingMergeTree(version) PARTITION BY toYYYYMM(ts)
// ORDER BY (id) SETTINGS index_granularity = 8192" — one line, the
// engine first. system.tables.engine already holds the name, so this
// exists for the arguments, which nothing else reports.
func engineTerm(engineFull string) string {
	cut := len(engineFull)
	for _, kw := range storageKeywords {
		if i := indexKeyword(engineFull, kw); i >= 0 && i < cut {
			cut = i
		}
	}
	return strings.TrimSpace(engineFull[:cut])
}

// storageTailOf pulls the TTL expression and the SETTINGS pairs out of
// engine_full.
//
// Neither has a column in system.tables, and both matter: a TTL
// silently deletes rows, and a SETTINGS difference is the sort of
// thing a hand-run ALTER leaves behind. TTL runs to the SETTINGS
// keyword or to the end; SETTINGS is always last, which is what makes
// reading it this way sound rather than lucky.
func storageTailOf(engineFull string) (ttl string, settings map[string]string) {
	settings = map[string]string{}
	if i := indexKeyword(engineFull, "SETTINGS"); i >= 0 {
		for _, pair := range splitTopLevel(engineFull[i+len("SETTINGS"):], ',') {
			k, v, ok := strings.Cut(pair, "=")
			if !ok {
				continue
			}
			settings[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
		engineFull = engineFull[:i]
	}
	if i := indexKeyword(engineFull, "TTL"); i >= 0 {
		ttl = strings.TrimSpace(engineFull[i+len("TTL"):])
	}
	return ttl, settings
}

// indexKeyword finds a clause keyword at the top level of a storage
// definition — outside quotes, outside parentheses, and bounded by
// non-identifier characters on both sides.
//
// The bounds are what keeps a column named "ttl_days" or a function
// called partitionBy() from being read as the start of a clause, and
// the depth check is what keeps the TTL inside a nested expression out
// of it.
func indexKeyword(text, keyword string) int {
	var (
		depth   int
		inQuote bool
	)
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case inQuote:
			if c == '\'' {
				inQuote = false
			}
			continue
		case c == '\'':
			inQuote = true
			continue
		case c == '(':
			depth++
			continue
		case c == ')':
			depth--
			continue
		case depth != 0:
			continue
		}
		if !strings.HasPrefix(text[i:], keyword) {
			continue
		}
		if i > 0 && isIdentByte(text[i-1]) {
			continue
		}
		if end := i + len(keyword); end < len(text) && isIdentByte(text[end]) {
			continue
		}
		return i
	}
	return -1
}

// isIdentByte reports whether c can appear inside an unquoted
// identifier.
func isIdentByte(c byte) bool {
	return c == '_' ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9')
}
