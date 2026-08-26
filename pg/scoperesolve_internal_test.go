package pg

import (
	"context"
	"testing"
)

// Resolution is not idempotent — every builder's resolved doc says so —
// and the way each of them keeps that promise is to hand back the
// RECEIVER when there was nothing to resolve, and a per-execution copy
// only when there was.
//
// The identity is not a micro-optimisation. resolveStatement reports
// "this differs from the original" by comparing the two pointers, and
// resolveExprs copies the whole list it is walking the moment any
// element reports a change. A builder that always answered with a fresh
// copy therefore told every statement it was nested in that it had
// changed, on every execution, and each of those copied a slice to hold
// values identical to the ones it already had. InsertBuilder.resolveCtx
// was the one that did — the other three tracked it — which is why this
// is a table over all four rather than a test of one.
func TestResolveCtxReturnsTheReceiverWhenNothingChanged(t *testing.T) {
	db := New(nil)
	plain := NewTable("ri_plain")
	id := Add(plain, BigSerial("id").PrimaryKey())
	n := Add(plain, BigInt("n"))

	scoped := NewTable("ri_scoped")
	sid := Add(scoped, BigSerial("id").PrimaryKey())
	tenant := Add(scoped, BigInt("tenantId").NotNull())
	scoped.ContextFilter(TenantFilter(tenant))
	scoped.ScopeWritesByTenant(tenant)

	ctx := WithTenant(context.Background(), int64(3))
	sub := func() *SelectBuilder { return db.Select(scoped.Col("id")).From(scoped) }

	tests := []struct {
		name string
		// same builds a statement with nothing to resolve; changed
		// builds one whose resolution has to produce a new value.
		same    func() (any, error)
		changed func() (any, error)
	}{
		{
			name: "SelectBuilder",
			same: func() (any, error) {
				b := db.Select(id).From(plain)
				r, err := b.resolveCtx(ctx)
				return r == b, err
			},
			changed: func() (any, error) {
				b := db.Select(id).From(plain).Where(Exists(sub()))
				r, err := b.resolveCtx(ctx)
				return r == b, err
			},
		},
		{
			name: "UpdateBuilder",
			same: func() (any, error) {
				b := db.Update(plain).Set(n.Val(1))
				r, err := b.resolveCtx(ctx)
				return r == b, err
			},
			changed: func() (any, error) {
				b := db.Update(plain).Set(n.Val(1)).Where(Exists(sub()))
				r, err := b.resolveCtx(ctx)
				return r == b, err
			},
		},
		{
			name: "DeleteBuilder",
			same: func() (any, error) {
				b := db.Delete(plain)
				r, err := b.resolveCtx(ctx)
				return r == b, err
			},
			changed: func() (any, error) {
				b := db.Delete(plain).Where(Exists(sub()))
				r, err := b.resolveCtx(ctx)
				return r == b, err
			},
		},
		{
			name: "InsertBuilder",
			same: func() (any, error) {
				b := db.Insert(plain).Row(n.Val(1))
				r, err := b.resolveCtx(ctx)
				return r == b, err
			},
			changed: func() (any, error) {
				// A tenant axis rebuilds the column list and every
				// row, so this one has genuinely changed.
				b := db.Insert(scoped).Row(sid.Val(1))
				r, err := b.resolveCtx(ctx)
				return r == b, err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.same()
			if err != nil {
				t.Fatalf("nothing to resolve: %v", err)
			}
			if got != true {
				t.Error("resolveCtx returned a copy of a statement it had nothing to resolve in: every enclosing walk now copies its list for nothing")
			}
			got, err = tc.changed()
			if err != nil {
				t.Fatalf("something to resolve: %v", err)
			}
			if got != false {
				t.Error("resolveCtx returned the receiver after resolving something: the change is invisible to the statement around it")
			}
		})
	}
}
