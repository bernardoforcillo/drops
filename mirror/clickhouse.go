package mirror

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/clickhouse"
	"github.com/bernardoforcillo/drops/pg"
)

// Bookkeeping columns every derived table carries. They are prefixed
// so they cannot collide with a source column: a Postgres identifier
// may start with an underscore, but a source table that already has a
// column by these names is rejected rather than silently overwritten.
const (
	// VersionColumn orders competing writes to the same key. It is
	// the ReplacingMergeTree version, so the row with the highest
	// value wins a merge.
	VersionColumn = "_drops_version"

	// DeletedColumn marks a tombstone. ClickHouse has no cheap row
	// delete, so a mirrored delete is an ordinary insert with this
	// set to 1 and a version above the row it retires.
	DeletedColumn = "_drops_deleted"
)

// DeriveOption configures [DeriveClickHouse].
type DeriveOption func(*deriveConfig)

type deriveConfig struct {
	name      string
	database  string
	engine    clickhouse.Engine
	orderBy   []string
	partition drops.Expression
	settings  [][2]string
}

// WithName overrides the derived table's name. Defaults to the source
// table's name, which is usually what you want — the two live in
// different databases.
func WithName(name string) DeriveOption {
	return func(c *deriveConfig) { c.name = name }
}

// WithDatabase places the derived table in a named ClickHouse
// database.
func WithDatabase(db string) DeriveOption {
	return func(c *deriveConfig) { c.database = db }
}

// WithEngine overrides the table engine. The default —
// ReplacingMergeTree keyed on [VersionColumn] — is what makes the
// mirror idempotent under the at-least-once delivery every durable
// source provides, so replace it only if you have another way to
// converge duplicate writes.
func WithEngine(e clickhouse.Engine) DeriveOption {
	return func(c *deriveConfig) { c.engine = e }
}

// WithOrderBy overrides the sorting key, given as source column
// names. Defaults to the source primary key, which is also what
// ReplacingMergeTree deduplicates on — change both together or the
// mirror will stop converging.
func WithOrderBy(cols ...string) DeriveOption {
	return func(c *deriveConfig) { c.orderBy = cols }
}

// WithPartitionBy sets the partitioning expression — most often a
// month bucket over a timestamp column, which is what makes dropping
// old data cheap.
func WithPartitionBy(e drops.Expression) DeriveOption {
	return func(c *deriveConfig) { c.partition = e }
}

// WithSetting appends a table-level SETTINGS pair. The two halves are
// raw SQL and are checked, on the terms [clickhouse.Table.Setting]
// states, to be one setting rather than this one and whatever the rest
// of the string says — the case a value built from configuration
// falls into.
//
// A pair that fails comes back as an error from [DeriveClickHouse],
// not as a panic here: this option's arguments are the kind that
// arrive from outside the program, and DeriveClickHouse already
// answers for everything else about them that way.
func WithSetting(key, value string) DeriveOption {
	return func(c *deriveConfig) { c.settings = append(c.settings, [2]string{key, value}) }
}

// DeriveClickHouse builds the ClickHouse table that mirrors a
// Postgres one.
//
// This is the point of the package. The analytics schema is a
// function of the transactional schema, so adding a column in
// Postgres adds it here, and the two cannot drift the way two
// hand-written declarations do.
//
// The derived table is the source's columns with their types mapped
// (see [MapType]), plus [VersionColumn] and [DeletedColumn]. It is a
// ReplacingMergeTree ordered by the source primary key, which is what
// makes re-applying a change converge instead of duplicating.
//
// Reads must account for both additions:
//
//	db.Select().From(chDocs).Final().Where(mirror.NotDeleted(chDocs))
//
// FINAL forces the merge that collapses superseded versions; without
// it a row that has been updated twice appears twice. [NotDeleted]
// drops the tombstones. Neither is free, which is the honest cost of
// mirroring a mutable table into an append-only store — for a table
// that is only ever inserted into, pass [WithEngine] a plain
// MergeTree and skip both.
func DeriveClickHouse(src *pg.Table, opts ...DeriveOption) (*clickhouse.Table, error) {
	if src == nil {
		return nil, fmt.Errorf("drops/mirror: DeriveClickHouse needs a source table")
	}
	cfg := deriveConfig{name: src.Name()}
	for _, o := range opts {
		o(&cfg)
	}

	var pk *pg.Column
	for _, c := range src.Columns() {
		switch c.Name() {
		case VersionColumn, DeletedColumn:
			return nil, fmt.Errorf("drops/mirror: source table %q already has a column named %q, which the mirror needs for its own bookkeeping",
				src.Name(), c.Name())
		}
		if c.IsPrimaryKey() {
			if pk != nil {
				return nil, fmt.Errorf("drops/mirror: source table %q has a composite primary key; mirroring needs a single key column to deduplicate on", src.Name())
			}
			pk = c
		}
	}
	if pk == nil {
		return nil, fmt.Errorf("drops/mirror: source table %q has no primary key, so mirrored rows have nothing to converge on", src.Name())
	}

	out := clickhouse.NewTable(cfg.name)
	if cfg.database != "" {
		out = clickhouse.NewDatabaseTable(cfg.database, cfg.name)
	}

	for _, c := range src.Columns() {
		if err := addMapped(out, c); err != nil {
			return nil, fmt.Errorf("drops/mirror: column %q: %w", c.Name(), err)
		}
	}
	version := clickhouse.Add(out, clickhouse.UInt64(VersionColumn))
	clickhouse.Add(out, clickhouse.UInt8(DeletedColumn))

	engine := cfg.engine
	if engine == nil {
		engine = clickhouse.ReplacingMergeTree(VersionColumn)
	}
	out.Engine(engine)

	orderCols := cfg.orderBy
	if len(orderCols) == 0 {
		orderCols = []string{pk.Name()}
	}
	refs := make([]clickhouse.ColRef, 0, len(orderCols))
	for _, name := range orderCols {
		col := out.Col(name)
		if col == nil {
			return nil, fmt.Errorf("drops/mirror: ORDER BY names column %q, which the source table does not have", name)
		}
		refs = append(refs, col)
	}
	out.OrderBy(refs...)
	if cfg.partition != nil {
		out.PartitionBy(cfg.partition)
	}
	for _, s := range cfg.settings {
		// Table.Setting panics on a bad pair, which is right where a
		// schema is being written and wrong here: these come in as
		// options and this function reports everything else about its
		// options as an error.
		if err := clickhouse.ValidateSetting(s[0], s[1]); err != nil {
			return nil, fmt.Errorf("drops/mirror: WithSetting(%q, %q): %w", s[0], s[1], err)
		}
		out.Setting(s[0], s[1])
	}
	_ = version
	return out, nil
}

// NotDeleted renders the predicate that hides tombstoned rows. Pair
// it with FINAL — see [DeriveClickHouse].
func NotDeleted(t *clickhouse.Table) drops.Expression {
	return clickhouse.Eq(t.Col(DeletedColumn), 0)
}

// MapType returns the ClickHouse type that mirrors a PostgreSQL one.
//
// The mapping is deliberately lossless-or-wider rather than exact:
//
//   - date becomes Date32, not Date, because Date stops at 2149 and
//     silently wraps before it.
//   - timestamps become DateTime64(6) — Postgres stores microseconds
//     and DateTime only holds seconds. timestamptz is stamped UTC,
//     which is what Postgres normalises to on the way in anyway.
//   - numeric(p,s) becomes Decimal(p,s); bare numeric has no
//     precision to carry over, so it becomes String rather than
//     silently choosing one.
//   - time, interval, json, jsonb and bytea become String.
//     ClickHouse has no interval or time-of-day type, and its JSON
//     type is still changing shape often enough that a String
//     column is the stable choice.
//
// nullable wraps the result in Nullable(). ClickHouse cannot sort by
// a Nullable column, so a column in the ORDER BY must be NOT NULL at
// the source — which a primary key already is.
func MapType(pgType string, nullable bool) (string, error) {
	base, args := splitType(pgType)
	var out string
	switch base {
	case "smallint":
		out = "Int16"
	case "integer", "serial":
		out = "Int32"
	case "bigint", "bigserial":
		out = "Int64"
	case "real":
		out = "Float32"
	case "double precision":
		out = "Float64"
	case "boolean":
		out = "Bool"
	case "uuid":
		out = "UUID"
	case "date":
		out = "Date32"
	case "timestamp":
		out = "DateTime64(6)"
	case "timestamptz":
		out = "DateTime64(6, 'UTC')"
	case "numeric":
		if args == "" {
			// No declared precision to carry across; String keeps
			// the value exactly rather than picking a Decimal width
			// that might truncate it.
			out = "String"
			break
		}
		out = "Decimal(" + args + ")"
	case "text", "varchar", "char", "interval", "time", "json", "jsonb", "bytea":
		out = "String"
	default:
		return "", fmt.Errorf("drops/mirror: no ClickHouse equivalent for PostgreSQL type %q", pgType)
	}
	if nullable {
		return "Nullable(" + out + ")", nil
	}
	return out, nil
}

// splitType breaks "numeric(10,2)" into ("numeric", "10,2").
func splitType(s string) (base, args string) {
	s = strings.TrimSpace(s)
	i := strings.IndexByte(s, '(')
	if i < 0 {
		return s, ""
	}
	return strings.TrimSpace(s[:i]), strings.TrimSuffix(s[i+1:], ")")
}

// addMapped appends the ClickHouse counterpart of a Postgres column.
//
// The switch exists because clickhouse.Add is generic over the
// column's Go value type, which has to be known at compile time,
// while the mapping is a runtime decision. Enumerating the finite set
// of Postgres types is the price of keeping the derived columns
// properly typed instead of erasing them all to a single type.
func addMapped(dst *clickhouse.Table, c *pg.Column) error {
	nullable := !c.IsNotNull() && !c.IsPrimaryKey()
	chType, err := MapType(c.Type().TypeSQL(), nullable)
	if err != nil {
		return err
	}
	name := c.Name()
	base, _ := splitType(c.Type().TypeSQL())
	if nullable {
		// Nullable columns all scan through a pointer, so one
		// instantiation covers them.
		clickhouse.Add(dst, clickhouse.Custom[any](name, chType))
		return nil
	}
	switch base {
	case "smallint":
		clickhouse.Add(dst, clickhouse.Custom[int16](name, chType))
	case "integer", "serial":
		clickhouse.Add(dst, clickhouse.Custom[int32](name, chType))
	case "bigint", "bigserial":
		clickhouse.Add(dst, clickhouse.Custom[int64](name, chType))
	case "real":
		clickhouse.Add(dst, clickhouse.Custom[float32](name, chType))
	case "double precision":
		clickhouse.Add(dst, clickhouse.Custom[float64](name, chType))
	case "boolean":
		clickhouse.Add(dst, clickhouse.Custom[bool](name, chType))
	case "date", "timestamp", "timestamptz":
		clickhouse.Add(dst, clickhouse.Custom[time.Time](name, chType))
	default:
		clickhouse.Add(dst, clickhouse.Custom[string](name, chType))
	}
	return nil
}

// ClickHouseSink writes mirrored changes into a derived table.
type ClickHouseSink struct {
	db    *clickhouse.DB
	table *clickhouse.Table
	cols  []*clickhouse.Column
}

// NewClickHouseSink returns a [Sink] that appends changes to a table
// built by [DeriveClickHouse].
//
// Every change — insert, update and delete alike — is an INSERT. That
// is not a shortcut: ClickHouse's mutations are asynchronous rewrites
// of whole parts, so issuing one per changed row would be far slower
// than the append-and-collapse the engine is built for. Updates land
// as a higher-version row that supersedes the old one at merge time,
// and deletes as a tombstone.
func NewClickHouseSink(db *clickhouse.DB, t *clickhouse.Table) (*ClickHouseSink, error) {
	if db == nil || t == nil {
		return nil, fmt.Errorf("drops/mirror: NewClickHouseSink needs a db and a table")
	}
	if t.Col(VersionColumn) == nil || t.Col(DeletedColumn) == nil {
		return nil, fmt.Errorf("drops/mirror: table %q is missing the %q/%q columns — build it with DeriveClickHouse",
			t.Name(), VersionColumn, DeletedColumn)
	}
	return &ClickHouseSink{db: db, table: t, cols: t.Columns()}, nil
}

func (s *ClickHouseSink) Name() string { return "clickhouse:" + s.table.Name() }

// VersionAware reports true: this sink is a [VersionAwareSink].
//
// It does not compare anything itself — it appends, and the engine
// resolves. ReplacingMergeTree keyed on [VersionColumn] keeps the
// highest version per sorting key and drops the rest at merge time,
// and a FINAL read applies the same rule before the merge has
// happened, so a change written with a version below the stored one
// is inert. That is what makes this sink a legal target for a
// fill-mode reseed, which writes outside the ordered stream.
//
// The claim rests on the engine [DeriveClickHouse] gives the derived
// table. Override it with [WithEngine] — a plain MergeTree for an
// insert-only table, say — and nothing deduplicates: every write
// survives, so a fill would add a second row for a key that already
// has one, and a replayed batch would duplicate rows the same way.
// WithEngine's doc says as much; it is repeated here because this is
// the promise that breaks.
func (s *ClickHouseSink) VersionAware() bool { return true }

// Apply inserts the batch as one statement.
func (s *ClickHouseSink) Apply(ctx context.Context, changes []Change) error {
	if len(changes) == 0 {
		return nil
	}
	ins := s.db.Insert(s.table)
	for _, ch := range changes {
		if err := ch.Validate(); err != nil {
			return err
		}
		vals := make([]clickhouse.ColumnValue, 0, len(s.cols))
		for _, col := range s.cols {
			vals = append(vals, clickhouse.Bind(col, s.valueFor(col, ch)))
		}
		ins.Row(vals...)
	}
	_, err := ins.Exec(ctx)
	return err
}

// valueFor picks the value a column takes for one change. The
// bookkeeping columns come from the change's metadata; a delete
// carries no row, so every source column falls back to its zero value
// except the key.
func (s *ClickHouseSink) valueFor(col *clickhouse.Column, ch Change) any {
	switch col.Name() {
	case VersionColumn:
		return ch.Version
	case DeletedColumn:
		if ch.IsDelete() {
			return uint8(1)
		}
		return uint8(0)
	}
	if v, ok := ch.Row[col.Name()]; ok {
		return v
	}
	if ch.IsDelete() {
		// A tombstone only has to be addressable: the sorting key
		// must carry the real value so the row lands on top of the
		// one it retires, and the rest can be left null.
		if key, ok := s.keyValue(col, ch); ok {
			return key
		}
	}
	return nil
}

// keyValue returns the change's key coerced to the ORDER BY column's
// type, so a tombstone sorts onto the row it retires.
func (s *ClickHouseSink) keyValue(col *clickhouse.Column, ch Change) (any, bool) {
	order := s.table.OrderByColumns()
	if len(order) != 1 || order[0] != col.Name() {
		return nil, false
	}
	if strings.HasPrefix(col.Type().TypeSQL(), "Int") || strings.HasPrefix(col.Type().TypeSQL(), "UInt") {
		n, err := strconv.ParseInt(ch.Key, 10, 64)
		if err != nil {
			return nil, false
		}
		return n, true
	}
	return ch.Key, true
}
