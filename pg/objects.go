package pg

// Schema-level objects: sequences, views, and (table-scoped) RLS
// policies — the runtime descriptors the snapshot/diff layer
// reads to emit CREATE / DROP / ALTER DDL during a migration.

// PgSequence describes a top-level PostgreSQL sequence. Wrap
// SequenceOptions (already used by CreateSequence) so the
// configuration matches whatever DDL the runtime emits at apply
// time.
type PgSequence struct {
	name    string
	options SequenceOptions
}

// NewSequence declares a sequence by name with optional
// configuration. opts is variadic so the common "default
// everything" case is a single-arg call.
func NewSequence(name string, opts ...SequenceOptions) *PgSequence {
	var o SequenceOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	return &PgSequence{name: name, options: o}
}

// Name returns the sequence's identifier.
func (s *PgSequence) Name() string { return s.name }

// Options returns the configuration the sequence was declared
// with.
func (s *PgSequence) Options() SequenceOptions { return s.options }

// ----------------------------------------------------------------------
// Views
// ----------------------------------------------------------------------

// PgView describes a (regular or materialized) view. Definition
// is the body that follows AS — typically a SELECT.
type PgView struct {
	name         string
	definition   string
	materialized bool
}

// NewView declares a regular VIEW.
func NewView(name, definition string) *PgView {
	return &PgView{name: name, definition: definition}
}

// NewMaterializedView declares a materialized view.
func NewMaterializedView(name, definition string) *PgView {
	return &PgView{name: name, definition: definition, materialized: true}
}

// Name returns the view's identifier.
func (v *PgView) Name() string { return v.name }

// Definition returns the view's SELECT body.
func (v *PgView) Definition() string { return v.definition }

// IsMaterialized reports whether this is a materialized view.
func (v *PgView) IsMaterialized() bool { return v.materialized }

// ----------------------------------------------------------------------
// RLS Policies
// ----------------------------------------------------------------------

// Policy is a row-level security policy attached to a table. The
// shape mirrors PostgreSQL's CREATE POLICY syntax exactly so
// emitting DDL stays mechanical.
type Policy struct {
	name      string
	as        string   // "PERMISSIVE" (default) or "RESTRICTIVE"
	command   string   // "ALL" / "SELECT" / "INSERT" / "UPDATE" / "DELETE"
	to        []string // role names; empty = PUBLIC
	using     string   // USING (<expr>); empty = no USING clause
	withCheck string   // WITH CHECK (<expr>); empty = inherits USING
}

// NewPolicy declares a row-level security policy by name. Pair
// with Table.AddPolicy.
func NewPolicy(name string) *Policy {
	return &Policy{name: name, as: "PERMISSIVE", command: "ALL"}
}

// Restrictive flips the policy to RESTRICTIVE (default is
// PERMISSIVE).
func (p *Policy) Restrictive() *Policy { p.as = "RESTRICTIVE"; return p }

// For sets the command the policy applies to ("ALL", "SELECT",
// "INSERT", "UPDATE", "DELETE"). Defaults to "ALL".
func (p *Policy) For(cmd string) *Policy { p.command = cmd; return p }

// To restricts the policy to the named roles. Without any role
// the policy applies to PUBLIC.
func (p *Policy) To(roles ...string) *Policy {
	p.to = append(p.to, roles...)
	return p
}

// Using sets the USING expression — the predicate that decides
// which rows are visible / mutable.
func (p *Policy) Using(expr string) *Policy { p.using = expr; return p }

// WithCheck sets the WITH CHECK expression — the predicate that
// rows must satisfy after an INSERT / UPDATE. Defaults to the
// USING expression when omitted.
func (p *Policy) WithCheck(expr string) *Policy { p.withCheck = expr; return p }

// Name returns the policy identifier.
func (p *Policy) Name() string { return p.name }

// As returns the policy mode ("PERMISSIVE" or "RESTRICTIVE").
func (p *Policy) As() string { return p.as }

// Command returns the SQL command the policy applies to.
func (p *Policy) Command() string { return p.command }

// Roles returns the roles the policy is scoped to (or nil for
// PUBLIC).
func (p *Policy) Roles() []string {
	out := make([]string, len(p.to))
	copy(out, p.to)
	return out
}

// UsingExpr returns the USING expression text.
func (p *Policy) UsingExpr() string { return p.using }

// WithCheckExpr returns the WITH CHECK expression text.
func (p *Policy) WithCheckExpr() string { return p.withCheck }
