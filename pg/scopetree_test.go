package pg_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// A subquery keeps its tenant axis however deep in a predicate it is.
//
// scopeguard_test.go made EXISTS, ANY/ALL, a scalar subquery and
// AsSubquery carry the scoping of the statements they are. It did it by
// giving those five constructors a struct that holds the inner
// statement, and it left every other combinator in this package
// returning a drops.ExprFunc — a closure that swallows its operands, so
// the walk that resolves a statement for a ctx terminates at it and the
// body inside renders through WriteSQL, which has no ctx.
//
// The boundary that produced was arbitrary in a way no caller could
// predict: pg.NotExists(sub) carried the tenant and pg.Not(pg.Exists(sub))
// did not; pg.AnySub(col, sub) did and pg.Eq(col, pg.Subquery(sub)) did
// not. readme.md:461 documents And/Or/Not as the composition tools and
// readme.md:934 documents pg.Exists(db.Select()...) as the subquery
// shape, so combining the two documented idioms was the leak.
//
// Every assertion here is on rendered SQL and on the bound arguments,
// because that is where the defect lives: a round trip against a
// single-tenant fixture returns the right rows through every one of
// these shapes.

// treeTenant is a second tenant, used to prove that resolution copies
// rather than pinning one request's value into a reusable predicate.
const treeTenant = int64(11)

// TestSubqueryUnderAnyCombinatorCarriesTheTenantAxis walks the operand
// positions a caller can hand a *SelectBuilder to. The list is not the
// point — the shapes below that nobody reported (an array operator, a
// JSON operator, a three-deep nest, an ON clause) are, because they
// come from the same node type and would each have needed their own
// case under the old design.
func TestSubqueryUnderAnyCombinatorCarriesTheTenantAxis(t *testing.T) {
	users, posts := reachTable("q_users"), reachTable("q_posts")
	plain := pg.NewTable("q_plain")
	pg.Add(plain, pg.BigSerial("id").PrimaryKey())
	db := pg.New(nil)

	sub := func() *pg.SelectBuilder { return db.Select(posts.Col("id")).From(posts) }
	// subSQL is the inner statement with its own tenant predicate; the
	// placeholder number differs per case, so it is built per use.
	subSQL := func(n string) string {
		return `SELECT "q_posts"."id" FROM "q_posts" ` +
			`WHERE ("q_posts"."deletedAt" IS NULL) AND ("q_posts"."tenantId" = ` + n + `)`
	}
	from := `SELECT * FROM "q_users" WHERE ("q_users"."deletedAt" IS NULL) AND `

	tests := []struct {
		name string
		sel  func() *pg.SelectBuilder
		want string
		args []any
	}{
		{
			// The headline leak: In took the subquery as one of its
			// values and rendered it through a closure.
			name: "IN a subquery",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).Where(pg.In(users.Col("id"), sub()))
			},
			want: from + `("q_users"."id" IN (` + subSQL(`$1`) + `)) AND ("q_users"."tenantId" = $2)`,
			args: []any{reachTenant, reachTenant},
		},
		{
			name: "NOT IN a subquery",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).Where(pg.NotIn(users.Col("id"), sub()))
			},
			want: from + `("q_users"."id" NOT IN (` + subSQL(`$1`) + `)) AND ("q_users"."tenantId" = $2)`,
			args: []any{reachTenant, reachTenant},
		},
		{
			// pg.NotExists(sub) was scoped and pg.Not(pg.Exists(sub))
			// was not, which is the same statement written two ways.
			name: "NOT over EXISTS",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).Where(pg.Not(pg.Exists(sub())))
			},
			want: from + `(NOT EXISTS (` + subSQL(`$1`) + `)) AND ("q_users"."tenantId" = $2)`,
			args: []any{reachTenant, reachTenant},
		},
		{
			name: "AND over EXISTS",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).Where(
					pg.And(pg.Exists(sub()), pg.Eq(users.Col("id"), int64(5))))
			},
			want: from + `(EXISTS (` + subSQL(`$1`) + `) AND ("q_users"."id" = $2)) ` +
				`AND ("q_users"."tenantId" = $3)`,
			args: []any{reachTenant, int64(5), reachTenant},
		},
		{
			name: "OR over EXISTS",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).Where(
					pg.Or(pg.Exists(sub()), pg.Eq(users.Col("id"), int64(5))))
			},
			want: from + `(EXISTS (` + subSQL(`$1`) + `) OR ("q_users"."id" = $2)) ` +
				`AND ("q_users"."tenantId" = $3)`,
			args: []any{reachTenant, int64(5), reachTenant},
		},
		{
			// pg.AnySub(col, sub) was scoped; the same comparison
			// written with the generic operator was not.
			name: "a scalar subquery as a comparison operand",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).Where(pg.Eq(users.Col("id"), pg.Subquery(sub())))
			},
			want: from + `("q_users"."id" = (` + subSQL(`$1`) + `)) AND ("q_users"."tenantId" = $2)`,
			args: []any{reachTenant, reachTenant},
		},
		{
			// Both bounds are operands, and each is a statement of its
			// own: two inner tenant predicates, then the outer one.
			name: "BETWEEN two scalar subqueries",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).Where(
					pg.Between(users.Col("id"), pg.Subquery(sub()), pg.Subquery(sub())))
			},
			want: from + `("q_users"."id" BETWEEN (` + subSQL(`$1`) + `) AND (` + subSQL(`$2`) + `)) ` +
				`AND ("q_users"."tenantId" = $3)`,
			args: []any{reachTenant, reachTenant, reachTenant},
		},
		{
			name: "= ANY over an array-valued subquery",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).Where(pg.Any(users.Col("id"), pg.Subquery(sub())))
			},
			want: from + `("q_users"."id" = ANY((` + subSQL(`$1`) + `))) AND ("q_users"."tenantId" = $2)`,
			args: []any{reachTenant, reachTenant},
		},
		{
			name: "= ALL over an array-valued subquery",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).Where(pg.All(users.Col("id"), pg.Subquery(sub())))
			},
			want: from + `("q_users"."id" = ALL((` + subSQL(`$1`) + `))) AND ("q_users"."tenantId" = $2)`,
			args: []any{reachTenant, reachTenant},
		},
		{
			name: "IS NULL over a scalar subquery",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).Where(pg.IsNull(pg.Subquery(sub())))
			},
			want: from + `((` + subSQL(`$1`) + `) IS NULL) AND ("q_users"."tenantId" = $2)`,
			args: []any{reachTenant, reachTenant},
		},
		{
			name: "IS NOT NULL over a scalar subquery",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).Where(pg.IsNotNull(pg.Subquery(sub())))
			},
			want: from + `((` + subSQL(`$1`) + `) IS NOT NULL) AND ("q_users"."tenantId" = $2)`,
			args: []any{reachTenant, reachTenant},
		},
		{
			// Nobody reported this one. It is an array operator in
			// array.go rather than a comparison in op.go, and it works
			// because both are built from the same node.
			name: "an array containment operator",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).Where(
					pg.ArrayContains(users.Col("id"), pg.Subquery(sub())))
			},
			want: from + `("q_users"."id" @> (` + subSQL(`$1`) + `)) AND ("q_users"."tenantId" = $2)`,
			args: []any{reachTenant, reachTenant},
		},
		{
			// Nor this one: json.go never mentions subqueries and gets
			// the same treatment, because it is built from binOp too.
			// The bound key ahead of the subquery pins the argument
			// order as well as the text.
			name: "a JSON operator",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).Where(
					pg.Eq(pg.JSONGetText(users.Col("id"), "k"), pg.Subquery(sub())))
			},
			want: from + `(("q_users"."id" ->> $1) = (` + subSQL(`$2`) + `)) ` +
				`AND ("q_users"."tenantId" = $3)`,
			args: []any{"k", reachTenant, reachTenant},
		},
		{
			// Three combinators deep, which is where a per-shape type
			// switch stops: each layer has to hand the walk down.
			name: "three combinators deep",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).Where(
					pg.And(
						pg.Or(pg.Not(pg.Exists(sub())), drops.Raw(`FALSE`)),
						pg.Eq(users.Col("id"), int64(5))))
			},
			want: from + `(((NOT EXISTS (` + subSQL(`$1`) + `)) OR FALSE) AND ("q_users"."id" = $2)) ` +
				`AND ("q_users"."tenantId" = $3)`,
			args: []any{reachTenant, int64(5), reachTenant},
		},
		{
			// A HAVING clause is a WHERE clause in a different place,
			// and it resolves through the same list.
			name: "IN a subquery in the HAVING clause",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).
					GroupBy(users.Col("id")).
					Having(pg.In(users.Col("id"), sub()))
			},
			want: `SELECT * FROM "q_users" WHERE ("q_users"."deletedAt" IS NULL) ` +
				`AND ("q_users"."tenantId" = $1) GROUP BY "q_users"."id" ` +
				`HAVING ("q_users"."id" IN (` + subSQL(`$2`) + `))`,
			args: []any{reachTenant, reachTenant},
		},
		{
			// An ON clause resolves through resolveJoinOns, which stops
			// at whatever the caller's condition is: an AND of a join
			// key and an EXISTS is the ordinary way to write this.
			name: "a subquery under AND in an ON clause",
			sel: func() *pg.SelectBuilder {
				return db.Select().From(users).Join(plain,
					pg.And(pg.Eq(users.Col("id"), plain.Col("id")), pg.Exists(sub())))
			},
			want: `SELECT * FROM "q_users" INNER JOIN "q_plain" ` +
				`ON (("q_users"."id" = "q_plain"."id") AND EXISTS (` + subSQL(`$1`) + `)) ` +
				`WHERE ("q_users"."deletedAt" IS NULL) AND ("q_users"."tenantId" = $2)`,
			args: []any{reachTenant, reachTenant},
		},
		{
			// The select list is an expression list like any other.
			name: "a subquery under a combinator in the select list",
			sel: func() *pg.SelectBuilder {
				return db.Select(pg.IsNull(pg.Subquery(sub()))).From(users)
			},
			want: `SELECT ((` + subSQL(`$1`) + `) IS NULL) FROM "q_users" ` +
				`WHERE ("q_users"."deletedAt" IS NULL) AND ("q_users"."tenantId" = $2)`,
			args: []any{reachTenant, reachTenant},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, args, err := tc.sel().ToSQLCtx(reachCtx())
			if err != nil {
				t.Fatalf("ToSQLCtx: %v", err)
			}
			if got != tc.want {
				t.Errorf("got = %v, want %v", got, tc.want)
			}
			if !sameArgs(args, tc.args) {
				t.Errorf("args = %v, want %v", args, tc.args)
			}
		})
	}
}

// A subquery under a combinator fails closed with no tenant on ctx, for
// the reason the FROM table does: the alternative is an inner SELECT
// spanning every tenant feeding an outer statement that does not.
//
// This was the worst half of the leak. Where(In(col, sub)) with no
// tenant anywhere on ctx rendered, was sent, and came back with another
// tenant's rows — a fail-OPEN in the one feature sold as fail-closed.
func TestSubqueryUnderCombinatorFailsClosedWithNoTenant(t *testing.T) {
	users, posts := reachTable("qf_users"), reachTable("qf_posts")
	db := pg.New(nil)
	sub := func() *pg.SelectBuilder { return db.Select(posts.Col("id")).From(posts) }

	tests := []struct {
		name string
		pred func() drops.Expression
	}{
		{"IN", func() drops.Expression { return pg.In(users.Col("id"), sub()) }},
		{"NOT IN", func() drops.Expression { return pg.NotIn(users.Col("id"), sub()) }},
		{"NOT over EXISTS", func() drops.Expression { return pg.Not(pg.Exists(sub())) }},
		{"AND over EXISTS", func() drops.Expression {
			return pg.And(pg.Exists(sub()), drops.Raw(`TRUE`))
		}},
		{"OR over EXISTS", func() drops.Expression {
			return pg.Or(pg.Exists(sub()), drops.Raw(`FALSE`))
		}},
		{"a scalar subquery operand", func() drops.Expression {
			return pg.Eq(users.Col("id"), pg.Subquery(sub()))
		}},
		{"BETWEEN", func() drops.Expression {
			return pg.Between(users.Col("id"), pg.Subquery(sub()), int64(9))
		}},
		{"= ANY", func() drops.Expression { return pg.Any(users.Col("id"), pg.Subquery(sub())) }},
		{"IS NULL", func() drops.Expression { return pg.IsNull(pg.Subquery(sub())) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Unscoped() covers the outer table only — a subquery is a
			// statement of its own and keeps its own scoping — so the
			// refusal here can only have come from the inner one.
			sel := db.Select().From(users).Unscoped().Where(tc.pred())
			sql, _, err := sel.ToSQLCtx(context.Background())
			if !errors.Is(err, pg.ErrTenantMissing) {
				t.Fatalf("got = %v (sql %q), want %v", err, sql, pg.ErrTenantMissing)
			}
			if sql != "" {
				t.Errorf("got sql = %q, want no statement at all", sql)
			}
		})
	}
}

// Resolution must copy: a predicate is a value a caller may build once
// and reuse across requests, and a resolved body written back into it
// would answer every later request with the first one's tenant. That is
// the whole reason the node holds its operands rather than mutating
// them in place.
func TestCombinatorPredicateIsReusableAcrossRequests(t *testing.T) {
	users, posts := reachTable("qr_users"), reachTable("qr_posts")
	db := pg.New(nil)

	// One predicate value, one inner builder, three renders.
	inner := db.Select(posts.Col("id")).From(posts)
	pred := pg.And(pg.In(users.Col("id"), inner), pg.Not(pg.Exists(inner)))
	sel := db.Select().From(users).Where(pred)

	want := `SELECT * FROM "qr_users" WHERE ("qr_users"."deletedAt" IS NULL) AND ` +
		`(("qr_users"."id" IN (SELECT "qr_posts"."id" FROM "qr_posts" ` +
		`WHERE ("qr_posts"."deletedAt" IS NULL) AND ("qr_posts"."tenantId" = $1))) AND ` +
		`(NOT EXISTS (SELECT "qr_posts"."id" FROM "qr_posts" ` +
		`WHERE ("qr_posts"."deletedAt" IS NULL) AND ("qr_posts"."tenantId" = $2)))) ` +
		`AND ("qr_users"."tenantId" = $3)`

	for _, tenant := range []int64{reachTenant, treeTenant, reachTenant} {
		got, args, err := sel.ToSQLCtx(pg.WithTenant(context.Background(), tenant))
		if err != nil {
			t.Fatalf("ToSQLCtx: %v", err)
		}
		if got != want {
			t.Errorf("tenant %d: got = %v, want %v", tenant, got, want)
		}
		if !sameArgs(args, []any{tenant, tenant, tenant}) {
			t.Errorf("tenant %d: args = %v, want three copies of it", tenant, args)
		}
	}
}

// ----------------------------------------------------------------------
// And/Or/Not join their operands the way a WHERE clause does
// ----------------------------------------------------------------------

// And and Or joined their operands with a bare separator and Not wrapped
// its operand with none, so pg.And(drops.Raw("a OR b"), guard)
// reassociated exactly as the WHERE clause did before writeAnd was
// fixed: "(a OR b AND (tenantId = $1))" is "(a OR (b AND T))" and every
// row matching "a" comes back for every tenant. pg.Not was worse — a
// negation that binds tighter than OR turned NOT (a OR b) into
// (NOT a) OR b, which is not a negation of anything the caller wrote.
//
// The bracketing is writeAnd's, so a predicate that cannot reach past
// itself renders exactly as it always did.
func TestCombinatorsBracketOperandsThatEscape(t *testing.T) {
	guard := pg.Eq(drops.Raw(`"t"."tenantId"`), int64(7))

	tests := []struct {
		name string
		expr drops.Expression
		want string
	}{
		{
			name: "AND over a top-level OR",
			expr: pg.And(drops.Raw(`a OR b`), guard),
			want: `((a OR b) AND ("t"."tenantId" = $1))`,
		},
		{
			name: "OR over a top-level OR is still bracketed, and means the same",
			expr: pg.Or(drops.Raw(`a OR b`), guard),
			want: `((a OR b) OR ("t"."tenantId" = $1))`,
		},
		{
			name: "NOT over a top-level OR",
			expr: pg.Not(drops.Raw(`a OR b`)),
			want: `(NOT (a OR b))`,
		},
		{
			// The comment eats the closing parenthesis instead of the
			// guard, so PostgreSQL refuses the statement rather than
			// running an unscoped one. Same trade writeAnd makes.
			name: "AND over a line comment",
			expr: pg.And(drops.Raw(`a --`), guard),
			want: `((a --) AND ("t"."tenantId" = $1))`,
		},
		{
			name: "NOT over a statement terminator",
			expr: pg.Not(drops.Raw(`a; DROP TABLE t`)),
			want: `(NOT (a; DROP TABLE t))`,
		},
		{
			// Unbalanced parentheses are not an expression, and closing
			// the injected one would turn a syntax error into a working
			// unscoped OR. Left as written, on purpose.
			name: "an unbalanced fragment is not repaired",
			expr: pg.And(drops.Raw(`a = 1) OR (1 = 1`), guard),
			want: `(a = 1) OR (1 = 1 AND ("t"."tenantId" = $1))`,
		},
		{
			name: "a predicate that binds tighter is left alone",
			expr: pg.And(drops.Raw(`a = 1`), guard),
			want: `(a = 1 AND ("t"."tenantId" = $1))`,
		},
		{
			// A lone operand is the whole expression: And with one
			// predicate returns it unchanged, as it always has.
			name: "a lone conjunct keeps the caller's shape",
			expr: pg.And(drops.Raw(`a OR b`)),
			want: `a OR b`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := drops.String(tc.expr)
			if got != tc.want {
				t.Errorf("got = %v, want %v", got, tc.want)
			}
		})
	}
}

// ----------------------------------------------------------------------
// Nothing else about the rendered SQL moves
// ----------------------------------------------------------------------

// Holding the operands instead of closing over them must not change one
// byte of any predicate that has no statement inside it — which is
// nearly every predicate this package has ever rendered. A fix to
// scoping that rewrote every query log would not be worth having.
func TestPredicatesWithNoStatementInsideRenderUnchanged(t *testing.T) {
	col := drops.Raw(`"t"."c"`)

	tests := []struct {
		name string
		expr drops.Expression
		want string
		args []any
	}{
		{"Eq", pg.Eq(col, 1), `("t"."c" = $1)`, []any{1}},
		{"Ne", pg.Ne(col, 1), `("t"."c" <> $1)`, []any{1}},
		{"Gt", pg.Gt(col, 1), `("t"."c" > $1)`, []any{1}},
		{"Gte", pg.Gte(col, 1), `("t"."c" >= $1)`, []any{1}},
		{"Lt", pg.Lt(col, 1), `("t"."c" < $1)`, []any{1}},
		{"Lte", pg.Lte(col, 1), `("t"."c" <= $1)`, []any{1}},
		{"Like", pg.Like(col, "a%"), `("t"."c" LIKE $1)`, []any{"a%"}},
		{"ILike", pg.ILike(col, "a%"), `("t"."c" ILIKE $1)`, []any{"a%"}},
		{"In", pg.In(col, 1, 2), `("t"."c" IN ($1, $2))`, []any{1, 2}},
		{"In of a slice", pg.In(col, []int{1, 2, 3}), `("t"."c" IN ($1, $2, $3))`, []any{1, 2, 3}},
		{"In of one value", pg.In(col, 1), `("t"."c" IN ($1))`, []any{1}},
		{"In of nothing", pg.In(col), `(false)`, nil},
		{"NotIn", pg.NotIn(col, 1, 2), `("t"."c" NOT IN ($1, $2))`, []any{1, 2}},
		{"NotIn of nothing", pg.NotIn(col), `(true)`, nil},
		{"IsNull", pg.IsNull(col), `("t"."c" IS NULL)`, nil},
		{"IsNotNull", pg.IsNotNull(col), `("t"."c" IS NOT NULL)`, nil},
		{"Between", pg.Between(col, 1, 9), `("t"."c" BETWEEN $1 AND $2)`, []any{1, 9}},
		{"And of nothing", pg.And(), `TRUE`, nil},
		{"Or of nothing", pg.Or(), `FALSE`, nil},
		{"And", pg.And(pg.Eq(col, 1), pg.Eq(col, 2)), `(("t"."c" = $1) AND ("t"."c" = $2))`, []any{1, 2}},
		{"Or", pg.Or(pg.Eq(col, 1), pg.Eq(col, 2)), `(("t"."c" = $1) OR ("t"."c" = $2))`, []any{1, 2}},
		{"Not", pg.Not(pg.Eq(col, 1)), `(NOT ("t"."c" = $1))`, []any{1}},
		{"Any", pg.Any(col, drops.Raw(`arr`)), `("t"."c" = ANY(arr))`, nil},
		{"All", pg.All(col, drops.Raw(`arr`)), `("t"."c" = ALL(arr))`, nil},
		{"ArrayContains", pg.ArrayContains(col, drops.Raw(`arr`)), `("t"."c" @> arr)`, nil},
		{"ArrayContainedIn", pg.ArrayContainedIn(col, drops.Raw(`arr`)), `("t"."c" <@ arr)`, nil},
		{"ArrayOverlaps", pg.ArrayOverlaps(col, drops.Raw(`arr`)), `("t"."c" && arr)`, nil},
		{"ArrayConcat", pg.ArrayConcat(col, drops.Raw(`arr`)), `("t"."c" || arr)`, nil},
		{"ArrayLit", pg.ArrayLit(1, 2), `ARRAY[$1, $2]`, []any{1, 2}},
		{"ArrayLit of one", pg.ArrayLit(1), `ARRAY[$1]`, []any{1}},
		{"ArrayLit of nothing", pg.ArrayLit(), `ARRAY[]`, nil},
		{"ArrayAgg", pg.ArrayAgg(col), `array_agg("t"."c")`, nil},
		{"ArrayLength", pg.ArrayLength(col, 1), `array_length("t"."c", $1)`, []any{1}},
		{"Cardinality", pg.Cardinality(col), `cardinality("t"."c")`, nil},
		{"Unnest", pg.Unnest(col), `unnest("t"."c")`, nil},
		{"ArrayReplace", pg.ArrayReplace(col, 1, 2), `array_replace("t"."c", $1, $2)`, []any{1, 2}},
		{"ArrayToString", pg.ArrayToString(col, ","), `array_to_string("t"."c", $1)`, []any{","}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, args := drops.String(tc.expr)
			if got != tc.want {
				t.Errorf("got = %v, want %v", got, tc.want)
			}
			if !sameArgs(args, tc.args) {
				t.Errorf("args = %v, want %v", args, tc.args)
			}
		})
	}
}

// The sentence a reader consults before deciding whether to wrap a
// SELECT used to say WriteSQL parenthesises it. It never has: Eq(col,
// sel) rendered "= SELECT ..." and FromExpr(sel) rendered "FROM SELECT
// ...", both 42601. This pins what the corrected sentence now claims —
// bare from WriteSQL, parenthesised by Subquery and AsSubquery.
func TestSelectRendersBareAndTheWrappersAddTheParentheses(t *testing.T) {
	posts := reachTable("qw_posts")
	db := pg.New(nil)
	sel := db.Select(posts.Col("id")).From(posts).Unscoped()

	bare, _ := drops.String(sel)
	if strings.HasPrefix(bare, "(") {
		t.Errorf("got = %v, want a bare SELECT with no parentheses of its own", bare)
	}
	if got, _ := drops.String(pg.Subquery(sel)); got != "("+bare+")" {
		t.Errorf("Subquery got = %v, want %v", got, "("+bare+")")
	}
	if got, _ := drops.String(sel.AsSubquery("p")); got != "("+bare+`) AS "p"` {
		t.Errorf("AsSubquery got = %v, want %v", got, "("+bare+`) AS "p"`)
	}
}

// sameArgs compares two bound-argument lists by value. The lists are
// short and hold only comparable scalars, so == per element says what
// reflect.DeepEqual would and reads better in a failure message.
func sameArgs(got, want []any) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// A predicate tree stays affordable to build and to render.
//
// The bracketing And, Or and Not now apply reads the operand's rendered
// text, and an operand can be another combinator. Deciding at render
// time would re-render each subtree once per enclosing level, per
// render — 2^depth for the chain below — so a predicate assembled from
// user-supplied filters a couple of dozen levels deep would cost more
// to bracket than to execute, and a caller could ask for that by
// sending a nested filter document. The decision is taken once, when
// the expression is built; this pins that, since at 2^25 the render
// below does not finish.
func TestDeepPredicateTreeIsAffordable(t *testing.T) {
	users := reachTable("qd_users")
	db := pg.New(nil)

	pred := drops.Expression(pg.Eq(users.Col("id"), int64(0)))
	for i := 0; i < 25; i++ {
		pred = pg.And(pg.Not(pred), pg.Eq(users.Col("id"), int64(i)))
	}

	got, args, err := db.Select().From(users).Where(pred).ToSQLCtx(reachCtx())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	// 26 bound ids, then the tenant.
	if len(args) != 27 {
		t.Errorf("args = %d, want 27", len(args))
	}
	if !strings.HasSuffix(got, `AND ("qd_users"."tenantId" = $27)`) {
		t.Errorf("got = %v, want the tenant predicate last", got)
	}
}
