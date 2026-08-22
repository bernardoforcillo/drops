package sqlite_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/sqlite"
)

// The axis guard was moved off handle identity and onto the rendered
// column name, and stopped one step short of what the renderer means.
//
// SQLite resolves a column name case-insensitively, quoted or not:
// "TENANTID" and "tenantId" are one column to it, so a handle spelled
// either way RENDERS as the axis. Compared byte for byte they are
// strangers, which is the original defect one shift key further in —
// the guard answers "not the axis" for a handle the server answers
// "the axis" for, and the statement goes out carrying it.
//
// What lands: the INSERT stamp is appended beside the caller's
// binding, and this is the dialect that accepts a duplicate column and
// keeps the FIRST occurrence, so the row is written under the caller's
// tenant while the ctx said another. The UPDATE assigns the axis with
// its WHERE clause still addressing the ctx tenant. Both are the leak
// this phase exists to close, reached without a single character of
// new API.
//
// The assertions are on rendered SQL and args: a fixture holding one
// tenant's rows answers a leaking statement and a scoped one alike.

// caseFoldSchema returns the table under test and a SECOND table whose
// handle for the same column is spelled in another case — which is what
// a schema assembled from two codegen runs, or from a hand-written
// declaration beside a generated one, produces.
func caseFoldSchema() (tbl *sqlite.Table, id, tenant, oddCase *sqlite.Col[int64], title *sqlite.Col[string]) {
	t := sqlite.NewTable("posts")
	idCol := sqlite.Add(t, sqlite.BigInt("id").PrimaryKey())
	tenantCol := sqlite.Add(t, sqlite.BigInt("tenantId").NotNull())
	titleCol := sqlite.Add(t, sqlite.Text("title"))
	t.ContextFilter(sqlite.TenantFilter(tenantCol)).ScopeWritesByTenant(tenantCol)

	other := sqlite.NewTable("widgets")
	sqlite.Add(other, sqlite.BigInt("id").PrimaryKey())
	odd := sqlite.Add(other, sqlite.BigInt("TENANTID").NotNull())
	return t, idCol, tenantCol, odd, titleCol
}

func TestInsertReadsACaseVariantHandleAsTheAxisItRenders(t *testing.T) {
	tests := []struct {
		name     string
		row      func(odd *sqlite.Col[int64], title *sqlite.Col[string]) []sqlite.ColumnValue
		wantErr  error
		wantSQL  string
		wantArgs []any
	}{
		{
			// The leak. Before the fix this rendered
			// INSERT INTO "posts" ("title", "TENANTID", "tenantId")
			// VALUES (?, ?, ?) with args ["x", 9, 77], and SQLite kept
			// the 9.
			name: "bound to another tenant through a differently-cased handle",
			row: func(odd *sqlite.Col[int64], title *sqlite.Col[string]) []sqlite.ColumnValue {
				return []sqlite.ColumnValue{title.Val("x"), odd.Val(9)}
			},
			wantErr: sqlite.ErrTenantMismatch,
		},
		{
			// The same handle carrying the right value is not an
			// error — it names the column it renders as, and the
			// statement binds the tenant ONCE, under the spelling the
			// caller used.
			name: "bound to the ctx tenant through a differently-cased handle",
			row: func(odd *sqlite.Col[int64], title *sqlite.Col[string]) []sqlite.ColumnValue {
				return []sqlite.ColumnValue{title.Val("x"), odd.Val(scopeTenant)}
			},
			wantSQL:  `INSERT INTO "posts" ("title", "TENANTID") VALUES (?, ?)`,
			wantArgs: []any{"x", scopeTenant},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			posts, _, _, odd, title := caseFoldSchema()
			db := sqlite.New(dropstest.New())

			sql, args, err := db.Insert(posts).Values(tt.row(odd, title)...).ToSQLCtx(scopeCtx())
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("ToSQLCtx = %v, want %v", err, tt.wantErr)
				}
				if sql != "" {
					t.Errorf("a refused INSERT still rendered: %s", sql)
				}
				return
			}
			if err != nil {
				t.Fatalf("ToSQLCtx: %v", err)
			}
			if sql != tt.wantSQL {
				t.Errorf("rendered SQL:\n got = %v\nwant = %v", sql, tt.wantSQL)
			}
			if !reflect.DeepEqual(args, tt.wantArgs) {
				t.Errorf("bound arguments:\n got = %#v\nwant = %#v", args, tt.wantArgs)
			}
		})
	}
}

// The UPDATE half: SET "TENANTID" = ? assigns the same column SET
// "tenantId" = ? does, under a WHERE clause still addressing the ctx
// tenant.
func TestUpdateRefusesACaseVariantAxisAssignment(t *testing.T) {
	tbl, id, _, odd, _ := caseFoldSchema()
	db := sqlite.New(dropstest.New())

	sql, _, err := db.Update(tbl).Set(odd.Val(999)).
		Where(sqlite.Eq(id, int64(7))).ToSQLCtx(scopeCtx())
	if !errors.Is(err, sqlite.ErrTenantMismatch) {
		t.Fatalf("ToSQLCtx = %v, want %v", err, sqlite.ErrTenantMismatch)
	}
	if sql != "" {
		t.Errorf("a refused UPDATE still rendered: %s", sql)
	}
}

// And a hook holding the differently-cased handle sees the caller's
// binding rather than adding a second one — the same column twice in a
// SET list, where SQLite keeps the last.
func TestUpdateHookSeesACaseVariantBinding(t *testing.T) {
	tbl, id, tenant, odd, _ := caseFoldSchema()
	saw := false
	tbl.OnUpdate(sqlite.UpdateHookFunc(func(hc *sqlite.UpdateHookCtx) {
		saw = hc.Has(odd.Column)
		hc.Set(odd.Val(999))
	}))
	db := sqlite.New(dropstest.New())

	sql, args, err := db.Update(tbl).Set(tenant.Val(scopeTenant)).
		Where(sqlite.Eq(id, int64(7))).ToSQLCtx(scopeCtx())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	if !saw {
		t.Error("Has answered false for a column the statement already assigns")
	}
	want := `UPDATE "posts" SET "tenantId" = ? ` +
		`WHERE ("posts"."id" = ?) AND ("posts"."tenantId" = ?)`
	if sql != want {
		t.Errorf("rendered SQL:\n got = %v\nwant = %v", sql, want)
	}
	if wantArgs := []any{scopeTenant, int64(7), scopeTenant}; !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("bound arguments:\n got = %#v\nwant = %#v", args, wantArgs)
	}
}
