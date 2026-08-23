package integration_test

import (
	"context"
	"testing"

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
