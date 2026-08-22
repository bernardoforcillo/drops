package sqlite_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/sqlite"
)

// What these assert is the TEXT of a statement an operator applies to
// a database and then stops looking at. A tenant guard that renders a
// WHEN clause one operator away from what it claims is a trigger that
// fires on the wrong rows for the life of the schema, so every case
// here pins the whole statement rather than a substring of it.
//
// The behaviour these renderings have against a real engine is settled
// in integration/sqlite_tenantguard_test.go, which runs in-process
// through modernc.org/sqlite and needs no container.

// guardTable builds a table with a tenant axis and a foreign key
// column, the shape every case below renders against.
func guardTable(name string) (*sqlite.Table, *sqlite.Col[string], *sqlite.Col[int64]) {
	t := sqlite.NewTable(name)
	sqlite.Add(t, sqlite.BigInt("id").PrimaryKey())
	tenant := sqlite.Add(t, sqlite.Text("tenantId").NotNull())
	author := sqlite.Add(t, sqlite.BigInt("authorId"))
	t.ScopeWritesByTenant(tenant)
	return t, tenant, author
}

func renderGuard(t *testing.T, exprs []drops.Expression) []string {
	t.Helper()
	out := make([]string, 0, len(exprs))
	for _, e := range exprs {
		sql, args := drops.StringWithDialect(sqlite.Dialect, e)
		if len(args) != 0 {
			t.Errorf("a guard statement bound %d args; a trigger body "+
				"cannot carry a placeholder, so every value in it must "+
				"render as a literal: %s %v", len(args), sql, args)
		}
		out = append(out, sql)
	}
	return out
}

func checkStatements(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("rendered %d statements, want %d:\n got: %s\nwant: %s",
			len(got), len(want), strings.Join(got, "\n      "), strings.Join(want, "\n      "))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("statement %d:\n got: %s\nwant: %s", i, got[i], want[i])
		}
	}
}

// The guard a table's own declared axis generates: nothing may write
// the axis as NULL, and nothing may move a row between tenants.
func TestTenantGuardRendersTheAxisTriggers(t *testing.T) {
	posts, _, _ := guardTable("tg_posts")

	got := renderGuard(t, sqlite.CreateTenantGuard(sqlite.TenantGuardFor(posts)))
	checkStatements(t, got, []string{
		`CREATE TRIGGER "tg_posts_tenantGuard_ins" BEFORE INSERT ON "tg_posts" ` +
			`FOR EACH ROW WHEN NEW."tenantId" IS NULL ` +
			`BEGIN SELECT RAISE(ABORT, 'drops/sqlite: "tg_posts"."tenantId" is null; ` +
			`the row would belong to no tenant'); END`,
		`CREATE TRIGGER "tg_posts_tenantGuard_upd" BEFORE UPDATE OF "tenantId" ON "tg_posts" ` +
			`FOR EACH ROW WHEN NEW."tenantId" IS NOT OLD."tenantId" ` +
			`BEGIN SELECT RAISE(ABORT, 'drops/sqlite: "tg_posts"."tenantId" is immutable; ` +
			`this statement would move a row between tenants'); END`,
	})
}

// The IF NOT EXISTS variant, which is what a migration re-run needs.
func TestTenantGuardRendersIfNotExists(t *testing.T) {
	posts, _, _ := guardTable("tg_ine")

	got := renderGuard(t, sqlite.CreateTenantGuardIfNotExists(sqlite.TenantGuardFor(posts)))
	for _, sql := range got {
		if !strings.HasPrefix(sql, `CREATE TRIGGER IF NOT EXISTS "tg_ine_tenantGuard_`) {
			t.Errorf("statement does not carry IF NOT EXISTS: %s", sql)
		}
	}
	if len(got) != 2 {
		t.Fatalf("rendered %d statements, want 2", len(got))
	}
}

// A guard pinned to one tenant: the file-per-tenant deployment, where
// the identity is the database file and the axis may hold exactly one
// value. The pin subsumes both axis triggers, so it replaces them
// rather than adding to them.
func TestTenantGuardPinnedToOneTenantReplacesTheAxisTriggers(t *testing.T) {
	posts, _, _ := guardTable("tg_pin")

	got := renderGuard(t, sqlite.CreateTenantGuard(sqlite.TenantGuardFor(posts).PinnedTo("acme")))
	checkStatements(t, got, []string{
		`CREATE TRIGGER "tg_pin_tenantGuard_ins" BEFORE INSERT ON "tg_pin" ` +
			`FOR EACH ROW WHEN NEW."tenantId" IS NOT 'acme' ` +
			`BEGIN SELECT RAISE(ABORT, 'drops/sqlite: "tg_pin"."tenantId" must be ''acme'' ` +
			`in this database'); END`,
		`CREATE TRIGGER "tg_pin_tenantGuard_upd" BEFORE UPDATE OF "tenantId" ON "tg_pin" ` +
			`FOR EACH ROW WHEN NEW."tenantId" IS NOT 'acme' ` +
			`BEGIN SELECT RAISE(ABORT, 'drops/sqlite: "tg_pin"."tenantId" must be ''acme'' ` +
			`in this database'); END`,
	})
}

// A tenant value carrying a quote closes its literal by doubling. The
// case exists because the value lands inside stored DDL: a literal
// that ends early is a trigger whose WHEN clause is not the one drops
// described, and SQLite would accept it.
func TestTenantGuardDoublesAQuoteInThePinnedValue(t *testing.T) {
	posts, _, _ := guardTable("tg_quote")

	got := renderGuard(t, sqlite.CreateTenantGuard(sqlite.TenantGuardFor(posts).PinnedTo("o'brien")))
	if !strings.Contains(got[0], `IS NOT 'o''brien'`) {
		t.Errorf("quote not doubled in the WHEN clause: %s", got[0])
	}
	if !strings.Contains(got[0], `must be ''o''''brien'' in this database`) {
		t.Errorf("quote not doubled in the RAISE message: %s", got[0])
	}
}

// An integer tenant renders as a numeric literal rather than a quoted
// one: SQLite compares 'acme' and "acme" by the column's affinity, and
// a quoted 42 against an INTEGER axis is a comparison that never
// matches.
func TestTenantGuardRendersAnIntegerTenantUnquoted(t *testing.T) {
	posts, _, _ := guardTable("tg_int")

	got := renderGuard(t, sqlite.CreateTenantGuard(sqlite.TenantGuardFor(posts).PinnedTo(int64(42))))
	if !strings.Contains(got[0], `IS NOT 42 `) {
		t.Errorf("integer tenant not rendered as a numeric literal: %s", got[0])
	}
}

// The cross-table guard: a row's axis must agree with the axis of the
// row it points at. This is the wrong-tenant write no predicate can
// catch, because every statement involved is correctly scoped to the
// tenant on the ctx — it is the FK that names another tenant's row.
func TestTenantGuardRendersTheParentAgreementTriggers(t *testing.T) {
	posts, _, author := guardTable("tg_child")
	users := sqlite.NewTable("tg_parent")
	userID := sqlite.Add(users, sqlite.BigInt("id").PrimaryKey())
	userTenant := sqlite.Add(users, sqlite.Text("tenantId").NotNull())

	g := sqlite.TenantGuardFor(posts).MatchingParent(author, userID, userTenant)
	got := renderGuard(t, sqlite.CreateTenantGuard(g))
	checkStatements(t, got, []string{
		`CREATE TRIGGER "tg_child_tenantGuard_ins" BEFORE INSERT ON "tg_child" ` +
			`FOR EACH ROW WHEN NEW."tenantId" IS NULL ` +
			`BEGIN SELECT RAISE(ABORT, 'drops/sqlite: "tg_child"."tenantId" is null; ` +
			`the row would belong to no tenant'); END`,
		`CREATE TRIGGER "tg_child_tenantGuard_upd" BEFORE UPDATE OF "tenantId" ON "tg_child" ` +
			`FOR EACH ROW WHEN NEW."tenantId" IS NOT OLD."tenantId" ` +
			`BEGIN SELECT RAISE(ABORT, 'drops/sqlite: "tg_child"."tenantId" is immutable; ` +
			`this statement would move a row between tenants'); END`,
		`CREATE TRIGGER "tg_child_tenantGuard_authorId_ins" BEFORE INSERT ON "tg_child" ` +
			`FOR EACH ROW WHEN NEW."tenantId" IS NOT ` +
			`(SELECT "tenantId" FROM "tg_parent" WHERE "id" IS NEW."authorId") ` +
			`BEGIN SELECT RAISE(ABORT, 'drops/sqlite: "tg_child"."tenantId" disagrees with ` +
			`"tg_parent"."tenantId" for the row "tg_child"."authorId" names'); END`,
		`CREATE TRIGGER "tg_child_tenantGuard_authorId_upd" ` +
			`BEFORE UPDATE OF "tenantId", "authorId" ON "tg_child" ` +
			`FOR EACH ROW WHEN NEW."tenantId" IS NOT ` +
			`(SELECT "tenantId" FROM "tg_parent" WHERE "id" IS NEW."authorId") ` +
			`BEGIN SELECT RAISE(ABORT, 'drops/sqlite: "tg_child"."tenantId" disagrees with ` +
			`"tg_parent"."tenantId" for the row "tg_child"."authorId" names'); END`,
	})
}

// DROP runs the triggers off in the reverse of the order CREATE put
// them on, so a half-applied migration rolls back in the order it was
// written.
func TestTenantGuardDropsInReverseOrder(t *testing.T) {
	posts, _, author := guardTable("tg_drop")
	users := sqlite.NewTable("tg_drop_parent")
	userID := sqlite.Add(users, sqlite.BigInt("id").PrimaryKey())
	userTenant := sqlite.Add(users, sqlite.Text("tenantId").NotNull())

	g := sqlite.TenantGuardFor(posts).MatchingParent(author, userID, userTenant)
	got := renderGuard(t, sqlite.DropTenantGuardIfExists(g))
	checkStatements(t, got, []string{
		`DROP TRIGGER IF EXISTS "tg_drop_tenantGuard_authorId_upd"`,
		`DROP TRIGGER IF EXISTS "tg_drop_tenantGuard_authorId_ins"`,
		`DROP TRIGGER IF EXISTS "tg_drop_tenantGuard_upd"`,
		`DROP TRIGGER IF EXISTS "tg_drop_tenantGuard_ins"`,
	})
}

// A DROP for a declaration that never validated still renders, because
// rolling back a half-written migration is exactly when the
// declaration is incomplete.
func TestTenantGuardDropsWithoutValidating(t *testing.T) {
	g := sqlite.NewTenantGuard("tg_bare")

	got := renderGuard(t, sqlite.DropTenantGuardIfExists(g))
	checkStatements(t, got, []string{
		`DROP TRIGGER IF EXISTS "tg_bare_upd"`,
		`DROP TRIGGER IF EXISTS "tg_bare_ins"`,
	})
}

// Every way a declaration can be wrong, and the error it reports. A
// guard whose declaration is refused must not render a trigger: an
// incomplete WHEN clause is a trigger SQLite accepts and that fires on
// rows nobody meant to name.
func TestTenantGuardRefusesIncompleteDeclarations(t *testing.T) {
	posts, tenant, author := guardTable("tg_bad")
	stranger := sqlite.NewTable("tg_stranger")
	strangerTenant := sqlite.Add(stranger, sqlite.Text("tenantId").NotNull())
	strangerID := sqlite.Add(stranger, sqlite.BigInt("id").PrimaryKey())

	unscoped := sqlite.NewTable("tg_unscoped")
	sqlite.Add(unscoped, sqlite.BigInt("id").PrimaryKey())

	tests := []struct {
		name string
		g    *sqlite.TenantGuard
		want error
	}{
		{
			name: "no table",
			g:    sqlite.NewTenantGuard("tg_none"),
			want: sqlite.ErrTenantGuardTargetRequired,
		},
		{
			name: "no axis",
			g:    sqlite.NewTenantGuard("tg_noaxis").On(posts),
			want: sqlite.ErrTenantGuardAxisRequired,
		},
		{
			name: "table declares no write axis",
			g:    sqlite.TenantGuardFor(unscoped),
			want: sqlite.ErrTenantGuardAxisRequired,
		},
		{
			name: "axis belongs to another table",
			g:    sqlite.NewTenantGuard("tg_foreign").On(posts).Axis(strangerTenant),
			want: sqlite.ErrTenantGuardAxisNotInTable,
		},
		{
			name: "pinned to a nil of some type",
			g:    sqlite.TenantGuardFor(posts).PinnedTo((*string)(nil)),
			want: sqlite.ErrTenantGuardTenantRequired,
		},
		{
			name: "pinned to a float",
			g:    sqlite.TenantGuardFor(posts).PinnedTo(1.5),
			want: sqlite.ErrTenantGuardUnsupportedLiteral,
		},
		{
			name: "pinned to bytes",
			g:    sqlite.TenantGuardFor(posts).PinnedTo([]byte("acme")),
			want: sqlite.ErrTenantGuardUnsupportedLiteral,
		},
		{
			name: "pinned to a control character",
			g:    sqlite.TenantGuardFor(posts).PinnedTo("acme\n"),
			want: sqlite.ErrTenantGuardAmbiguousLiteral,
		},
		{
			name: "local key belongs to another table",
			g:    sqlite.TenantGuardFor(posts).MatchingParent(strangerID, strangerID, strangerTenant),
			want: sqlite.ErrTenantGuardParentNotInTable,
		},
		{
			name: "parent key and parent axis are different tables",
			g:    sqlite.TenantGuardFor(posts).MatchingParent(author, strangerID, tenant),
			want: sqlite.ErrTenantGuardParentNotInTable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.g.Err(); !errors.Is(err, tc.want) {
				t.Fatalf("Err() = %v, want %v", err, tc.want)
			}
			// The renderer breaks the statement rather than emitting a
			// trigger whose scope drops cannot state.
			for _, sql := range renderGuard(t, sqlite.CreateTenantGuard(tc.g)) {
				if !strings.Contains(sql, "/* drops/sqlite: ") {
					t.Errorf("a refused declaration rendered a statement: %s", sql)
				}
				if strings.Contains(sql, "RAISE(") {
					t.Errorf("a refused declaration rendered a trigger body: %s", sql)
				}
			}
		})
	}
}

// A complete declaration reports no error. The negative cases above
// are worth nothing if the positive one also fails.
func TestTenantGuardAcceptsACompleteDeclaration(t *testing.T) {
	posts, _, author := guardTable("tg_ok")
	users := sqlite.NewTable("tg_ok_parent")
	userID := sqlite.Add(users, sqlite.BigInt("id").PrimaryKey())
	userTenant := sqlite.Add(users, sqlite.Text("tenantId").NotNull())

	g := sqlite.TenantGuardFor(posts).PinnedTo("acme").MatchingParent(author, userID, userTenant)
	if err := g.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil", err)
	}
}

// A handle taken off an alias of the guarded table names the same
// column: aliasing is a query-scope rename and a stored trigger body
// has no query around it to rename anything.
func TestTenantGuardAcceptsAnAxisTakenOffAnAlias(t *testing.T) {
	posts, _, _ := guardTable("tg_alias")
	aliased := posts.As("p")

	g := sqlite.NewTenantGuard("tg_alias_g").On(posts).Axis(aliased.Col("tenantId"))
	if err := g.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil", err)
	}
	got := renderGuard(t, sqlite.CreateTenantGuard(g))
	if !strings.Contains(got[0], `ON "tg_alias" `) || strings.Contains(got[0], `"p"`) {
		t.Errorf("the alias reached the stored body: %s", got[0])
	}
}

// renderChecks renders the preflight queries and reports the trigger
// each one mirrors, so a case can assert both halves at once. A check
// that bound an argument is a bug of the same shape as a trigger body
// that did: the query is meant to be pasted into a shell by whoever is
// deciding whether to apply the guard.
func renderChecks(t *testing.T, checks []sqlite.TenantGuardCheck) ([]string, []string) {
	t.Helper()
	sqls := make([]string, 0, len(checks))
	names := make([]string, 0, len(checks))
	for _, c := range checks {
		sql, args := drops.StringWithDialect(sqlite.Dialect, c.Count)
		if len(args) != 0 {
			t.Errorf("a preflight query bound %d args: %s %v", len(args), sql, args)
		}
		sqls = append(sqls, sql)
		names = append(names, c.Trigger)
	}
	return sqls, names
}

// The preflight for the plain axis guard: the rows the INSERT trigger
// would refuse are exactly the rows whose axis is already NULL.
//
// The condition is rendered by the same function that renders the
// trigger's WHEN clause, which is the whole reason this is worth
// having — a count that answered a DIFFERENT question from the guard
// it precedes would be worse than no count at all.
func TestTenantGuardViolationsCountsTheRowsTheGuardWouldRefuse(t *testing.T) {
	posts, _, _ := guardTable("tg_pre")

	got, names := renderChecks(t, sqlite.TenantGuardViolations(sqlite.TenantGuardFor(posts)))
	checkStatements(t, got, []string{
		`SELECT count(*) FROM "tg_pre" WHERE "tg_pre"."tenantId" IS NULL`,
	})
	checkStatements(t, names, []string{"tg_pre_tenantGuard_ins"})
}

// The immutability trigger has no preflight and must not render one:
// it refuses a TRANSITION, and a transition needs two rows. A check
// claiming to count the rows it would refuse would be counting
// nothing, and reading zero from it would be reading a guarantee that
// was never measured.
func TestTenantGuardViolationsSkipsTheImmutabilityTrigger(t *testing.T) {
	posts, _, _ := guardTable("tg_pre_upd")

	_, names := renderChecks(t, sqlite.TenantGuardViolations(sqlite.TenantGuardFor(posts)))
	for _, n := range names {
		if strings.HasSuffix(n, "_upd") {
			t.Errorf("a BEFORE UPDATE trigger got a preflight query: %s", n)
		}
	}
}

// A pinned guard counts every row that is not the file's tenant, NULL
// included: IS NOT is true for both.
func TestTenantGuardViolationsCountsAPinnedAxis(t *testing.T) {
	posts, _, _ := guardTable("tg_pre_pin")

	got, names := renderChecks(t, sqlite.TenantGuardViolations(
		sqlite.TenantGuardFor(posts).PinnedTo("acme")))
	checkStatements(t, got, []string{
		`SELECT count(*) FROM "tg_pre_pin" WHERE "tg_pre_pin"."tenantId" IS NOT 'acme'`,
	})
	checkStatements(t, names, []string{"tg_pre_pin_tenantGuard_ins"})
}

// The parent-agreement preflight, and the reason the guarded row is
// QUALIFIED rather than left bare the way NEW makes it inside a
// trigger body: the correlated subquery has the parent table in scope,
// so an unqualified "authorId" would bind to the parent's own column
// of that name if it had one, and the check would silently compare a
// row against itself.
func TestTenantGuardViolationsQualifiesTheGuardedRow(t *testing.T) {
	posts, _, author := guardTable("tg_pre_par")
	users := sqlite.NewTable("tg_pre_par_users")
	userID := sqlite.Add(users, sqlite.BigInt("id").PrimaryKey())
	userTenant := sqlite.Add(users, sqlite.Text("tenantId").NotNull())
	// The collision this qualification exists for: the parent table has
	// a column of the same name as the guarded table's foreign key.
	sqlite.Add(users, sqlite.BigInt("authorId"))

	got, names := renderChecks(t, sqlite.TenantGuardViolations(
		sqlite.TenantGuardFor(posts).MatchingParent(author, userID, userTenant)))
	checkStatements(t, got, []string{
		`SELECT count(*) FROM "tg_pre_par" WHERE "tg_pre_par"."tenantId" IS NULL`,
		`SELECT count(*) FROM "tg_pre_par" WHERE "tg_pre_par"."tenantId" IS NOT ` +
			`(SELECT "tenantId" FROM "tg_pre_par_users" WHERE "id" IS "tg_pre_par"."authorId")`,
	})
	checkStatements(t, names, []string{
		"tg_pre_par_tenantGuard_ins",
		"tg_pre_par_tenantGuard_authorId_ins",
	})
}

// A broken declaration yields one query that FAILS, not an empty list.
// An empty list reads as "nothing to repair", which is the one answer
// a preflight must never give by accident.
func TestTenantGuardViolationsBreaksOnABrokenDeclaration(t *testing.T) {
	g := sqlite.NewTenantGuard("tg_pre_broken")

	checks := sqlite.TenantGuardViolations(g)
	if len(checks) != 1 {
		t.Fatalf("a broken guard rendered %d checks, want 1", len(checks))
	}
	got, _ := renderChecks(t, checks)
	if !strings.Contains(got[0], "/* drops/sqlite: ") {
		t.Errorf("a refused declaration rendered a runnable query: %s", got[0])
	}
	if !errors.Is(g.Err(), sqlite.ErrTenantGuardTargetRequired) {
		t.Errorf("Err() = %v, want ErrTenantGuardTargetRequired", g.Err())
	}
}
