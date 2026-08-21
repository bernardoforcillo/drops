package pg

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Row-level security, at runtime.
//
// This package could already WRITE a policy — [Table.EnableRLS],
// [NewPolicy] and [Table.AddPolicy] put one into a migration — and
// until this file it could not SATISFY one. A policy is a predicate
// the server evaluates against the identity of the session running the
// statement: the role the statement runs as, or a setting the session
// carries and the policy reads back with current_setting. Nothing in
// drops made a pooled connection carry a request's identity, so every
// policy drops could emit was a policy drops could not answer, and the
// package doc's conclusion — "PostgreSQL row-level security is the
// boundary" — was an instruction to go and write the other half by
// hand. [DB.InTxAs] is that other half.
//
// # It composes with the guards; it does not replace them
//
// The predicates defend the application, the policies defend the
// database. A table's [Table.ContextFilter] puts "tenantId = $1" into
// the statement, where a query plan and a reviewer can both see it,
// and refuses a request that carries no tenant before it reaches the
// server. A policy decides what the server will show whatever the
// statement asked for. The package doc says at length, under "The
// predicates are not the boundary", why the first can never be the
// second: [github.com/bernardoforcillo/drops.Builder] is an exported
// escape hatch whose text drops never sees, the source checks that
// enforce the scoping are a lint and a lint is advice to exactly the
// person it constrains, and [SelectBuilder.Unscoped] exists and has
// to. Everything here is the layer that makes those three facts
// survivable, and neither layer makes the other redundant: a policy
// with no predicate in front of it turns a forgotten WHERE clause into
// an empty result set that reads like "no such row", and a predicate
// with no policy under it turns a forgotten WHERE clause into somebody
// else's rows.
//
// # LOCAL is the load-bearing word
//
// SET ROLE and set_config(k, v, false) change the session, and a
// session outlives the request: a pooled connection handed back to the
// pool still carries the role and the settings the last request left
// on it, and the next request to draw that connection inherits an
// identity nobody gave it. That is not a subtle failure. It is how
// every hand-rolled version of this breaks, it breaks in the direction
// of MORE access rather than less, and it breaks under load — when the
// pool is actually recycling connections — rather than in the test
// that opened one connection and used it once.
//
// SET LOCAL ROLE and set_config(k, v, true) are the transaction-scoped
// forms. Their effects are reverted at COMMIT or ROLLBACK, by the
// server, whichever way the transaction ends, so a connection cannot
// carry one request's identity to the next request that gets it. That
// is why this is a transaction API and not a connection API: the
// promise is exactly as long as a transaction, so the thing that makes
// the promise has to own the transaction's lifetime. It is also why
// SET LOCAL outside a transaction block is not a smaller version of
// this — PostgreSQL answers that with a warning and no effect, and a
// warning is not something a driver reports.
//
// # What this cannot promise
//
// Nothing here stops the body of the transaction issuing RESET ROLE,
// SET ROLE, or set_config with is_local false; see [DB.InTxAs] for
// what that means and why the answer is documentation rather than a
// guard.
//
// A caller's identity is set on the connection the transaction holds.
// It reaches every statement that transaction runs and no statement
// outside it — including statements the same goroutine runs on the
// same *DB but outside the closure, which draw their own connection
// from the pool.

// ErrEmptySession is returned by [DB.InTxAs] when the [Session] it was
// given names no role and no settings.
//
// Refusing is the fail-closed reading of an empty session and the
// reason is the shape of the calling code. A Session is normally
// derived from a request — the role for this API key, the tenant id
// off the ctx — and the way that derivation fails is by producing
// nothing. Running the body anyway would run it as the pool's own
// user, which is the account that owns the tables and is frequently
// the account RLS does not apply to, on precisely the request whose
// identity could not be established. A caller that genuinely wants the
// pool's default identity has [DB.InTx] one method away and says so
// there, in a call a reviewer can see.
var ErrEmptySession = errors.New("drops/pg: session names no role and no settings")

// ErrInvalidRole is returned by [DB.InTxAs] when [Session].Role is not
// a name this package will write into a statement.
//
// The role has to be validated rather than bound because PostgreSQL's
// grammar has no placeholder slot in SET ROLE: the role there is an
// identifier in the statement text, not a value, so there is no $1 to
// put it in. That leaves quoting, and quoting is only safe for the
// bytes [validateIdent] admits — the same predicate the schema
// declarations are held to, reached through the same function, so a
// name that is a legal table name is a legal role name and there is no
// second answer to maintain. A name it refuses is refused here, before
// a transaction is opened, instead of being interpolated and finding
// out what the server made of it.
//
// The contrast with a setting is the whole shape of this file:
// set_config takes its key and its value as ordinary text arguments,
// so both are bound as $1 and $2 and neither is ever quoted, escaped
// or inspected for SQL. See [Session].Settings.
var ErrInvalidRole = errors.New("drops/pg: invalid role name")

// ErrInvalidSetting is returned by [DB.InTxAs] when a key of
// [Session].Settings carries bytes that cannot reach the server
// intact — empty, invalid UTF-8, or containing a NUL.
//
// A setting name IS bound as a parameter, so this is not a quoting
// check and there is no injection to prevent: it is the three byte
// patterns a text parameter cannot carry. PostgreSQL would refuse all
// three itself. Refusing them here names the setting that was wrong,
// in the caller's own terms, and does it before a transaction is
// opened and a role established — rather than after, in a driver error
// about a byte sequence.
var ErrInvalidSetting = errors.New("drops/pg: invalid session setting name")

// sessionError carries one of this file's sentinels together with the
// reason [validateIdent] produced, without the message naming the
// package twice.
//
// Wrapping with fmt.Errorf would have done that: validateIdent's
// errors already open with "drops/pg: ", as do the sentinels, and
// fmt.Errorf("%w: %w", ...) would print the prefix twice for a message
// that only ever appears when something is already wrong and nobody is
// reading carefully. pg/errorprefix_test.go exists because three of
// those had shipped.
//
// errors.Is reaches both sides: the sentinel through Is, and
// [ErrInvalidIdentifier] through Unwrap, which is honest — the reason
// the name was refused really is that it is not a usable identifier.
type sessionError struct {
	sentinel error
	err      error
}

func (e *sessionError) Error() string {
	reason := strings.TrimPrefix(e.err.Error(), ErrInvalidIdentifier.Error()+": ")
	return e.sentinel.Error() + ": " + reason
}

func (e *sessionError) Unwrap() error { return e.err }

func (e *sessionError) Is(target error) bool { return target == e.sentinel }

// Session is the PostgreSQL identity a transaction runs under: a role,
// a set of session settings, or both. It is the argument to
// [DB.InTxAs], and it is a plain struct because it is data — a request
// handler builds one from whatever it knows about the caller and hands
// it over.
//
//	sess := pg.Session{
//	    Role:     "app_tenant",
//	    Settings: map[string]string{"app.tenantId": tenantID},
//	}
//	err := db.InTxAs(ctx, sess, func(tx *pg.DB) error { ... })
//
// Which of the two halves a schema uses is a schema decision, and both
// are ordinary. A policy written as USING (tenantId =
// current_setting('app.tenantId')) reads a setting and needs only the
// Settings half, with one application role for every tenant. A policy
// written as TO tenant_reader, or a set of table grants, reads the
// role and needs only the Role half. Using both gives a policy two
// independent things to test, which is the arrangement to prefer when
// the roles are few and the tenants are many.
type Session struct {
	// Role is the PostgreSQL role the transaction runs as, established
	// with SET LOCAL ROLE. Empty leaves the role alone, which is the
	// right setting when the identity lives entirely in Settings.
	//
	// The pool's own user must be a member of the role, or the server
	// refuses the SET and [DB.InTxAs] aborts the transaction.
	//
	// The name is validated as an identifier and then quoted, so a role
	// whose name is a SQL keyword ("user", "select") or which needs
	// case preserved ("appTenant") is handled exactly as a table of
	// that name would be. One consequence is worth knowing before it
	// surprises someone: the quoting is what makes a keyword safe, and
	// it is also what stops SET ROLE's own keywords being keywords, so
	// Role: "none" asks for a role literally named "none" rather than
	// meaning RESET ROLE, and fails if no such role exists. Refusing to
	// run is the correct outcome there; the way to not set a role is to
	// leave this empty.
	Role string

	// Settings are the session settings the transaction carries, each
	// established with set_config(key, value, true) — the is_local
	// argument being the point of the whole exercise.
	//
	// Both halves of each pair are bound as parameters. The key is not
	// quoted, escaped or interpolated, so a value arriving from a
	// request is a value and not a fragment of SQL; the only check
	// applied to a key is the byte-level one described on
	// [ErrInvalidSetting], and none at all is applied to a value.
	//
	// Values are strings because that is what a setting is:
	// current_setting returns text whatever was put in, so a policy
	// comparing against a bigint column casts —
	// current_setting('app.tenantId')::bigint — and the cast belongs in
	// the policy, where it is written once, rather than in every call
	// site that sets the value. Use a prefixed name ("app.tenantId"):
	// PostgreSQL accepts a custom setting only when its name has a
	// prefix, and an unprefixed name is likely to be a real GUC.
	//
	// The settings are established in sorted key order, so the same
	// Session renders the same statements in the same order every time.
	// Their order carries no meaning otherwise: each set_config call is
	// independent of the others.
	Settings map[string]string
}

// InTxAs runs fn inside a transaction that carries s — a PostgreSQL
// role, a set of session settings, or both — for exactly the lifetime
// of that transaction. It is how a statement drops sends satisfies a
// row-level security policy drops declared.
//
//	sess := pg.Session{Settings: map[string]string{"app.tenantId": id}}
//	err := db.InTxAs(ctx, sess, func(tx *pg.DB) error {
//	    posts, err := tx.Select().From(Posts).All(ctx, &out)
//	    ...
//	})
//
// The statements it issues, in this order, are:
//
//	SET LOCAL ROLE "app_tenant"
//	SELECT set_config($1, $2, true)   -- once per setting, sorted by key
//
// then fn, then the COMMIT or ROLLBACK that [DB.InTx] performs — at
// which point the server discards both, because LOCAL is what both of
// them say. See the file comment for why that word is the feature.
//
// # A failure to establish the identity aborts the transaction
//
// This is the single most important behaviour in the method and the
// reason it exists as a method rather than as a paragraph of
// documentation telling people to run two statements. If the SET LOCAL
// ROLE is refused — the role does not exist, the pool's user is not a
// member of it, the connection died — or if any set_config is refused,
// InTxAs returns the error and fn is never called. The transaction
// rolls back.
//
// The alternative, and it is the one a hand-rolled version reaches by
// accident every time, is to log the failure and carry on. That runs
// the request as the pool's own user: the account that owns the
// tables, that is frequently a superuser, and that a policy written
// without FORCE ROW LEVEL SECURITY does not apply to at all. So the
// request that failed to prove who it was is the one that sees
// everything. Every scoping defect this package has been audited for
// was a variant of that sentence, and a fail-open here would undo all
// of them at once, because it fails open below the layer they defend.
//
// # Cancellation
//
// ctx reaches every statement InTxAs issues, and it is checked once
// more after the last of them and before fn: a ctx cancelled while the
// identity was being established means the caller has gone, and the
// body must not run — least of all a body whose first statement would
// be the one to notice.
//
// # What it does not defend against
//
// fn holds a *DB bound to the transaction, and there is nothing
// stopping it running RESET ROLE, another SET ROLE, or set_config with
// is_local false. It will work. drops does not inspect the statements
// fn sends, could not recognise the shapes that matter — the string
// arrives through [DB.Exec], or through a [github.com/bernardoforcillo/drops.Raw],
// or out of a builder as text — and refusing on a pattern match would
// buy a check that a rename defeats while suggesting a guarantee that
// does not exist.
//
// So the guarantee is stated exactly: what InTxAs promises is about
// the transaction BOUNDARY, not about the transaction's interior. No
// identity it establishes survives the commit or the rollback, whatever
// the body did in between, because the server is what reverts it and
// the server reverts it at the boundary. A body that changes its own
// role has changed it for the rest of that transaction and for nothing
// after. That is the same standing as the "escape hatch" entries in
// the package doc: drops says where the line is instead of pretending
// there is a wall.
//
// # Composition
//
// InTxAs delegates to [DB.InTx], so it inherits the hook events, the
// panic-safe rollback and any [RetryPolicy] installed with WithRetry —
// and a retried attempt re-establishes the identity, because the
// establishing happens inside the retried body rather than once around
// it. A retry that re-ran the work without re-running the SETs would
// run the second attempt as the pool user, which is the fail-open this
// method exists to refuse.
//
// The SET statements go through [DB.Exec], so an attached
// [github.com/bernardoforcillo/drops.Hook] sees them in the same
// stream as the statements they govern: a query log shows the identity
// immediately above the work it authorised.
//
// Nesting depends on the driver. InTxAs on a *DB already bound to a
// transaction asks that driver for a nested transaction; database/sql
// refuses (use a SAVEPOINT), and a driver that allows it gets an inner
// SET LOCAL scoped to the inner transaction, reverted when the inner
// one ends. Either way the outer identity is unaffected by the inner
// one having been asked for.
func (db *DB) InTxAs(ctx context.Context, s Session, fn func(*DB) error) error {
	keys, err := s.check()
	if err != nil {
		return err
	}
	return db.InTx(ctx, func(tx *DB) error {
		if err := s.establish(ctx, tx, keys); err != nil {
			return err
		}
		// The identity is established and the caller may already be
		// gone. Checking here rather than letting fn's first statement
		// discover it keeps the refusal at the boundary — and a fn that
		// issues no statement at all (a cache hit, an early return that
		// still writes) would otherwise run to completion for a request
		// nobody is waiting for.
		if err := ctx.Err(); err != nil {
			return err
		}
		return fn(tx)
	})
}

// check validates s and returns its setting keys in the order they are
// to be established. It runs before the transaction is opened, so a
// Session that can never work costs no round trip and leaves no
// transaction to roll back.
func (s Session) check() ([]string, error) {
	if s.Role == "" && len(s.Settings) == 0 {
		return nil, ErrEmptySession
	}
	if s.Role != "" {
		if err := validateIdent("role", s.Role); err != nil {
			return nil, &sessionError{sentinel: ErrInvalidRole, err: err}
		}
	}
	keys := make([]string, 0, len(s.Settings))
	for k := range s.Settings {
		if err := validateIdent("setting", k); err != nil {
			return nil, &sessionError{sentinel: ErrInvalidSetting, err: err}
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}

// establish issues the SET LOCAL ROLE and set_config statements on tx.
// Every error it returns aborts the transaction, which is the whole
// contract; see [DB.InTxAs].
//
// The role goes first so that the window in which the transaction
// holds the pool user's authority is as short as it can be, and so
// that every setting is established under exactly the authority the
// body will run with — a setting the role is not permitted to change
// is then refused here rather than being applied by the pool's user
// and read back by a policy as though the role had set it.
func (s Session) establish(ctx context.Context, tx *DB, keys []string) error {
	if s.Role != "" {
		// quoteIdent, not a placeholder: SET ROLE takes an identifier
		// and PostgreSQL's grammar has nowhere to bind one. check has
		// already put the name through validateIdent, which is what
		// makes the quoting sufficient rather than hopeful.
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+quoteIdent(s.Role)); err != nil {
			return fmt.Errorf("drops/pg: transaction aborted, could not SET LOCAL ROLE %q: %w", s.Role, err)
		}
	}
	for _, k := range keys {
		// Both arguments bound. The contrast with the role above is
		// not a stylistic one: set_config is an ordinary function, so
		// its key and value are values, and a value is never text this
		// package assembles.
		if _, err := tx.Exec(ctx, "SELECT set_config($1, $2, true)", k, s.Settings[k]); err != nil {
			return fmt.Errorf("drops/pg: transaction aborted, could not set %q: %w", k, err)
		}
	}
	return nil
}
