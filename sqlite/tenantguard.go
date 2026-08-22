package sqlite

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/bernardoforcillo/drops"
)

// SQLite tenant isolation: what the engine can enforce, what drops can
// declare, and the one thing neither can do.
//
// SQLite has no users, no roles, no grants and no row-level security.
// A process that can open the file can read every byte in it, and this
// package's doc has said so from the beginning. What it also said, and
// what was wrong in the direction that matters, is that the
// application predicates were therefore the WHOLE of what there is on
// this dialect.
//
// They are not. SQLite has triggers, and a trigger is inside the
// database. It runs for every statement that reaches the table,
// whoever wrote it, through whatever connection, in whatever process,
// including SQL that drops did not build and never saw — a raw
// [DB.Exec], a [drops.Raw] fragment, a hand-run migration, the
// `sqlite3` shell. That is the exact set the predicates cannot reach,
// and it is where every silent wrong-tenant write this phase found
// actually happened.
//
// # What this file declares
//
// A [TenantGuard] renders CREATE TRIGGER statements from a table's
// declared tenant axis. Three guards, each answering a write that a
// correctly-scoped statement can still make:
//
//   - the axis may not be NULL. That is the "row belonged to nobody"
//     failure section 1 of the tenant policy block names — reachable
//     through Unscoped, through a column list that dropped the axis
//     binding, and through any INSERT written by hand.
//   - the axis may not CHANGE. An UPDATE that assigns the axis moves a
//     row between tenants; the package refuses one it builds, and this
//     refuses one it did not.
//   - the axis must AGREE with the axis of the row a foreign key names
//     — see [TenantGuard.MatchingParent]. This is the wrong-tenant
//     write no predicate can catch, because every statement involved
//     is correctly scoped: the tenant on the ctx is stamped onto the
//     row, the WHERE clause carries it, and the FK points at another
//     tenant's row anyway.
//
// And, for the deployment where the database file IS the tenant, a
// fourth: [TenantGuard.PinnedTo] fixes the axis to one literal value.
//
// # Why a trigger and not a CHECK constraint
//
// A CHECK is the shorter answer and it cannot express any of these.
// Measured on SQLite 3.53.3, in-process through modernc.org/sqlite —
// integration/sqlite_tenantguard_test.go runs each of these:
//
//   - a CHECK cannot reference another table, so the parent-agreement
//     guard has no CHECK form at all;
//   - a CHECK sees one row, never a pair, so "the axis may not change"
//     has no CHECK form either: there is no OLD;
//   - PRAGMA ignore_check_constraints = ON turns every CHECK in the
//     database off for the connection that says it, and it is an
//     ordinary statement any code on that connection may run. There is
//     no pragma that disables triggers.
//
// The fourth reason is the usual one and it is the weakest: SQLite's
// documented ALTER TABLE cannot add a constraint, so putting a CHECK
// on a table that already exists means the twelve-step rebuild. Weak
// because it turns out not to be quite true — ALTER TABLE <t> ADD
// CONSTRAINT <name> CHECK (<expr>) is accepted here, is stored in the
// schema text, is enforced, survives a reopen and passes
// integrity_check, while the UNIQUE and FOREIGN KEY forms of the same
// statement are syntax errors. That form is outside the grammar SQLite
// documents, so this package measures it and does not emit it. The
// three reasons above do not depend on it.
//
// # What a trigger is, and what it is not
//
// It is a guard, not a boundary, and the difference is the word
// "principal". PostgreSQL's row-level security binds rows to a ROLE,
// so a policy holds against code that authenticates as that role and
// wants to see more. A ClickHouse row policy binds to a user. A MySQL
// definer-rights view binds to an account. SQLite has none of those
// to bind to: whoever can write the file can DROP TRIGGER, or set
// PRAGMA writable_schema = ON and delete the row out of sqlite_master.
// Both measured.
//
// So a tenant guard holds against MISTAKES and not against an
// adversary — against the raw statement, the wrong join, the backfill
// script, the FK that points at the wrong row. That is worth having,
// because that is what actually leaks; it is not worth calling a
// boundary, and this package does not.
//
// # The boundary this dialect does have: one file per tenant
//
// SQLite's isolation primitive is the FILE. A database per tenant is a
// real boundary and the only one here, because it is enforced by the
// filesystem and the process's own open handles rather than by
// anything inside the database: a connection to acme.db cannot name a
// row in globex.db. It is what libSQL and Turso are built on, and it
// is a deployment decision rather than a library feature — drops has
// no more business owning it than it has owning where the files live.
//
// What it costs, plainly, because these costs are why it is not the
// default:
//
//   - every migration runs N times, once per file, and a run that
//     fails halfway leaves the fleet on two schema versions. drops
//     helps here and this is the part it genuinely owns: [Migrator]
//     tracks applied migrations per database, [Snapshot] and [Diff]
//     derive the statements, and [Drift] reads a live file and reports
//     where it stopped matching. Point them at each file in turn.
//   - a cross-tenant question — "how many documents does the whole
//     fleet hold" — stops being a query. It becomes a loop over files,
//     or ATTACH plus a hand-written UNION ALL naming each one, with
//     SQLITE_MAX_ATTACHED (10 by default) capping how many at once.
//   - connection management becomes per tenant. A pool per file, N
//     files open, N sets of page caches; or open-on-demand and pay the
//     open per request.
//   - a tenant lookup that used to be a WHERE clause becomes routing:
//     something has to map the request to a filename before any SQL
//     runs, and that something is now the boundary. Get it wrong and
//     the isolation is perfect and pointed at the wrong tenant.
//
// [TenantGuard.PinnedTo] is the half of that arrangement drops can
// check: in a per-tenant file, every row of a scoped table must carry
// that file's tenant, so the guard pins the axis to a literal and the
// file refuses a row belonging to anyone else. It turns "we run one
// file per tenant" from a claim about the deployment into a statement
// the database enforces — which matters most on the day a restore, a
// merge or a copied file puts two tenants' rows in one place.
//
// # Why there is no request-scoped guard, and must not be
//
// The obvious wish is a trigger that compares the row's axis against
// the tenant of the CURRENT REQUEST, the way pg's RLS policy compares
// against current_setting. It cannot be built here, and the near-miss
// is worse than nothing. All of this is measured:
//
//   - SQLite has no session variables and no set_config.
//   - a trigger body cannot carry a placeholder: CREATE TRIGGER with a
//     ? in its WHEN clause is refused with "trigger cannot use
//     variables". (MySQL refuses the same shape in a view body with
//     ERROR 1351. Two dialects, one answer.)
//   - the per-connection state SQLite does have is the TEMP schema, and
//     a persistent trigger cannot see it: CREATE TRIGGER on a main
//     table whose body selects from temp is refused outright with
//     "trigger ... cannot reference objects in database temp".
//   - a TEMP TRIGGER can. Install a temp table holding the request's
//     tenant and a temp trigger reading it, and the guard works
//     exactly as wanted — on that connection.
//
// And that last word is the whole problem. A temp trigger lives on one
// connection. database/sql hands out connections from a pool, so the
// write that the request believes is guarded runs on whichever
// connection was free, and on every connection but the one that
// installed it there is NO TRIGGER AT ALL. Measured: with the temp
// trigger installed on a pinned [sql.Conn], the same INSERT was
// refused on that connection and accepted six times in a row through
// the pool, no error anywhere, the rows landing under the tenant the
// guard existed to refuse.
//
// That is fail-OPEN, and it is silent, and it would be shipped by a
// library under a name that says "guard". A caller who genuinely wants
// it can pin a connection and install it; drops will not hand it out.
//
// # The sharp edges of the guards it does hand out
//
// RAISE(ABORT) undoes the STATEMENT, not the transaction. Inside an
// explicit transaction the refused statement rolls back and everything
// already written stays, so a caller who ignores the error and commits
// keeps the rest of its work — measured. RAISE(ROLLBACK) is the
// alternative and this package does not use it: it would discard
// unrelated work the caller had done in the same transaction, and the
// row the guard refused is not written either way.
//
// INSERT OR REPLACE is not covered on the side that destroys rows, and
// this is the sharpest edge in the file. REPLACE resolves a collision
// by DELETING the rows in the way, and SQLite fires delete triggers
// for that implicit delete only when PRAGMA recursive_triggers is on —
// it is OFF by default. Measured: with a BEFORE DELETE trigger that
// aborts unconditionally, INSERT OR REPLACE destroyed the other
// tenant's row and reported success; with recursive_triggers = ON the
// same statement was refused. The row's foreign-key ON DELETE CASCADE
// actions run either way, so the damage is not confined to the one
// table — also measured. The INSERT half of a REPLACE does fire BEFORE
// INSERT triggers, so the row it writes is still guarded; the row it
// destroyed is not. drops refuses OR REPLACE on a scoped table for
// this reason ([ErrReplaceScoped]) — the guard is what remains when
// the statement was not written through drops.
//
// A trigger body's names are not resolved when it is created. A guard
// naming a parent table that does not exist is accepted by CREATE
// TRIGGER and fails when it fires — with "no such table", refusing the
// write. Fail-closed, which is the right direction, but it means a
// guard applied against a schema it does not match takes writes to
// that table down rather than quietly doing nothing.
//
// A table rebuild takes its triggers with it. DROP TABLE drops every
// trigger on that table, and SQLite's twelve-step rebuild — the way a
// column is changed here — is a DROP. drops' own [Diff] replays the
// triggers it found in the snapshot after a rebuild it emits; a
// rebuild done by hand or by another tool does not, and reports
// success.
//
// Nothing here reaches Diff, Snapshot or Push as a DECLARATION. A
// tenant guard is a trigger, drops/sqlite's schema tooling models
// tables, and triggers are preserved rather than diffed on purpose
// (diff.go says why). Put these statements in a migration.

// Errors a [TenantGuard] can carry. Each names a declaration that
// would otherwise render as a trigger SQLite accepts and drops cannot
// state the meaning of.
var (
	// ErrTenantGuardTargetRequired means no table was named. A trigger
	// with no table is not a trigger.
	ErrTenantGuardTargetRequired = errors.New("drops/sqlite: tenant guard has no table; call On(table)")

	// ErrTenantGuardAxisRequired means no tenant column was named, and
	// the table did not declare one either. It is checked rather than
	// defaulted because the WHEN clause that would render without it
	// names no column at all — and a trigger whose condition is
	// missing is one whose meaning drops cannot state.
	ErrTenantGuardAxisRequired = errors.New("drops/sqlite: tenant guard has no tenant axis; call Axis(column) or ScopeWritesByTenant on the table")

	// ErrTenantGuardAxisNotInTable means the axis column belongs to a
	// different table than the one the trigger is on, so NEW.<axis>
	// would name a column the row does not have. SQLite accepts that
	// at CREATE TRIGGER time and answers "no such column: NEW.<axis>"
	// when the trigger fires — which refuses every write to the table.
	ErrTenantGuardAxisNotInTable = errors.New("drops/sqlite: tenant guard axis is not a column of the guarded table")

	// ErrTenantGuardParentNotInTable means a parent link named columns
	// from the wrong tables: the local key must belong to the guarded
	// table, and the parent key and parent axis must belong to one
	// table between them.
	ErrTenantGuardParentNotInTable = errors.New("drops/sqlite: tenant guard parent link names a column of another table")

	// ErrTenantGuardTenantRequired means PinnedTo was given no value,
	// or a nil of some type. Section 1 of the tenant policy block in
	// tenant.go settles nil for the runtime paths and this is the same
	// rule: a guard pinned to NULL refuses every row, including the
	// ones it exists to admit.
	ErrTenantGuardTenantRequired = errors.New("drops/sqlite: tenant guard has no tenant value; call PinnedTo(value)")

	// ErrTenantGuardAmbiguousLiteral means the pinned value has no
	// rendering an operator can read. See tenantGuardLiteral.
	ErrTenantGuardAmbiguousLiteral = errors.New("drops/sqlite: tenant value has no unambiguous literal form")

	// ErrTenantGuardUnsupportedLiteral means the pinned value has a
	// type with no SQLite literal form drops will guess at.
	ErrTenantGuardUnsupportedLiteral = errors.New("drops/sqlite: unsupported tenant value type")
)

// TenantGuard declares the triggers that hold a table's tenant axis
// against SQL drops did not build.
//
// Read the file comment before reaching for it. These triggers are a
// guard inside the database and not an isolation boundary: SQLite has
// no principal to bind rows to, and whoever can write the file can
// remove them.
//
// A zero TenantGuard is not usable; start from [NewTenantGuard] or
// [TenantGuardFor]. The builder methods return the receiver so a
// declaration reads as one expression, and a bad argument is recorded
// on the guard rather than returned, surfacing at [TenantGuard.Err] or
// as a broken statement at render time.
type TenantGuard struct {
	name  string
	table *Table
	axis  *Column

	tenant    any
	hasTenant bool

	parents []guardParent

	err error
}

// guardParent is one "this row's tenant must equal that row's tenant"
// link: a foreign key on the guarded table, and the key and axis of
// the table it points at.
type guardParent struct {
	local      *Column
	parentKey  *Column
	parentAxis *Column
}

// NewTenantGuard declares a guard under a name.
//
// The name prefixes every trigger the guard renders, and the suffixes
// are fixed, so the statements a migration applies and the rows in
// sqlite_master can be read against each other by eye. A trigger name
// is unique per DATABASE in SQLite rather than per table, which is why
// the name is asked for rather than derived from the table alone —
// though [TenantGuardFor] does derive one, and derives it from the
// table name for exactly that uniqueness.
//
// The name is validated like every other identifier this package
// writes and a bad one panics at startup, which is where a schema
// declaration wants to fail.
func NewTenantGuard(name string) *TenantGuard {
	mustIdent("trigger", name)
	return &TenantGuard{name: name}
}

// TenantGuardFor derives a guard from what the table already declares:
// the write axis set by [Table.ScopeWritesByTenant].
//
// This is the spelling to prefer. The axis a guard pins is then the
// same handle the predicates filter on and the same one an INSERT
// stamps, by construction rather than by two declarations agreeing —
// and a guard describing a different column from the one the package
// scopes by is precisely the mistake that would leave both looking
// correct.
//
// A table with no declared write axis yields a guard that reports
// [ErrTenantGuardAxisRequired] rather than one that guards nothing.
func TenantGuardFor(t *Table) *TenantGuard {
	g := NewTenantGuard(t.Name() + "_tenantGuard")
	g.On(t)
	if axis := t.tenantAxis(); axis != nil {
		g.axis = axis
	}
	return g
}

// On names the table the triggers are created on.
func (g *TenantGuard) On(t *Table) *TenantGuard {
	g.table = t
	return g
}

// Axis names the tenant column the guards are about.
//
// It is the same column a table hands [TenantFilter] or an entity
// hands ScopeByTenant, and naming the same one in both places is the
// point: the predicate and the trigger then describe the same axis.
func (g *TenantGuard) Axis(col ColRef) *TenantGuard {
	if col == nil {
		return g
	}
	// Collapsed onto the declared column, so a handle taken off an
	// alias of the same table names the same axis: aliasing is a
	// query-scope rename and a stored trigger body has no query around
	// it to rename anything.
	g.axis = col.col().key()
	return g
}

// PinnedTo fixes the axis to one tenant, for the deployment where the
// database file IS the tenant.
//
// The pin subsumes both of the axis triggers rather than adding to
// them: "IS NOT 'acme'" is true for NULL and true for any other
// tenant, so a pinned guard renders two triggers where an unpinned one
// renders two, and each of them says more.
//
// The value is rendered into the stored trigger body as a literal,
// because a trigger body cannot carry a placeholder — SQLite answers
// "trigger cannot use variables" to one. A value with no unambiguous
// literal form is refused here rather than escaped; see
// tenantGuardLiteral for which and why.
func (g *TenantGuard) PinnedTo(value any) *TenantGuard {
	// The pairing is TenantFrom's, and for its reason: isNilTenant sees
	// a nil held inside a non-nil interface, and an untyped nil is not
	// that shape — reflect reports it as Invalid. Both are no tenant.
	if value == nil || isNilTenant(value) {
		g.tenant, g.hasTenant = nil, false
		g.fail(fmt.Errorf("%w: a nil of any type is no tenant", ErrTenantGuardTenantRequired))
		return g
	}
	if _, err := tenantGuardLiteral(value); err != nil {
		g.tenant, g.hasTenant = nil, false
		g.fail(err)
		return g
	}
	g.tenant, g.hasTenant = value, true
	return g
}

// MatchingParent requires that a row's tenant equal the tenant of the
// row its foreign key names.
//
// local is a column of the guarded table; parentKey and parentAxis are
// the key it points at and that table's own tenant axis.
//
//	sqlite.TenantGuardFor(posts).
//	    MatchingParent(postAuthorID, userID, userTenantID)
//
// This is the guard the predicates have no form of. A statement that
// inserts a post for tenant "acme" whose authorId names a user of
// tenant "globex" is correctly scoped at every step — the ctx tenant
// is acme, the stamp writes acme, any WHERE clause carries acme — and
// it still builds a row that reads one tenant's data through another
// tenant's row. Only something that looks at BOTH rows can refuse it,
// and in this dialect that is a trigger.
//
// A row whose parent does not exist is refused too: the subquery
// returns NULL, NULL IS NOT <anything> is true, and the guard aborts.
// That is deliberate and it is the fail-closed direction — a row
// pointing at nothing has no tenant to agree with — but it does mean
// the guard behaves as a foreign key on the way in. Declare the FK as
// well; this is not a replacement for one, because it says nothing
// about what happens when the parent is deleted.
//
// Note what maintains the agreement afterwards: the parent's OWN axis
// must not move. That is the second trigger this guard renders on the
// parent's table, when the parent table declares a guard of its own.
// A parent renamed under ON UPDATE CASCADE does fire the child's
// update trigger, measured, so the cascade is refused rather than
// silently rewriting the children.
func (g *TenantGuard) MatchingParent(local, parentKey, parentAxis ColRef) *TenantGuard {
	if local == nil || parentKey == nil || parentAxis == nil {
		g.fail(fmt.Errorf("%w: a parent link needs a local key, a parent key and a parent axis",
			ErrTenantGuardParentNotInTable))
		return g
	}
	g.parents = append(g.parents, guardParent{
		local:      local.col().key(),
		parentKey:  parentKey.col().key(),
		parentAxis: parentAxis.col().key(),
	})
	return g
}

// fail records the first error. The first is kept because it is the
// one a reader can act on: a later complaint is usually a consequence.
func (g *TenantGuard) fail(err error) {
	if g.err == nil {
		g.err = err
	}
}

// Err reports the first thing wrong with the declaration, or nil.
//
// Check it. The emitters render a broken statement rather than a
// silent one, but a migration that reads Err at declaration time fails
// before it has run anything.
func (g *TenantGuard) Err() error {
	if g.err != nil {
		return g.err
	}
	return g.validate()
}

// validate reports the first reason this guard cannot be rendered.
func (g *TenantGuard) validate() error {
	if g.err != nil {
		return g.err
	}
	if g.table == nil {
		return ErrTenantGuardTargetRequired
	}
	if g.axis == nil {
		return ErrTenantGuardAxisRequired
	}
	// The axis must be a column of the table the trigger is ON, and
	// this is the refusal the column-key census records for this file.
	//
	// The WHEN clause renders the axis as NEW.<name>, so a handle for
	// the same-named column of some other table object is a stranger to
	// Column.key and the same column to the engine. Asking membership
	// of the guarded table's own declared columns is what makes that
	// impossible: the name that goes out is always one the row has.
	// Comparing table identity instead would leave a column with no
	// table at all — one built by Text and never passed to Add —
	// rendering its name against a table that may not have it.
	if !tableOwnsColumn(g.table, g.axis) {
		return fmt.Errorf("%w: axis %q is not a column of %q",
			ErrTenantGuardAxisNotInTable, g.axis.Name(), g.table.Name())
	}
	if g.hasTenant {
		if _, err := tenantGuardLiteral(g.tenant); err != nil {
			return err
		}
	}
	for _, p := range g.parents {
		if !tableOwnsColumn(g.table, p.local) {
			return fmt.Errorf("%w: local key %q is not a column of %q",
				ErrTenantGuardParentNotInTable, p.local.Name(), g.table.Name())
		}
		parent := p.parentKey.Table()
		if parent == nil || !tableOwnsColumn(parent, p.parentAxis) {
			return fmt.Errorf("%w: parent key %s and parent axis %s are not one table",
				ErrTenantGuardParentNotInTable, columnPath(p.parentKey), columnPath(p.parentAxis))
		}
	}
	return nil
}

// tableOwnsColumn reports whether c is one of t's declared columns.
//
// Both sides are collapsed onto their declared origin first, so a
// handle taken off an alias of the same table answers yes.
func tableOwnsColumn(t *Table, c *Column) bool {
	if t == nil || c == nil {
		return false
	}
	for _, own := range t.Columns() {
		if own.key() == c.key() {
			return true
		}
	}
	return false
}

// tenantGuardLiteral renders a tenant value as a SQLite literal.
//
// The value is TEXT inside stored DDL: drops renders it, an operator
// applies it later, and from then on it is a trigger body nobody
// reads again. So the rendering has to mean one thing.
//
// A single quote is safe to render, and this is where SQLite is
// simpler than its siblings: string literals have no escape
// character at all, so doubling the quote is the whole of the rule
// and it holds in every configuration. drops/mysql has to REFUSE a
// backslash in the same position, because whether it escapes depends
// on the sql_mode in force where the statement is applied. SQLite has
// no such mode and no such ambiguity.
//
// Control characters are refused on the other ground mysql refuses
// them: this DDL is meant to be read by the operator who applies it,
// and a NUL or a newline inside the literal is invisible in that
// reading.
//
// []byte and []rune are refused for the reason section 1 of the
// tenant policy block gives. They convert onto a string and back
// losing nothing, so nothing here would catch them, but sameTenant
// treats a bytes-vs-text pair as the type confusion it is and refuses
// it at runtime. A guard pinned to []byte("acme") would be a guard no
// ctx tenant could ever satisfy.
//
// Floats are refused because no float has one spelling, and a tenant
// axis addressed by a value that round-trips through a decimal
// rendering is not an axis. Bool likewise: SQLite has no boolean type,
// and a tenant identified by true is a schema mistake worth surfacing.
func tenantGuardLiteral(v any) (string, error) {
	switch x := v.(type) {
	case string:
		if err := guardLiteralIsUnambiguous(x); err != nil {
			return "", err
		}
		return sqlTextLiteral(x), nil
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
		return "", fmt.Errorf("%w: %T", ErrTenantGuardUnsupportedLiteral, v)
	}
}

// guardLiteralIsUnambiguous reports why a string has no fixed
// rendering.
func guardLiteralIsUnambiguous(s string) error {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: %q contains a control character, which is invisible "+
				"to the operator who reads this DDL before applying it",
				ErrTenantGuardAmbiguousLiteral, s)
		}
	}
	return nil
}

// sqlTextLiteral renders a Go string as a SQLite text literal.
//
// Doubling the quote is complete here — SQLite recognises no escape
// character inside a string literal — which is why this does not have
// mysql's refusal of a backslash beside it.
func sqlTextLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// guardPath renders a column as "table"."column" for a message stored
// inside a trigger body.
//
// It quotes because the message is read next to the SQL it was raised
// by, and an unquoted name with a space or a dot in it reads as two
// things.
func guardPath(c *Column) string {
	if c == nil {
		return `"?"`
	}
	name := `"` + strings.ReplaceAll(c.Name(), `"`, `""`) + `"`
	if t := c.Table(); t != nil {
		return `"` + strings.ReplaceAll(t.Name(), `"`, `""`) + `".` + name
	}
	return name
}

// ----------------------------------------------------------------------
// DDL
// ----------------------------------------------------------------------

// CreateTenantGuard renders the CREATE TRIGGER statements.
//
// One guard is several triggers, so this returns a statement list
// rather than a statement: SQLite's own grammar has one trigger per
// CREATE, and drops renders one statement per [drops.Expression].
// Apply them in order.
//
// Like [CreateTable] each statement renders rather than returning an
// error, and an incomplete declaration emits a marked comment that
// breaks the statement, so a stray call in a migration fails at exec
// instead of installing something. The choice is sharper here than for
// a table: a CREATE TRIGGER whose WHEN clause lost a conjunct is a
// statement SQLite ACCEPTS, and the trigger it installs looks exactly
// like the one that was meant. Breaking the statement on purpose is
// the only way for that to be visible. Check [TenantGuard.Err] where a
// definite error is wanted.
func CreateTenantGuard(g *TenantGuard) []drops.Expression {
	return createTenantGuard(g, false)
}

// CreateTenantGuardIfNotExists is the IF NOT EXISTS variant.
//
// Prefer it in a migration that is the single source of a guard's
// definition: re-running it converges, where a plain CREATE fails with
// "trigger <name> already exists" on the second run. Note what it does
// NOT do — it does not replace a trigger of the same name whose body
// differs, so a guard whose definition changed needs the DROP first.
func CreateTenantGuardIfNotExists(g *TenantGuard) []drops.Expression {
	return createTenantGuard(g, true)
}

func createTenantGuard(g *TenantGuard, ifNotExists bool) []drops.Expression {
	specs := g.triggerSpecs()
	out := make([]drops.Expression, 0, len(specs))
	for _, s := range specs {
		spec := s
		out = append(out, drops.ExprFunc(func(b *drops.Builder) {
			writeCreateTenantGuard(b, g, spec, ifNotExists)
		}))
	}
	return out
}

// DropTenantGuard renders the DROP TRIGGER statements, in the reverse
// of the order [CreateTenantGuard] renders them.
func DropTenantGuard(g *TenantGuard) []drops.Expression {
	return dropTenantGuard(g, false)
}

// DropTenantGuardIfExists is the IF EXISTS variant.
func DropTenantGuardIfExists(g *TenantGuard) []drops.Expression {
	return dropTenantGuard(g, true)
}

func dropTenantGuard(g *TenantGuard, ifExists bool) []drops.Expression {
	// The names alone are wanted, so this does not validate: dropping
	// the triggers of a guard whose declaration was never completed is
	// exactly what rolling back a half-written migration does. A guard
	// that never named an axis still names its triggers, because the
	// suffixes are fixed.
	specs := g.triggerNames()
	out := make([]drops.Expression, 0, len(specs))
	for i := len(specs) - 1; i >= 0; i-- {
		name := specs[i]
		out = append(out, drops.ExprFunc(func(b *drops.Builder) {
			b.WriteString("DROP TRIGGER ")
			if ifExists {
				b.WriteString("IF EXISTS ")
			}
			b.WriteIdent(name)
		}))
	}
	return out
}

// triggerSpec is one rendered trigger: when it fires, on which
// columns, the condition that refuses, and what the refusal says.
type triggerSpec struct {
	name string
	// update is false for BEFORE INSERT, true for BEFORE UPDATE OF.
	update bool
	// cols are the UPDATE OF columns. A statement whose SET list names
	// none of them does not fire the trigger, which is why the parent
	// guard lists the foreign key as well as the axis: either one
	// changing can break the agreement.
	cols []*Column
	// when renders the condition under which the write is refused.
	when func(b *drops.Builder)
	// message is the text RAISE(ABORT) carries, before literal
	// quoting.
	message string
}

// triggerNames lists the trigger names in creation order, for a guard
// that may not be complete.
func (g *TenantGuard) triggerNames() []string {
	names := []string{g.name + "_ins", g.name + "_upd"}
	for _, p := range g.parents {
		if p.local == nil {
			continue
		}
		names = append(names,
			g.name+"_"+p.local.Name()+"_ins",
			g.name+"_"+p.local.Name()+"_upd")
	}
	return names
}

// triggerSpecs builds the triggers this guard renders.
//
// A guard whose declaration is broken still yields the same NUMBER of
// specs, so a caller that renders one statement per spec gets one
// broken statement per trigger it expected rather than an empty list
// that silently applies nothing.
func (g *TenantGuard) triggerSpecs() []triggerSpec {
	if err := g.validate(); err != nil {
		names := g.triggerNames()
		out := make([]triggerSpec, 0, len(names))
		for _, n := range names {
			out = append(out, triggerSpec{name: n})
		}
		return out
	}

	axis := g.axis
	specs := make([]triggerSpec, 0, 2+2*len(g.parents))

	if g.hasTenant {
		// Pinned: one condition, both timings. It subsumes the null and
		// immutability guards — IS NOT <literal> is true for NULL and
		// for every other tenant — so it replaces them.
		lit, _ := tenantGuardLiteral(g.tenant)
		msg := guardPath(axis) + " must be " + lit + " in this database"
		when := func(b *drops.Builder) {
			writeNewCol(b, axis)
			b.WriteString(" IS NOT ")
			b.WriteString(lit)
		}
		specs = append(specs,
			triggerSpec{name: g.name + "_ins", when: when, message: msg},
			triggerSpec{name: g.name + "_upd", update: true, cols: []*Column{axis}, when: when, message: msg})
	} else {
		specs = append(specs, triggerSpec{
			name: g.name + "_ins",
			when: func(b *drops.Builder) {
				writeNewCol(b, axis)
				b.WriteString(" IS NULL")
			},
			message: guardPath(axis) + " is null; the row would belong to no tenant",
		})
		specs = append(specs, triggerSpec{
			name:   g.name + "_upd",
			update: true,
			cols:   []*Column{axis},
			when: func(b *drops.Builder) {
				// IS NOT rather than <>, because <> is NULL when either
				// side is NULL and a NULL condition does not fire the
				// trigger: an UPDATE moving a row from NULL to a tenant,
				// or to NULL, would go unrefused.
				writeNewCol(b, axis)
				b.WriteString(" IS NOT OLD.")
				b.WriteIdent(axis.Name())
			},
			message: guardPath(axis) + " is immutable; this statement would move a row between tenants",
		})
	}

	for _, p := range g.parents {
		link := p
		msg := guardPath(axis) + " disagrees with " + guardPath(link.parentAxis) +
			" for the row " + guardPath(link.local) + " names"
		when := func(b *drops.Builder) {
			writeNewCol(b, axis)
			b.WriteString(" IS NOT (SELECT ")
			b.WriteIdent(link.parentAxis.Name())
			b.WriteString(" FROM ")
			b.WriteIdent(link.parentKey.Table().Name())
			b.WriteString(" WHERE ")
			b.WriteIdent(link.parentKey.Name())
			// IS rather than =, so a NULL foreign key matches no parent
			// row rather than making the whole condition NULL — which
			// would not fire the trigger and would let a row with a null
			// key past a guard that had not looked at it.
			b.WriteString(" IS ")
			writeNewCol(b, link.local)
			b.WriteString(")")
		}
		specs = append(specs,
			triggerSpec{
				name:    g.name + "_" + link.local.Name() + "_ins",
				when:    when,
				message: msg,
			},
			triggerSpec{
				name:    g.name + "_" + link.local.Name() + "_upd",
				update:  true,
				cols:    []*Column{axis, link.local},
				when:    when,
				message: msg,
			})
	}
	return specs
}

// writeNewCol renders NEW."col". The row reference is a keyword rather
// than an identifier, so it is written unquoted and the column is not.
func writeNewCol(b *drops.Builder, c *Column) {
	b.WriteString("NEW.")
	b.WriteIdent(c.Name())
}

func writeCreateTenantGuard(b *drops.Builder, g *TenantGuard, s triggerSpec, ifNotExists bool) {
	b.WriteString("CREATE TRIGGER ")
	if ifNotExists {
		b.WriteString("IF NOT EXISTS ")
	}
	if err := g.validate(); err != nil {
		// Break the statement rather than install a trigger whose
		// condition drops cannot state. See CreateTenantGuard.
		b.WriteString("/* drops/sqlite: " + err.Error() + " */")
		return
	}
	b.WriteIdent(s.name)
	if s.update {
		b.WriteString(" BEFORE UPDATE OF ")
		for i, c := range s.cols {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteIdent(c.Name())
		}
	} else {
		b.WriteString(" BEFORE INSERT")
	}
	b.WriteString(" ON ")
	// The table's own name rather than writeFrom, which renders an
	// alias when one is set: an aliased name in a stored trigger body
	// would name the alias in a context that has no query around it.
	b.WriteIdent(g.table.Name())
	b.WriteString(" FOR EACH ROW WHEN ")
	s.when(b)
	// ABORT rather than ROLLBACK: the statement is undone and the
	// caller's transaction is left alone. See the file comment.
	b.WriteString(" BEGIN SELECT RAISE(ABORT, ")
	b.WriteString(sqlTextLiteral("drops/sqlite: " + s.message))
	b.WriteString("); END")
}
