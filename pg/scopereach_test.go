package pg_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/pg"
)

// The shapes tenant scoping did not reach.
//
// P0-1 says every statement that reads a scoped table takes the tenant
// from ctx and carries the predicate. resolveCtx made that true of the
// FROM table and of a set operation's operands, and of nothing else, so
// three ways of naming a table in a SELECT went unfiltered — a CTE
// body, a joined table — and one, an aliased FROM, started rendering a
// predicate PostgreSQL cannot resolve.
//
// Every assertion here is on rendered SQL for the same reason
// scoping_test.go gives: the failure being fixed is a missing or
// misplaced WHERE clause, and a round trip against a single-tenant
// fixture passes straight through all of it.

const reachTenant = int64(7)

func reachCtx() context.Context { return pg.WithTenant(context.Background(), reachTenant) }

// reachTable builds a table with both kinds of automatic predicate: a
// soft-delete DefaultFilter resolved at render time and a tenant
// ContextFilter resolved by the executor.
func reachTable(name string) *pg.Table {
	t := pg.NewTable(name)
	pg.Add(t, pg.BigSerial("id").PrimaryKey())
	tenant := pg.Add(t, pg.BigInt("tenantId").NotNull())
	pg.ApplyMixins(t, &pg.SoftDeleteMixin{})
	t.ContextFilter(pg.TenantFilter(tenant))
	return t
}

// ----------------------------------------------------------------------
// A CTE body is a statement and is scoped like one
// ----------------------------------------------------------------------

// A CTE body used to render through WriteSQL, which has no ctx, so
// WITH recent AS (SELECT * FROM posts) read every tenant's posts while
// the outer SELECT carried its own tenant predicate — the worst shape
// to lose, since WITH ... AS (SELECT ... FROM t) is how a multi-tenant
// analytics query gets written in the first place.
func TestCTEBodyCarriesTheTenantAxis(t *testing.T) {
	users, posts := reachTable("c_users"), reachTable("c_posts")
	db := pg.New(nil)

	tests := []struct {
		name string
		sel  func() *pg.SelectBuilder
		want string
	}{
		{
			name: "With",
			sel: func() *pg.SelectBuilder {
				recent := pg.CTEDef("recent", db.Select().From(posts))
				return db.Select().With(recent).From(users)
			},
			want: `WITH "recent" AS (SELECT * FROM "c_posts" WHERE ("c_posts"."deletedAt" IS NULL) AND ("c_posts"."tenantId" = $1)) ` +
				`SELECT * FROM "c_users" WHERE ("c_users"."deletedAt" IS NULL) AND ("c_users"."tenantId" = $2)`,
		},
		{
			name: "WithRecursive",
			sel: func() *pg.SelectBuilder {
				tree := pg.CTEDef("tree", db.Select().From(posts))
				return db.Select().WithRecursive(tree).From(users)
			},
			want: `WITH RECURSIVE "tree" AS (SELECT * FROM "c_posts" WHERE ("c_posts"."deletedAt" IS NULL) AND ("c_posts"."tenantId" = $1)) ` +
				`SELECT * FROM "c_users" WHERE ("c_users"."deletedAt" IS NULL) AND ("c_users"."tenantId" = $2)`,
		},
		{
			// A body the executor cannot resolve stays the caller's
			// problem, and says so by rendering exactly what it was
			// given rather than half a predicate.
			name: "raw body is left alone",
			sel: func() *pg.SelectBuilder {
				raw := pg.CTEDef("raw", drops.Raw(`SELECT 1`))
				return db.Select().With(raw).From(users)
			},
			want: `WITH "raw" AS (SELECT 1) ` +
				`SELECT * FROM "c_users" WHERE ("c_users"."deletedAt" IS NULL) AND ("c_users"."tenantId" = $1)`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := tc.sel().ToSQLCtx(reachCtx())
			if err != nil {
				t.Fatalf("ToSQLCtx: %v", err)
			}
			if got != tc.want {
				t.Errorf("got = %v, want %v", got, tc.want)
			}
		})
	}
}

// A CTE body that refuses fails the whole statement, because the
// alternative is a WITH clause that spans every tenant feeding a SELECT
// that does not.
func TestCTEBodyWithNoTenantRefusesTheStatement(t *testing.T) {
	posts := reachTable("cm_posts")
	db := pg.New(nil)
	sel := db.Select().With(pg.CTEDef("recent", db.Select().From(posts))).FromExpr(drops.Raw(`"recent"`))

	if _, _, err := sel.ToSQLCtx(context.Background()); !errors.Is(err, pg.ErrTenantMissing) {
		t.Errorf("got = %v, want %v", err, pg.ErrTenantMissing)
	}
}

// Resolving a CTE must not write the resolved body back into the *CTE
// the caller holds: one is a value meant to be attached to more than
// one query, and a body pinned to the first request's tenant would
// answer every later statement with it.
func TestCTEDefIsReusableAcrossRequests(t *testing.T) {
	posts := reachTable("cr_posts")
	db := pg.New(nil)
	recent := pg.CTEDef("recent", db.Select().From(posts))

	first, args, err := db.Select().With(recent).FromExpr(drops.Raw(`"recent"`)).ToSQLCtx(reachCtx())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	second, args2, err := db.Select().With(recent).FromExpr(drops.Raw(`"recent"`)).
		ToSQLCtx(pg.WithTenant(context.Background(), int64(9)))
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	if first != second {
		t.Errorf("got = %v, want %v", second, first)
	}
	if len(args) != 1 || len(args2) != 1 {
		t.Fatalf("got = %v / %v, want one bound tenant each", args, args2)
	}
	if args[0] != any(reachTenant) || args2[0] != any(int64(9)) {
		t.Errorf("got = %v / %v, want %v / %v", args[0], args2[0], reachTenant, int64(9))
	}
}

// ----------------------------------------------------------------------
// A joined table brings its own predicates, in the clause its kind allows
// ----------------------------------------------------------------------

// writeCore rendered s.joins as bare relation names: neither the
// joined table's default filters nor its context filters appeared
// anywhere in the statement. Whether a foreign row then came back
// depended on the join key happening to be unique per tenant.
func TestJoinedTableCarriesItsFilters(t *testing.T) {
	users, posts := reachTable("j_users"), reachTable("j_posts")
	on := pg.Eq(users.Col("id"), posts.Col("id"))
	db := pg.New(nil)

	tests := []struct {
		name string
		join func(*pg.SelectBuilder) *pg.SelectBuilder
		want string
	}{
		{
			// Matched pairs only, so ON and WHERE select the same
			// rows; the WHERE clause keeps every restriction together.
			name: "inner join filters in the WHERE clause",
			join: func(s *pg.SelectBuilder) *pg.SelectBuilder { return s.Join(posts, on) },
			want: `SELECT * FROM "j_users" INNER JOIN "j_posts" ON ("j_users"."id" = "j_posts"."id") ` +
				`WHERE ("j_users"."deletedAt" IS NULL) AND ("j_posts"."deletedAt" IS NULL) ` +
				`AND ("j_users"."tenantId" = $1) AND ("j_posts"."tenantId" = $2)`,
		},
		{
			// The joined side is nullable: the same predicates in the
			// WHERE clause would drop every parent with no matching
			// child and turn this into an inner join.
			name: "left join filters in the ON clause",
			join: func(s *pg.SelectBuilder) *pg.SelectBuilder { return s.LeftJoin(posts, on) },
			want: `SELECT * FROM "j_users" LEFT JOIN "j_posts" ` +
				`ON (("j_users"."id" = "j_posts"."id") AND ("j_posts"."deletedAt" IS NULL) AND ("j_posts"."tenantId" = $1)) ` +
				`WHERE ("j_users"."deletedAt" IS NULL) AND ("j_users"."tenantId" = $2)`,
		},
		{
			// The joined side is the preserved one, so the predicates
			// go back into the WHERE clause — in the ON clause an
			// unmatched row of another tenant would survive the join.
			name: "right join filters in the WHERE clause",
			join: func(s *pg.SelectBuilder) *pg.SelectBuilder { return s.RightJoin(posts, on) },
			want: `SELECT * FROM "j_users" RIGHT JOIN "j_posts" ON ("j_users"."id" = "j_posts"."id") ` +
				`WHERE ("j_users"."deletedAt" IS NULL) AND ("j_posts"."deletedAt" IS NULL) ` +
				`AND ("j_users"."tenantId" = $1) AND ("j_posts"."tenantId" = $2)`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := tc.join(db.Select().From(users)).ToSQLCtx(reachCtx())
			if err != nil {
				t.Fatalf("ToSQLCtx: %v", err)
			}
			if got != tc.want {
				t.Errorf("got = %v, want %v", got, tc.want)
			}
		})
	}
}

// A join with no ON condition still has to carry the joined table's
// predicates, and must not render a dangling AND to do it.
func TestJoinWithNoConditionStillCarriesFilters(t *testing.T) {
	users, posts := reachTable("jn_users"), reachTable("jn_posts")
	db := pg.New(nil)

	got, _, err := db.Select().From(users).LeftJoin(posts, nil).ToSQLCtx(reachCtx())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	want := `SELECT * FROM "jn_users" LEFT JOIN "jn_posts" ` +
		`ON (("jn_posts"."deletedAt" IS NULL) AND ("jn_posts"."tenantId" = $1)) ` +
		`WHERE ("jn_users"."deletedAt" IS NULL) AND ("jn_users"."tenantId" = $2)`
	if got != want {
		t.Errorf("got = %v, want %v", got, want)
	}
}

// A FULL JOIN preserves both sides, so a predicate on the joined table
// is wrong in the ON clause (its unmatched rows survive unfiltered) and
// wrong in the WHERE clause (the FROM table's unmatched rows are
// dropped). A row-visibility boundary with nowhere correct to go is
// refused rather than placed wrongly.
func TestFullJoinOnScopedTableIsRefused(t *testing.T) {
	users, posts := reachTable("f_users"), reachTable("f_posts")
	db := pg.New(nil)
	on := pg.Eq(users.Col("id"), posts.Col("id"))

	_, _, err := db.Select().From(users).FullJoin(posts, on).ToSQLCtx(reachCtx())
	if !errors.Is(err, pg.ErrFullJoinScoped) {
		t.Fatalf("got = %v, want %v", err, pg.ErrFullJoinScoped)
	}
	if !strings.Contains(err.Error(), `"f_posts"`) {
		t.Errorf("got = %v, want the refusal to name the table", err)
	}

	// Unscoped is the documented way through, and it has to actually
	// produce a statement rather than the same refusal.
	got, _, err := db.Select().From(users).FullJoin(posts, on).Unscoped().ToSQLCtx(reachCtx())
	if err != nil {
		t.Fatalf("Unscoped full join: %v", err)
	}
	want := `SELECT * FROM "f_users" FULL JOIN "f_posts" ON ("f_users"."id" = "f_posts"."id")`
	if got != want {
		t.Errorf("got = %v, want %v", got, want)
	}
}

// Unscoped is statement-wide: a joined table's automatic predicates go
// with the FROM table's, or the caller who asked to see everything gets
// a silently narrowed answer instead.
func TestUnscopedReachesJoinedTables(t *testing.T) {
	users, posts := reachTable("u_users"), reachTable("u_posts")
	db := pg.New(nil)

	got, _, err := db.Select().From(users).
		Join(posts, pg.Eq(users.Col("id"), posts.Col("id"))).
		Unscoped().ToSQLCtx(context.Background())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	want := `SELECT * FROM "u_users" INNER JOIN "u_posts" ON ("u_users"."id" = "u_posts"."id")`
	if got != want {
		t.Errorf("got = %v, want %v", got, want)
	}
}

// A join onto a scoped table with no tenant on ctx fails closed, the
// same way the FROM table does. The join is where a caller is most
// likely to reach a scoped table without meaning to.
func TestJoinedTableFailsClosedWithNoTenant(t *testing.T) {
	users := pg.NewTable("jf_users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	posts := reachTable("jf_posts")
	db := pg.New(nil)

	_, _, err := db.Select().From(users).
		Join(posts, pg.Eq(users.Col("id"), posts.Col("id"))).
		ToSQLCtx(context.Background())
	if !errors.Is(err, pg.ErrTenantMissing) {
		t.Errorf("got = %v, want %v", err, pg.ErrTenantMissing)
	}
}

// ----------------------------------------------------------------------
// An aliased table is the same table, predicates included
// ----------------------------------------------------------------------

// As copies the filter list verbatim and every filter this package
// registers closes over the declared column, so an aliased query
// rendered "notes"."tenantId" against FROM "notes" AS "n" — 42P01, a
// query that cannot run at all. Before the tenant axis moved onto the
// table such a query ran (unscoped); afterwards every one of them
// errored, which made the one table shape that must never lose its axis
// the one shape that could not be queried under an alias.
func TestAliasedScopedTableQualifiesWithTheAlias(t *testing.T) {
	notes := reachTable("a_notes")
	n := notes.As("n")
	db := pg.New(nil)

	tests := []struct {
		name string
		sel  func() *pg.SelectBuilder
		want string
	}{
		{
			name: "aliased FROM",
			sel:  func() *pg.SelectBuilder { return db.Select().From(n) },
			want: `SELECT * FROM "a_notes" AS "n" WHERE ("n"."deletedAt" IS NULL) AND ("n"."tenantId" = $1)`,
		},
		{
			name: "aliased join",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(notes).Join(n, pg.Eq(notes.Col("id"), n.Col("id")))
			},
			want: `SELECT * FROM "a_notes" INNER JOIN "a_notes" AS "n" ON ("a_notes"."id" = "n"."id") ` +
				`WHERE ("a_notes"."deletedAt" IS NULL) AND ("n"."deletedAt" IS NULL) ` +
				`AND ("a_notes"."tenantId" = $1) AND ("n"."tenantId" = $2)`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := tc.sel().ToSQLCtx(reachCtx())
			if err != nil {
				t.Fatalf("ToSQLCtx: %v", err)
			}
			if got != tc.want {
				t.Errorf("got = %v, want %v", got, tc.want)
			}
		})
	}
}

// The rename is scoped to the predicate that installed it: a reference
// to another table inside a filter, and every reference the caller
// wrote outside one, render as they always did.
func TestAliasRenameStopsAtTheFilter(t *testing.T) {
	notes := reachTable("ar_notes")
	other := pg.NewTable("ar_other")
	otherID := pg.Add(other, pg.BigSerial("id").PrimaryKey())
	notes.ContextFilter(func(context.Context) (drops.Expression, error) {
		return pg.Eq(otherID, 1), nil
	})
	n := notes.As("n")
	db := pg.New(nil)

	got, _, err := db.Select().From(n).Where(pg.Eq(otherID, 2)).ToSQLCtx(reachCtx())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	want := `SELECT * FROM "ar_notes" AS "n" WHERE ("n"."deletedAt" IS NULL) ` +
		`AND ("ar_other"."id" = $1) AND ("n"."tenantId" = $2) AND ("ar_other"."id" = $3)`
	if got != want {
		t.Errorf("got = %v, want %v", got, want)
	}
}

// The base table keeps rendering as itself once the alias's predicate
// has been written — the rename is restored, not left installed.
func TestAliasRenameIsRestoredAfterEachFilter(t *testing.T) {
	notes := reachTable("ax_notes")
	n := notes.As("n")
	db := pg.New(nil)

	if _, _, err := db.Select().From(n).ToSQLCtx(reachCtx()); err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	got, _, err := db.Select().From(notes).ToSQLCtx(reachCtx())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	want := `SELECT * FROM "ax_notes" WHERE ("ax_notes"."deletedAt" IS NULL) AND ("ax_notes"."tenantId" = $1)`
	if got != want {
		t.Errorf("got = %v, want %v", got, want)
	}
}

// ----------------------------------------------------------------------
// Registering a filter while queries are running
// ----------------------------------------------------------------------

// The ctxFilter key exists so that an entity built inside a request
// handler replaces its own filter instead of stacking one per request.
// That makes the pattern supported, and the pattern registers onto a
// table other goroutines are querying at that moment: an unsynchronised
// append against a read from every executor in flight. Run under -race,
// this reproduced it.
func TestContextFilterRegistrationRacesWithQueries(t *testing.T) {
	shared := reachTable("race_rows")
	drv := dropstest.New().Rows([]string{"id"}, []any{int64(1)})
	db := pg.New(drv)
	ctx := reachCtx()

	const workers = 8
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				// An entity rebuilt per request, as the ContextFilter
				// documentation invites: both registrations are keyed,
				// so the list stays two long however many run.
				e := pg.NewEntity[raceRow](shared).ScopeByTenant(shared.Col("tenantId"))
				e.AuthorizeWith(nil)
				// And a keyless one, which always appends — the write
				// that moves the slice out from under a reader.
				shared.As("copy")
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				rows, err := db.Select().From(shared).Rows(ctx)
				if err != nil {
					t.Errorf("Rows: %v", err)
					return
				}
				rows.Close()
			}
		}()
	}
	wg.Wait()
}

type raceRow struct {
	ID       int64 `drop:"id"`
	TenantID int64 `drop:"tenantId"`
}
