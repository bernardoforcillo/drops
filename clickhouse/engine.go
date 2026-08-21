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
//
// ClickHouse reads the summed columns as one tuple argument, not as a
// parameter list: a bare SummingMergeTree(a, b) leaves "a" over as an
// engine argument the MergeTree parameter check then rejects.
func SummingMergeTree(cols ...string) Engine {
	if len(cols) == 0 {
		return engineFamily{name: "SummingMergeTree"}
	}
	return engineFamily{
		name: "SummingMergeTree",
		args: []string{"(" + strings.Join(quoteIdents(cols), ", ") + ")"},
	}
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

// quoteIdent renders s as a double-quoted ClickHouse identifier.
//
// ClickHouse does not read a quoted token the way the SQL standard
// does. Its lexer runs one routine over every quoted token — string,
// double-quoted identifier, backticked identifier — and that routine
// honours a backslash as an escape as well as the doubled quote. So
// doubling alone is not enough: a name ending in a backslash renders
// as "a\", the server reads the backslash as escaping the quote, the
// token never closes, and the remainder of the statement is read as
// part of the identifier. A name like `a\", x String) ENGINE = Log; --`
// is accepted by validateIdent and would rewrite the CREATE TABLE it
// appears in.
//
// Backslashes are therefore escaped as well. The output is unchanged
// for every name that contains none, which is every name in practice.
func quoteIdent(s string) string {
	return `"` + escapeQuoted(s, '"') + `"`
}

// escapeQuoted prepares s for the inside of a q-delimited ClickHouse
// token: backslashes doubled first (so the doubling below cannot be
// mistaken for one), then the delimiter doubled.
func escapeQuoted(s string, q byte) string {
	esc := strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(esc, string(q), string(q)+string(q))
}

func quoteIdents(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = quoteIdent(n)
	}
	return out
}

// quoteLiteral renders s as a ClickHouse string literal. It escapes
// backslashes for the same reason quoteIdent does — the lexer is the
// same one, and a comment or a ZooKeeper path ending in a backslash
// would otherwise swallow the rest of the statement.
func quoteLiteral(s string) string {
	return "'" + escapeQuoted(s, '\'') + "'"
}

// engineName returns the engine's name token — the identifier before
// the parameter list — or "" when the spec does not start with one.
//
// It reads the rendered text rather than a field, so it answers the
// same way for a typed constructor and for [Raw]. A schema that spells
// its engine Raw("ReplicatedReplacingMergeTree('/p', '{replica}')") is
// making exactly the declaration the typed constructors make, and a
// check that only understood the typed ones would exempt the spelling
// most production schemas use.
func engineName(e Engine) string {
	if e == nil {
		return ""
	}
	b := drops.NewBuilder(Placeholder)
	e.WriteEngine(b)
	sql, _ := b.SQL()
	i := 0
	for i < len(sql) {
		c := sql[i]
		if c == '_' || (c >= '0' && c <= '9') ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			i++
			continue
		}
		break
	}
	return sql[:i]
}

// mergingEngines are the MergeTree-family engines that FOLD rows
// sharing a sorting key into one, as opposed to merely storing them in
// sorted order.
//
// Membership is what [ErrTenantNotInSortingKey] turns on, and the list
// is by behaviour rather than by name: plain MergeTree sorts and never
// folds, so two tenants' rows with an equal key coexist; every engine
// here replaces, cancels, sums or aggregates them instead. Graphite is
// included for the same reason the others are — it rolls points up by
// key — even though this package has no constructor for it, because a
// schema can name it through [Raw].
var mergingEngines = map[string]bool{
	"ReplacingMergeTree":           true,
	"CollapsingMergeTree":          true,
	"VersionedCollapsingMergeTree": true,
	"SummingMergeTree":             true,
	"AggregatingMergeTree":         true,
	"GraphiteMergeTree":            true,
}

// mergesBySortingKey reports whether name is one of the folding engines,
// looking through the replication and shared-storage prefixes.
//
// The prefixes are stripped rather than enumerated because they compose
// and they multiply: ClickHouse builds every family member's replicated
// and shared variants by prefixing the name, so a list of full names
// would have to be three times as long and would go stale on the next
// storage backend. What Replicated adds is where the parts live, not
// what a merge does to them.
func mergesBySortingKey(name string) bool {
	for _, prefix := range []string{"Shared", "Replicated"} {
		name = strings.TrimPrefix(name, prefix)
	}
	return mergingEngines[name]
}
