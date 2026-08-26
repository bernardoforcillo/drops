package pg_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// The third invariant, stated once and tested here:
//
//	No expression list the renderer reads may be invisible to the
//	resolver.
//
// Round 4 established that an operand a caller can hand a
// *SelectBuilder to must not be an opaque closure, and round 5 enforced
// it with a source census of every exported constructor. Two leaks
// survived both, because neither is a constructor. One sat on a builder
// FIELD that writeCore rendered and resolveCtx never listed
// (SelectBuilder.distinctOn). The others sat on the RETURN value of a
// table's own filters — the predicate a ContextFilter or a
// DefaultFilter answers with, handed to the builder and rendered
// without ever being walked.
//
// All three rendered, and sent, on a context.Background() with no
// tenant anywhere:
//
//	SELECT DISTINCT ON ((SELECT "posts"."id" FROM "posts")) ... FROM "plain"
//	SELECT "guarded"."id" FROM "guarded" WHERE ("guarded"."id" IN ((SELECT "posts"."id" FROM "posts")))
//
// The second is the worse of the two. A Guard — including
// AuthorizeWith(CustomGuard(...)) — is the feature sold as the thing
// that decides what a request may see, and the shape it leaked through
// is the ordinary one: "the rows this subject may see are the ones
// named by some other scoped table". It leaked on SELECT FROM, on JOIN,
// on UPDATE and on DELETE alike, because all four reach a table's
// filters through Table.resolveContextFilters.
//
// The structural half of this round lives in scopefunc_test.go, where
// TestNoRenderedExpressionListIsInvisibleToTheResolver reads the
// package source and fails the day a field is rendered without being
// resolved. This file is the behavioural half: what each of those
// shapes renders, and what it refuses.

// listTenant is the tenant these tests put on ctx. It differs from
// every other tenant constant in the package's tests so a predicate
// that arrived from the wrong place cannot pass by coincidence.
const listTenant = int64(41)

func listCtx() context.Context { return pg.WithTenant(context.Background(), listTenant) }

// scopedSubSQL is what a SELECT of "id" from a reachTable renders once
// its own filters are resolved: the soft-delete default and the tenant
// axis, in that order.
func scopedSubSQL(table, placeholder string) string {
	return `SELECT "` + table + `"."id" FROM "` + table + `" ` +
		`WHERE ("` + table + `"."deletedAt" IS NULL) AND ("` + table + `"."tenantId" = ` + placeholder + `)`
}

// ----------------------------------------------------------------------
// A builder field the renderer reads: DISTINCT ON
// ----------------------------------------------------------------------

// DISTINCT ON is written by writeCore and was absent from the list
// resolveCtx walks, so a scoped statement used as a distinct key was
// reached only by the renderer — which has no ctx, writes the inner
// statement's DefaultFilters, and writes none of its ContextFilters.
//
// The outer table is deliberately unscoped: the tenant predicate in the
// rendered SQL and the tenant in the args can only have come from the
// statement inside the DISTINCT ON list.
func TestDistinctOnCarriesTheTenantAxis(t *testing.T) {
	plain := pg.NewTable("dl_plain")
	pg.Add(plain, pg.BigSerial("id").PrimaryKey())
	posts := reachTable("dl_posts")
	db := pg.New(nil)

	sel := func() *pg.SelectBuilder {
		return db.Select(plain.Col("id")).From(plain).Unscoped().
			DistinctOn(pg.Subquery(db.Select(posts.Col("id")).From(posts)))
	}

	want := `SELECT DISTINCT ON ((` + scopedSubSQL("dl_posts", "$1") + `)) ` +
		`"dl_plain"."id" FROM "dl_plain"`
	got, args, err := sel().ToSQLCtx(listCtx())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	if got != want {
		t.Errorf("got = %v, want %v", got, want)
	}
	if !sameArgs(args, []any{listTenant}) {
		t.Errorf("args = %v, want %v", args, []any{listTenant})
	}

	sql, _, err := sel().ToSQLCtx(context.Background())
	if !errors.Is(err, pg.ErrTenantMissing) {
		t.Fatalf("got = %v (sql %q), want %v", err, sql, pg.ErrTenantMissing)
	}
	if sql != "" {
		t.Errorf("got sql = %q, want no statement at all", sql)
	}
}

// A DISTINCT ON list with no statement inside it renders the bytes it
// always did — resolution that rebuilt the list would rewrite every
// query log for nothing.
func TestDistinctOnWithoutAStatementRendersUnchanged(t *testing.T) {
	plain := pg.NewTable("du_plain")
	id := pg.Add(plain, pg.BigSerial("id").PrimaryKey())
	n := pg.Add(plain, pg.BigInt("n"))
	db := pg.New(nil)

	want := `SELECT DISTINCT ON ("du_plain"."id", "du_plain"."n") "du_plain"."id" FROM "du_plain"`
	sel := db.Select(id).From(plain).DistinctOn(id, n)

	if got, _ := sel.ToSQL(); got != want {
		t.Errorf("ToSQL got = %v, want %v", got, want)
	}
	got, args, err := sel.ToSQLCtx(listCtx())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	if got != want {
		t.Errorf("ToSQLCtx got = %v, want %v", got, want)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want none", args)
	}
}

// ----------------------------------------------------------------------
// A filter's own predicate
// ----------------------------------------------------------------------

// guardedTable returns a table whose single ContextFilter answers with
// a predicate that embeds a statement over the scoped table sub — the
// shape AuthorizeWith(CustomGuard(...)) produces when a policy is
// expressed as "the ids some other table names".
func guardedTable(name string, sub *pg.Table, db *pg.DB) (*pg.Table, *pg.Col[int64]) {
	t := pg.NewTable(name)
	id := pg.Add(t, pg.BigSerial("id").PrimaryKey())
	t.ContextFilter(func(ctx context.Context) (drops.Expression, error) {
		return pg.In(id, pg.Subquery(db.Select(sub.Col("id")).From(sub))), nil
	})
	return t, id
}

// defaultFilteredTable is guardedTable's declaration-time twin: the
// same predicate, registered as a DefaultFilter. The renderer reads
// that list too, so the resolver has to walk it too.
func defaultFilteredTable(name string, sub *pg.Table, db *pg.DB) *pg.Table {
	t := pg.NewTable(name)
	id := pg.Add(t, pg.BigSerial("id").PrimaryKey())
	t.DefaultFilter(pg.In(id, pg.Subquery(db.Select(sub.Col("id")).From(sub))))
	return t
}

// Every statement kind reaches a table's automatic filters, so every
// statement kind leaked the read written inside one. The subquery's
// table is the only scoped thing in any of these, so the tenant
// predicate and the bound tenant can only have come from the filter's
// own predicate.
func TestFilterPredicatesCarryTheTenantAxis(t *testing.T) {
	db := pg.New(nil)
	posts := reachTable("gl_posts")
	plain := pg.NewTable("gl_plain")
	pg.Add(plain, pg.BigSerial("id").PrimaryKey())

	ctxGuarded, ctxGuardedID := guardedTable("gl_ctx", posts, db)
	defGuarded := defaultFilteredTable("gl_def", posts, db)

	tests := []struct {
		name string
		stmt func() ctxRenderer
		want string
	}{
		{
			name: "SELECT FROM a context-filtered table",
			stmt: func() ctxRenderer { return db.Select(ctxGuarded.Col("id")).From(ctxGuarded) },
			want: `SELECT "gl_ctx"."id" FROM "gl_ctx" ` +
				`WHERE ("gl_ctx"."id" IN ((` + scopedSubSQL("gl_posts", "$1") + `)))`,
		},
		{
			name: "SELECT FROM a context-filtered table under an alias",
			stmt: func() ctxRenderer {
				a := ctxGuarded.As("g")
				return db.Select(a.Col("id")).From(a)
			},
			want: `SELECT "g"."id" FROM "gl_ctx" AS "g" ` +
				`WHERE ("g"."id" IN ((` + scopedSubSQL("gl_posts", "$1") + `)))`,
		},
		{
			name: "INNER JOIN of a context-filtered table",
			stmt: func() ctxRenderer {
				return db.Select(plain.Col("id")).From(plain).
					Join(ctxGuarded, pg.Eq(plain.Col("id"), ctxGuarded.Col("id")))
			},
			want: `SELECT "gl_plain"."id" FROM "gl_plain" ` +
				`INNER JOIN "gl_ctx" ON ("gl_plain"."id" = "gl_ctx"."id") ` +
				`WHERE ("gl_ctx"."id" IN ((` + scopedSubSQL("gl_posts", "$1") + `)))`,
		},
		{
			name: "LEFT JOIN of a context-filtered table puts it in the ON clause",
			stmt: func() ctxRenderer {
				return db.Select(plain.Col("id")).From(plain).
					LeftJoin(ctxGuarded, pg.Eq(plain.Col("id"), ctxGuarded.Col("id")))
			},
			want: `SELECT "gl_plain"."id" FROM "gl_plain" ` +
				`LEFT JOIN "gl_ctx" ON (("gl_plain"."id" = "gl_ctx"."id") ` +
				`AND ("gl_ctx"."id" IN ((` + scopedSubSQL("gl_posts", "$1") + `))))`,
		},
		{
			name: "UPDATE of a context-filtered table",
			stmt: func() ctxRenderer {
				return db.Update(ctxGuarded).Set(ctxGuardedID.Val(1))
			},
			want: `UPDATE "gl_ctx" SET "id" = $1 ` +
				`WHERE ("gl_ctx"."id" IN ((` + scopedSubSQL("gl_posts", "$2") + `)))`,
		},
		{
			name: "DELETE from a context-filtered table",
			stmt: func() ctxRenderer { return db.Delete(ctxGuarded) },
			want: `DELETE FROM "gl_ctx" ` +
				`WHERE ("gl_ctx"."id" IN ((` + scopedSubSQL("gl_posts", "$1") + `)))`,
		},
		{
			name: "SELECT FROM a default-filtered table",
			stmt: func() ctxRenderer { return db.Select(defGuarded.Col("id")).From(defGuarded) },
			want: `SELECT "gl_def"."id" FROM "gl_def" ` +
				`WHERE ("gl_def"."id" IN ((` + scopedSubSQL("gl_posts", "$1") + `)))`,
		},
		{
			name: "DELETE from a default-filtered table",
			stmt: func() ctxRenderer { return db.Delete(defGuarded) },
			want: `DELETE FROM "gl_def" ` +
				`WHERE ("gl_def"."id" IN ((` + scopedSubSQL("gl_posts", "$1") + `)))`,
		},
		{
			name: "DELETE USING a default-filtered table",
			stmt: func() ctxRenderer {
				return db.Delete(plain).Using(defGuarded).
					Where(pg.Eq(plain.Col("id"), defGuarded.Col("id")))
			},
			want: `DELETE FROM "gl_plain" USING "gl_def" ` +
				`WHERE ("gl_def"."id" IN ((` + scopedSubSQL("gl_posts", "$1") + `))) ` +
				`AND ("gl_plain"."id" = "gl_def"."id")`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, args, err := tc.stmt().ToSQLCtx(listCtx())
			if err != nil {
				t.Fatalf("ToSQLCtx: %v", err)
			}
			if got != tc.want {
				t.Errorf("got = %v, want %v", got, tc.want)
			}
			if !containsArg(args, listTenant) {
				t.Errorf("args = %v, want them to bind the tenant %v", args, listTenant)
			}

			// And the half that matters: with no tenant on ctx at all,
			// every one of these rendered and was sent.
			sql, _, err := tc.stmt().ToSQLCtx(context.Background())
			if !errors.Is(err, pg.ErrTenantMissing) {
				t.Fatalf("bare ctx: got = %v (sql %q), want %v", err, sql, pg.ErrTenantMissing)
			}
			if sql != "" {
				t.Errorf("bare ctx: got sql = %q, want no statement at all", sql)
			}
		})
	}
}

// ctxRenderer is the one method the four statement builders share that
// this file needs: render the whole statement for a ctx, or refuse.
type ctxRenderer interface {
	ToSQLCtx(ctx context.Context) (string, []any, error)
}

// A filter is a value the table holds and every request reads. Resolving
// one must not write the resolution back onto the table, or the second
// request would be answered with the first one's tenant — the property
// scopetree_test.go pins for the builders, restated for the filter
// lists now that those are walked too.
func TestFilterPredicateResolutionIsPerRequest(t *testing.T) {
	db := pg.New(nil)
	posts := reachTable("gr_posts")
	ctxGuarded, _ := guardedTable("gr_ctx", posts, db)
	defGuarded := defaultFilteredTable("gr_def", posts, db)

	for _, tc := range []struct {
		name  string
		table *pg.Table
	}{
		{"ContextFilter", ctxGuarded},
		{"DefaultFilter", defGuarded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sel := db.Select(tc.table.Col("id")).From(tc.table)
			want := `SELECT "` + tc.table.Name() + `"."id" FROM "` + tc.table.Name() + `" ` +
				`WHERE ("` + tc.table.Name() + `"."id" IN ((` + scopedSubSQL("gr_posts", "$1") + `)))`
			for _, tenant := range []int64{listTenant, treeTenant, listTenant} {
				got, args, err := sel.ToSQLCtx(pg.WithTenant(context.Background(), tenant))
				if err != nil {
					t.Fatalf("ToSQLCtx: %v", err)
				}
				if got != want {
					t.Errorf("got = %v, want %v", got, want)
				}
				if !sameArgs(args, []any{tenant}) {
					t.Errorf("args = %v, want %v", args, []any{tenant})
				}
			}
		})
	}
}

// ----------------------------------------------------------------------
// The cycle the walk makes reachable
// ----------------------------------------------------------------------

// Walking a filter's own predicate is what makes a self-referential
// filter reachable, and following one does not converge: every turn
// asks the filter for a fresh predicate, so the resolver would recurse
// until the stack ran out — in the request path, holding a connection.
// It is refused by name instead. See [pg.ErrContextFilterCycle] for why
// that answer rather than a depth limit or a flag on the table.
func TestSelfReferentialFilterIsRefused(t *testing.T) {
	db := pg.New(nil)

	selfCtx := pg.NewTable("cy_self")
	selfCtxID := pg.Add(selfCtx, pg.BigSerial("id").PrimaryKey())
	selfCtx.ContextFilter(func(ctx context.Context) (drops.Expression, error) {
		return pg.In(selfCtxID, pg.Subquery(db.Select(selfCtx.Col("id")).From(selfCtx))), nil
	})

	selfDef := pg.NewTable("cy_def")
	selfDefID := pg.Add(selfDef, pg.BigSerial("id").PrimaryKey())
	selfDef.DefaultFilter(pg.In(selfDefID, pg.Subquery(db.Select(selfDef.Col("id")).From(selfDef))))

	// Through an alias, which is the same relation and so the same
	// cycle however the inner statement spells it.
	selfAlias := pg.NewTable("cy_alias")
	selfAliasID := pg.Add(selfAlias, pg.BigSerial("id").PrimaryKey())
	selfAlias.ContextFilter(func(ctx context.Context) (drops.Expression, error) {
		a := selfAlias.As("a")
		return pg.In(selfAliasID, pg.Subquery(db.Select(a.Col("id")).From(a))), nil
	})

	// And mutually: A's filter reads B, B's filter reads A.
	mutualA := pg.NewTable("cy_a")
	mutualAID := pg.Add(mutualA, pg.BigSerial("id").PrimaryKey())
	mutualB := pg.NewTable("cy_b")
	mutualBID := pg.Add(mutualB, pg.BigSerial("id").PrimaryKey())
	mutualA.ContextFilter(func(ctx context.Context) (drops.Expression, error) {
		return pg.In(mutualAID, pg.Subquery(db.Select(mutualB.Col("id")).From(mutualB))), nil
	})
	mutualB.ContextFilter(func(ctx context.Context) (drops.Expression, error) {
		return pg.In(mutualBID, pg.Subquery(db.Select(mutualA.Col("id")).From(mutualA))), nil
	})

	tests := []struct {
		name string
		stmt func() ctxRenderer
	}{
		{"SELECT with a self-referential context filter",
			func() ctxRenderer { return db.Select(selfCtx.Col("id")).From(selfCtx) }},
		{"SELECT with a self-referential default filter",
			func() ctxRenderer { return db.Select(selfDef.Col("id")).From(selfDef) }},
		{"SELECT whose filter reads the same table under an alias",
			func() ctxRenderer { return db.Select(selfAlias.Col("id")).From(selfAlias) }},
		{"SELECT with two filters that read each other",
			func() ctxRenderer { return db.Select(mutualA.Col("id")).From(mutualA) }},
		{"DELETE with a self-referential context filter",
			func() ctxRenderer { return db.Delete(selfCtx) }},
		{"UPDATE with a self-referential context filter",
			func() ctxRenderer { return db.Update(selfCtx).Set(selfCtxID.Val(1)) }},
		{"JOIN of a self-referentially filtered table",
			func() ctxRenderer {
				plain := pg.NewTable("cy_plain")
				pg.Add(plain, pg.BigSerial("id").PrimaryKey())
				return db.Select(plain.Col("id")).From(plain).
					Join(selfCtx, pg.Eq(plain.Col("id"), selfCtx.Col("id")))
			}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sql, _, err := tc.stmt().ToSQLCtx(listCtx())
			if !errors.Is(err, pg.ErrContextFilterCycle) {
				t.Fatalf("got = %v (sql %q), want %v", err, sql, pg.ErrContextFilterCycle)
			}
			if sql != "" {
				t.Errorf("got sql = %q, want no statement at all", sql)
			}
		})
	}
}

// The refusal names the cycle it found, in the order the resolver
// walked it, so a mutual one is diagnosable from the message alone.
func TestFilterCycleErrorNamesTheCycle(t *testing.T) {
	db := pg.New(nil)
	a := pg.NewTable("cm_a")
	aID := pg.Add(a, pg.BigSerial("id").PrimaryKey())
	b := pg.NewTable("cm_b")
	bID := pg.Add(b, pg.BigSerial("id").PrimaryKey())
	a.ContextFilter(func(ctx context.Context) (drops.Expression, error) {
		return pg.In(aID, pg.Subquery(db.Select(b.Col("id")).From(b))), nil
	})
	b.ContextFilter(func(ctx context.Context) (drops.Expression, error) {
		return pg.In(bID, pg.Subquery(db.Select(a.Col("id")).From(a))), nil
	})

	_, _, err := db.Select(a.Col("id")).From(a).ToSQLCtx(listCtx())
	if err == nil {
		t.Fatal("got no error, want a cycle refusal")
	}
	if want := "cm_a -> cm_b -> cm_a"; !contains(err.Error(), want) {
		t.Errorf("got = %v, want it to name the cycle %q", err, want)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// The refusal is not a dead end. A filter that has to consult its own
// table says Unscoped on the statement it embeds — which terminates,
// and which is what the author meant: the inner read names the rows the
// filter is about to restrict, so restricting it with the filter being
// built is circular in the SQL too.
func TestSelfReferentialFilterIsAllowedWhenTheInnerStatementIsUnscoped(t *testing.T) {
	db := pg.New(nil)
	tbl := pg.NewTable("cu_self")
	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	tbl.ContextFilter(func(ctx context.Context) (drops.Expression, error) {
		return pg.In(id, pg.Subquery(db.Select(tbl.Col("id")).From(tbl).Unscoped())), nil
	})

	want := `SELECT "cu_self"."id" FROM "cu_self" ` +
		`WHERE ("cu_self"."id" IN ((SELECT "cu_self"."id" FROM "cu_self")))`
	got, args, err := db.Select(tbl.Col("id")).From(tbl).ToSQLCtx(listCtx())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	if got != want {
		t.Errorf("got = %v, want %v", got, want)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want none", args)
	}
}

// A statement that names the same filtered table twice is not a cycle:
// the second resolution starts after the first has finished, so the
// chain the refusal reads is empty by then. A self-join of a filtered
// table has to keep working, and each side keeps its own restatement.
func TestTheSameFilteredTableTwiceIsNotACycle(t *testing.T) {
	db := pg.New(nil)
	posts := reachTable("cs_posts")
	guarded, _ := guardedTable("cs_guarded", posts, db)
	other := guarded.As("g2")

	want := `SELECT "cs_guarded"."id" FROM "cs_guarded" ` +
		`INNER JOIN "cs_guarded" AS "g2" ON ("cs_guarded"."id" = "g2"."id") ` +
		`WHERE ("cs_guarded"."id" IN ((` + scopedSubSQL("cs_posts", "$1") + `))) ` +
		`AND ("g2"."id" IN ((` + scopedSubSQL("cs_posts", "$2") + `)))`
	got, args, err := db.Select(guarded.Col("id")).From(guarded).
		Join(other, pg.Eq(guarded.Col("id"), other.Col("id"))).ToSQLCtx(listCtx())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	if got != want {
		t.Errorf("got = %v, want %v", got, want)
	}
	if !sameArgs(args, []any{listTenant, listTenant}) {
		t.Errorf("args = %v, want %v", args, []any{listTenant, listTenant})
	}
}

// Rule six of this work: an automatic filter with no statement inside
// it renders the bytes it always did, on both paths. Nearly every
// filter is one of these — a soft-delete guard, a tenant equality — and
// a walk that rebuilt them would rewrite every query log in exchange
// for nothing.
func TestFiltersWithoutStatementsRenderUnchanged(t *testing.T) {
	db := pg.New(nil)
	tbl := pg.NewTable("cp_notes")
	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	pg.ApplyMixins(tbl, &pg.SoftDeleteMixin{})
	tenant := pg.Add(tbl, pg.BigInt("tenantId").NotNull())
	tbl.ContextFilter(pg.TenantFilter(tenant))

	t.Run("ToSQL renders the default filters alone", func(t *testing.T) {
		want := `SELECT "cp_notes"."id" FROM "cp_notes" WHERE ("cp_notes"."deletedAt" IS NULL)`
		if got, _ := db.Select(id).From(tbl).ToSQL(); got != want {
			t.Errorf("got = %v, want %v", got, want)
		}
	})

	t.Run("ToSQLCtx adds the context filter and nothing else", func(t *testing.T) {
		want := `SELECT "cp_notes"."id" FROM "cp_notes" ` +
			`WHERE ("cp_notes"."deletedAt" IS NULL) AND ("cp_notes"."tenantId" = $1)`
		got, args, err := db.Select(id).From(tbl).ToSQLCtx(listCtx())
		if err != nil {
			t.Fatalf("ToSQLCtx: %v", err)
		}
		if got != want {
			t.Errorf("got = %v, want %v", got, want)
		}
		if !sameArgs(args, []any{listTenant}) {
			t.Errorf("args = %v, want %v", args, []any{listTenant})
		}
	})

	t.Run("under an alias, restated against the instance named", func(t *testing.T) {
		a := tbl.As("n")
		want := `SELECT "n"."id" FROM "cp_notes" AS "n" ` +
			`WHERE ("n"."deletedAt" IS NULL) AND ("n"."tenantId" = $1)`
		got, args, err := db.Select(a.Col("id")).From(a).ToSQLCtx(listCtx())
		if err != nil {
			t.Fatalf("ToSQLCtx: %v", err)
		}
		if got != want {
			t.Errorf("got = %v, want %v", got, want)
		}
		if !sameArgs(args, []any{listTenant}) {
			t.Errorf("args = %v, want %v", args, []any{listTenant})
		}
	})
}

// ----------------------------------------------------------------------
// The fast path in front of the walk
// ----------------------------------------------------------------------

// listForeign is a statement drops did not build: it has a WriteSQL and
// a ToSQLCtx, which is all [pg.DB.ExecExpr] and resolveExpr's
// ctxStatement arm ask of one. Its WriteSQL renders the statement
// inside it blind — no ctx, so no context filters — which is exactly
// what a foreign statement type does and why the resolver asks it for
// its ctx form instead.
type listForeign struct {
	inner *pg.SelectBuilder
	// asked records the ctx form being requested, so a test can tell
	// "rendered correctly by luck" from "asked, and answered".
	asked *bool
}

func (f listForeign) WriteSQL(b *drops.Builder) {
	b.WriteString("EXISTS (")
	f.inner.WriteSQL(b)
	b.WriteString(")")
}

func (f listForeign) ToSQLCtx(ctx context.Context) (string, []any, error) {
	*f.asked = true
	sql, args, err := f.inner.ToSQLCtx(ctx)
	if err != nil {
		return "", nil, err
	}
	return "EXISTS (" + sql + ")", args, nil
}

// TestDefaultFilterFastPathAdmitsEveryShapeTheResolverEnters is the
// behavioural half of the round-8 audit, and it names a real leak.
//
// resolveDefaultFilterExprs is guarded by mayHoldStatements, a fast
// path that answers "can walking this list do anything at all?" without
// building a chain — worth having, because a soft-delete guard is the
// common case and it holds no statement. But the guard was a COPY of
// resolveExpr's type switch, written as a list of names, and it went
// stale exactly the way the switch itself did before round 7: it
// admitted *SelectBuilder and subqueryResolver, and knew nothing of the
// ctxStatement arm.
//
// So a default filter whose predicate is a statement drops did not
// build was never offered to the resolver at all. The ctxStatement arm
// exists to keep that case FAIL-CLOSED — it asks the foreign statement
// for its ctx form so that a filter refusing for want of a tenant
// refuses the whole statement — and the fast path skipped the question.
// On a ctx with no tenant this rendered and was sent:
//
//	SELECT "gate_rows"."id" FROM "gate_rows" WHERE (EXISTS (SELECT ... FROM "gate_posts"))
//
// with the inner statement carrying its render-time defaults and none
// of its context filters: another tenant's rows, through the exported
// API, from the feature that decides what a request may see.
func TestDefaultFilterFastPathAdmitsEveryShapeTheResolverEnters(t *testing.T) {
	db := pg.New(nil)
	posts := reachTable("gate_posts")

	asked := false
	tbl := pg.NewTable("gate_rows")
	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	tbl.DefaultFilter(listForeign{
		inner: db.Select(posts.Col("id")).From(posts),
		asked: &asked,
	})

	// With a tenant: the foreign statement is asked for its ctx form.
	// It renders through its own WriteSQL either way — finished SQL
	// numbered from $1 cannot be spliced into the statement around it,
	// which is what ctxResolvable documents — so what the ctx form buys
	// is the refusal below, and being asked is the assertion here.
	sel := db.Select(id).From(tbl)
	if _, _, err := sel.ToSQLCtx(listCtx()); err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	if !asked {
		t.Errorf("the foreign statement in the default filter was never asked for its ctx form")
	}

	// And with no tenant the whole statement is refused, rather than
	// sent with the guard's inner statement unscoped.
	sql, _, err := db.Select(id).From(tbl).ToSQLCtx(context.Background())
	if !errors.Is(err, pg.ErrTenantMissing) {
		t.Fatalf("got = %v (sql %q), want %v", err, sql, pg.ErrTenantMissing)
	}
	if sql != "" {
		t.Errorf("got sql = %q, want no statement at all", sql)
	}
}
