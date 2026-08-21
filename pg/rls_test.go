package pg_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/pg"
)

// drops could write a row-level security policy and could not satisfy
// one: nothing in the package made a pooled connection carry the
// identity a policy reads. InTxAs is that half, and every test below
// asserts on the recorded statement stream rather than on a return
// value, because the difference between "the identity was established"
// and "the identity was not" is invisible in what InTxAs returns and
// entirely visible in what it sent.
//
// The stream includes the transaction boundaries on purpose. A SET
// LOCAL outside a transaction block is a warning and no effect, and a
// SET LOCAL in the wrong transaction is somebody else's identity, so
// "which statements, in which order, between which begin and which
// commit" is the whole subject.

// rlsTable is the fixture the tests share. It is a table with a policy
// declared on it, because that is the shape the feature is for — the
// policy is not what these tests assert (a recorder does not evaluate
// one), but a fixture that omitted it would be describing a different
// feature.
func rlsTable() (*pg.Table, *pg.Col[string]) {
	tbl := pg.NewTable("rls_posts")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	title := pg.Add(tbl, pg.Text("title").NotNull())
	tbl.EnableRLS().AddPolicy(
		pg.NewPolicy("tenant_isolation").
			Using(`"tenantId" = current_setting('app.tenantId')::bigint`))
	return tbl, title
}

// work is the body a test runs inside the transaction: one statement,
// so the stream shows whether the body ran at all.
func work(tx *pg.DB) error {
	_, err := tx.Exec(context.Background(), "SELECT 1")
	return err
}

func ops(stmts []dropstest.Statement) []string {
	out := make([]string, len(stmts))
	for i, s := range stmts {
		out[i] = s.String()
	}
	return out
}

// The statements InTxAs sends for a session, in order, with the
// boundaries around them. The role is quoted into the text because
// PostgreSQL has no placeholder slot for it; the settings are bound,
// key and value both, because set_config is an ordinary function.
func TestInTxAsSendsTheSessionItWasGiven(t *testing.T) {
	tests := []struct {
		name    string
		session pg.Session
		want    []dropstest.Statement
	}{
		{
			name:    "role only",
			session: pg.Session{Role: "app_tenant"},
			want: []dropstest.Statement{
				{Op: dropstest.OpBegin},
				{Op: dropstest.OpExec, SQL: `SET LOCAL ROLE "app_tenant"`},
				{Op: dropstest.OpExec, SQL: "SELECT 1"},
				{Op: dropstest.OpCommit},
			},
		},
		{
			name:    "settings only",
			session: pg.Session{Settings: map[string]string{"app.tenantId": "42"}},
			want: []dropstest.Statement{
				{Op: dropstest.OpBegin},
				{Op: dropstest.OpExec, SQL: "SELECT set_config($1, $2, true)", Args: []any{"app.tenantId", "42"}},
				{Op: dropstest.OpExec, SQL: "SELECT 1"},
				{Op: dropstest.OpCommit},
			},
		},
		{
			name: "role and settings, role first",
			session: pg.Session{
				Role:     "app_tenant",
				Settings: map[string]string{"app.tenantId": "42"},
			},
			want: []dropstest.Statement{
				{Op: dropstest.OpBegin},
				{Op: dropstest.OpExec, SQL: `SET LOCAL ROLE "app_tenant"`},
				{Op: dropstest.OpExec, SQL: "SELECT set_config($1, $2, true)", Args: []any{"app.tenantId", "42"}},
				{Op: dropstest.OpExec, SQL: "SELECT 1"},
				{Op: dropstest.OpCommit},
			},
		},
		{
			name: "several settings, sorted by key",
			session: pg.Session{Settings: map[string]string{
				"app.userId":   "9",
				"app.tenantId": "42",
				"app.role":     "reader",
			}},
			want: []dropstest.Statement{
				{Op: dropstest.OpBegin},
				{Op: dropstest.OpExec, SQL: "SELECT set_config($1, $2, true)", Args: []any{"app.role", "reader"}},
				{Op: dropstest.OpExec, SQL: "SELECT set_config($1, $2, true)", Args: []any{"app.tenantId", "42"}},
				{Op: dropstest.OpExec, SQL: "SELECT set_config($1, $2, true)", Args: []any{"app.userId", "9"}},
				{Op: dropstest.OpExec, SQL: "SELECT 1"},
				{Op: dropstest.OpCommit},
			},
		},
		{
			name:    "an empty value is a setting, not an absent one",
			session: pg.Session{Settings: map[string]string{"app.tenantId": ""}},
			want: []dropstest.Statement{
				{Op: dropstest.OpBegin},
				{Op: dropstest.OpExec, SQL: "SELECT set_config($1, $2, true)", Args: []any{"app.tenantId", ""}},
				{Op: dropstest.OpExec, SQL: "SELECT 1"},
				{Op: dropstest.OpCommit},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			drv := dropstest.New()
			db := pg.New(drv)

			if err := db.InTxAs(context.Background(), tc.session, work); err != nil {
				t.Fatalf("InTxAs: %v", err)
			}
			if got := drv.Statements(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("statements =\n  %v\nwant\n  %v", ops(got), ops(tc.want))
			}
		})
	}
}

// LOCAL is the load-bearing word, so it is worth one assertion of its
// own that says so in the terms the failure would take: a SET that
// omitted LOCAL, or a set_config whose is_local argument were false,
// leaves the identity on the pooled connection for whatever request
// draws it next. Neither is visible in a return value.
func TestInTxAsScopesTheIdentityToTheTransaction(t *testing.T) {
	drv := dropstest.New()
	db := pg.New(drv)

	sess := pg.Session{Role: "app_tenant", Settings: map[string]string{"app.tenantId": "42"}}
	if err := db.InTxAs(context.Background(), sess, work); err != nil {
		t.Fatalf("InTxAs: %v", err)
	}

	for _, s := range drv.Statements() {
		switch {
		case strings.HasPrefix(s.SQL, "SET "):
			if !strings.HasPrefix(s.SQL, "SET LOCAL ") {
				t.Errorf("%q is a session-lifetime SET: the pooled connection carries it to the next request", s.SQL)
			}
		case strings.HasPrefix(s.SQL, "SELECT set_config("):
			if !strings.HasSuffix(s.SQL, ", true)") {
				t.Errorf("%q does not pass is_local = true: the setting outlives the transaction", s.SQL)
			}
		}
	}
}

// A role name is an identifier in the statement text, so the names
// that would break a hand-rolled concatenation are the interesting
// ones. Quoting handles them; the keyword cases in particular are why
// concatenating a bare name is wrong even when the name is "clean".
func TestInTxAsQuotesTheRole(t *testing.T) {
	tests := []struct {
		name string
		role string
		want string
	}{
		{name: "ordinary", role: "app_tenant", want: `SET LOCAL ROLE "app_tenant"`},
		{name: "sql keyword", role: "select", want: `SET LOCAL ROLE "select"`},
		{name: "reserved word", role: "user", want: `SET LOCAL ROLE "user"`},
		{name: "camel case is preserved", role: "appTenant", want: `SET LOCAL ROLE "appTenant"`},
		{name: "uppercase is not folded", role: "APP_TENANT", want: `SET LOCAL ROLE "APP_TENANT"`},
		{name: "a space is not a statement break", role: "app tenant", want: `SET LOCAL ROLE "app tenant"`},
		{name: "a semicolon stays inside the name", role: "a; DROP TABLE users; --", want: `SET LOCAL ROLE "a; DROP TABLE users; --"`},
		{name: "an embedded quote is doubled", role: `a"b`, want: `SET LOCAL ROLE "a""b"`},
		{name: "unicode", role: "tenànt", want: `SET LOCAL ROLE "tenànt"`},
		// SET ROLE NONE means RESET ROLE, but a quoted "none" is a
		// name. Asking for it therefore asks for a role called none and
		// fails on a server where none exists — which is the right
		// answer: the way to not set a role is to leave Role empty.
		{name: "none is a name, not the keyword", role: "none", want: `SET LOCAL ROLE "none"`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			drv := dropstest.New()
			db := pg.New(drv)

			if err := db.InTxAs(context.Background(), pg.Session{Role: tc.role}, work); err != nil {
				t.Fatalf("InTxAs: %v", err)
			}
			got := drv.SQL()
			if len(got) == 0 {
				t.Fatal("no statement was sent at all")
			}
			if got[0] != tc.want {
				t.Errorf("role statement = %q, want %q", got[0], tc.want)
			}
		})
	}
}

// A Session that cannot work is refused before a transaction is
// opened. The assertion on the statement stream matters as much as the
// one on the error: a refusal that had already begun a transaction
// would leave one to roll back, and a refusal that had already sent
// the role would have set an identity for a body that never runs.
func TestInTxAsRefusesASessionItCannotEstablish(t *testing.T) {
	tests := []struct {
		name    string
		session pg.Session
		want    error
	}{
		{
			name:    "no role and no settings",
			session: pg.Session{},
			want:    pg.ErrEmptySession,
		},
		{
			name:    "an empty settings map is still empty",
			session: pg.Session{Settings: map[string]string{}},
			want:    pg.ErrEmptySession,
		},
		{
			name:    "a NUL in the role would truncate the statement",
			session: pg.Session{Role: "app\x00tenant"},
			want:    pg.ErrInvalidRole,
		},
		{
			name:    "a role that is not valid UTF-8",
			session: pg.Session{Role: "\xff\xfe"},
			want:    pg.ErrInvalidRole,
		},
		{
			name:    "an empty setting name",
			session: pg.Session{Settings: map[string]string{"": "42"}},
			want:    pg.ErrInvalidSetting,
		},
		{
			name:    "a NUL in the setting name",
			session: pg.Session{Settings: map[string]string{"app.\x00id": "42"}},
			want:    pg.ErrInvalidSetting,
		},
		{
			name:    "a setting name that is not valid UTF-8",
			session: pg.Session{Settings: map[string]string{"\xff\xfe": "42"}},
			want:    pg.ErrInvalidSetting,
		},
		{
			name: "one bad setting refuses the whole session",
			session: pg.Session{
				Role:     "app_tenant",
				Settings: map[string]string{"app.tenantId": "42", "": "9"},
			},
			want: pg.ErrInvalidSetting,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			drv := dropstest.New()
			db := pg.New(drv)

			ran := false
			err := db.InTxAs(context.Background(), tc.session, func(tx *pg.DB) error {
				ran = true
				return nil
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want errors.Is(err, %v)", err, tc.want)
			}
			if ran {
				t.Error("the body ran under a session that was refused")
			}
			if got := drv.Statements(); len(got) != 0 {
				t.Errorf("statements = %v, want none — the refusal happened before the transaction", ops(got))
			}
		})
	}
}

// The single most important behaviour in the file. A failure to
// establish the identity aborts: the body never runs, and the
// transaction rolls back. The alternative — log it and carry on — runs
// the request as the pool's own user, which is the account that owns
// the tables and that a policy written without FORCE ROW LEVEL
// SECURITY does not apply to at all.
func TestInTxAsAbortsWhenTheIdentityCannotBeEstablished(t *testing.T) {
	boom := errors.New("server said no")

	tests := []struct {
		name    string
		session pg.Session
		// failAfter is the number of statements to let through before
		// the injected failure fires.
		failAfter int
		wantSent  []string
	}{
		{
			name:      "the role is refused",
			session:   pg.Session{Role: "app_tenant", Settings: map[string]string{"app.tenantId": "42"}},
			failAfter: 0,
			wantSent:  []string{`SET LOCAL ROLE "app_tenant"`},
		},
		{
			name:      "the only setting is refused",
			session:   pg.Session{Settings: map[string]string{"app.tenantId": "42"}},
			failAfter: 0,
			wantSent:  []string{"SELECT set_config($1, $2, true)"},
		},
		{
			name: "a setting is refused midway",
			session: pg.Session{Settings: map[string]string{
				"app.tenantId": "42",
				"app.userId":   "9",
			}},
			failAfter: 1,
			wantSent: []string{
				"SELECT set_config($1, $2, true)",
				"SELECT set_config($1, $2, true)",
			},
		},
		{
			name: "the role succeeds and a setting is refused",
			session: pg.Session{
				Role:     "app_tenant",
				Settings: map[string]string{"app.tenantId": "42"},
			},
			failAfter: 1,
			wantSent: []string{
				`SET LOCAL ROLE "app_tenant"`,
				"SELECT set_config($1, $2, true)",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			drv := dropstest.New()
			seen := 0
			db := pg.New(drv).WithHook(drops.Hook(func(_ context.Context, e drops.QueryEvent) {
				if e.Kind != "exec" && e.Kind != "query" {
					return
				}
				seen++
				if seen == tc.failAfter {
					drv.FailNext(boom)
				}
			}))
			if tc.failAfter == 0 {
				drv.FailNext(boom)
			}

			ran := false
			err := db.InTxAs(context.Background(), tc.session, func(tx *pg.DB) error {
				ran = true
				return nil
			})
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v, want it to wrap %v", err, boom)
			}
			if ran {
				t.Error("the body ran after the identity could not be established — this is the fail-open the method exists to refuse")
			}
			if got := drv.SQL(); !reflect.DeepEqual(got, tc.wantSent) {
				t.Errorf("statements = %v, want %v", got, tc.wantSent)
			}
			last := drv.Statements()
			if n := len(last); n == 0 || last[n-1].Op != dropstest.OpRollback {
				t.Errorf("stream ends with %v, want a rollback", ops(last))
			}
		})
	}
}

// The error a refused identity produces has to say which half failed
// and has to name the package exactly once. These messages only ever
// appear when something is already wrong and nobody is reading
// carefully, which is how three double-prefixed ones shipped before;
// see pg/errorprefix_test.go.
func TestInTxAsErrorsNameThePackageOnce(t *testing.T) {
	tests := []struct {
		name     string
		session  pg.Session
		contains []string
	}{
		{
			name:     "empty session",
			session:  pg.Session{},
			contains: []string{"names no role and no settings"},
		},
		{
			name:     "bad role",
			session:  pg.Session{Role: "a\x00b"},
			contains: []string{"invalid role name", "NUL"},
		},
		{
			name:     "bad setting",
			session:  pg.Session{Settings: map[string]string{"": "x"}},
			contains: []string{"invalid session setting name", "empty"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := pg.New(dropstest.New()).InTxAs(context.Background(), tc.session, work)
			if err == nil {
				t.Fatal("want an error")
			}
			msg := err.Error()
			if n := strings.Count(msg, "drops/pg:"); n != 1 {
				t.Errorf("%q names the package %d times, want exactly 1", msg, n)
			}
			for _, want := range tc.contains {
				if !strings.Contains(msg, want) {
					t.Errorf("%q does not mention %q", msg, want)
				}
			}
		})
	}
}

// A refused role is refused for the reason validateIdent gives, and
// that reason is reachable: the name really is not a usable
// identifier, so both sentinels match.
func TestInTxAsRefusalsCarryBothSentinels(t *testing.T) {
	err := pg.New(dropstest.New()).InTxAs(context.Background(), pg.Session{Role: "a\x00b"}, work)
	for _, want := range []error{pg.ErrInvalidRole, pg.ErrInvalidIdentifier} {
		if !errors.Is(err, want) {
			t.Errorf("errors.Is(err, %v) = false; err = %v", want, err)
		}
	}
	if errors.Is(err, pg.ErrInvalidSetting) {
		t.Errorf("a bad role reported itself as a bad setting: %v", err)
	}
}

// A ctx cancelled while the identity was being established means the
// caller has gone. The body must not run — least of all a body that
// issues no statement of its own and would therefore never notice.
func TestInTxAsDoesNotRunTheBodyForACancelledCaller(t *testing.T) {
	drv := dropstest.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Cancel the moment the last set_config has been sent: the
	// identity is established and the caller is gone.
	db := pg.New(drv).WithHook(drops.Hook(func(_ context.Context, e drops.QueryEvent) {
		if strings.HasPrefix(e.SQL, "SELECT set_config(") {
			cancel()
		}
	}))

	ran := false
	sess := pg.Session{Role: "app_tenant", Settings: map[string]string{"app.tenantId": "42"}}
	err := db.InTxAs(ctx, sess, func(tx *pg.DB) error {
		ran = true
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if ran {
		t.Error("the body ran for a caller that had already gone")
	}
	want := []string{`SET LOCAL ROLE "app_tenant"`, "SELECT set_config($1, $2, true)"}
	if got := drv.SQL(); !reflect.DeepEqual(got, want) {
		t.Errorf("statements = %v, want %v", got, want)
	}
	stmts := drv.Statements()
	if n := len(stmts); n == 0 || stmts[n-1].Op != dropstest.OpRollback {
		t.Errorf("stream ends with %v, want a rollback", ops(stmts))
	}
}

// A ctx already dead when InTxAs is called never opens a transaction.
func TestInTxAsRefusesAnAlreadyCancelledCtx(t *testing.T) {
	drv := dropstest.New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ran := false
	err := pg.New(drv).InTxAs(ctx, pg.Session{Role: "app_tenant"}, func(tx *pg.DB) error {
		ran = true
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if ran {
		t.Error("the body ran on a cancelled ctx")
	}
	if got := drv.Statements(); len(got) != 0 {
		t.Errorf("statements = %v, want none", ops(got))
	}
}

// A panicking body rolls back and re-panics, and the identity dies
// with the transaction the same way it would on an ordinary error —
// which is the property that makes a panic in a request handler not a
// cross-tenant event.
func TestInTxAsRollsBackAndRepanicsWhenTheBodyPanics(t *testing.T) {
	drv := dropstest.New()
	db := pg.New(drv)

	func() {
		defer func() {
			p := recover()
			if p == nil {
				t.Error("the panic did not reach the caller")
			}
			if got, want := fmt.Sprint(p), "boom"; got != want {
				t.Errorf("recovered %q, want %q", got, want)
			}
		}()
		_ = db.InTxAs(context.Background(), pg.Session{Role: "app_tenant"}, func(tx *pg.DB) error {
			panic("boom")
		})
	}()

	want := []dropstest.Statement{
		{Op: dropstest.OpBegin},
		{Op: dropstest.OpExec, SQL: `SET LOCAL ROLE "app_tenant"`},
		{Op: dropstest.OpRollback},
	}
	if got := drv.Statements(); !reflect.DeepEqual(got, want) {
		t.Errorf("statements =\n  %v\nwant\n  %v", ops(got), ops(want))
	}
}

// A retried attempt re-establishes the identity. The failure this
// pins is specific and would be invisible: hoisting the SETs out of
// the retried body would leave the second attempt running as the pool
// user, since the first attempt's transaction — and with it its LOCAL
// settings — is gone.
func TestInTxAsReestablishesTheIdentityOnEveryAttempt(t *testing.T) {
	drv := dropstest.New()
	db := pg.New(drv).WithRetry(pg.RetryPolicy{
		MaxAttempts: 2,
		Errors:      []error{pg.ErrSerializationFailure},
	})

	attempts := 0
	sess := pg.Session{Role: "app_tenant", Settings: map[string]string{"app.tenantId": "42"}}
	err := db.InTxAs(context.Background(), sess, func(tx *pg.DB) error {
		attempts++
		if attempts == 1 {
			return pg.ErrSerializationFailure
		}
		return nil
	})
	if err != nil {
		t.Fatalf("InTxAs: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}

	want := []dropstest.Statement{
		{Op: dropstest.OpBegin},
		{Op: dropstest.OpExec, SQL: `SET LOCAL ROLE "app_tenant"`},
		{Op: dropstest.OpExec, SQL: "SELECT set_config($1, $2, true)", Args: []any{"app.tenantId", "42"}},
		{Op: dropstest.OpRollback},
		{Op: dropstest.OpBegin},
		{Op: dropstest.OpExec, SQL: `SET LOCAL ROLE "app_tenant"`},
		{Op: dropstest.OpExec, SQL: "SELECT set_config($1, $2, true)", Args: []any{"app.tenantId", "42"}},
		{Op: dropstest.OpCommit},
	}
	if got := drv.Statements(); !reflect.DeepEqual(got, want) {
		t.Errorf("statements =\n  %v\nwant\n  %v", ops(got), ops(want))
	}
}

// Nesting, against a driver that permits it: the inner SET LOCAL lands
// inside the inner transaction, which is where it is reverted. The
// outer identity is untouched by the inner one having been asked for.
//
// database/sql refuses a nested Begin outright, so on the drivers most
// callers use the inner InTxAs fails and never sends anything at all —
// covered below.
func TestInTxAsNestsIntoTheInnerTransaction(t *testing.T) {
	drv := dropstest.New()
	db := pg.New(drv)

	outer := pg.Session{Role: "app_outer"}
	inner := pg.Session{Role: "app_inner"}
	err := db.InTxAs(context.Background(), outer, func(tx *pg.DB) error {
		return tx.InTxAs(context.Background(), inner, work)
	})
	if err != nil {
		t.Fatalf("InTxAs: %v", err)
	}

	want := []dropstest.Statement{
		{Op: dropstest.OpBegin},
		{Op: dropstest.OpExec, SQL: `SET LOCAL ROLE "app_outer"`},
		{Op: dropstest.OpBegin},
		{Op: dropstest.OpExec, SQL: `SET LOCAL ROLE "app_inner"`},
		{Op: dropstest.OpExec, SQL: "SELECT 1"},
		{Op: dropstest.OpCommit},
		{Op: dropstest.OpCommit},
	}
	if got := drv.Statements(); !reflect.DeepEqual(got, want) {
		t.Errorf("statements =\n  %v\nwant\n  %v", ops(got), ops(want))
	}
}

// noNestDriver refuses a nested Begin the way database/sql does. The
// question it answers is what an inner InTxAs does when it cannot get
// a transaction: it must refuse, not fall back to running the body on
// the outer transaction's identity.
type noNestDriver struct{ *dropstest.Driver }

var errNoNest = errors.New("nested transactions are not supported")

func (d noNestDriver) Begin(ctx context.Context) (drops.Tx, error) {
	tx, err := d.Driver.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return noNestTx{tx}, nil
}

type noNestTx struct{ drops.Tx }

func (t noNestTx) Begin(context.Context) (drops.Tx, error) { return nil, errNoNest }

func TestInTxAsRefusesWhenTheDriverHasNoNestedTransaction(t *testing.T) {
	rec := dropstest.New()
	db := pg.New(noNestDriver{rec})

	ran := false
	err := db.InTxAs(context.Background(), pg.Session{Role: "app_outer"}, func(tx *pg.DB) error {
		return tx.InTxAs(context.Background(), pg.Session{Role: "app_inner"}, func(*pg.DB) error {
			ran = true
			return nil
		})
	})
	if !errors.Is(err, errNoNest) {
		t.Fatalf("err = %v, want it to wrap %v", err, errNoNest)
	}
	if ran {
		t.Error("the inner body ran without its own transaction, and so on the outer identity")
	}
	want := []string{`SET LOCAL ROLE "app_outer"`}
	if got := rec.SQL(); !reflect.DeepEqual(got, want) {
		t.Errorf("statements = %v, want %v", got, want)
	}
}

// The documented limit, asserted so it stays documented and stays a
// limit. Nothing stops the body changing its own role: drops does not
// read the statements the body sends, could not recognise the shapes
// that matter, and a pattern match would buy a check a rename defeats
// while implying a guarantee that does not exist.
//
// What InTxAs promises is about the transaction BOUNDARY. Whatever the
// body did, the server reverts every LOCAL setting at the commit or
// the rollback, so the connection goes back to the pool clean — and
// that is the property the next request depends on.
func TestInTxAsDoesNotPoliceTheBodysOwnRoleChanges(t *testing.T) {
	drv := dropstest.New()
	db := pg.New(drv)

	err := db.InTxAs(context.Background(), pg.Session{Role: "app_tenant"}, func(tx *pg.DB) error {
		_, err := tx.Exec(context.Background(), "RESET ROLE")
		return err
	})
	if err != nil {
		t.Fatalf("InTxAs: %v", err)
	}

	want := []dropstest.Statement{
		{Op: dropstest.OpBegin},
		{Op: dropstest.OpExec, SQL: `SET LOCAL ROLE "app_tenant"`},
		{Op: dropstest.OpExec, SQL: "RESET ROLE"},
		{Op: dropstest.OpCommit},
	}
	if got := drv.Statements(); !reflect.DeepEqual(got, want) {
		t.Fatalf("statements =\n  %v\nwant\n  %v", ops(got), ops(want))
	}
	// And the transaction still ended, which is what reverts it.
	if last := drv.Statements(); last[len(last)-1].Op != dropstest.OpCommit {
		t.Error("the transaction did not end, so nothing reverted the identity")
	}
}

// The identity and the work reach the hook in one stream, which is
// what makes a query log show who a statement ran as. It is also the
// only place a reviewer can check the claim after the fact.
func TestInTxAsPutsTheIdentityInTheHookStream(t *testing.T) {
	var kinds, sqls []string
	drv := dropstest.New()
	db := pg.New(drv).WithHook(drops.Hook(func(_ context.Context, e drops.QueryEvent) {
		kinds = append(kinds, e.Kind)
		sqls = append(sqls, e.SQL)
	}))

	sess := pg.Session{Role: "app_tenant", Settings: map[string]string{"app.tenantId": "42"}}
	if err := db.InTxAs(context.Background(), sess, work); err != nil {
		t.Fatalf("InTxAs: %v", err)
	}

	wantKinds := []string{"begin", "exec", "exec", "exec", "commit"}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Errorf("hook kinds = %v, want %v", kinds, wantKinds)
	}
	wantSQL := []string{"", `SET LOCAL ROLE "app_tenant"`, "SELECT set_config($1, $2, true)", "SELECT 1", ""}
	if !reflect.DeepEqual(sqls, wantSQL) {
		t.Errorf("hook SQL = %v, want %v", sqls, wantSQL)
	}
}

// The two layers compose. A statement built inside InTxAs still
// carries the table's tenant predicate — the predicates defend the
// application, the policies defend the database, and neither switches
// the other off.
func TestInTxAsLeavesTheTenantPredicateInPlace(t *testing.T) {
	tbl, _ := rlsTable()
	ten := pg.Add(tbl, pg.BigInt("tenantId").NotNull())
	tbl.ContextFilter(pg.TenantFilter(ten))

	drv := dropstest.New()
	db := pg.New(drv)
	ctx := pg.WithTenant(context.Background(), int64(42))

	sess := pg.Session{Settings: map[string]string{"app.tenantId": "42"}}
	err := db.InTxAs(ctx, sess, func(tx *pg.DB) error {
		var out []struct{}
		return tx.Select().From(tbl).All(ctx, &out)
	})
	if err != nil {
		t.Fatalf("InTxAs: %v", err)
	}

	want := []string{
		"SELECT set_config($1, $2, true)",
		`SELECT * FROM "rls_posts" WHERE ("rls_posts"."tenantId" = $1)`,
	}
	if got := drv.SQL(); !reflect.DeepEqual(got, want) {
		t.Errorf("statements = %v, want %v", got, want)
	}
}

// And a ctx with no tenant is still refused by the predicate layer,
// inside a transaction that had established a perfectly good identity.
// The refusal is the application-level one, which is the point: it
// happens before the server is asked, and it says "no tenant" rather
// than returning the empty result a policy alone would.
func TestInTxAsDoesNotSuppressTheTenantRefusal(t *testing.T) {
	tbl, _ := rlsTable()
	ten := pg.Add(tbl, pg.BigInt("tenantId").NotNull())
	tbl.ContextFilter(pg.TenantFilter(ten))

	drv := dropstest.New()
	db := pg.New(drv)

	sess := pg.Session{Settings: map[string]string{"app.tenantId": "42"}}
	err := db.InTxAs(context.Background(), sess, func(tx *pg.DB) error {
		var out []struct{}
		return tx.Select().From(tbl).All(context.Background(), &out)
	})
	if !errors.Is(err, pg.ErrTenantMissing) {
		t.Fatalf("err = %v, want ErrTenantMissing", err)
	}
	want := []string{"SELECT set_config($1, $2, true)"}
	if got := drv.SQL(); !reflect.DeepEqual(got, want) {
		t.Errorf("statements = %v, want %v — the query must not have been sent", got, want)
	}
}
