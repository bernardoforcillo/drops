package mysql

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/bernardoforcillo/drops"
)

// MySQL tenant isolation: what the server can enforce, what drops can
// render, and the gap between the two that no statement closes.
//
// MySQL has no row-level security. drops/pg's boundary does not port,
// and until this file the package doc said so and stopped — naming a
// definer-rights view once, as "a schema object drops does not
// manage", and leaving the reader with the impression that the
// application predicates were all MySQL had. That was wrong in the
// direction that matters. MySQL has a mechanism that is in one respect
// STRONGER than the ClickHouse row policies drops now declares: it
// covers writes.
//
// # The mechanism
//
// A view, owned by an account that can read the base table, filtered
// to one tenant, carrying WITH CASCADED CHECK OPTION — and a tenant
// account that holds privileges on THE VIEW AND NOT ON THE TABLE:
//
//	CREATE DEFINER = `app`@`localhost` SQL SECURITY DEFINER
//	    VIEW `shop`.`v_docs_acme` AS
//	    SELECT `id`, `tenantId`, `body` FROM `shop`.`docs`
//	    WHERE `tenantId` = 'acme' WITH CASCADED CHECK OPTION;
//	GRANT SELECT, INSERT, UPDATE, DELETE
//	    ON `shop`.`v_docs_acme` TO `acme`@`%`;
//
// SQL SECURITY DEFINER makes the view run with the DEFINER's rights,
// so the tenant account reads the base table THROUGH the view without
// holding any privilege on it. CHECK OPTION supplies what a ClickHouse
// row policy has no syntax for: a write that would produce a row the
// view cannot see is refused.
//
// Measured on a live MySQL 8.0.46 and a live MariaDB 10.11.14, with
// identical results on both — see mysql/tenantview_test.go for the
// provenance and integration/mysql_tenantview_test.go for the runnable
// sequence. As the tenant account, against a three-row table holding
// two tenants:
//
//   - SELECT through the view returned that tenant's rows only;
//   - SELECT against the base table: ERROR 1142, command denied;
//   - INSERT of another tenant's axis value: ERROR 1369, CHECK OPTION
//     failed;
//   - INSERT of its own: accepted;
//   - UPDATE moving a row to another tenant: ERROR 1369;
//   - UPDATE of another tenant's row: matched nothing;
//   - DELETE FROM the view with no WHERE: removed only the rows the
//     view could see, and the other tenant's row survived;
//   - CREATE VIEW to build an unfiltered one: ERROR 1142.
//
// That holds whatever SQL the application sends, because it is the
// privilege system enforcing it and not a predicate. It is a boundary
// in the sense this phase has used the word.
//
// # What drops can render, and what it cannot
//
// This file renders the two statements above and stops. It does not
// execute them, and there is deliberately no DB method that does.
//
// The reason is that the two statements are not the boundary. The
// boundary is the ABSENCE of a grant on the base table, and an absence
// is not a statement. drops can emit a CREATE VIEW and a GRANT; it
// cannot emit "and nothing else anywhere in this server grants this
// account access to `shop`.`docs`", because that is a property of the
// whole privilege state — of accounts drops never hears about, of
// roles, of grants on `shop`.* made years earlier, of the account the
// application pool actually authenticates as. A library that executed
// these would be reporting success for a boundary it had not
// established and could not check.
//
// Nor does it emit a REVOKE. Rendering one would suggest drops could
// take the base table away, and it would be the wrong shape twice
// over: REVOKE errors with ERROR 1147 when there is no grant to
// remove, which is the state a correct deployment is already in, and
// REVOKE IF EXISTS — the form that tolerates it — is MySQL 8.0.16 and
// later only. MariaDB 10.11.14 answers ERROR 1064 to it. Both measured
// here.
//
// So: a documented deployment pattern, not a library feature. drops
// renders exactly the DDL an operator would have written, the operator
// reads it and applies it, and the grant state stays the operator's.
//
// # What it costs
//
// The identity is the MySQL ACCOUNT. One view and one account per
// tenant, and the application must connect AS that account.
//
// That is a real cost and it is why this is not the default answer for
// every MySQL deployment. A pooled connection shared across tenants
// cannot use it: the pool authenticates once, as one account, and the
// per-request tenant is not expressible. It does not scale to
// thousands of tenants, and each new tenant is a DDL step and an
// account.
//
// Where it fits is the shape it was built for: a small number of
// tenants that are genuinely separate principals — a per-customer
// deployment, a regulated tenant that has to be provably unable to
// read the others, a reporting account that must see one tenant.
//
// # Why there is no runtime half, and must not be
//
// drops/pg pairs its declaration surface with a runtime one because
// PostgreSQL gives a per-request identity a LIFETIME: SET LOCAL dies
// with the transaction, so a request cannot leak its identity to the
// next request that borrows the same pooled connection.
//
// MySQL offers nothing with that lifetime, and the near-misses are
// worse than nothing. Measured here:
//
//   - a view body cannot reference a session variable at all. CREATE
//     VIEW ... WHERE tenant = @tenant is ERROR 1351, "View's SELECT
//     contains a variable or parameter", on both servers. The same
//     error refuses a prepared CREATE VIEW with a placeholder, which
//     is why the tenant value here is a literal.
//   - the variable CAN be smuggled in through a stored function that
//     returns it, and the result is a trap. With the function in
//     place, setting @tenant to another tenant's value returned that
//     tenant's rows; setting it to NULL returned zero rows and NO
//     error, which reads exactly like "no such row". @tenant is a
//     SESSION variable: it outlives the statement, so on a pooled
//     connection it outlives the request that set it, and it is
//     settable by precisely the code it would be constraining.
//
// A convenience wrapper around SET would therefore read like a
// boundary and be a variable the guarded code assigns to itself. drops
// does not ship one.
//
// # Why none of this reaches Diff, Snapshot or Push
//
// A tenant view is a view, and drops/mysql's schema tooling models
// tables. More to the point, the GRANT half is not schema at all: it
// lives in the mysql.* system tables alongside accounts and roles,
// frequently owned by a different team. Folding it into a snapshot
// would make a push an instrument for editing access control as a side
// effect of a column change. Put these statements in a migration, or
// run them yourself through [DB.ExecExpr] as an account that may.

// Errors a [TenantView] can carry. Each names a declaration that would
// otherwise render as a statement MySQL accepts and drops cannot state
// the meaning of.
var (
	// ErrTenantViewTargetRequired means no base table was named. A
	// view with no FROM is not a view.
	ErrTenantViewTargetRequired = errors.New("drops/mysql: tenant view has no base table; call On(table)")

	// ErrTenantViewAxisRequired means no tenant column was named.
	// This is checked rather than defaulted because the statement that
	// would render without it is a view with no WHERE — one MySQL
	// accepts, and one that publishes every tenant's rows to whoever
	// holds the grant on it.
	ErrTenantViewAxisRequired = errors.New("drops/mysql: tenant view has no tenant axis; call Axis(column)")

	// ErrTenantViewAxisNotInTable means the axis column belongs to a
	// different table than the one the view selects from, so the WHERE
	// would name a column the FROM does not have. MySQL answers
	// ERROR 1054 to that, at migration time.
	ErrTenantViewAxisNotInTable = errors.New("drops/mysql: tenant view axis is not a column of the base table")

	// ErrTenantViewTenantRequired means no tenant value was given, or
	// the value given was a nil of some type. Section 1 of the tenant
	// policy block in tenant.go settles nil for the runtime paths and
	// this is the same rule: a view scoped to NULL matches no row
	// under any comparison.
	ErrTenantViewTenantRequired = errors.New("drops/mysql: tenant view has no tenant value; call ForTenant(value)")

	// ErrTenantViewDefinerRequired means no definer account was named.
	// It is not defaulted to the current user — MySQL's own default —
	// because that silently makes whichever account ran the migration
	// the owner of the boundary, and the view stops working the day
	// that account is dropped.
	ErrTenantViewDefinerRequired = errors.New("drops/mysql: tenant view has no definer; call DefinedBy(account)")

	// ErrTenantViewAmbiguousLiteral means the tenant value has no
	// rendering whose meaning is fixed. See tenantLiteral.
	ErrTenantViewAmbiguousLiteral = errors.New("drops/mysql: tenant value has no unambiguous literal form")

	// ErrTenantViewUnsupportedLiteral means the tenant value has a type
	// with no MySQL literal form drops will guess at.
	ErrTenantViewUnsupportedLiteral = errors.New("drops/mysql: unsupported tenant value type")

	// ErrTenantViewInvalidAccount means a user or host was not
	// renderable as an account.
	ErrTenantViewInvalidAccount = errors.New("drops/mysql: invalid account")

	// ErrTenantViewInvalidPrivilege means a privilege outside the set
	// this package will render was named. The value lands in a GRANT,
	// so it is checked against a fixed list rather than quoted:
	// there is no quoting for a privilege keyword.
	ErrTenantViewInvalidPrivilege = errors.New("drops/mysql: unsupported privilege")
)

// Privilege is one of the table privileges a tenant account can hold
// on a tenant view.
//
// It is a fixed set rather than a string a caller composes because a
// privilege keyword sits in a GRANT unquoted — there is no quoting for
// it — so an unchecked one is a statement the caller writes and drops
// signs.
type Privilege string

// The privileges a tenant view can carry. A view with CHECK OPTION
// accepts exactly these four; anything wider (ALL PRIVILEGES, GRANT
// OPTION, DROP) is either meaningless on a view or hands the account
// the means to remove the boundary, so this package will not render it.
const (
	PrivSelect Privilege = "SELECT"
	PrivInsert Privilege = "INSERT"
	PrivUpdate Privilege = "UPDATE"
	PrivDelete Privilege = "DELETE"
)

func validPrivilege(p Privilege) bool {
	switch p {
	case PrivSelect, PrivInsert, PrivUpdate, PrivDelete:
		return true
	}
	return false
}

// Account is a MySQL account: a user and the host pattern it may
// connect from. Both halves are part of the identity — 'acme'@'%' and
// 'acme'@'localhost' are two accounts and can hold different grants.
type Account struct {
	User string
	Host string
}

// Acct declares an account. The host is the pattern MySQL matches a
// connection against: "%" for any, "localhost" for the socket.
func Acct(user, host string) Account { return Account{User: user, Host: host} }

// validate reports why an account cannot be rendered.
//
// An empty user is refused rather than rendered as MySQL's anonymous
// account: an anonymous definer owns nothing, and an anonymous grantee
// is a pattern matching connections nobody meant to name.
func (a Account) validate() error {
	if a.User == "" {
		return fmt.Errorf("%w: user is empty", ErrTenantViewInvalidAccount)
	}
	if a.Host == "" {
		return fmt.Errorf("%w: host is empty for user %q", ErrTenantViewInvalidAccount, a.User)
	}
	if err := validateIdent("account user", a.User); err != nil {
		return fmt.Errorf("%w: %s", ErrTenantViewInvalidAccount, err)
	}
	if err := validateIdent("account host", a.Host); err != nil {
		return fmt.Errorf("%w: %s", ErrTenantViewInvalidAccount, err)
	}
	return nil
}

// write renders the account as MySQL's own SHOW GRANTS and
// SHOW CREATE VIEW spell it: `user`@`host`, backtick-quoted on both
// halves. The single-quoted form is equally valid and this package
// picks the one the server echoes back, so a rendered statement and
// the server's account of it compare by eye.
func (a Account) write(b *drops.Builder) {
	b.WriteIdent(a.User)
	b.WriteByte('@')
	b.WriteIdent(a.Host)
}

// TenantView declares the definer-rights view that scopes one tenant,
// and the grant that lets that tenant's account reach it.
//
// It renders DDL and nothing else. Read the file comment before
// reaching for it: the view and the grant are not the boundary on
// their own, and what makes them one is a property of the server's
// privilege state that drops cannot render, execute or check.
//
// A zero TenantView is not usable; start from [NewTenantView]. The
// builder methods return the receiver so a declaration reads as one
// expression, and a bad argument is recorded on the view rather than
// returned, surfacing at [TenantView.Err] or as a broken statement at
// render time.
type TenantView struct {
	name string

	database string
	table    *Table
	axis     *Column

	tenant    any
	hasTenant bool

	definer    Account
	hasDefiner bool

	err error
}

// NewTenantView declares a view by name.
//
// The name is validated like every other identifier this package
// writes and a bad one panics at startup, which is where a schema
// declaration wants to fail: a view name that cannot be rendered is
// not something to discover when a migration runs against production.
func NewTenantView(name string) *TenantView {
	mustIdent("view", name)
	return &TenantView{name: name}
}

// On names the base table the view selects from. The view is created
// in the table's database, so a table declared with [NewDatabaseTable]
// gives a qualified view and one declared with [NewTable] gives a view
// in whichever database the connection is using.
func (v *TenantView) On(t *Table) *TenantView {
	v.table = t
	v.database = t.Database()
	return v
}

// Axis names the tenant column the view filters on. It is the same
// column a table hands [TenantFilter] or an entity hands
// ScopeByTenant, and naming the same one in both places is the point:
// the predicate and the view then describe the same axis.
func (v *TenantView) Axis(col ColRef) *TenantView {
	if col == nil {
		return v
	}
	v.axis = col.col().key()
	return v
}

// ForTenant fixes the tenant this view is scoped to.
//
// The value is rendered into the stored view body as a literal,
// because a view body cannot carry a placeholder — MySQL answers
// ERROR 1351 to one. A value with no unambiguous literal form is
// refused here rather than escaped; see tenantLiteral for which and
// why.
func (v *TenantView) ForTenant(value any) *TenantView {
	// The pairing is TenantFrom's, and for its reason: isNilTenant sees
	// a nil held inside a non-nil interface, and an untyped nil is not
	// that shape — reflect reports it as Invalid. Both are no tenant.
	if value == nil || isNilTenant(value) {
		v.tenant, v.hasTenant = nil, false
		v.fail(fmt.Errorf("%w: a nil of any type is no tenant", ErrTenantViewTenantRequired))
		return v
	}
	if _, err := tenantLiteral(value); err != nil {
		v.tenant, v.hasTenant = nil, false
		v.fail(err)
		return v
	}
	v.tenant, v.hasTenant = value, true
	return v
}

// DefinedBy names the account the view runs as.
//
// This account is the one that must hold privileges on the base table:
// it is what the tenant account borrows by reading through the view.
// It has no default. MySQL's own default is the account that ran the
// CREATE, which quietly makes a migration runner the owner of the
// boundary and breaks every view it created the day that account is
// dropped.
func (v *TenantView) DefinedBy(a Account) *TenantView {
	if err := a.validate(); err != nil {
		v.fail(err)
		return v
	}
	v.definer, v.hasDefiner = a, true
	return v
}

// fail records the first error. The first is kept because it is the
// one a reader can act on: a later complaint is usually a consequence.
func (v *TenantView) fail(err error) {
	if v.err == nil {
		v.err = err
	}
}

// Err reports the first thing wrong with the declaration, or nil.
//
// Check it. The emitters render a broken statement rather than a
// silent one, but a migration that reads Err at declaration time fails
// before it has run anything.
func (v *TenantView) Err() error {
	if v.err != nil {
		return v.err
	}
	return v.validate()
}

// validate reports the first reason this view cannot be rendered.
func (v *TenantView) validate() error {
	if v.err != nil {
		return v.err
	}
	if v.table == nil {
		return ErrTenantViewTargetRequired
	}
	if v.axis == nil {
		return ErrTenantViewAxisRequired
	}
	// The axis must be a column of the table the view selects FROM,
	// and this is the refusal the column-key census records for both
	// sites in this file.
	//
	// The view's WHERE renders the axis as a BARE name, so a handle for
	// the same-named column of some other table object is a stranger to
	// Column.key and the same column to the server. Asking membership
	// of the base table's own declared columns is what makes that
	// impossible: the bare name that goes out is always one the FROM
	// has. Comparing table identity instead would leave a column with
	// no table at all — one built by Varchar and never passed to Add —
	// rendering its name against a table that may not have it.
	if !v.tableOwnsAxis() {
		return fmt.Errorf("%w: axis %q is not a column of %q",
			ErrTenantViewAxisNotInTable, v.axis.Name(), v.table.Name())
	}
	if !v.hasTenant {
		return ErrTenantViewTenantRequired
	}
	if !v.hasDefiner {
		return ErrTenantViewDefinerRequired
	}
	return nil
}

// tableOwnsAxis reports whether the axis is one of the base table's
// own declared columns.
//
// Both sides are collapsed onto their declared origin first, so a
// handle taken off an alias of the same table answers yes: aliasing is
// a query-scope rename and the view body has no query around it to
// rename anything.
func (v *TenantView) tableOwnsAxis() bool {
	for _, c := range v.table.Columns() {
		if c.key() == v.axis {
			return true
		}
	}
	return false
}

// tenantLiteral renders a tenant value as a MySQL literal whose
// meaning does not depend on anything drops cannot see.
//
// This is the sharpest edge in the file, and it is why the function
// refuses rather than escapes.
//
// A view body cannot carry a placeholder, so the tenant value is TEXT
// inside stored DDL. drops renders that text; an operator applies it
// later, on a server whose sql_mode drops never saw. A single quote is
// safe to render, because doubling it closes the literal correctly in
// every mode. A BACKSLASH is not. Under the default sql_mode a
// backslash introduces an escape and must be doubled; under
// NO_BACKSLASH_ESCAPES it is an ordinary character and must not be.
//
// Measured, on a live MySQL 8.0.46: the identical statement text
// "... WHERE tenant = 'a\\b'" installed under the default mode stored
// a view selecting the row whose tenant is a\b, and installed under
// NO_BACKSLASH_ESCAPES stored one selecting the row whose tenant is
// a\\b. Same text, different tenant. The package's own quoteLiteral
// doubles the backslash and documents that it corrupts the text but
// never the statement around it; that trade is right for an enum
// member or a column comment and wrong here, because here the
// corrupted text is the predicate that IS the boundary, and the
// failure is a view scoped to a tenant nobody named — which permits
// silently rather than refusing loudly.
//
// Control characters are refused on a second ground: this DDL is meant
// to be read by the operator who applies it, and a NUL or a newline
// inside the literal is invisible in that reading.
//
// []byte and []rune are refused for the reason section 1 of the tenant
// policy block gives. They convert onto a string and back losing
// nothing, so nothing here would catch them, but sameTenant treats a
// bytes-vs-text pair as the type confusion it is and refuses it at
// runtime. A view declared for []byte("acme") would be a view no ctx
// tenant could ever match.
//
// Floats are refused because no float has one spelling, and a tenant
// axis addressed by a value that round-trips through a decimal
// rendering is not an axis. Bool likewise: MySQL has no BOOLEAN, and a
// tenant identified by true is a schema mistake worth surfacing.
func tenantLiteral(v any) (string, error) {
	switch x := v.(type) {
	case string:
		if err := literalIsUnambiguous(x); err != nil {
			return "", err
		}
		return "'" + strings.ReplaceAll(x, "'", "''") + "'", nil
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
	default:
		return "", fmt.Errorf("%w: %T", ErrTenantViewUnsupportedLiteral, v)
	}
}

// literalIsUnambiguous reports why a string has no fixed rendering.
func literalIsUnambiguous(s string) error {
	if strings.ContainsRune(s, '\\') {
		return fmt.Errorf("%w: %q contains a backslash, whose escaping depends on the "+
			"sql_mode in force where the statement is applied, and drops does not see it",
			ErrTenantViewAmbiguousLiteral, s)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: %q contains a control character, which is invisible "+
				"to the operator who reads this DDL before applying it",
				ErrTenantViewAmbiguousLiteral, s)
		}
	}
	return nil
}

// ----------------------------------------------------------------------
// DDL
// ----------------------------------------------------------------------

// CreateTenantView renders the CREATE VIEW.
//
// Like [CreateTable] it renders rather than returning an error, and an
// incomplete declaration emits a marked comment that breaks the
// statement, so a stray call in a migration fails at exec instead of
// installing something. The choice is sharper here than for a table: a
// CREATE VIEW that lost its WHERE is a statement MySQL ACCEPTS, and it
// would publish every tenant's rows to whoever holds the grant on the
// view. Breaking the statement on purpose is the only way for that to
// be visible. Check [TenantView.Err] where a definite error is wanted.
func CreateTenantView(v *TenantView) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { writeCreateTenantView(b, v, false) })
}

// CreateOrReplaceTenantView is the OR REPLACE variant.
//
// Prefer it in a migration that is the single source of a view's
// definition: re-running it converges, where a plain CREATE fails on
// the second run and leaves whatever the first run installed. Note
// what it does NOT do — replacing the view does not touch the grants
// on it, which live in the privilege tables and survive.
func CreateOrReplaceTenantView(v *TenantView) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { writeCreateTenantView(b, v, true) })
}

func writeCreateTenantView(b *drops.Builder, v *TenantView, orReplace bool) {
	b.WriteString("CREATE ")
	if orReplace {
		b.WriteString("OR REPLACE ")
	}
	if err := v.validate(); err != nil {
		// Break the statement rather than install a view whose scope
		// drops cannot state. See CreateTenantView.
		b.WriteString("/* drops/mysql: " + err.Error() + " */")
		return
	}
	lit, err := tenantLiteral(v.tenant)
	if err != nil {
		b.WriteString("/* drops/mysql: " + err.Error() + " */")
		return
	}

	b.WriteString("DEFINER = ")
	v.definer.write(b)
	// SQL SECURITY DEFINER is the whole mechanism: it is what lets the
	// tenant account read the base table through the view while holding
	// no privilege on it. The INVOKER alternative would require the
	// tenant account to hold that privilege itself, which is precisely
	// the grant whose absence is the boundary.
	b.WriteString(" SQL SECURITY DEFINER VIEW ")
	writeTenantViewName(b, v)
	b.WriteString(" AS SELECT ")
	for i, c := range v.table.Columns() {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteIdent(c.Name())
	}
	b.WriteString(" FROM ")
	writeTenantViewTable(b, v)
	b.WriteString(" WHERE ")
	b.WriteIdent(v.axis.Name())
	b.WriteString(" = ")
	b.WriteString(lit)
	// CASCADED rather than LOCAL, and not a knob. LOCAL checks this
	// view's own condition and not those of the views underneath it,
	// so a tenant view built over another view would accept a write
	// the lower view excludes. CASCADED is MySQL's default when the
	// word is omitted; it is written out because a reader of this DDL
	// should not have to know that.
	b.WriteString(" WITH CASCADED CHECK OPTION")
}

// writeTenantViewName renders the view's own qualified name.
func writeTenantViewName(b *drops.Builder, v *TenantView) {
	if v.database != "" {
		b.WriteIdent(v.database)
		b.WriteByte('.')
	}
	b.WriteIdent(v.name)
}

// writeTenantViewTable renders the base table's qualified name.
//
// It writes the name directly rather than going through the table's
// own writeFrom because that renders an alias when one is set, and an
// aliased handle in a stored view body would name the alias in a
// context that has no query around it.
func writeTenantViewTable(b *drops.Builder, v *TenantView) {
	if db := v.table.Database(); db != "" {
		b.WriteIdent(db)
		b.WriteByte('.')
	}
	b.WriteIdent(v.table.Name())
}

// DropTenantView renders DROP VIEW.
func DropTenantView(v *TenantView) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { writeDropTenantView(b, v, false) })
}

// DropTenantViewIfExists is the IF EXISTS variant.
func DropTenantViewIfExists(v *TenantView) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { writeDropTenantView(b, v, true) })
}

// writeDropTenantView renders DROP VIEW.
//
// It needs the name alone, so it does not run validate: dropping a
// view whose tenant value was never filled in is exactly what rolling
// back a half-written migration wants to do.
func writeDropTenantView(b *drops.Builder, v *TenantView, ifExists bool) {
	b.WriteString("DROP VIEW ")
	if ifExists {
		b.WriteString("IF EXISTS ")
	}
	writeTenantViewName(b, v)
}

// GrantTenantView renders the GRANT that lets one account reach the
// view.
//
// Naming no privilege grants SELECT: a caller who forgot the argument
// gets a reader rather than a writer, which is the default that cannot
// widen anything.
//
// Read the file comment for what this statement is and is not. It
// grants ON THE VIEW. What makes the pair a boundary is that the same
// account holds nothing on the base table, and that is a property of
// the server's whole privilege state — not of this statement, and not
// of anything drops can render. Nothing in this package emits a REVOKE
// to establish it: REVOKE fails with ERROR 1147 when there is no grant
// to remove, which is the state a correct deployment is already in,
// and the REVOKE IF EXISTS that tolerates it does not exist in
// MariaDB.
func GrantTenantView(v *TenantView, to Account, privs ...Privilege) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { writeGrantTenantView(b, v, to, privs) })
}

func writeGrantTenantView(b *drops.Builder, v *TenantView, to Account, privs []Privilege) {
	b.WriteString("GRANT ")
	// A grant on a view whose declaration was refused would name an
	// object that does not exist — or worse, one left behind by an
	// earlier run with a different scope.
	if err := v.validate(); err != nil {
		b.WriteString("/* drops/mysql: " + err.Error() + " */")
		return
	}
	if err := to.validate(); err != nil {
		b.WriteString("/* drops/mysql: " + err.Error() + " */")
		return
	}
	if len(privs) == 0 {
		privs = []Privilege{PrivSelect}
	}
	for _, p := range privs {
		if !validPrivilege(p) {
			b.WriteString("/* drops/mysql: " +
				fmt.Errorf("%w: %q", ErrTenantViewInvalidPrivilege, string(p)).Error() + " */")
			return
		}
	}
	for i, p := range privs {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(string(p))
	}
	b.WriteString(" ON ")
	writeTenantViewName(b, v)
	b.WriteString(" TO ")
	to.write(b)
}
