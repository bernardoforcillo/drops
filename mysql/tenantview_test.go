package mysql_test

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/mysql"
)

// What a real server said, and what it did not.
//
// Every statement asserted below was executed against a live MySQL
// 8.0.46 and a live MariaDB 10.11.14, not merely rendered. The
// boundary the file comment in tenantview.go describes was measured
// end to end on both: reading through the view returned one tenant's
// rows, reading the base table was refused with ERROR 1142, an INSERT
// and an UPDATE carrying another tenant's axis value were refused with
// ERROR 1369 CHECK OPTION failed, an UPDATE of another tenant's row
// matched nothing, and an unqualified DELETE FROM the view removed
// only the rows the view could see. See integration/mysql_tenantview_test.go,
// which runs that sequence under DROPS_MYSQL_DSN.
//
// Three measurements are load-bearing for the DESIGN and are cited
// again at the tests that encode them:
//
//   - a view body cannot carry a placeholder. PREPARE of a CREATE VIEW
//     whose predicate is "?" fails with ERROR 1351, "View's SELECT
//     contains a variable or parameter". The tenant value must be a
//     LITERAL, which is why this file asserts literal rendering at all.
//   - the same DDL text means different things under different
//     sql_mode. "tenant = 'a\\b'" installed under the default mode
//     selects the row holding a\b; installed under NO_BACKSLASH_ESCAPES
//     it selects the row holding a\\b. That is a view scoped to the
//     wrong tenant, silently, so a backslash is refused rather than
//     escaped.
//   - REVOKE IF EXISTS is MySQL 8.0.16+ only; MariaDB 10.11 answers
//     ERROR 1064. Nothing here emits a REVOKE.

var (
	tvDocs   = mysql.NewDatabaseTable("shop", "docs")
	tvID     = mysql.Add(tvDocs, mysql.BigSerial("id").PrimaryKey())
	tvTenant = mysql.Add(tvDocs, mysql.Varchar("tenantId", 64))
	tvBody   = mysql.Add(tvDocs, mysql.Text("body"))

	tvApp  = mysql.Acct("app", "localhost")
	tvAcme = mysql.Acct("acme", "%")
)

var _ = tvBody

// tvFixture is the declaration every rendering case starts from: the
// complete one. A case that means to test an incomplete declaration
// takes this and drops exactly the piece it is about, so the thing
// under test is the omission and not the whole builder.
func tvFixture() *mysql.TenantView {
	return mysql.NewTenantView("v_docs_acme").
		On(tvDocs).
		Axis(tvTenant).
		ForTenant("acme").
		DefinedBy(tvApp)
}

func TestCreateTenantViewRendersTheStatementAnOperatorWouldWrite(t *testing.T) {
	// The expected text is not invented here: it is what MySQL itself
	// echoes back from SHOW CREATE VIEW for this view, modulo the
	// ALGORITHM clause the server adds and the schema-qualification it
	// expands column names into.
	got, args := sqlOf(mysql.CreateTenantView(tvFixture()))
	want := "CREATE DEFINER = `app`@`localhost` SQL SECURITY DEFINER VIEW `shop`.`v_docs_acme` " +
		"AS SELECT `id`, `tenantId`, `body` FROM `shop`.`docs` " +
		"WHERE `tenantId` = 'acme' WITH CASCADED CHECK OPTION"
	if got != want {
		t.Errorf("SQL = %s\nwant  %s", got, want)
	}
	// The tenant value is IN the statement, not bound beside it. A
	// bound argument would be a placeholder, and ERROR 1351 says a view
	// body may not carry one.
	if len(args) != 0 {
		t.Errorf("args = %v, want none: a view body cannot carry a placeholder", args)
	}
}

func TestCreateOrReplaceTenantViewRendersTheReplaceForm(t *testing.T) {
	got, _ := sqlOf(mysql.CreateOrReplaceTenantView(tvFixture()))
	if !strings.HasPrefix(got, "CREATE OR REPLACE DEFINER = `app`@`localhost` SQL SECURITY DEFINER VIEW ") {
		t.Errorf("SQL = %s\nwant CREATE OR REPLACE ... DEFINER prefix", got)
	}
	if !strings.HasSuffix(got, " WITH CASCADED CHECK OPTION") {
		t.Errorf("SQL = %s\nwant the CHECK OPTION to survive the replace form", got)
	}
}

// TestTenantViewLiteralRendering pins the literal forms. The tenant
// value lands in stored DDL, so each form is asserted rather than left
// to a fallback such as fmt.Sprint: guessing at it once is guessing at
// it for every statement the view later filters.
func TestTenantViewLiteralRendering(t *testing.T) {
	cases := []struct {
		name   string
		tenant any
		want   string
	}{
		{"string", "acme", "'acme'"},
		{"string with a doubled quote", "o'brien", "'o''brien'"},
		{"empty string is a tenant", "", "''"},
		{"int", 42, "42"},
		{"int64", int64(-7), "-7"},
		{"uint64 above int64", uint64(1) << 63, "9223372036854775808"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := tvFixture().ForTenant(tc.tenant)
			if err := v.Err(); err != nil {
				t.Fatalf("Err() = %v, want nil", err)
			}
			got, _ := sqlOf(mysql.CreateTenantView(v))
			want := "WHERE `tenantId` = " + tc.want + " WITH"
			if !strings.Contains(got, want) {
				t.Errorf("SQL = %s\nwant it to contain %s", got, want)
			}
		})
	}
}

// TestTenantViewRefusesLiteralsWhoseMeaningDependsOnSQLMode is the
// measurement that shaped this file.
//
// The identical DDL text "tenant = 'a\\b'" was installed on a live
// MySQL 8.0.46 twice. Under the default sql_mode the stored view
// selected the row holding a\b; under NO_BACKSLASH_ESCAPES it selected
// the row holding a\\b. drops renders text that an operator applies
// later, under an sql_mode drops never sees, so a backslash has no
// rendering that is correct in both — and the wrong one is a view
// scoped to a tenant nobody named, which fails by SILENTLY PERMITTING.
// The package's own quoteLiteral doubles the backslash and says it
// corrupts the text but never the statement; that trade is right for an
// enum member or a comment and wrong here, where the corrupted text IS
// the boundary.
func TestTenantViewRefusesLiteralsWhoseMeaningDependsOnSQLMode(t *testing.T) {
	for _, tenant := range []string{`a\b`, `\`, `acme\`, `a\\b`} {
		v := tvFixture().ForTenant(tenant)
		if !errors.Is(v.Err(), mysql.ErrTenantViewAmbiguousLiteral) {
			t.Errorf("ForTenant(%q).Err() = %v, want ErrTenantViewAmbiguousLiteral", tenant, v.Err())
		}
	}
}

// A NUL or a newline is refused on a different ground: the operator
// reads this DDL before applying it, and a control character is
// invisible in that reading.
func TestTenantViewRefusesUnreadableLiterals(t *testing.T) {
	for _, tenant := range []string{"a\x00b", "a\nb", "a\rb"} {
		v := tvFixture().ForTenant(tenant)
		if !errors.Is(v.Err(), mysql.ErrTenantViewAmbiguousLiteral) {
			t.Errorf("ForTenant(%q).Err() = %v, want ErrTenantViewAmbiguousLiteral", tenant, v.Err())
		}
	}
}

// []byte is refused for the reason section 1 of the tenant policy block
// gives: a schema holding its tenant as bytes on one table and as text
// on another is reporting a type confusion, and sameTenant refuses that
// pair at runtime. Rendering it here would let a view be declared for a
// tenant no ctx can ever match.
func TestTenantViewRefusesUnsupportedLiteralTypes(t *testing.T) {
	for _, tenant := range []any{[]byte("acme"), []rune("acme"), 1.5, true, struct{}{}} {
		v := tvFixture().ForTenant(tenant)
		if !errors.Is(v.Err(), mysql.ErrTenantViewUnsupportedLiteral) {
			t.Errorf("ForTenant(%T).Err() = %v, want ErrTenantViewUnsupportedLiteral", tenant, v.Err())
		}
	}
}

// A nil of any type is no tenant. Section 1 of the tenant policy block
// settles this for the runtime paths; a declaration obeys the same rule,
// because a view scoped to NULL matches no row under any comparison and
// would read as a tenant whose data had gone missing.
func TestTenantViewRefusesANilTenant(t *testing.T) {
	var typedNil *string
	for _, tenant := range []any{nil, typedNil, (*int)(nil)} {
		v := tvFixture().ForTenant(tenant)
		if !errors.Is(v.Err(), mysql.ErrTenantViewTenantRequired) {
			t.Errorf("ForTenant(%#v).Err() = %v, want ErrTenantViewTenantRequired", tenant, v.Err())
		}
	}
}

// TestIncompleteTenantViewBreaksTheStatement.
//
// An incomplete declaration renders a marked comment instead of a
// statement, so a stray call in a migration fails at exec rather than
// installing something. The choice matters more here than it does for a
// table: a CREATE VIEW that lost its WHERE is a statement MySQL
// ACCEPTS, and it publishes every tenant's rows to whoever holds the
// grant on the view.
func TestIncompleteTenantViewBreaksTheStatement(t *testing.T) {
	cases := []struct {
		name string
		view *mysql.TenantView
		err  error
	}{
		{"no table", mysql.NewTenantView("v").Axis(tvTenant).ForTenant("acme").DefinedBy(tvApp),
			mysql.ErrTenantViewTargetRequired},
		{"no axis", mysql.NewTenantView("v").On(tvDocs).ForTenant("acme").DefinedBy(tvApp),
			mysql.ErrTenantViewAxisRequired},
		{"no tenant", mysql.NewTenantView("v").On(tvDocs).Axis(tvTenant).DefinedBy(tvApp),
			mysql.ErrTenantViewTenantRequired},
		{"no definer", mysql.NewTenantView("v").On(tvDocs).Axis(tvTenant).ForTenant("acme"),
			mysql.ErrTenantViewDefinerRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !errors.Is(tc.view.Err(), tc.err) {
				t.Errorf("Err() = %v, want %v", tc.view.Err(), tc.err)
			}
			got, _ := sqlOf(mysql.CreateTenantView(tc.view))
			if !strings.Contains(got, "/* drops/mysql: ") {
				t.Errorf("SQL = %s\nwant a marked comment breaking the statement", got)
			}
			// The broken form must not be a runnable CREATE VIEW.
			if strings.Contains(got, " AS SELECT ") {
				t.Errorf("SQL = %s\nwant no view body on an incomplete declaration", got)
			}
		})
	}
}

// The axis has to be a column of the table the view selects FROM.
// Without this the WHERE names a column the FROM does not have, and
// MySQL answers ERROR 1054 at CREATE time — late, and after the
// migration has already reported progress.
func TestTenantViewRefusesAnAxisFromAnotherTable(t *testing.T) {
	other := mysql.NewDatabaseTable("shop", "orders")
	otherCol := mysql.Add(other, mysql.Varchar("tenantId", 64))

	v := mysql.NewTenantView("v").On(tvDocs).Axis(otherCol).ForTenant("acme").DefinedBy(tvApp)
	if !errors.Is(v.Err(), mysql.ErrTenantViewAxisNotInTable) {
		t.Errorf("Err() = %v, want ErrTenantViewAxisNotInTable", v.Err())
	}
}

func TestGrantTenantViewRendersThePrivilegesOnTheViewOnly(t *testing.T) {
	got, args := sqlOf(mysql.GrantTenantView(tvFixture(), tvAcme,
		mysql.PrivSelect, mysql.PrivInsert, mysql.PrivUpdate, mysql.PrivDelete))
	want := "GRANT SELECT, INSERT, UPDATE, DELETE ON `shop`.`v_docs_acme` TO `acme`@`%`"
	if got != want {
		t.Errorf("SQL = %s\nwant  %s", got, want)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want none", args)
	}
	// The base table must appear nowhere in a grant this package emits.
	// A grant naming it is the one statement that would dissolve the
	// boundary, so its absence is asserted rather than assumed.
	if strings.Contains(got, "`docs`") {
		t.Errorf("SQL = %s\nnames the base table: the grant must reach the view alone", got)
	}
}

func TestGrantTenantViewDefaultsToReadOnly(t *testing.T) {
	// Naming no privilege grants SELECT. The default is the one that
	// cannot widen anything: a caller who forgot the argument gets a
	// reader, not a writer.
	got, _ := sqlOf(mysql.GrantTenantView(tvFixture(), tvAcme))
	want := "GRANT SELECT ON `shop`.`v_docs_acme` TO `acme`@`%`"
	if got != want {
		t.Errorf("SQL = %s\nwant  %s", got, want)
	}
}

func TestGrantTenantViewRefusesAnUnknownPrivilege(t *testing.T) {
	got, _ := sqlOf(mysql.GrantTenantView(tvFixture(), tvAcme, mysql.Privilege("DROP; -- ")))
	if !strings.Contains(got, "/* drops/mysql: ") {
		t.Errorf("SQL = %s\nwant a marked comment rather than an unvalidated privilege", got)
	}
}

func TestGrantTenantViewRefusesAnIncompleteView(t *testing.T) {
	// A grant on a view whose CREATE was refused would name an object
	// that does not exist, or worse, one left over from an earlier run.
	incomplete := mysql.NewTenantView("v").On(tvDocs).Axis(tvTenant).DefinedBy(tvApp)
	got, _ := sqlOf(mysql.GrantTenantView(incomplete, tvAcme))
	if !strings.Contains(got, "/* drops/mysql: ") {
		t.Errorf("SQL = %s\nwant the grant to break when the view declaration did", got)
	}
}

func TestDropTenantViewRenders(t *testing.T) {
	got, _ := sqlOf(mysql.DropTenantView(tvFixture()))
	if want := "DROP VIEW `shop`.`v_docs_acme`"; got != want {
		t.Errorf("SQL = %s\nwant  %s", got, want)
	}
	got, _ = sqlOf(mysql.DropTenantViewIfExists(tvFixture()))
	if want := "DROP VIEW IF EXISTS `shop`.`v_docs_acme`"; got != want {
		t.Errorf("SQL = %s\nwant  %s", got, want)
	}
}

// A DROP needs the name alone, so it renders for a declaration that
// never got its tenant — which is exactly what rolling back a
// half-written migration wants to do.
func TestDropTenantViewRendersForAnIncompleteDeclaration(t *testing.T) {
	incomplete := mysql.NewTenantView("v_docs_acme").On(tvDocs)
	got, _ := sqlOf(mysql.DropTenantViewIfExists(incomplete))
	if want := "DROP VIEW IF EXISTS `shop`.`v_docs_acme`"; got != want {
		t.Errorf("SQL = %s\nwant  %s", got, want)
	}
}

// An unqualified table gets an unqualified view: the view belongs to
// whichever database the connection is using, exactly as the table does.
func TestTenantViewFollowsTheTableIntoTheDefaultDatabase(t *testing.T) {
	plain := mysql.NewTable("docs")
	tenant := mysql.Add(plain, mysql.Varchar("tenantId", 64))
	v := mysql.NewTenantView("v_docs_acme").On(plain).Axis(tenant).
		ForTenant("acme").DefinedBy(tvApp)

	got, _ := sqlOf(mysql.CreateTenantView(v))
	want := "CREATE DEFINER = `app`@`localhost` SQL SECURITY DEFINER VIEW `v_docs_acme` " +
		"AS SELECT `tenantId` FROM `docs` WHERE `tenantId` = 'acme' WITH CASCADED CHECK OPTION"
	if got != want {
		t.Errorf("SQL = %s\nwant  %s", got, want)
	}
}

// TestTenantViewShipsNoExecutionAndNoRevoke pins two decisions that are
// arguments rather than code, and that a later round could undo without
// any other test noticing.
//
// Both are documented at length in tenantview.go. A test is here
// because a doc comment does not fail:
//
//   - nothing in this file executes. The view and the grant are not
//     the boundary — the absence of a base-table grant is — so a
//     drops call that ran them would report success for something it
//     had not established and could not check.
//   - nothing in this file emits a REVOKE. It would read like drops
//     taking the base table away, and it is the wrong shape twice:
//     REVOKE answers ERROR 1147 when there is no grant to remove,
//     which is the state a correct deployment is already in, and the
//     REVOKE IF EXISTS that tolerates that was measured as MySQL-only
//     — MariaDB 10.11.14 answers ERROR 1064 to it.
func TestTenantViewShipsNoExecutionAndNoRevoke(t *testing.T) {
	src, err := os.ReadFile("tenantview.go")
	if err != nil {
		t.Fatalf("read tenantview.go: %v", err)
	}
	text := string(src)

	// Only the doc comments may say the word; no rendered statement may.
	for _, line := range strings.Split(text, "\n") {
		code := strings.TrimSpace(line)
		if strings.HasPrefix(code, "//") {
			continue
		}
		if strings.Contains(code, "REVOKE") {
			t.Errorf("tenantview.go renders a REVOKE:\n\t%s\n"+
				"REVOKE fails with 1147 where a correct deployment already is, and "+
				"REVOKE IF EXISTS does not exist in MariaDB", code)
		}
		for _, run := range []string{"ExecExpr(", "db.Exec(", "*DB)"} {
			if strings.Contains(code, run) {
				t.Errorf("tenantview.go reaches for execution (%s):\n\t%s\n"+
					"this file renders DDL and stops; the boundary is a grant state "+
					"drops cannot establish or verify", run, code)
			}
		}
	}
}
