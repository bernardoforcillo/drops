package mysql_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/mysql"
)

// The axis guard matches by the name a handle RENDERS as, and "the
// same name" is the server's question. MySQL folds a column name's
// case on every platform and in every configuration —
// lower_case_table_names governs table names, not column names — so
// `TENANTID` and `tenantId` are one column and a handle spelled either
// way names the axis. That much is settled, and update_axis_test.go
// pins it.
//
// What is NOT settled is the non-ASCII half. Whether MySQL reads
// "tenantid" and "tenantİd" as one column is its identifier
// collation's answer, and the answers differ by server version and by
// configuration. strings.ToLower is a third answer again, agreeing
// with neither, and using it here folded a pair the schema may well
// have declared as two columns.
//
// The cost of that over-fold is not the refusal it looks like.
// stampTenantColumn reads a match as the axis being bound already and
// appends no stamp, so an INSERT naming the ordinary column rendered
// with the tenant column absent from it — the row lands under whatever
// the schema defaults to and is reported as written. So identKey folds
// ASCII, which is the part every MySQL agrees on, and leaves the pairs
// it cannot settle as two columns. Reading them as two is the answer
// that refuses nothing it should not and stamps everything it should.
//
// No MySQL server was reachable while this was written: every
// assertion here is on the statement drops renders and the arguments
// it binds. What the server would then do with a non-ASCII pair is
// exactly the part this package declines to claim, and it is written
// down in tenant.go's "Where the automatic scoping stops" list.

// overFoldSchema declares a table whose write axis is axisName and
// which also carries otherName — a column strings.ToLower maps onto
// the axis.
func overFoldSchema(axisName, otherName string) (tbl *mysql.Table, id *mysql.Col[int64], tenant, other *mysql.Col[string]) {
	t := mysql.NewTable("posts")
	idCol := mysql.Add(t, mysql.BigInt("id").PrimaryKey())
	tenantCol := mysql.Add(t, mysql.Text(axisName).NotNull())
	otherCol := mysql.Add(t, mysql.Text(otherName))
	t.ContextFilter(mysql.TenantFilter(tenantCol)).ScopeWritesByTenant(tenantCol)
	return t, idCol, tenantCol, otherCol
}

const (
	// dottedI is U+0130; strings.ToLower maps it onto "i".
	dottedIAxis  = "tenantid"
	dottedIOther = "tenantİd"
	// kelvin is U+212A; strings.ToLower maps it onto "k".
	kelvinAxis  = "tenantkey"
	kelvinOther = "tenantKey"
)

func TestInsertStampsWhenAColumnOnlyUnicodeFoldsOntoTheAxis(t *testing.T) {
	const tenant = "acme"
	tests := []struct {
		name     string
		axis     string
		other    string
		bound    string
		wantSQL  string
		wantArgs []any
	}{
		{
			// Before the fix this rendered
			// INSERT INTO `posts` (`tenantİd`) VALUES (?) with args
			// ["acme"] — the axis `tenantid` nowhere in the statement.
			name:     "U+0130 is not an ASCII case pair",
			axis:     dottedIAxis,
			other:    dottedIOther,
			bound:    tenant,
			wantSQL:  "INSERT INTO `posts` (`tenantİd`, `tenantid`) VALUES (?, ?)",
			wantArgs: []any{tenant, tenant},
		},
		{
			// The same family, with a value that is NOT the ctx
			// tenant: before the fix an ordinary column holding an
			// ordinary string was refused as another tenant's axis
			// value.
			name:     "U+212A is not an ASCII case pair",
			axis:     kelvinAxis,
			other:    kelvinOther,
			bound:    "celsius",
			wantSQL:  "INSERT INTO `posts` (`tenantKey`, `tenantkey`) VALUES (?, ?)",
			wantArgs: []any{"celsius", tenant},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			posts, _, _, other := overFoldSchema(tt.axis, tt.other)
			db := mysql.New(dropstest.New())
			ctx := mysql.WithTenant(context.Background(), tenant)

			sql, args, err := db.Insert(posts).Row(other.Val(tt.bound)).ToSQLCtx(ctx)
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

// The UPDATE half: assigning a column that only Unicode-folds onto the
// axis is an ordinary assignment, and the axis predicate still lands.
func TestUpdateAssignsAColumnThatOnlyUnicodeFoldsOntoTheAxis(t *testing.T) {
	const tenant = "acme"
	posts, id, _, other := overFoldSchema(dottedIAxis, dottedIOther)
	db := mysql.New(dropstest.New())
	ctx := mysql.WithTenant(context.Background(), tenant)

	sql, args, err := db.Update(posts).Set(other.Val("x")).
		Where(mysql.Eq(id, int64(7))).ToSQLCtx(ctx)
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	want := "UPDATE `posts` SET `tenantİd` = ? " +
		"WHERE (`posts`.`id` = ?) AND (`posts`.`tenantid` = ?)"
	if sql != want {
		t.Errorf("rendered SQL:\n got = %v\nwant = %v", sql, want)
	}
	if wantArgs := []any{"x", int64(7), tenant}; !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("bound arguments:\n got = %#v\nwant = %#v", args, wantArgs)
	}
}
