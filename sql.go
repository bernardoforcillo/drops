package drops

// SQL builds an [Expression] out of alternating SQL text and values —
// the escape hatch for the operator the DSL does not model, without
// leaving parameterisation behind on the way out:
//
//	drops.SQL("lower(", Users.Email, ") = ", drops.Arg(strings.ToLower(in)))
//
// [Raw] is the older escape hatch and carries no arguments at all, so
// reaching for it costs the type system and the bound parameters in one
// move. SQL costs only the type system.
//
// Each part is rendered according to its Go type:
//
//   - a string is literal SQL text, written verbatim;
//   - an [Expression] writes itself — a column reference renders as its
//     qualified quoted identifier, a nested SQL fragment inlines, a
//     [Param] binds;
//   - a table (see below) renders as the identifier that refers to it;
//   - anything else is bound as a parameter through [Builder.AddArg].
//
// # A bare string is SQL text, never a value
//
// That first rule is the dangerous one, and it is deliberate. A
// fragment is mostly operators, parentheses and punctuation; a design
// where every one of them had to be wrapped in a marker type would be
// worse than the escape hatch it replaces, and nobody would use it. The
// price is that a string variable holding user input is SQL text too:
//
//	drops.SQL(Users.Email, " = ", drops.Arg(in)) // bound parameter — safe
//	drops.SQL(Users.Email, " = '", in, "'")      // string interpolation — an injection
//
// So a string that is a value must say so, with [Arg] or [Param]. Every
// other Go type — int, time.Time, []byte, a named string type, nil —
// binds without a wrapper, because none of them can be confused for SQL
// text; [Arg] is never wrong on them either, and is worth the habit.
//
// # Tables
//
// A table renders as a reference to itself: its alias when it has one,
// otherwise its schema- or database-qualified name. It is deliberately
// not the FROM-clause form ("users" AS "u") that passing the same table
// to [Builder.Append] would produce — inside an expression fragment the
// AS clause is a syntax error, and once a query has aliased a table
// PostgreSQL will not accept the original name in place of the alias.
// Introducing the alias remains the job of the builder's FROM.
//
// Every dialect's *Table is recognised through the methods it already
// has; so is any other type that carries a name and an alias.
func SQL(parts ...any) Expression { return sqlFragment(parts) }

// Arg marks a value as a bound parameter. It exists for [SQL], where a
// bare string is SQL text and a string that is a value has to be said
// out loud:
//
//	drops.SQL(Users.Name, " ILIKE ", drops.Arg("%"+q+"%"))
//
// It is [Param] by another name, spelled as a call so it reads as a
// wrapper at the call site rather than as a struct literal.
func Arg(v any) Expression { return Param{Value: v} }

// sqlFragment is the Expression [SQL] returns. It keeps the parts as
// given and dispatches on their dynamic type at render time, so the
// same fragment can be rendered by two dialects.
type sqlFragment []any

// WriteSQL implements Expression.
func (f sqlFragment) WriteSQL(b *Builder) {
	for _, p := range f {
		switch v := p.(type) {
		case string:
			b.WriteString(v)
		case tableIdent:
			// Before the Expression case: a *Table satisfies both, and
			// its own WriteSQL is the FROM form.
			writeTableIdent(b, v)
		case Expression:
			v.WriteSQL(b)
		default:
			b.AddArg(v)
		}
	}
}

// tableIdent is the shape every dialect's *Table already has and no
// column does: an [Expression] that knows its own name and its
// query-scope alias. Recognising a table through it keeps the root
// package free of any import of pg, mysql, sqlite or clickhouse — it
// cannot import them, they import it — and free of the reflection that
// would otherwise be the only way to tell a table handle from a column
// handle.
type tableIdent interface {
	Expression
	Name() string
	Alias() string
}

// writeTableIdent writes the identifier that refers to t.
func writeTableIdent(b *Builder, t tableIdent) {
	if a := t.Alias(); a != "" {
		b.WriteIdent(a)
		return
	}
	b.WriteQualified(tableQualifier(t), t.Name())
}

// tableQualifier returns the namespace a table lives in. The dialects
// disagree on the word — PostgreSQL calls it a schema, MySQL and
// ClickHouse a database, SQLite has neither — so both spellings are
// tried and a table with neither renders unqualified.
func tableQualifier(t tableIdent) string {
	if s, ok := t.(interface{ Schema() string }); ok {
		return s.Schema()
	}
	if d, ok := t.(interface{ Database() string }); ok {
		return d.Database()
	}
	return ""
}
