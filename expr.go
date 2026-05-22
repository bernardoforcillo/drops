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

// WriteByte appends a single raw byte of SQL. The error returned is
// always nil; the signature satisfies io.ByteWriter.
func (b *Builder) WriteByte(c byte) error { return b.sb.WriteByte(c) }

// WriteIdent appends a quoted identifier. Embedded double quotes are
// doubled per the SQL standard.
func (b *Builder) WriteIdent(name string) {
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
func (b *Builder) SQL() (string, []any) {
	return b.sb.String(), b.args
}

// String renders an Expression to its SQL text and bound argument list.
// Useful in tests and for logging.
func String(e Expression) (string, []any) {
	b := NewBuilder()
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
