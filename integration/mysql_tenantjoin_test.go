package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"

	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/mysql"
)

// The tenant guard defect that only a server can show, because it
// loses rows rather than leaking them.
//
// mysql/tenant.go carried this as a known gap for several rounds: a
// scoped table joined BEFORE a RIGHT JOIN kept its guard in the WHERE
// clause, and the RIGHT JOIN NULL-extends everything to its left — so
// the guard was false for exactly the rows the RIGHT JOIN exists to
// preserve, and they came back missing. It was written down rather
// than fixed because moving where a guard lands is a change to the
// scoping mechanism, and that wanted a server to check it against.
//
// This is that check. A rendering test can say the predicate moved
// clause; only an engine can say the rows came back, and that moving
// it did not put another tenant's data in them.
//
// Measured on MySQL 8.0.46 and MariaDB 10.11.14, which agree row for
// row.
func TestMySQLGuardBeforeARightJoinKeepsThePreservedRows(t *testing.T) {
	db := openMySQL(t)
	ctx := mysql.WithTenant(context.Background(), "acme")

	// posts is the scoped table, joined INNER — the placement that used
	// to send its guard to the WHERE clause.
	posts := mysql.NewTable(integration.UniqueName(t, "rjposts"))
	postID := mysql.Add(posts, mysql.BigInt("id").PrimaryKey())
	postTenant := mysql.Add(posts, mysql.Varchar("tenantId", 32).NotNull())
	posts.ContextFilter(mysql.TenantFilter(postTenant))

	// links sits in the FROM clause and carries no axis of its own, so
	// every predicate in the statement can only have come from posts.
	links := mysql.NewTable(integration.UniqueName(t, "rjlinks"))
	linkPost := mysql.Add(links, mysql.BigInt("postId").NotNull())

	// audit is the preserved side: the RIGHT JOIN exists to return
	// every one of its rows, matched or not.
	audit := mysql.NewTable(integration.UniqueName(t, "rjaudit"))
	auditID := mysql.Add(audit, mysql.BigInt("id").PrimaryKey())
	auditPost := mysql.Add(audit, mysql.BigInt("postId").NotNull())

	for _, tbl := range []*mysql.Table{posts, links, audit} {
		dropMySQL(t, db, tbl)
		execMySQL(t, db, mysql.CreateTable(tbl))
	}

	// One post per tenant, a link to each, and three audit rows: one
	// reaching this tenant's post, one reaching the other tenant's, and
	// one reaching no post at all.
	if _, err := db.Insert(posts).
		Row(postID.Val(10), postTenant.Val("acme")).
		Row(postID.Val(20), postTenant.Val("globex")).
		Unscoped().Exec(context.Background()); err != nil {
		t.Fatalf("seed posts: %v", err)
	}
	if _, err := db.Insert(links).
		Row(linkPost.Val(10)).Row(linkPost.Val(20)).
		Exec(context.Background()); err != nil {
		t.Fatalf("seed links: %v", err)
	}
	if _, err := db.Insert(audit).
		Row(auditID.Val(100), auditPost.Val(10)).
		Row(auditID.Val(200), auditPost.Val(20)).
		Row(auditID.Val(300), auditPost.Val(99)).
		Exec(context.Background()); err != nil {
		t.Fatalf("seed audit: %v", err)
	}

	sel := db.Select(auditID, postTenant).
		From(links).
		Join(posts, mysql.Eq(linkPost, postID)).
		RightJoin(audit, mysql.Eq(linkPost, auditPost))

	text, args, err := sel.ToSQLCtx(ctx)
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	rows, err := sel.Rows(ctx)
	if err != nil {
		t.Fatalf("MySQL rejected the statement: %v\n%s\nargs: %v", err, text, args)
	}
	defer rows.Close()

	got := map[int64]string{}
	for rows.Next() {
		var id int64
		var tenant *string
		if err := rows.Scan(&id, &tenant); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if tenant == nil {
			got[id] = ""
			continue
		}
		got[id] = *tenant
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v\n%s", err, text)
	}

	// Every audit row survives — that is what RIGHT JOIN means, and
	// with the guard in the WHERE clause only the first one did.
	want := map[int64]string{
		100: "acme", // matched this tenant's post
		200: "",     // matched the other tenant's post: NULL-extended
		300: "",     // matched no post at all
	}
	if len(got) != len(want) {
		t.Errorf("%d rows came back, want %d — a guard on the nullable side of a RIGHT JOIN dropped the preserved rows\n%s\nargs: %v\ngot: %v",
			len(got), len(want), text, args, got)
	}
	for id, tenant := range want {
		g, ok := got[id]
		if !ok {
			t.Errorf("audit row %d is missing; the RIGHT JOIN exists to preserve it\n%s\nargs: %v", id, text, args)
			continue
		}
		if g != tenant {
			t.Errorf("audit row %d came back with tenant %q, want %q — %s", id, g, tenant,
				map[bool]string{true: "the other tenant's row must be NULL-extended, not shown", false: "this tenant's own row"}[tenant == ""])
		}
	}

	// And the guard is real: no row carries a tenant that is not ours.
	for id, tenant := range got {
		if tenant != "" && tenant != "acme" {
			t.Errorf("audit row %d exposed tenant %q\n%s\nargs: %v", id, tenant, text, args)
		}
	}
}

// Why mysql.TenantView REFUSES a tenant value containing a backslash
// rather than escaping one.
//
// tenantLiteral's doc calls this the sharpest edge in that file, and
// until now it was the sharpest edge with no runnable test: a view
// body cannot carry a placeholder, so the tenant value is TEXT inside
// stored DDL, and drops renders that text for an operator to apply
// later against a server whose sql_mode drops never saw. A single
// quote is safe because doubling it closes the literal correctly in
// every mode. A backslash has no such spelling.
//
// The statement text here is byte-identical between the two views. The
// only difference is the sql_mode in force when each was installed —
// and they end up scoped to two DIFFERENT tenants. A boundary whose
// predicate depends on a server setting the renderer cannot see is
// not a boundary, which is why the refusal is a refusal and not an
// escape.
//
// Measured on MySQL 8.0.46 and MariaDB 10.11.14, which agree exactly.
// The values are VARBINARY and compared as hex, because a client that
// escapes backslashes on the way out is the reason this is easy to
// measure wrong.
func TestMySQLABackslashInAViewLiteralMeansTwoThings(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()

	tbl := integration.UniqueName(t, "bsdocs")
	quoted := mysql.Dialect.QuoteIdent(tbl)
	v1, v2 := mysql.Dialect.QuoteIdent(tbl+"_a"), mysql.Dialect.QuoteIdent(tbl+"_b")
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), "DROP VIEW IF EXISTS "+v1)
		_, _ = db.Exec(context.Background(), "DROP VIEW IF EXISTS "+v2)
		_, _ = db.Exec(context.Background(), "DROP TABLE IF EXISTS "+quoted)
	})

	for _, stmt := range []string{
		"DROP TABLE IF EXISTS " + quoted,
		"CREATE TABLE " + quoted + " (`id` INT PRIMARY KEY, `tenantId` VARBINARY(16) NOT NULL)",
		// 61 5C 62 is a\b with ONE backslash; 61 5C 5C 62 has two.
		// Written as hex so no sql_mode can reinterpret the seed.
		"INSERT INTO " + quoted + " VALUES (1, UNHEX('615C62')), (2, UNHEX('615C5C62'))",
	} {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	// One statement text, installed twice. This is exactly what
	// mysql.CreateTenantView would render for a tenant whose value
	// contains a backslash, if tenantLiteral let it.
	body := " AS SELECT `id` FROM " + quoted + " WHERE `tenantId` = 'a\\\\b'"

	// The session sql_mode has to be set on the connection the CREATE
	// VIEW runs on, so both go through one pinned pool.
	pinned, _ := openMySQLPinnedConn(t)
	var restore string
	if err := pinned.Select(drops.Raw("@@session.sql_mode")).One(ctx, &restore); err != nil {
		t.Fatalf("read sql_mode: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pinned.Exec(context.Background(), "SET @@session.sql_mode = ?", restore)
	})

	if _, err := pinned.Exec(ctx, "SET @@session.sql_mode = ?", restore); err != nil {
		t.Fatalf("set default mode: %v", err)
	}
	if _, err := pinned.Exec(ctx, "CREATE VIEW "+v1+body); err != nil {
		t.Fatalf("create the default-mode view: %v", err)
	}
	if _, err := pinned.Exec(ctx, "SET @@session.sql_mode = ?", restore+",NO_BACKSLASH_ESCAPES"); err != nil {
		t.Fatalf("set NO_BACKSLASH_ESCAPES: %v", err)
	}
	if _, err := pinned.Exec(ctx, "CREATE VIEW "+v2+body); err != nil {
		t.Fatalf("create the NO_BACKSLASH_ESCAPES view: %v", err)
	}
	if _, err := pinned.Exec(ctx, "SET @@session.sql_mode = ?", restore); err != nil {
		t.Fatalf("restore sql_mode: %v", err)
	}

	ids := func(view string) []int64 {
		t.Helper()
		var out []int64
		rows, err := db.Select(drops.Raw("`id`")).FromExpr(drops.Raw(view)).Rows(ctx)
		if err != nil {
			t.Fatalf("select from %s: %v", view, err)
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		return out
	}

	// Row 1 is the one-backslash tenant, row 2 the two-backslash one.
	// Same text in, different tenant out.
	if got := ids(v1); len(got) != 1 || got[0] != 1 {
		t.Errorf("the view installed under the default sql_mode selects %v, want row 1 (the one-backslash tenant)", got)
	}
	if got := ids(v2); len(got) != 1 || got[0] != 2 {
		t.Errorf("the view installed under NO_BACKSLASH_ESCAPES selects %v, want row 2 (the two-backslash tenant)", got)
	}

	// And the refusal that makes the above unreachable through drops.
	base := mysql.NewTable(tbl)
	mysql.Add(base, mysql.BigInt("id").PrimaryKey())
	axis := mysql.Add(base, mysql.Varchar("tenantId", 16).NotNull())
	sound := mysql.NewTenantView("v_bs_ok").On(base).Axis(axis).
		ForTenant("acme").DefinedBy(mysql.Acct("app", "localhost"))
	if err := sound.Err(); err != nil {
		t.Fatalf("the same declaration without a backslash has to be accepted, "+
			"or the refusal below proves nothing: %v", err)
	}
	bad := mysql.NewTenantView("v_bs").On(base).Axis(axis).
		ForTenant(`a\b`).DefinedBy(mysql.Acct("app", "localhost"))
	if err := bad.Err(); err == nil {
		t.Error("TenantView accepted a tenant value carrying a backslash; " +
			"the two views above are what that renders into")
	} else if !strings.Contains(err.Error(), "backslash") {
		t.Errorf("refused for a reason other than the backslash: %v", err)
	}
}
