package pg

import (
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

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

// View opens a fluent builder for a regular view. Chain As (typed
// SelectBuilder) or AsSQL (raw SELECT text), optionally
// Materialized, to finalise:
//
//	pg.View("activeUsers").As(
//	    db.Select(Users.ID, Users.Name).
//	        From(Users).
//	        Where(Users.Active.Eq(true)))
//
//	pg.View("playerStats").
//	    Materialized().
//	    As(query)
//
// NewView / NewMaterializedView remain available for the
// definition-as-string fast path.
func View(name string) *PgView {
	return &PgView{name: name}
}

// MaterializedView is the materialised counterpart of View — the
// same fluent builder with the materialised flag pre-set so the
// chain reads naturally.
func MaterializedView(name string) *PgView {
	return &PgView{name: name, materialized: true}
}

// As wires the view body from a SelectBuilder. Parameter bindings
// ($1, $2, ...) are inlined as SQL literals because view
// definitions are static SQL — CREATE VIEW doesn't accept bound
// parameters. Unsupported parameter types panic at declaration
// time so the schema fails loudly at startup instead of producing
// broken DDL.
func (v *PgView) As(sel *SelectBuilder) *PgView {
	sql, args := sel.ToSQL()
	body, err := inlineSQLLiterals(sql, args)
	if err != nil {
		panic(fmt.Sprintf("drops/pg: View(%q).As: %v", v.name, err))
	}
	v.definition = body
	return v
}

// AsSQL sets the view body from raw SQL — escape hatch for
// SELECT shapes the builder doesn't cover (recursive CTEs,
// dialect-specific functions, hand-tuned plans). Caller is
// responsible for any literal escaping.
func (v *PgView) AsSQL(definition string) *PgView {
	v.definition = definition
	return v
}

// Materialized flips the view kind to materialised. Returns the
// same *PgView for chaining.
func (v *PgView) Materialized() *PgView {
	v.materialized = true
	return v
}

// paramRE matches a PostgreSQL parameter placeholder ($1, $2, ...).
// drops only emits these via Builder.AddArg, so they never appear
// inside quoted literals in generated SQL.
var paramRE = regexp.MustCompile(`\$(\d+)`)

// inlineSQLLiterals substitutes every $N in sql with the SQL
// literal form of args[N-1]. Used by PgView.As because view
// definitions are static SQL.
func inlineSQLLiterals(sql string, args []any) (string, error) {
	var firstErr error
	out := paramRE.ReplaceAllStringFunc(sql, func(match string) string {
		n, _ := strconv.Atoi(match[1:])
		if n < 1 || n > len(args) {
			if firstErr == nil {
				firstErr = fmt.Errorf("placeholder $%d out of range (have %d args)", n, len(args))
			}
			return match
		}
		lit, err := sqlLiteral(args[n-1])
		if err != nil && firstErr == nil {
			firstErr = err
		}
		return lit
	})
	return out, firstErr
}

// sqlLiteral renders v as a PostgreSQL literal suitable for
// embedding directly in DDL. The supported set covers the common
// view-definition cases — extend as needed.
func sqlLiteral(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "NULL", nil
	case bool:
		if x {
			return "true", nil
		}
		return "false", nil
	case int:
		return strconv.FormatInt(int64(x), 10), nil
	case int8:
		return strconv.FormatInt(int64(x), 10), nil
	case int16:
		return strconv.FormatInt(int64(x), 10), nil
	case int32:
		return strconv.FormatInt(int64(x), 10), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case uint:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint8:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint16:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint32:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint64:
		return strconv.FormatUint(x, 10), nil
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32), nil
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), nil
	case string:
		return quoteLiteral(x), nil
	case time.Time:
		return quoteLiteral(x.UTC().Format(time.RFC3339Nano)) + "::timestamptz", nil
	case []byte:
		return `'\x` + hex.EncodeToString(x) + `'::bytea`, nil
	default:
		return "", fmt.Errorf("unsupported parameter type %T", v)
	}
}

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
