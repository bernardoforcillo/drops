package pg

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
)

// Resolution is not idempotent, and the walk that reaches a subquery
// under a combinator can reach a statement that has already been
// resolved: find.go:1017 resolves the inner builder itself and then
// embeds it, so the outer statement's walk arrives at a builder whose
// tenant predicate is already in place. Walking it twice binds the
// tenant twice — the same value, so the rows come back right and
// nothing fails until an argument limit or a query log shows it.
//
// This is an internal test because the shape it pins — a caller-visible
// expression holding an already-resolved builder — is only reachable
// from inside the package, and asserting on it through the exported API
// would be asserting on find.go's rewrite rather than on the flag.
func TestResolvedSubqueryUnderCombinatorIsNotScopedTwice(t *testing.T) {
	users, posts := treeTable("ti_users"), treeTable("ti_posts")
	db := New(nil)
	ctx := WithTenant(context.Background(), 7)

	inner, err := db.Select(posts.Col("id")).From(posts).resolveCtx(ctx)
	if err != nil {
		t.Fatalf("resolveCtx: %v", err)
	}

	// Every combinator that now holds its operands, each handed the
	// already-resolved builder.
	tests := []struct {
		name string
		pred func() drops.Expression
	}{
		{"IN", func() drops.Expression { return In(users.Col("id"), inner) }},
		{"NOT over EXISTS", func() drops.Expression { return Not(Exists(inner)) }},
		{"AND over EXISTS", func() drops.Expression { return And(Exists(inner), drops.Raw(`TRUE`)) }},
		{"OR over EXISTS", func() drops.Expression { return Or(Exists(inner), drops.Raw(`FALSE`)) }},
		{"a scalar subquery operand", func() drops.Expression { return Eq(users.Col("id"), Subquery(inner)) }},
		{"= ANY", func() drops.Expression { return Any(users.Col("id"), Subquery(inner)) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sql, args, err := db.Select().From(users).Unscoped().Where(tc.pred()).ToSQLCtx(ctx)
			if err != nil {
				t.Fatalf("ToSQLCtx: %v", err)
			}
			if n := strings.Count(sql, "$"); n != 1 {
				t.Errorf("got %d placeholders in %q, want 1", n, sql)
			}
			if len(args) != 1 {
				t.Errorf("args = %v, want one bound tenant", args)
			}
		})
	}
}

// treeTable is the internal-test twin of scopereach_test.go's
// reachTable: a table with a render-time DefaultFilter and a
// ctx-resolved ContextFilter.
func treeTable(name string) *Table {
	t := NewTable(name)
	Add(t, BigSerial("id").PrimaryKey())
	tenant := Add(t, BigInt("tenantId").NotNull())
	ApplyMixins(t, &SoftDeleteMixin{})
	t.ContextFilter(TenantFilter(tenant))
	return t
}
