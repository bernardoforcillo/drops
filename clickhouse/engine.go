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

// engineName is the engine's leading identifier — the token that says
// which family it belongs to, with its parameters left off.
//
// It is asked of an [Engine] rather than of a string because a schema
// spells its engine two ways: through a constructor, which holds the
// name in a field, and through [Raw], which holds a whole literal. A
// question answered for only one of them is answered for the shape
// nobody writes in production.
func engineName(e Engine) string {
	switch v := e.(type) {
	case nil:
		return ""
	case engineFamily:
		return v.name
	case engineRaw:
		text := strings.TrimSpace(string(v))
		if i := strings.IndexAny(text, "( \t"); i >= 0 {
			return text[:i]
		}
		return text
	}
	// An Engine implemented outside this package renders itself and
	// nothing here can read a name out of it. Empty is the answer that
	// fails OPEN for the refusal that consults this — see
	// mergesBySortingKey, and the paragraph there about which way it is
	// safe to be wrong.
	return ""
}

// mergesBySortingKey reports whether an engine of this name folds rows
// that share a sorting key into one.
//
// It is the question [ErrTenantNotInSortingKey] turns on: such an
// engine compares rows by the SORTING KEY and by nothing else, so a
// tenant column outside that key is not part of the comparison, and two
// tenants' rows that agree on the key become one row in the background
// — no statement, no error, and nothing that names the tenant anywhere
// in it. A plain MergeTree folds nothing and is not here.
//
// The Replicated prefix composes with the family name rather than
// replacing it: ReplicatedReplacingMergeTree folds exactly as
// ReplacingMergeTree does, and a production schema spells it that way
// far more often than not. So it is stripped before the comparison,
// which also leaves ReplicatedMergeTree answering as MergeTree does.
//
// The list is closed rather than a suffix test on "MergeTree", and
// that is the direction it is safe to be wrong in. A name this does not
// know answers false, so the refusal does not fire and the INSERT goes
// out — which is what a caller reaching for an engine drops has never
// heard of already gets, and is the same answer they got before the
// check existed. A suffix test would instead refuse every future
// MergeTree variant that does not fold, and a refusal nobody can make
// sense of is turned off wholesale.
func mergesBySortingKey(name string) bool {
	name = strings.TrimPrefix(name, "Replicated")
	switch name {
	case "ReplacingMergeTree",
		"CollapsingMergeTree",
		"VersionedCollapsingMergeTree",
		"SummingMergeTree",
		"AggregatingMergeTree",
		"GraphiteMergeTree":
		return true
	}
	return false
}

// helpers ------------------------------------------------------------

// quoteIdent renders name as one ClickHouse identifier.
//
// The backslash is escaped BEFORE the quote, and both are escaped,
// because ClickHouse's lexer honours backslash escapes inside a quoted
// token as well as the doubled quote. Doubling the quote alone is what
// the standard renderer does, and it is not enough here in two
// different ways:
//
//   - a name ending in a backslash — "a\" — renders as "a\" with the
//     backslash escaping the CLOSING quote, so the token never closes
//     and everything after it, including the rest of the statement, is
//     read as part of the identifier;
//   - a name containing one — a\b — renders as "a\b", which
//     ClickHouse reads as the two bytes 61 08, an 'a' followed by a
//     backspace. The column the statement asked for is not the column
//     that exists, and every later reference to it fails to resolve.
//
// Order matters: doubling the quote introduces no backslashes, so
// backslashes first is the only order in which each escape is applied
// exactly once.
func quoteIdent(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func quoteIdents(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = quoteIdent(n)
	}
	return out
}

// quoteLiteral wraps s as a ClickHouse string literal.
//
// The backslash is escaped first and it is not optional: ClickHouse's
// lexer reads C-style escapes inside a string literal, so a comment
// ending in one — 'C:\' — swallows its own closing quote and the rest
// of the statement with it, and a comment of \' OR 1=1 -- leaves the
// literal entirely. Doubling the quote, which is what the PostgreSQL
// dialect does and where this function came from, is right there and
// not enough here.
func quoteLiteral(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
