package clickhouse

import (
	"strings"

	"github.com/bernardoforcillo/drops"
)

// Engine is the table-engine specification. ClickHouse requires every
// table to declare one; Engine values render as the text right after
// the ENGINE = keyword inside CREATE TABLE.
//
// Common engines are provided as constructor helpers below; for
// anything else (Distributed, Kafka, MaterializedView source engines,
// AggregatingMergeTree with explicit parameters) use Raw.
type Engine interface {
	WriteEngine(b *drops.Builder)
}

// engineRaw renders an arbitrary literal — used by Raw.
type engineRaw string

func (e engineRaw) WriteEngine(b *drops.Builder) { b.WriteString(string(e)) }

// Raw builds an engine spec from a literal string. Use for engines
// the typed constructors don't cover.
//
//	clickhouse.Raw("Distributed(cluster, db, table, rand())")
func Raw(text string) Engine { return engineRaw(text) }

// engineFamily covers the MergeTree-family engines that share the
// same constructor parameter list. Specific engines differ only in
// the name token they emit.
type engineFamily struct {
	name string
	args []string // raw engine arguments
}

func (e engineFamily) WriteEngine(b *drops.Builder) {
	b.WriteString(e.name)
	b.WriteByte('(')
	if len(e.args) > 0 {
		b.WriteString(strings.Join(e.args, ", "))
	}
	b.WriteByte(')')
}

// MergeTree returns the MergeTree() engine. ORDER BY / PARTITION BY /
// settings are configured on the table itself (Table.OrderBy etc.).
func MergeTree() Engine { return engineFamily{name: "MergeTree"} }

// ReplacingMergeTree(version_column) — version is optional; pass an
// empty string for the default form ReplacingMergeTree().
func ReplacingMergeTree(versionCol string) Engine {
	if versionCol == "" {
		return engineFamily{name: "ReplacingMergeTree"}
	}
	return engineFamily{name: "ReplacingMergeTree", args: []string{quoteIdent(versionCol)}}
}

// SummingMergeTree(columns...) — optional list of columns to sum.
func SummingMergeTree(cols ...string) Engine {
	return engineFamily{name: "SummingMergeTree", args: quoteIdents(cols)}
}

// AggregatingMergeTree is the empty-constructor form.
func AggregatingMergeTree() Engine { return engineFamily{name: "AggregatingMergeTree"} }

// CollapsingMergeTree(sign_column).
func CollapsingMergeTree(signCol string) Engine {
	return engineFamily{name: "CollapsingMergeTree", args: []string{quoteIdent(signCol)}}
}

// VersionedCollapsingMergeTree(sign_column, version_column).
func VersionedCollapsingMergeTree(signCol, versionCol string) Engine {
	return engineFamily{
		name: "VersionedCollapsingMergeTree",
		args: []string{quoteIdent(signCol), quoteIdent(versionCol)},
	}
}

// ReplicatedMergeTree(zk_path, replica). The path/replica strings are
// passed verbatim — typically use macros: '/clickhouse/tables/{shard}/foo'.
func ReplicatedMergeTree(zkPath, replica string) Engine {
	return engineFamily{
		name: "ReplicatedMergeTree",
		args: []string{quoteLiteral(zkPath), quoteLiteral(replica)},
	}
}

// Non-merge engines.

func Memory() Engine    { return engineFamily{name: "Memory"} }
func Log() Engine       { return engineFamily{name: "Log"} }
func TinyLog() Engine   { return engineFamily{name: "TinyLog"} }
func StripeLog() Engine { return engineFamily{name: "StripeLog"} }
func Null() Engine      { return engineFamily{name: "Null"} }

// helpers ------------------------------------------------------------

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func quoteIdents(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = quoteIdent(n)
	}
	return out
}

func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
