package drops

import (
	"strconv"
	"strings"
)

// Expression is anything that can render itself into a Builder. Every
// fragment of a query — a column reference, an operator, a subquery —
// implements it.
//
// Expressions are stateless and safe to reuse across goroutines. The
// Builder they write into is not.
type Expression interface {
	WriteSQL(b *Builder)
}

// Builder accumulates SQL text and bound parameters. By default it
// uses PostgreSQL's $N numbered placeholders; dialects that need a
// different style (ClickHouse / MySQL use `?`) install one via
// WithPlaceholder.
//
// A Builder is not safe for concurrent use; create one per query.
type Builder struct {
	sb          strings.Builder
	args        []any
	placeholder func(n int) string
	dialect     Dialect
	bareIdents  bool

	// relRenameFrom / relRenameTo carry the one relation rename in
	// force while the current fragment is being written. See
	// RelationAlias.
	relRenameFrom string
	relRenameTo   string
}

// BuilderOption configures a Builder at construction time.
type BuilderOption func(*Builder)

// WithPlaceholder overrides how parameter placeholders are rendered.
// fn receives the 1-based index of the bound argument. The default
// emits "$<n>" (PostgreSQL); pass `func(int) string { return "?" }` for
// dialects (ClickHouse, MySQL) that use positional question marks.
func WithPlaceholder(fn func(n int) string) BuilderOption {
	return func(b *Builder) { b.placeholder = fn }
}

// WithDialect installs d so the Builder renders placeholders and quotes
// identifiers the dialect's way. It is the swap point for targeting a
// different SQL-like backend: the query builders are otherwise
// dialect-agnostic. Passing nil is a no-op (PostgreSQL defaults stay in
// force). WithDialect also sets the placeholder function from
// d.Placeholder unless a later WithPlaceholder overrides it.
func WithDialect(d Dialect) BuilderOption {
	return func(b *Builder) {
		if d == nil {
			return
		}
		b.dialect = d
		b.placeholder = d.Placeholder
	}
}

// Dialect returns the Builder's installed dialect, or nil when it is
// using the default PostgreSQL syntax.
func (b *Builder) Dialect() Dialect { return b.dialect }

// BareIdents reports whether column references should render as
// unqualified identifiers ("id") rather than table-qualified ones
// ("docs"."id"). Column implementations consult it in WriteSQL.
//
// The distinction is not cosmetic. Qualification is required in a
// SELECT and rejected in DDL that defines the very table being
// referenced: an index column list, a ClickHouse sorting or
// partitioning key. Rendering "docs"."id" there is a syntax error,
// and because it is a syntax error the database never sees a working
// statement — which is how such bugs survive a test suite that only
// string-compares generated SQL.
func (b *Builder) BareIdents() bool { return b.bareIdents }

// SetBareIdents switches the mode and returns the previous value, so
// a DDL renderer can scope it to one clause:
//
//	defer b.SetBareIdents(b.SetBareIdents(true))
//
// It is a Builder mode rather than a per-call argument because the
// column reference is often nested inside an arbitrary expression —
// toYYYYMM(ts) — where the renderer has no way to reach it.
func (b *Builder) SetBareIdents(on bool) bool {
	prev := b.bareIdents
	b.bareIdents = on
	return prev
}

// RelationAlias reports the identifier that references to the relation
// called name must render as while the current fragment is being
// written, or "" when that relation is not being renamed. name is the
// relation as it was declared — "table", or "schema.table" when the
// table is schema-qualified.
//
// It exists for the reason BareIdents does, and answers the mirror
// image of the same question. A predicate reaches a Builder as an
// opaque tree of closures, so a renderer that needs one relation inside
// it to qualify differently has no way to reach in and rewrite the
// column references — the problem SetBareIdents names, one degree
// further along. Where it bites is a table's automatic predicates: a
// default scope, a tenant axis. They are built once from the declared
// column handles and then rendered into statements whose FROM entry may
// be an *alias* of that table, and "users"."tenantId" against
// FROM "users" AS "u" names a relation the statement never mentions —
// PostgreSQL answers 42P01 and the query cannot run at all. The rename
// lets the fragment carrying such a predicate say, for the length of
// that fragment, that references to users are references to u.
//
// Only a relation reference is renamed, never a column name, and only
// while the fragment that installed it is rendering — so a predicate
// that embeds a subquery selecting from the renamed relation in its own
// right is rewritten too, which is the one shape the rename gets wrong.
func (b *Builder) RelationAlias(name string) string {
	if name == "" || name != b.relRenameFrom {
		return ""
	}
	return b.relRenameTo
}

// SetRelationAlias installs a relation rename and returns the pair it
// displaced, so a renderer can scope it to one fragment exactly the way
// SetBareIdents is scoped:
//
//	defer b.SetRelationAlias(b.SetRelationAlias("users", "u"))
//
// One rename is in force at a time rather than a stack of them. The
// renames a statement needs do not overlap — each automatic predicate
// belongs to exactly one table instance, and is rendered on its own —
// and the save/restore pair composes without a stack anyway.
func (b *Builder) SetRelationAlias(name, alias string) (prevName, prevAlias string) {
	prevName, prevAlias = b.relRenameFrom, b.relRenameTo
	b.relRenameFrom, b.relRenameTo = name, alias
	return prevName, prevAlias
}

// NewBuilder returns an empty Builder.
func NewBuilder(opts ...BuilderOption) *Builder {
	b := &Builder{}
	for _, o := range opts {
		o(b)
	}
	return b
}

// WriteString appends raw SQL. Callers must ensure it is safe (no
// unsanitised user input).
func (b *Builder) WriteString(s string) { b.sb.WriteString(s) }

// WriteByte appends a single raw byte of SQL. The returned error is
// always nil (strings.Builder.WriteByte never fails); the signature
// matches io.ByteWriter, which go vet's stdmethods check requires for a
// method named WriteByte. Callers ignore the error — errcheck is
// configured to skip this method, mirroring how it skips the identical
// (*strings.Builder).WriteByte by default.
func (b *Builder) WriteByte(c byte) error { return b.sb.WriteByte(c) }

// WriteIdent appends a quoted identifier. With no dialect installed it
// uses standard double quotes with "" doubling; a dialect can override
// the quoting (e.g. MySQL backticks) via QuoteIdent.
func (b *Builder) WriteIdent(name string) {
	if b.dialect != nil {
		b.sb.WriteString(b.dialect.QuoteIdent(name))
		return
	}
	b.sb.WriteByte('"')
	b.sb.WriteString(strings.ReplaceAll(name, `"`, `""`))
	b.sb.WriteByte('"')
}

// WriteQualified appends a qualified identifier such as "schema"."table"
// or "table"."column". Empty parts are skipped.
func (b *Builder) WriteQualified(parts ...string) {
	first := true
	for _, p := range parts {
		if p == "" {
			continue
		}
		if !first {
			b.sb.WriteByte('.')
		}
		b.WriteIdent(p)
		first = false
	}
}

// AddArg binds a value and writes its placeholder. The placeholder
// index is 1-based; the default rendering is "$<n>" (PostgreSQL) and
// can be overridden via WithPlaceholder.
func (b *Builder) AddArg(v any) {
	b.args = append(b.args, v)
	n := len(b.args)
	if b.placeholder != nil {
		b.sb.WriteString(b.placeholder(n))
		return
	}
	b.sb.WriteByte('$')
	b.sb.WriteString(strconv.Itoa(n))
}

// Append writes an Expression into the Builder. nil is a no-op.
func (b *Builder) Append(e Expression) {
	if e == nil {
		return
	}
	e.WriteSQL(b)
}

// AppendList writes a list of expressions separated by sep.
func (b *Builder) AppendList(sep string, exprs []Expression) {
	for i, e := range exprs {
		if i > 0 {
			b.sb.WriteString(sep)
		}
		b.Append(e)
	}
}

// SQL returns the accumulated SQL text and bound arguments.
func (b *Builder) SQL() (sql string, args []any) {
	return b.sb.String(), b.args
}

// String renders an Expression to its SQL text and bound argument list
// using the default PostgreSQL syntax. Useful in tests and for logging.
func String(e Expression) (sql string, args []any) {
	b := NewBuilder()
	b.Append(e)
	return b.SQL()
}

// StringWithDialect renders an Expression using d's placeholder and
// identifier-quoting rules. It is the dialect-aware counterpart to
// String — dialect packages and their tests use it to render SQL the
// backend's way.
func StringWithDialect(d Dialect, e Expression) (sql string, args []any) {
	b := NewBuilder(WithDialect(d))
	b.Append(e)
	return b.SQL()
}

// Raw is an Expression containing pre-formed SQL text. Use sparingly —
// values inside Raw are not parameter-checked; for parameterised
// fragments, compose Param or ExprFunc instead.
type Raw string

// WriteSQL implements Expression by writing the raw SQL verbatim.
func (r Raw) WriteSQL(b *Builder) { b.sb.WriteString(string(r)) }

// Param wraps a Go value as a parameter expression.
type Param struct{ Value any }

// WriteSQL implements Expression.
func (p Param) WriteSQL(b *Builder) { b.AddArg(p.Value) }

// ExprFunc adapts a plain function into an Expression.
type ExprFunc func(b *Builder)

// WriteSQL implements Expression.
func (f ExprFunc) WriteSQL(b *Builder) { f(b) }
