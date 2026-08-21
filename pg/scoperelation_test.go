package pg_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/pg"
)

// ----------------------------------------------------------------------
// The fifth invariant
//
//	Every RELATION a statement names is scoped, however it got in.
//
// The four checks before this one are about expressions: an operand
// must not be an opaque closure, a rendered expression list must not be
// invisible to the resolver, the resolver must decide by what a value
// can do rather than by what it is called. All four are blind to this
// one for the same reason, and it is a reason worth stating once:
//
//	The resolver walks EXPRESSIONS. It does not walk RELATIONS — so a
//	table reached any way other than From() is scoped by nobody.
//
// Two doors were open. FromExpr takes a drops.Expression, and a *Table
// is one, so the documented way to comma-join two tables put a relation
// into the statement through the expression path: offered to
// resolveExpr, matched by none of its arms — none of them is about
// relations — and rendered as a bare name. And Table.As handed back a
// relation carrying a SNAPSHOT of its table's scoping, so an alias
// taken before the table was scoped was a relation nothing would ever
// scope again.
//
// Both are silent cross-tenant access through the exported API on
// context.Background(), one a read and one a DELETE.

// relScoped is a table carrying every automatic predicate a statement
// has to honour — a DefaultFilter, a ContextFilter, and the write-side
// tenant column an INSERT stamps — so a statement that loses one and
// keeps another cannot pass by accident. The three are declared by
// three different calls and reach the statement through three different
// paths.
func relScoped(name string) *pg.Table {
	t := pg.NewTable(name)
	pg.Add(t, pg.BigSerial("id").PrimaryKey())
	tenant := pg.Add(t, pg.BigInt("tenantId").NotNull())
	pg.ApplyMixins(t, &pg.SoftDeleteMixin{})
	t.ContextFilter(pg.TenantFilter(tenant))
	t.ScopeWritesByTenant(tenant)
	return t
}

// relPlain is the unscoped scenery: whatever predicate appears in a
// rendered statement came from the relation under test.
func relPlain(name string) *pg.Table {
	t := pg.NewTable(name)
	pg.Add(t, pg.BigSerial("id").PrimaryKey())
	return t
}

// ----------------------------------------------------------------------
// FromExpr(*Table) — a relation on the expression path
// ----------------------------------------------------------------------

// A comma join is an inner join, so the comma-joined table's predicates
// belong in the WHERE clause — which is what joinKind.filterPlacement
// answers for an INNER JOIN, and this has to agree with it.
func TestFromExprTableIsScopedLikeAJoinedTable(t *testing.T) {
	db := pg.New(nil)
	plain, scoped := relPlain("fx_plain"), relScoped("fx_scoped")

	checkCtx(t, wctx(),
		db.Select(plain.Col("id")).From(plain).FromExpr(scoped),
		`SELECT "fx_plain"."id" FROM "fx_plain", "fx_scoped" `+
			`WHERE ("fx_scoped"."deletedAt" IS NULL) AND ("fx_scoped"."tenantId" = $1)`,
		wtenant)
}

// The refusal is the half that matters most: with no tenant on ctx the
// statement must not be sent at all. It rendered
// SELECT * FROM "fx_plain", "rf_scoped" — every tenant's rows, no
// error — for as long as FromExpr had existed.
func TestFromExprTableRefusesWithNoTenant(t *testing.T) {
	db := pg.New(nil)
	plain, scoped := relPlain("rf_plain"), relScoped("rf_scoped")

	sql, _, err := db.Select(plain.Col("id")).From(plain).FromExpr(scoped).ToSQLCtx(context.Background())
	if !errors.Is(err, pg.ErrTenantMissing) {
		t.Fatalf("got = %v (sql %q), want %v", err, sql, pg.ErrTenantMissing)
	}
	if sql != "" {
		t.Errorf("got sql = %q, want no statement at all", sql)
	}
}

// An aliased table comma-joined to itself is two relations, and each
// one is restricted separately — the reading resolveJoins already takes
// for a self-join.
func TestFromExprTableScopesEachInstance(t *testing.T) {
	db := pg.New(nil)
	scoped := relScoped("fs_scoped")

	checkCtx(t, wctx(),
		db.Select(scoped.Col("id")).From(scoped).FromExpr(scoped.As("b")),
		`SELECT "fs_scoped"."id" FROM "fs_scoped", "fs_scoped" AS "b" `+
			`WHERE ("fs_scoped"."deletedAt" IS NULL) AND ("b"."deletedAt" IS NULL) `+
			`AND ("fs_scoped"."tenantId" = $1) AND ("b"."tenantId" = $2)`,
		wtenant, wtenant)
}

// A comma-joined relation sits on the same side of a later join as the
// FROM table does, so a RIGHT JOIN moves its predicates into that
// join's ON clause with the FROM table's. Putting them in the WHERE
// clause would make them false for every NULL-extended row and turn
// the RIGHT JOIN into an INNER JOIN — see fromFilterJoin.
func TestFromExprTableFollowsTheFromTablePlacement(t *testing.T) {
	db := pg.New(nil)
	plain, scoped, right := relPlain("fp_plain"), relScoped("fp_scoped"), relPlain("fp_right")

	checkCtx(t, wctx(),
		db.Select(plain.Col("id")).From(plain).FromExpr(scoped).
			RightJoin(right, pg.Eq(plain.Col("id"), right.Col("id"))),
		`SELECT "fp_plain"."id" FROM "fp_plain", "fp_scoped" `+
			`RIGHT JOIN "fp_right" ON (("fp_plain"."id" = "fp_right"."id") AND `+
			`("fp_scoped"."deletedAt" IS NULL) AND ("fp_scoped"."tenantId" = $1))`,
		wtenant)
}

// A FULL JOIN has nowhere correct to put them, and a comma-joined
// relation is in exactly the position the FROM table is — so it gets
// the same refusal rather than a predicate in a place that leaks one
// side or drops the other.
func TestFromExprTableRefusesUnderAFullJoin(t *testing.T) {
	db := pg.New(nil)
	plain, scoped, far := relPlain("ff_plain"), relScoped("ff_scoped"), relPlain("ff_far")

	sql, _, err := db.Select(plain.Col("id")).From(plain).FromExpr(scoped).
		FullJoin(far, pg.Eq(plain.Col("id"), far.Col("id"))).
		ToSQLCtx(wctx())
	if !errors.Is(err, pg.ErrFullJoinScoped) {
		t.Fatalf("got = %v (sql %q), want %v", err, sql, pg.ErrFullJoinScoped)
	}
	if sql != "" {
		t.Errorf("got sql = %q, want no statement at all", sql)
	}
	// And the message says which relations, not just that something
	// was wrong — with the package prefix exactly once.
	if got := err.Error(); strings.Count(got, "drops/pg: ") != 1 {
		t.Errorf("error prefix is not written exactly once: %q", got)
	}
}

// Unscoped is statement-wide and reaches the comma-joined relation
// like every other one — a flag that unscoped the FROM table and left
// this relation's tenant axis on would answer with a silently narrowed
// slice of what was asked for.
func TestFromExprTableHonoursUnscoped(t *testing.T) {
	db := pg.New(nil)
	plain, scoped := relPlain("fu_plain"), relScoped("fu_scoped")

	checkCtx(t, context.Background(),
		db.Select(plain.Col("id")).From(plain).FromExpr(scoped).Unscoped(),
		`SELECT "fu_plain"."id" FROM "fu_plain", "fu_scoped"`)
}

// What FromExpr holds that is NOT a relation goes on being resolved as
// the expression it is, which is the invariant round 6 pinned. This is
// here so a relation arm cannot be added by narrowing the expression
// path.
func TestFromExprSubqueryIsStillResolvedAsAStatement(t *testing.T) {
	db := pg.New(nil)
	plain, scoped := relPlain("fq_plain"), relScoped("fq_scoped")

	checkCtx(t, wctx(),
		db.Select(plain.Col("id")).From(plain).
			FromExpr(db.Select(scoped.Col("id")).From(scoped).AsSubquery("s")),
		`SELECT "fq_plain"."id" FROM "fq_plain", (SELECT "fq_scoped"."id" FROM "fq_scoped" `+
			`WHERE ("fq_scoped"."deletedAt" IS NULL) AND ("fq_scoped"."tenantId" = $1)) AS "s"`,
		wtenant)
}

// ----------------------------------------------------------------------
// A join with no condition left
// ----------------------------------------------------------------------

// Every join kind PostgreSQL has requires a condition, so a join whose
// ON clause came out empty rendered `... INNER JOIN "q" ON ` — a
// dangling keyword the server rejects with 42601. It was reachable in
// three of the five shapes: any INNER or RIGHT join, and any join at
// all whose table contributes no automatic predicate.
func TestJoinWithNoConditionRendersOnTrue(t *testing.T) {
	db := pg.New(nil)
	plain, other, scoped := relPlain("jn_plain"), relPlain("jn_other"), relScoped("jn_scoped")

	tests := []struct {
		name string
		q    func() *pg.SelectBuilder
		want string
	}{
		{
			name: "inner, unscoped table",
			q:    func() *pg.SelectBuilder { return db.Select().From(plain).Join(other, nil) },
			want: `SELECT * FROM "jn_plain" INNER JOIN "jn_other" ON TRUE`,
		},
		{
			name: "left, unscoped table",
			q:    func() *pg.SelectBuilder { return db.Select().From(plain).LeftJoin(other, nil) },
			want: `SELECT * FROM "jn_plain" LEFT JOIN "jn_other" ON TRUE`,
		},
		{
			name: "right, unscoped table",
			q:    func() *pg.SelectBuilder { return db.Select().From(plain).RightJoin(other, nil) },
			want: `SELECT * FROM "jn_plain" RIGHT JOIN "jn_other" ON TRUE`,
		},
		{
			name: "full, unscoped table",
			q:    func() *pg.SelectBuilder { return db.Select().From(plain).FullJoin(other, nil) },
			want: `SELECT * FROM "jn_plain" FULL JOIN "jn_other" ON TRUE`,
		},
		{
			// An INNER join places the joined table's predicates in
			// the WHERE clause, so having filters does not save the
			// ON clause from being empty.
			name: "inner, scoped table",
			q:    func() *pg.SelectBuilder { return db.Select().From(plain).Join(scoped, nil) },
			want: `SELECT * FROM "jn_plain" INNER JOIN "jn_scoped" ON TRUE ` +
				`WHERE ("jn_scoped"."deletedAt" IS NULL)`,
		},
		{
			// The one shape that was already valid: a LEFT join fills
			// its own ON clause from the joined table's filters.
			name: "left, scoped table",
			q:    func() *pg.SelectBuilder { return db.Select().From(plain).LeftJoin(scoped, nil) },
			want: `SELECT * FROM "jn_plain" LEFT JOIN "jn_scoped" ` +
				`ON ("jn_scoped"."deletedAt" IS NULL)`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			check(t, tc.q(), tc.want)
		})
	}
}

// A join written WITH a condition renders exactly the bytes it always
// did: no TRUE is introduced anywhere a condition already exists.
func TestJoinWithAConditionIsUnchanged(t *testing.T) {
	db := pg.New(nil)
	plain, other := relPlain("jc_plain"), relPlain("jc_other")
	check(t, db.Select().From(plain).Join(other, pg.Eq(plain.Col("id"), other.Col("id"))),
		`SELECT * FROM "jc_plain" INNER JOIN "jc_other" ON ("jc_plain"."id" = "jc_other"."id")`)
}

// ----------------------------------------------------------------------
// The error prefix, written once
// ----------------------------------------------------------------------

// A sentinel in this package carries the "drops/pg: " prefix, so a
// wrapper that adds one again reads back to the user as
// "drops/pg: drops/pg: a FULL JOIN cannot carry...". The doubling is
// cosmetic and the check is not: it is the shape that says nobody read
// the message after it shipped.
func TestPackageErrorsCarryThePrefixExactlyOnce(t *testing.T) {
	db := pg.New(nil)
	scoped, far := relScoped("px_scoped"), relPlain("px_far")

	tests := []struct {
		name string
		run  func() error
	}{
		{
			name: "FULL JOIN onto a scoped table",
			run: func() error {
				_, _, err := db.Select().From(far).
					FullJoin(scoped, pg.Eq(far.Col("id"), scoped.Col("id"))).ToSQLCtx(wctx())
				return err
			},
		},
		{
			name: "FULL JOIN with a scoped FROM table",
			run: func() error {
				_, _, err := db.Select().From(scoped).
					FullJoin(far, pg.Eq(far.Col("id"), scoped.Col("id"))).ToSQLCtx(wctx())
				return err
			},
		},
		{
			name: "two CTEs sharing a name",
			run: func() error {
				body := db.Select(far.Col("id")).From(far)
				_, _, err := db.Select().From(far).
					With(pg.CTEDef("dup", body), pg.CTEDef("dup", body)).ToSQLCtx(wctx())
				return err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil {
				t.Fatal("got no error, want one")
			}
			if got := strings.Count(err.Error(), "drops/pg: "); got != 1 {
				t.Errorf("prefix written %d times, want 1: %q", got, err.Error())
			}
		})
	}
}

// ----------------------------------------------------------------------
// The lock an alias shares with its table
// ----------------------------------------------------------------------

// Sharing the filter and hook lists between a table and its aliases is
// what makes an alias the same table; sharing the LOCK is what keeps
// that safe. The lock lives inside the shared tableScope precisely so
// that a per-alias one cannot be written by accident — an alias
// serialising against a mutex nobody else takes would be the round-2
// data race rebuilt one handle over, while another goroutine replaced
// the very slice it was walking.
//
// That is reachable through a pattern this package documents as
// supported: [Table.ContextFilter] invites registration while queries
// are in flight, and an entity rebuilt per request does exactly that —
// and sharing doubled the surface, since a registration on either
// handle is now a write every reader of the other has to be protected
// from. Run under -race, this is the reproduction: registration goes
// through both handles and so do the queries, because all four
// combinations have to hold.
func TestAliasAndTableShareOneLockForTheirScoping(t *testing.T) {
	base := relScoped("lock_rows")
	u := base.As("u")
	drv := dropstest.New().Rows([]string{"id"}, []any{int64(1)})
	db := pg.New(drv)
	ctx := wctx()

	const workers = 8
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		for _, reg := range []*pg.Table{base, u} {
			reg := reg
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 50; j++ {
					// Keyed registration, which replaces: an entity
					// rebuilt per request, the shape the docs invite.
					pg.NewEntity[lockRow](reg).ScopeByTenant(reg.Col("tenantId"))
					// Keyless registration, which appends: the write
					// that moves the slice out from under a reader.
					reg.OnUpdate(pg.UpdateHookFunc(func(*pg.UpdateHookCtx) {}))
				}
			}()
		}
		for _, q := range []*pg.Table{base, u} {
			q := q
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 50; j++ {
					rows, err := db.Select(q.Col("id")).From(q).Rows(ctx)
					if err != nil {
						t.Errorf("Rows: %v", err)
						return
					}
					rows.Close()
				}
			}()
		}
	}
	wg.Wait()
}

type lockRow struct {
	ID       int64 `drop:"id"`
	TenantID int64 `drop:"tenantId"`
}
