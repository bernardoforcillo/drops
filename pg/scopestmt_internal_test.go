package pg

import (
	"context"
	"reflect"
	"testing"

	"github.com/bernardoforcillo/drops"
)

// Resolution is not idempotent. The target table still carries its
// context filters after a pass, so a second pass appends the tenant
// predicate again and binds its value again: the statement still
// matches the same rows, the query returns the same answer, and the
// only evidence is a longer argument list. SelectBuilder has carried a
// resolved flag against that since round 4, because find.go's
// per-parent-limit rewrite embeds a resolved statement in a new one on
// purpose.
//
// The three write builders had no such flag, and did not need one while
// resolveExpr could not enter them. It can now — a resolved write is
// what a resolved CTE or a resolved operand holds — so they carry the
// same flag and this test holds all four to the same rule, from inside
// the package where the second resolution can actually be asked for.
//
// The assertion is on the bound arguments rather than on the flag: the
// flag is the mechanism and the argument list is the promise.

// stmtInternalTable mirrors the fixture the external tests use: a table
// scoped on both axes.
func stmtInternalTable() (*Table, *Col[string]) {
	tbl := NewTable("ax_rows")
	Add(tbl, BigSerial("id").PrimaryKey())
	ten := Add(tbl, BigInt("tenantId").NotNull())
	nm := Add(tbl, Text("name").NotNull())
	tbl.ContextFilter(TenantFilter(ten)).ScopeWritesByTenant(ten)
	return tbl, nm
}

func TestResolvingAResolvedStatementBindsTheTenantOnce(t *testing.T) {
	tests := []struct {
		name     string
		build    func(db *DB, tbl *Table, name *Col[string]) drops.Expression
		wantArgs []any
	}{
		{
			name: "select",
			build: func(db *DB, tbl *Table, _ *Col[string]) drops.Expression {
				return db.Select().From(tbl)
			},
			wantArgs: []any{int64(7)},
		},
		{
			name: "update",
			build: func(db *DB, tbl *Table, name *Col[string]) drops.Expression {
				return db.Update(tbl).Set(name.Val("x"))
			},
			wantArgs: []any{"x", int64(7)},
		},
		{
			name: "delete",
			build: func(db *DB, tbl *Table, _ *Col[string]) drops.Expression {
				return db.Delete(tbl)
			},
			wantArgs: []any{int64(7)},
		},
		{
			name: "insert",
			build: func(db *DB, tbl *Table, name *Col[string]) drops.Expression {
				return db.Insert(tbl).Row(name.Val("x"))
			},
			wantArgs: []any{"x", int64(7)},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tbl, name := stmtInternalTable()
			db := New(nil)
			ctx := WithTenant(context.Background(), int64(7))

			once, changed, err := resolveExpr(ctx, tc.build(db, tbl, name))
			if err != nil {
				t.Fatalf("first resolution: %v", err)
			}
			if !changed {
				t.Fatalf("first resolution reported no change on a scoped statement")
			}
			firstSQL, firstArgs := drops.String(once)
			if !reflect.DeepEqual(firstArgs, tc.wantArgs) {
				t.Fatalf("bound arguments after one resolution: got = %v, want %v", firstArgs, tc.wantArgs)
			}

			twice, changed, err := resolveExpr(ctx, once)
			if err != nil {
				t.Fatalf("second resolution: %v", err)
			}
			if changed {
				t.Errorf("resolving a resolved statement reported a change — the statement was resolved twice")
			}
			secondSQL, secondArgs := drops.String(twice)
			if secondSQL != firstSQL {
				t.Errorf("SQL after a second resolution:\n got = %v\nwant = %v", secondSQL, firstSQL)
			}
			if !reflect.DeepEqual(secondArgs, tc.wantArgs) {
				t.Errorf("bound arguments after a second resolution: got = %v, want %v — the tenant was bound twice",
					secondArgs, tc.wantArgs)
			}
		})
	}
}

// TestResolutionCopiesRatherThanMutating is the other half of the same
// discipline, and the one that decides what the NEXT request sends. A
// builder is a value a caller may hold and execute twice; a resolved
// predicate or a stamped tenant written back into it would pin the
// first request's tenant into every later use of it — silent in the
// SQL, correct-looking in a log, and wrong in the row it writes.
func TestResolutionCopiesRatherThanMutating(t *testing.T) {
	tbl, name := stmtInternalTable()
	db := New(nil)
	seven := WithTenant(context.Background(), int64(7))
	nine := WithTenant(context.Background(), int64(9))

	tests := []struct {
		name  string
		build func() drops.Expression
	}{
		{"select", func() drops.Expression { return db.Select().From(tbl) }},
		{"update", func() drops.Expression { return db.Update(tbl).Set(name.Val("x")) }},
		{"delete", func() drops.Expression { return db.Delete(tbl) }},
		{"insert", func() drops.Expression { return db.Insert(tbl).Row(name.Val("x")) }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			held := tc.build()
			bare, bareArgs := drops.String(held)

			if _, _, err := resolveExpr(seven, held); err != nil {
				t.Fatalf("resolve for tenant 7: %v", err)
			}
			after, afterArgs := drops.String(held)
			if after != bare || !reflect.DeepEqual(afterArgs, bareArgs) {
				t.Errorf("the held builder changed under resolution:\n got = %v %v\nwant = %v %v",
					after, afterArgs, bare, bareArgs)
			}

			nineExpr, _, err := resolveExpr(nine, held)
			if err != nil {
				t.Fatalf("resolve for tenant 9: %v", err)
			}
			_, nineArgs := drops.String(nineExpr)
			if !containsValue(nineArgs, int64(9)) {
				t.Errorf("the second request bound %v, which does not name its own tenant", nineArgs)
			}
			if containsValue(nineArgs, int64(7)) {
				t.Errorf("the second request bound the first request's tenant: %v", nineArgs)
			}
		})
	}
}

func containsValue(args []any, v any) bool {
	for _, a := range args {
		if reflect.DeepEqual(a, v) {
			return true
		}
	}
	return false
}
