package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	mysqldriver "github.com/go-sql-driver/mysql"

	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/mysql"
)

// The claim mysql/tenantview.go makes, asked of a real server.
//
// [mysql.TenantView] renders DDL and stops, and the package doc calls
// the result a boundary in the sense this phase has used the word: it
// holds whatever SQL the application sends, because the privilege
// system enforces it rather than a predicate. That is a claim about a
// server, so a rendering test cannot settle it. This one runs the
// statements drops emits, then connects AS the tenant account and
// tries to get out.
//
// It was developed against MySQL 8.0.46 and MariaDB 10.11.14, and the
// sequence below passed identically on both. The one difference either
// server showed is in SQLSTATE and not in behaviour: MySQL reports
// ERROR 1369 with SQLSTATE HY000, MariaDB with 44000. The assertions
// below read the MySQL error NUMBER, which both agree on.
//
// This test needs an account that may CREATE USER and GRANT. Where the
// DSN's account cannot, the test skips rather than failing — except
// under DROPS_REQUIRE_ALL, where a skip would report green for the one
// claim in the package that most needs a server.

// mysqlErrNumber returns the server's error number, or 0.
func mysqlErrNumber(err error) uint16 {
	var me *mysqldriver.MySQLError
	if errors.As(err, &me) {
		return me.Number
	}
	return 0
}

const (
	errTableAccessDenied = 1142 // ER_TABLEACCESS_DENIED_ERROR
	errViewCheckFailed   = 1369 // ER_VIEW_CHECK_FAILED
)

// openMySQLAs reopens the configured server under a different account,
// which is the whole point: the boundary is a property of who the
// connection authenticated as.
func openMySQLAs(t *testing.T, user, password string) (*sql.DB, error) {
	t.Helper()
	cfg, err := mysqldriver.ParseDSN(integration.DSN(t, integration.EnvMySQL))
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.User, cfg.Passwd = user, password
	// A tenant account is granted nothing on the schema itself, so the
	// handshake must not try to select one.
	dbName := cfg.DBName
	cfg.DBName = ""
	sqlDB, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	if err := sqlDB.PingContext(context.Background()); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	_ = dbName
	return sqlDB, nil
}

// openMySQLRaw is the admin handle the assertions read ground truth
// through. It is a raw *sql.DB rather than a *mysql.DB because what it
// asks — SHOW GRANTS, CURRENT_USER, a count against the base table —
// is about the server's state and not about anything drops renders.
func openMySQLRaw(t *testing.T) *sql.DB {
	t.Helper()
	sqlDB, err := sql.Open("mysql", integration.DSN(t, integration.EnvMySQL))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := sqlDB.PingContext(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return sqlDB
}

// currentMySQLAccount reports the account the admin connection
// authenticated as, which is the account the view is defined by: it is
// the one demonstrably able to read the base table it just created.
func currentMySQLAccount(t *testing.T, db *sql.DB) (user, host string) {
	t.Helper()
	var cu string
	if err := db.QueryRowContext(context.Background(), "SELECT CURRENT_USER()").Scan(&cu); err != nil {
		t.Fatalf("CURRENT_USER: %v", err)
	}
	at := strings.LastIndex(cu, "@")
	if at < 0 {
		t.Fatalf("CURRENT_USER() = %q, want user@host", cu)
	}
	return cu[:at], cu[at+1:]
}

// assertNoBaseTableGrant is the assertion the whole mechanism rests on,
// and it is made explicitly rather than assumed.
//
// The view and the grant are not the boundary; the ABSENCE of a grant
// on the base table is. That absence is a property of the server's
// privilege state, so the test reads the state rather than trusting
// that it never issued the grant.
func assertNoBaseTableGrant(t *testing.T, db *sql.DB, acct, table string) {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		fmt.Sprintf("SHOW GRANTS FOR '%s'@'%%'", acct))
	if err != nil {
		t.Fatalf("SHOW GRANTS: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			t.Fatalf("scan grant: %v", err)
		}
		if strings.Contains(g, "`"+table+"`") {
			t.Fatalf("the tenant account holds a grant naming the base table, "+
				"so nothing below tests a boundary:\n%s", g)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("SHOW GRANTS: %v", err)
	}
}

// qualifiedNames are the two objects spelled as the tenant connection
// must spell them. That connection selects no default database, so
// every reference it makes is schema-qualified.
type qualifiedNames struct{ view, table string }

func qualify(t *testing.T, db *sql.DB, viewName, tableName string) qualifiedNames {
	t.Helper()
	var schema string
	if err := db.QueryRowContext(context.Background(), "SELECT DATABASE()").Scan(&schema); err != nil {
		t.Fatalf("SELECT DATABASE(): %v", err)
	}
	if schema == "" {
		t.Fatal("the configured DSN selects no database; this test needs one")
	}
	q := func(n string) string { return "`" + schema + "`.`" + n + "`" }
	return qualifiedNames{view: q(viewName), table: q(tableName)}
}

func TestMySQLTenantViewIsABoundary(t *testing.T) {
	admin := openMySQL(t)
	raw := openMySQLRaw(t)
	ctx := context.Background()

	tbl := mysql.NewTable(integration.UniqueName(t, "tvdocs"))
	id := mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	tenant := mysql.Add(tbl, mysql.Varchar("tenantId", 64).NotNull())
	body := mysql.Add(tbl, mysql.Text("body"))
	dropMySQL(t, admin, tbl)
	execMySQL(t, admin, mysql.CreateTable(tbl))

	for _, row := range []struct {
		tenant, body string
	}{{"acme", "a1"}, {"acme", "a2"}, {"globex", "g1"}} {
		if _, err := admin.Insert(tbl).
			Row(mysql.Bind(tenant, row.tenant), mysql.Bind(body, row.body)).
			Exec(ctx); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// The account the tenant connects as, and the account the view
	// runs as. The definer here is the DSN's own account: it is the one
	// that demonstrably can read the base table.
	acct := strings.ToLower(integration.UniqueName(t, "tv"))
	if len(acct) > 32 {
		acct = acct[:32]
	}
	const pw = "tvpw_9f3a"

	definerUser, definerHost := currentMySQLAccount(t, raw)
	viewName := integration.UniqueName(t, "v_acme")
	view := mysql.NewTenantView(viewName).
		On(tbl).
		Axis(tenant).
		ForTenant("acme").
		DefinedBy(mysql.Acct(definerUser, definerHost))
	if err := view.Err(); err != nil {
		t.Fatalf("view declaration: %v", err)
	}

	// CREATE USER is not something drops renders — an account is not a
	// schema object, and the file comment says why the grant state stays
	// the operator's. The test is the operator here.
	if _, err := raw.ExecContext(ctx,
		fmt.Sprintf("CREATE USER '%s'@'%%' IDENTIFIED BY '%s'", acct, pw)); err != nil {
		if integration.RequireAll() {
			t.Fatalf("CREATE USER: %v", err)
		}
		t.Skipf("the configured account cannot CREATE USER (%v); this test needs one that can", err)
	}
	t.Cleanup(func() {
		_, _ = raw.ExecContext(context.Background(), fmt.Sprintf("DROP USER IF EXISTS '%s'@'%%'", acct))
	})

	execMySQL(t, admin, mysql.CreateTenantView(view))
	t.Cleanup(func() {
		_, _ = admin.ExecExpr(context.Background(), mysql.DropTenantViewIfExists(view))
	})
	execMySQL(t, admin, mysql.GrantTenantView(view, mysql.Acct(acct, "%"),
		mysql.PrivSelect, mysql.PrivInsert, mysql.PrivUpdate, mysql.PrivDelete))

	// Nothing granted the account anything on the base table, and that
	// absence is the boundary. Assert the account holds what we think
	// it holds before trusting anything below.
	assertNoBaseTableGrant(t, raw, acct, tbl.Name())

	tenantConn, err := openMySQLAs(t, acct, pw)
	if err != nil {
		t.Fatalf("connect as the tenant account: %v", err)
	}
	qualified := qualify(t, raw, viewName, tbl.Name())

	t.Run("reads only its own tenant through the view", func(t *testing.T) {
		var n int
		if err := tenantConn.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM "+qualified.view).Scan(&n); err != nil {
			t.Fatalf("select through view: %v", err)
		}
		if n != 2 {
			t.Errorf("view returned %d rows, want the 2 belonging to acme", n)
		}
	})

	t.Run("cannot read the base table at all", func(t *testing.T) {
		_, err := tenantConn.QueryContext(ctx, "SELECT * FROM "+qualified.table)
		if got := mysqlErrNumber(err); got != errTableAccessDenied {
			t.Fatalf("base table SELECT gave error %d (%v), want %d command denied",
				got, err, errTableAccessDenied)
		}
	})

	t.Run("cannot insert a row belonging to another tenant", func(t *testing.T) {
		_, err := tenantConn.ExecContext(ctx,
			"INSERT INTO "+qualified.view+" (`tenantId`, `body`) VALUES ('globex', 'x')")
		if got := mysqlErrNumber(err); got != errViewCheckFailed {
			t.Fatalf("cross-tenant INSERT gave error %d (%v), want %d CHECK OPTION failed",
				got, err, errViewCheckFailed)
		}
	})

	t.Run("can insert a row belonging to itself", func(t *testing.T) {
		if _, err := tenantConn.ExecContext(ctx,
			"INSERT INTO "+qualified.view+" (`tenantId`, `body`) VALUES ('acme', 'a3')"); err != nil {
			t.Fatalf("own-tenant INSERT: %v", err)
		}
	})

	t.Run("cannot move a row out of its tenant", func(t *testing.T) {
		_, err := tenantConn.ExecContext(ctx,
			"UPDATE "+qualified.view+" SET `tenantId` = 'globex'")
		if got := mysqlErrNumber(err); got != errViewCheckFailed {
			t.Fatalf("tenant-moving UPDATE gave error %d (%v), want %d CHECK OPTION failed",
				got, err, errViewCheckFailed)
		}
	})

	t.Run("cannot touch another tenant's row", func(t *testing.T) {
		res, err := tenantConn.ExecContext(ctx,
			"UPDATE "+qualified.view+" SET `body` = 'hacked' WHERE `body` = 'g1'")
		if err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 0 {
			t.Errorf("UPDATE touched %d rows of another tenant, want 0", n)
		}
	})

	t.Run("an unqualified DELETE reaches only its own rows", func(t *testing.T) {
		if _, err := tenantConn.ExecContext(ctx, "DELETE FROM "+qualified.view); err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		// Read the ground truth as the admin, not through the view:
		// the view cannot see what it failed to delete, so asking it
		// would confirm the boundary using the boundary.
		var survivors int
		if err := raw.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM "+qualified.table+" WHERE `tenantId` = 'globex'").
			Scan(&survivors); err != nil {
			t.Fatalf("count survivors: %v", err)
		}
		if survivors != 1 {
			t.Errorf("globex has %d rows after the tenant deleted everything it could see, want 1", survivors)
		}
	})

	_ = id
}
