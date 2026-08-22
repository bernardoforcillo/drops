package clickhouse

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/bernardoforcillo/drops"
)

// ClickHouse row policies: the boundary the predicates DO have, and
// the half of it that does not exist.
//
// Until this file, nothing in drops mentioned that ClickHouse has
// server-side row policies at all, and both this package's doc.go and
// tenant.go said it had "no equivalent of PostgreSQL row-level
// security". That was wrong in the direction that matters: a reader
// deploying a multi-tenant ClickHouse was told the application
// predicates were the only thing available and had no reason to look
// for the mechanism the server has shipped since 20.1.
//
// What ClickHouse has is CREATE ROW POLICY … USING <cond> TO <roles>:
// a filter the server ANDs into every SELECT a matching principal
// issues against the table, evaluated where the caller cannot reach
// it. That is a real boundary, and it is what [RowPolicy] declares.
//
// # Why this is not called Policy, the way drops/pg's is
//
// Because the guarantee does not port, and a shared name would invite
// a reader to assume it does.
//
// A PostgreSQL policy has two halves: USING decides which rows are
// visible and WITH CHECK decides which rows a write may leave behind.
// A ClickHouse row policy has only the first. FOR INSERT is not an
// unimplemented option, it is a syntax error — the 26.7.2.1 parser
// answers "Expected one of: ALL, SELECT, end of query" — and
// system.row_policies has exactly one filter column, select_filter,
// to store a condition in. The parser tolerates a WITH CHECK token
// but there is nowhere for the condition to go and the phrase appears
// nowhere in ClickHouse's documentation, so this package does not
// emit one.
//
// The consequence, checked against a running engine rather than
// inferred: under a policy of tenant = 'acme', a SELECT of a
// three-row table returned the two acme rows, an INSERT of a fourth
// row belonging to another tenant succeeded with no error, and
// system.parts then reported four physical rows against a SELECT that
// still returned two. A principal that can write can write any tenant
// id it likes. ClickHouse says as much itself: row policies "make
// sense only if you have readonly access… if you can modify table or
// copy partitions between tables, it defeats the restrictions of row
// policies."
//
// So on ClickHouse the write side of tenant isolation is drops'
// predicates and the schema, with no server-side floor under it. The
// read side has a floor. Saying which half is which is the whole
// point of shipping this.
//
// # Why there is no runtime half here, and must not be
//
// drops/pg pairs its declaration surface with a runtime one, because
// PostgreSQL gives a per-request identity a LIFETIME: SET LOCAL dies
// with the transaction, so a request that sets a role or a
// set_config key cannot leak it to the next request that borrows the
// same pooled connection.
//
// ClickHouse offers nothing with that lifetime. A row policy's TO
// clause names users and roles, resolved from the account the
// connection authenticated as — an identity fixed before any request
// exists, not one a request can assume and give back. There is no
// transaction to scope an assumption to, because there are no
// transactions in the sense pg's design depends on. The one shape
// that looks like per-request identity — a policy written over
// getSetting('…') and a SET issued per request — is a SESSION
// setting: on a pooled connection it outlives the request that set
// it, and it is settable by exactly the code it would be constraining.
// That is precisely the leak [DB.InTx]-scoped identity exists to
// prevent in pg, so drops does not offer it here. Shipping a
// convenience wrapper around SET would be worse than shipping
// nothing, because it would read like a boundary.
//
// The deployment shape ClickHouse itself documents is one policy per
// tenant principal — CREATE ROW POLICY p1 ON events USING tenant_id=1
// TO user_1 — with the application connecting AS that principal.
// That is a documented deployment pattern, not a library feature, and
// drops states it as one: this file emits the DDL, and which account
// a connection authenticates as is the deployment's answer, not
// drops'.
//
// # Why none of this reaches Diff, Snapshot or Push
//
// It could not, and should not.
//
// Could not: this package has no schema introspection and no Push at
// all (see doc.go). There is nothing for a policy to be diffed
// against.
//
// Should not, even when there is. A ClickHouse row policy is not
// table metadata. It lives in the server's ACCESS storage — users.xml,
// a local_directory, or a replicated access storage — alongside users
// and roles, keyed by (name, table) and able to outlive the table it
// names. A schema snapshot describes the objects a schema owns; an
// access entity is owned by the cluster's access control, frequently
// by a different team, and often replicated by a different mechanism.
// Folding it into a table snapshot would make `drops push` an
// instrument for editing access control as a side effect of a column
// change. So a policy is a statement you place in a migration, or
// execute yourself through [DB.ExecExpr], and drops does not track
// its drift. That is a deliberate scope decision, not an omission
// waiting to be filled in.
//
// # What rests on rendering alone
//
// Every statement this file emits was parsed by a real ClickHouse
// 26.7.2.1 (via EXPLAIN AST on the embedded engine), and the read /
// write asymmetry above was executed. NOT verified here, and taken
// from ClickHouse's documentation and changelog: how permissive and
// restrictive policies combine, how policies behave on Distributed
// tables and under FINAL, and the two server defaults below — which
// are the sharpest edge in the whole mechanism and belong in front of
// anyone who declares a policy:
//
//   - users_without_row_policies_can_read_rows defaults to TRUE. If a
//     policy exists for user A and none for user B, B reads EVERY
//     row. Adding a principal — a migration account, a BI connector,
//     a new service — silently exempts it.
//   - throw_on_unmatched_row_policies, which turns that case into an
//     error, arrived in 26.2 and defaults to FALSE.
//
// A ClickHouse row policy therefore fails OPEN unless the deployment
// says otherwise. A drops-emitted policy does not change that, and no
// doc comment here should be read as if it did.

// Errors a [RowPolicy] can carry. Each names a declaration that would
// otherwise render as a statement the server accepts and drops cannot
// state the meaning of.
var (
	// ErrRowPolicyTargetRequired means no table was named. A policy
	// with no ON clause is not a policy.
	ErrRowPolicyTargetRequired = errors.New("drops/clickhouse: row policy has no target; call On(table) or OnAllTablesIn(database)")

	// ErrRowPolicyConditionRequired means no USING expression was
	// given. ClickHouse's parser accepts a policy without one, which
	// is why this is checked here: the statement would succeed and
	// install an access rule whose effect drops cannot describe.
	ErrRowPolicyConditionRequired = errors.New("drops/clickhouse: row policy has no USING condition; call Using or UsingEq")

	// ErrRowPolicyUnsupportedLiteral means UsingEq was handed a value
	// with no unambiguous ClickHouse literal form. Nothing here
	// reaches for a fallback such as fmt.Sprint: the text becomes a
	// stored access rule, and guessing at it once is guessing at it
	// for every later SELECT.
	ErrRowPolicyUnsupportedLiteral = errors.New("drops/clickhouse: unsupported row policy literal type")
)

// RowPolicy declares a ClickHouse row policy — a server-side filter
// ANDed into every SELECT the named roles issue against the target.
//
// It is deliberately narrower than drops/pg's Policy: there is no For
// and no WithCheck, because ClickHouse has no command to point them
// at. See the file comment for what that costs and why the type is
// not called Policy.
//
// A zero RowPolicy is not usable; start from [NewRowPolicy]. The
// builder methods return the receiver so a declaration reads as one
// expression, and a bad argument is recorded on the policy rather
// than returned, surfacing at [CreateRowPolicyErr] or [RowPolicy.Err].
type RowPolicy struct {
	name    string
	cluster string

	// database / table name the policy applies to. allTables renders
	// the ON db.* form, which covers tables added later — the shape
	// that does not silently miss a table somebody forgot.
	database  string
	table     string
	allTables bool

	restrictive bool
	using       string

	toAll  bool
	to     []string
	except []string

	err error
}

// NewRowPolicy declares a row policy by name. Pair it with On (or
// OnAllTablesIn) and Using, then render with [CreateRowPolicy].
//
// The name is validated like every other identifier this package
// writes and a bad one panics at startup — see [ErrInvalidIdentifier].
// Startup is where a schema declaration wants to fail: a policy name
// that cannot be rendered is not something to discover when a
// migration runs against production.
func NewRowPolicy(name string) *RowPolicy {
	mustIdent("row policy", name)
	return &RowPolicy{name: name}
}

// On targets a table. The policy takes the table's database, so a
// table declared with [NewDatabaseTable] renders as "db"."table" and
// one declared with [NewTable] renders unqualified against whichever
// database the connection is using.
func (p *RowPolicy) On(t *Table) *RowPolicy {
	p.database = t.Database()
	p.table = t.Name()
	p.allTables = false
	return p
}

// OnAllTablesIn targets every table in a database — ClickHouse's
// ON db.* form.
//
// It is worth reaching for over a policy per table for the reason
// database-wide grants exist: a per-table set covers the tables
// somebody remembered, and the table added next quarter is the one
// that leaks. ClickHouse combines a database-level policy with any
// table-level ones by the usual permissive / restrictive rules.
func (p *RowPolicy) OnAllTablesIn(database string) *RowPolicy {
	mustIdent("database", database)
	p.database = database
	p.table = ""
	p.allTables = true
	return p
}

// OnCluster distributes the statement with ON CLUSTER.
//
// ClickHouse puts the clause in different places in the two
// statements — after the policy name in CREATE, after the target in
// DROP — and both orders were confirmed against the 26.7.2.1 parser.
// Holding the cluster on the policy rather than passing it to each
// emitter is what keeps a declaration from being right in the CREATE
// and forgotten in the DROP.
func (p *RowPolicy) OnCluster(cluster string) *RowPolicy {
	mustIdent("cluster", cluster)
	p.cluster = cluster
	return p
}

// Restrictive flips the policy to AS RESTRICTIVE. Permissive policies
// for one principal OR together; restrictive ones AND with the
// result, so a restrictive policy can only ever narrow.
//
// One ClickHouse behaviour to know before relying on it, from its
// documentation rather than from a server this package reached: a
// table whose only policies are restrictive leaves every row visible,
// because there is no permissive term for them to narrow. A
// restrictive-only declaration is therefore not a boundary on its own.
func (p *RowPolicy) Restrictive() *RowPolicy { p.restrictive = true; return p }

// Using sets the USING condition verbatim.
//
// The text is stored by the server and re-evaluated on every later
// SELECT, so a quotation mark smuggled into it is not one bad query —
// it is a permanently widened filter that no application code can
// take back. Build conditions over caller-supplied values with
// [RowPolicy.UsingEq], which quotes them; reach for Using where the
// condition is written by hand in the schema.
//
// The condition names columns of the target table and must reference
// them unqualified: it is evaluated against the row, not against a
// FROM list.
func (p *RowPolicy) Using(expr string) *RowPolicy { p.using = expr; return p }

// UsingEq sets the USING condition to `"col" = <literal>` with the
// column quoted the ClickHouse way and the value rendered as a
// literal.
//
// It exists because the common condition — a tenant column equal to a
// tenant key — is the one most likely to be built by string
// concatenation, and the one where doing so is worst: see
// [RowPolicy.Using]. A value with no unambiguous literal form records
// [ErrRowPolicyUnsupportedLiteral] rather than being formatted by
// fmt.Sprint.
//
// The column renders bare, with no table qualifier, for the same
// reason a ClickHouse sorting key does — see drops.Builder.BareIdents.
// It takes a [ColRef] rather than a *Column so a typed handle from
// [Add] can be passed straight in, like every other clause that names
// a column.
func (p *RowPolicy) UsingEq(c ColRef, value any) *RowPolicy {
	lit, err := rowPolicyLiteral(value)
	if err != nil {
		p.err = err
		return p
	}
	p.using = quoteIdent(c.col().Name()) + " = " + lit
	return p
}

// To scopes the policy to the named roles or users.
//
// A policy with no TO clause at all is legal ClickHouse and applies
// to nobody, which is why it is not the way to say "everyone" —
// [RowPolicy.ToAll] is.
func (p *RowPolicy) To(roles ...string) *RowPolicy {
	for _, r := range roles {
		mustIdent("role", r)
	}
	p.to = append(p.to, roles...)
	p.toAll = false
	return p
}

// ToAll applies the policy to every principal (TO ALL).
func (p *RowPolicy) ToAll() *RowPolicy {
	p.toAll = true
	p.except = nil
	return p
}

// ToAllExcept applies the policy to every principal but the named
// ones (TO ALL EXCEPT).
//
// This is the form that answers the fail-open default described in
// the file comment: a policy scoped TO ALL EXCEPT the accounts that
// are meant to see everything covers the principals nobody thought
// of, where a policy naming each tenant role leaves the next account
// somebody adds unfiltered.
func (p *RowPolicy) ToAllExcept(roles ...string) *RowPolicy {
	for _, r := range roles {
		mustIdent("role", r)
	}
	p.toAll = true
	p.except = append(p.except, roles...)
	return p
}

// Name returns the policy identifier.
func (p *RowPolicy) Name() string { return p.name }

// Cluster returns the ON CLUSTER name, or "".
func (p *RowPolicy) Cluster() string { return p.cluster }

// TargetDatabase returns the database the policy applies to, or ""
// when the target table was declared without one.
func (p *RowPolicy) TargetDatabase() string { return p.database }

// TargetTable returns the table the policy applies to, or "" for a
// database-wide policy.
func (p *RowPolicy) TargetTable() string { return p.table }

// AppliesToAllTables reports whether the policy targets ON db.*.
func (p *RowPolicy) AppliesToAllTables() bool { return p.allTables }

// IsRestrictive reports whether the policy renders AS RESTRICTIVE.
func (p *RowPolicy) IsRestrictive() bool { return p.restrictive }

// UsingExpr returns the USING condition text.
func (p *RowPolicy) UsingExpr() string { return p.using }

// Roles returns the roles named by To, as a copy: the slice is part
// of an access declaration and handing back the live one would let a
// caller edit a policy through a value it only meant to read.
func (p *RowPolicy) Roles() []string {
	out := make([]string, len(p.to))
	copy(out, p.to)
	return out
}

// ExceptRoles returns the roles excluded by ToAllExcept, as a copy.
func (p *RowPolicy) ExceptRoles() []string {
	out := make([]string, len(p.except))
	copy(out, p.except)
	return out
}

// AppliesToAllRoles reports whether the policy renders a bare TO ALL.
func (p *RowPolicy) AppliesToAllRoles() bool { return p.toAll && len(p.except) == 0 }

// Err returns the first error recorded while the policy was declared,
// or the reason it is not renderable yet.
func (p *RowPolicy) Err() error { return p.validate() }

func (p *RowPolicy) validate() error {
	if p.err != nil {
		return p.err
	}
	if p.table == "" && !p.allTables {
		return ErrRowPolicyTargetRequired
	}
	if strings.TrimSpace(p.using) == "" {
		return ErrRowPolicyConditionRequired
	}
	return nil
}

// rowPolicyLiteral renders v as a ClickHouse literal.
//
// The accepted set is the set of types a tenant key is plausibly held
// in, and it stops there on purpose. There is no default arm: an
// unrecognised type is an error, not a fmt.Sprint, because the result
// is stored server-side as an access rule and a wrong guess is not
// one wrong query.
func rowPolicyLiteral(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return quoteLiteral(x), nil
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
		return strconv.FormatFloat(float64(x), 'g', -1, 32), nil
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64), nil
	default:
		return "", fmt.Errorf("%w: %T", ErrRowPolicyUnsupportedLiteral, v)
	}
}

// ----------------------------------------------------------------------
// DDL
// ----------------------------------------------------------------------

// CreateRowPolicy renders CREATE ROW POLICY for p.
//
// Like [CreateTable], it renders rather than returning an error: an
// incomplete declaration emits a statement carrying a marked comment,
// so a stray call in an init script fails loudly at exec time instead
// of installing something. Here the choice is sharper than it is for
// a table. A CREATE ROW POLICY missing its USING clause is a
// statement ClickHouse ACCEPTS, and it would install an access rule
// whose effect drops cannot state; breaking the statement on purpose
// is the only way for the failure to be visible. Use
// [CreateRowPolicyErr] where a definite error is wanted.
func CreateRowPolicy(p *RowPolicy) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { writeCreateRowPolicy(b, p, "") })
}

// CreateRowPolicyIfNotExists is the IF NOT EXISTS variant: an existing
// policy of that name is left exactly as it is.
func CreateRowPolicyIfNotExists(p *RowPolicy) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { writeCreateRowPolicy(b, p, "IF NOT EXISTS ") })
}

// CreateOrReplaceRowPolicy is the OR REPLACE variant.
//
// Prefer it in a migration that is the single source of a policy's
// text: IF NOT EXISTS leaves a policy edited by hand on the server in
// place, so the rule in the repository and the rule being enforced
// drift apart silently, which for an access rule is the drift that
// matters.
func CreateOrReplaceRowPolicy(p *RowPolicy) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { writeCreateRowPolicy(b, p, "OR REPLACE ") })
}

// CreateRowPolicyErr returns the DDL, or the reason p cannot be
// rendered. Use it in migration tooling that wants a definite error
// rather than a statement built to fail.
func CreateRowPolicyErr(p *RowPolicy) (drops.Expression, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	return CreateRowPolicy(p), nil
}

// DropRowPolicy renders DROP ROW POLICY for p's name and target.
func DropRowPolicy(p *RowPolicy) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { writeDropRowPolicy(b, p, false) })
}

// DropRowPolicyIfExists is the IF EXISTS variant.
func DropRowPolicyIfExists(p *RowPolicy) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { writeDropRowPolicy(b, p, true) })
}

// writeCreateRowPolicy renders the statement in the ONE clause order
// ClickHouse's parser accepts.
//
// The order is not drops/pg's, and the difference is not cosmetic: the
// pg emitter writes AS, then FOR, then TO, then USING, and TO before
// USING is a syntax error here — "Expected one of: token, Comma,
// USING, WITH CHECK, end of query", from the 26.7.2.1 parser. A reader
// porting a declaration between the two dialects will write the pg
// order, so the emitter is pinned to the one that parses, and pinned
// by a test that says why.
//
// FOR SELECT is written out even though it is the only value the
// grammar admits. Rendering it makes every emitted statement say what
// the policy does and does not cover, in the one place a reader is
// most likely to be carrying a PostgreSQL expectation across.
func writeCreateRowPolicy(b *drops.Builder, p *RowPolicy, modifier string) {
	b.WriteString("CREATE ROW POLICY ")
	b.WriteString(modifier)
	if err := p.validate(); err != nil {
		// Break the statement rather than emit an access rule whose
		// meaning drops cannot state. See CreateRowPolicy.
		b.WriteString("/* drops/clickhouse: " + err.Error() + " */")
		return
	}
	b.WriteIdent(p.name)
	if p.cluster != "" {
		b.WriteString(" ON CLUSTER ")
		b.WriteIdent(p.cluster)
	}
	b.WriteString(" ON ")
	writeRowPolicyTarget(b, p)
	b.WriteString(" FOR SELECT USING (")
	b.WriteString(p.using)
	b.WriteByte(')')
	if p.restrictive {
		b.WriteString(" AS RESTRICTIVE")
	}
	writeRowPolicyTo(b, p)
}

// writeDropRowPolicy renders DROP ROW POLICY.
//
// It needs only the name and the target, so it does not run validate:
// dropping a policy whose USING clause was never filled in is exactly
// what a rollback of a half-written migration wants to do.
func writeDropRowPolicy(b *drops.Builder, p *RowPolicy, ifExists bool) {
	b.WriteString("DROP ROW POLICY ")
	if ifExists {
		b.WriteString("IF EXISTS ")
	}
	if p.table == "" && !p.allTables {
		b.WriteString("/* drops/clickhouse: " + ErrRowPolicyTargetRequired.Error() + " */")
		return
	}
	b.WriteIdent(p.name)
	b.WriteString(" ON ")
	writeRowPolicyTarget(b, p)
	// ON CLUSTER trails the target in DROP where it precedes it in
	// CREATE. The asymmetry is ClickHouse's grammar, not a choice.
	if p.cluster != "" {
		b.WriteString(" ON CLUSTER ")
		b.WriteIdent(p.cluster)
	}
}

func writeRowPolicyTarget(b *drops.Builder, p *RowPolicy) {
	if p.allTables {
		b.WriteIdent(p.database)
		b.WriteString(".*")
		return
	}
	if p.database != "" {
		b.WriteIdent(p.database)
		b.WriteByte('.')
	}
	b.WriteIdent(p.table)
}

func writeRowPolicyTo(b *drops.Builder, p *RowPolicy) {
	switch {
	case p.toAll:
		b.WriteString(" TO ALL")
		if len(p.except) > 0 {
			b.WriteString(" EXCEPT ")
			for i, r := range p.except {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteIdent(r)
			}
		}
	case len(p.to) > 0:
		b.WriteString(" TO ")
		for i, r := range p.to {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteIdent(r)
		}
	}
}

// ----------------------------------------------------------------------
// Declaring policies beside the table
// ----------------------------------------------------------------------

// AddRowPolicy attaches a row policy to the table, so a schema can
// declare its access rules where it declares its columns — the shape
// drops/pg's Table.AddPolicy has.
//
// A policy with no target of its own takes this table's. One that
// already named a target keeps it: a policy declared ON db.* and then
// hung off one table for readability must not be silently narrowed to
// that table, which would turn a rule covering every table into a rule
// covering one.
//
// Attaching does NOT make the policy part of the table's schema.
// Nothing carries it into a snapshot or a diff, and that is
// deliberate — see the file comment for why an access entity does not
// belong in a schema snapshot even once this package has one. What
// the table gives you is a place to keep the declaration and
// [Table.RowPolicies] to walk it when a migration emits the DDL.
func (t *Table) AddRowPolicy(p *RowPolicy) *Table {
	if p.table == "" && !p.allTables {
		p.On(t)
	}
	t.rowPolicies = append(t.rowPolicies, p)
	return t
}

// RowPolicies returns the policies declared on the table, in
// declaration order, as a copy of the slice.
func (t *Table) RowPolicies() []*RowPolicy {
	out := make([]*RowPolicy, len(t.rowPolicies))
	copy(out, t.rowPolicies)
	return out
}
