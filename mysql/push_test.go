package mysql_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/mysql"
)

// The half of P0-7 that shipped for drops/pg and not for drops/mysql:
// consent for a change that destroys data. The asymmetry mattered more
// than it looked. drops/pg refuses the push and rolls back if anything
// slips; here there is no transaction at all, so a DROP COLUMN that
// should not have run is a permanent fact about the database — which is
// the argument Push's own doc comment makes about unmanaged tables, one
// object further along.

// liveColumn / liveTable describe the database the fake server has, in
// the terms information_schema reports it.
type liveColumn struct {
	name          string
	colType       string
	dataType      string
	notNull       bool
	autoIncrement bool
}

type liveTable struct {
	name    string
	columns []liveColumn
	pk      string
}

func sharedDatabase() []liveTable {
	return []liveTable{
		{
			name: "users",
			pk:   "id",
			columns: []liveColumn{
				{name: "id", colType: "bigint", dataType: "bigint", notNull: true, autoIncrement: true},
				{name: "email", colType: "varchar(190)", dataType: "varchar", notNull: true},
				{name: "amount", colType: "varchar(32)", dataType: "varchar", notNull: true},
			},
		},
		{
			name: "audit_log",
			pk:   "id",
			columns: []liveColumn{
				{name: "id", colType: "bigint", dataType: "bigint", notNull: true, autoIncrement: true},
				{name: "payload", colType: "text", dataType: "text"},
			},
		},
	}
}

// pushDriver answers the introspection queries for tables, columns and
// keys, the row estimate from estimates, and the emptiness probe from
// empty. A table missing from estimates has a NULL TABLE_ROWS, which is
// what a server with no statistics reports and what sends Push to the
// probe.
//
// It answers by statement rather than by position: the order of Push's
// catalogue reads is Push's business — it skips the CHECK reader
// entirely on MySQL 5.7 — and a queue of result sets would encode an
// order the test has no business knowing.
func pushDriver(tables []liveTable, estimates map[string]int64, empty map[string]bool) *dropstest.Driver {
	return dropstest.New().Answer(func(q string, _ []any) ([]string, [][]any, bool) {
		switch {
		case strings.Contains(q, "VERSION()"):
			return []string{"version"}, [][]any{{"8.0.36"}}, true
		case strings.Contains(q, "SELECT EXISTS"):
			for name, isEmpty := range empty {
				if strings.Contains(q, "`"+name+"`") {
					return []string{"exists"}, [][]any{{!isEmpty}}, true
				}
			}
			return []string{"exists"}, [][]any{{true}}, true
		case strings.Contains(q, "TABLE_ROWS"):
			data := [][]any{}
			for _, t := range tables {
				var est any
				if n, ok := estimates[t.name]; ok {
					est = n
				}
				data = append(data, []any{t.name, est})
			}
			return []string{"TABLE_NAME", "TABLE_ROWS"}, data, true
		case strings.Contains(q, "information_schema.TABLES"):
			data := [][]any{}
			for _, t := range tables {
				data = append(data, []any{t.name, "InnoDB", "utf8mb4_general_ci"})
			}
			return []string{"TABLE_NAME", "ENGINE", "TABLE_COLLATION"}, data, true
		case strings.Contains(q, "information_schema.COLUMNS"):
			data := [][]any{}
			for _, t := range tables {
				for _, c := range t.columns {
					nullable := "YES"
					if c.notNull {
						nullable = "NO"
					}
					extra := ""
					if c.autoIncrement {
						extra = "auto_increment"
					}
					data = append(data, []any{
						t.name, c.name, c.colType, c.dataType, nullable, nil, extra, "",
					})
				}
			}
			return []string{
				"TABLE_NAME", "COLUMN_NAME", "COLUMN_TYPE", "DATA_TYPE",
				"IS_NULLABLE", "COLUMN_DEFAULT", "EXTRA", "COLUMN_COMMENT",
			}, data, true
		case strings.Contains(q, "information_schema.STATISTICS"):
			data := [][]any{}
			for _, t := range tables {
				if t.pk == "" {
					continue
				}
				data = append(data, []any{t.name, "PRIMARY", 0, t.pk, nil, "BTREE", ""})
			}
			return []string{
				"TABLE_NAME", "INDEX_NAME", "NON_UNIQUE", "COLUMN_NAME",
				"SUB_PART", "INDEX_TYPE", "INDEX_COMMENT",
			}, data, true
		}
		return nil, nil, false
	})
}

// usersSchema is the Go schema under test: the users table alone, so
// audit_log is undeclared, with email optional and amount optionally
// retyped so one subtest can ask for each kind of destructive change.
func usersSchema(withEmail, retypeAmount bool) *mysql.Schema {
	users := mysql.NewTable("users")
	mysql.Add(users, mysql.BigSerial("id").PrimaryKey())
	if withEmail {
		mysql.Add(users, mysql.Varchar("email", 190).NotNull())
	}
	if retypeAmount {
		mysql.Add(users, mysql.BigInt("amount").NotNull())
	} else {
		mysql.Add(users, mysql.Varchar("amount", 32).NotNull())
	}
	return mysql.NewSchema(users)
}

// appliedDDL returns the schema statements the driver was asked to run,
// so a test can prove a withheld statement never reached the server.
func appliedDDL(drv *dropstest.Driver) []string {
	var out []string
	for _, q := range drv.SQL() {
		s := strings.TrimSpace(q)
		if strings.HasPrefix(s, "DROP TABLE") || strings.HasPrefix(s, "ALTER TABLE") ||
			strings.HasPrefix(s, "CREATE TABLE") {
			out = append(out, s)
		}
	}
	return out
}

func TestPushWithholdsDestructiveChangesToTablesWithRows(t *testing.T) {
	tests := []struct {
		name      string
		schema    *mysql.Schema
		opts      mysql.PushOptions
		estimates map[string]int64
		empty     map[string]bool
		wantOp    mysql.DestructiveOp
		wantObj   string
		wantRows  int64
	}{
		{
			name:      "drop table with rows",
			schema:    usersSchema(true, false),
			opts:      mysql.PushOptions{DropUnmanagedTables: true},
			estimates: map[string]int64{"users": 0, "audit_log": 4200},
			empty:     map[string]bool{"users": true},
			wantOp:    mysql.OpDropTable,
			wantRows:  4200,
		},
		{
			// TABLE_ROWS is NULL on a table the server has no
			// statistics for. The probe settles it, and finds rows.
			name:      "drop table with no statistics but holding rows",
			schema:    usersSchema(true, false),
			opts:      mysql.PushOptions{DropUnmanagedTables: true},
			estimates: map[string]int64{"users": 0},
			empty:     map[string]bool{"users": true},
			wantOp:    mysql.OpDropTable,
			wantRows:  -1,
		},
		{
			// The InnoDB case the estimate cannot be trusted on: the
			// server reports 0 for a table that is not empty.
			name:      "drop column on a table InnoDB estimates at zero",
			schema:    usersSchema(false, false),
			estimates: map[string]int64{"users": 0, "audit_log": 0},
			empty:     map[string]bool{"audit_log": true},
			wantOp:    mysql.OpDropColumn,
			wantObj:   "email",
			wantRows:  -1,
		},
		{
			name:      "drop column with rows",
			schema:    usersSchema(false, false),
			estimates: map[string]int64{"users": 12, "audit_log": 0},
			wantOp:    mysql.OpDropColumn,
			wantObj:   "email",
			wantRows:  12,
		},
		{
			name:      "retype column with rows",
			schema:    usersSchema(true, true),
			estimates: map[string]int64{"users": 12, "audit_log": 0},
			wantOp:    mysql.OpRetypeColumn,
			wantObj:   "amount",
			wantRows:  12,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			drv := pushDriver(sharedDatabase(), tt.estimates, tt.empty)
			db := mysql.New(drv)

			res, err := mysql.Push(context.Background(), db, tt.schema, tt.opts)
			if !errors.Is(err, mysql.ErrDestructivePush) {
				t.Fatalf("err = %v, want ErrDestructivePush", err)
			}
			if res == nil {
				t.Fatal("PushResult = nil, want the withheld plan alongside the error")
			}
			if res.Applied || res.AppliedCount != 0 {
				t.Errorf("Applied = %v, AppliedCount = %v, want false and 0: nothing may run before the refusal",
					res.Applied, res.AppliedCount)
			}
			if len(res.DataLoss) != 1 {
				t.Fatalf("DataLoss = %+v, want exactly one entry", res.DataLoss)
			}
			got := res.DataLoss[0]
			if got.Op != tt.wantOp {
				t.Errorf("Op = %v, want %v", got.Op, tt.wantOp)
			}
			if got.Object != tt.wantObj {
				t.Errorf("Object = %v, want %v", got.Object, tt.wantObj)
			}
			if got.Rows != tt.wantRows {
				t.Errorf("Rows = %v, want %v", got.Rows, tt.wantRows)
			}
			if got.SQL == "" || got.Suggestion == "" {
				t.Errorf("DataLoss = %+v, want the withheld SQL and a way to allow it", got)
			}
			if ddl := appliedDDL(drv); len(ddl) != 0 {
				t.Errorf("executed = %v, want nothing executed", ddl)
			}
		})
	}
}

func TestPushAppliesDestructiveChangeThatWasNamed(t *testing.T) {
	tests := []struct {
		name  string
		allow []mysql.Destructive
		want  bool
	}{
		{
			name:  "the exact column",
			allow: []mysql.Destructive{{Op: mysql.OpDropColumn, Table: "users", Object: "email"}},
			want:  true,
		},
		{
			name:  "yesterday's consent for another column",
			allow: []mysql.Destructive{{Op: mysql.OpDropColumn, Table: "users", Object: "amount"}},
			want:  false,
		},
		{
			name:  "the right object, the wrong operation",
			allow: []mysql.Destructive{{Op: mysql.OpDropTable, Table: "users"}},
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			drv := pushDriver(sharedDatabase(), map[string]int64{"users": 12, "audit_log": 0}, nil)
			db := mysql.New(drv)

			res, err := mysql.Push(context.Background(), db, usersSchema(false, false),
				mysql.PushOptions{Allow: tt.allow})
			if tt.want {
				if err != nil {
					t.Fatalf("Push: %v", err)
				}
				if !res.Applied {
					t.Error("Applied = false, want true")
				}
				want := "ALTER TABLE `users` DROP COLUMN `email`;"
				if got := appliedDDL(drv); len(got) != 1 || got[0] != want {
					t.Errorf("executed = %v, want [%v]", got, want)
				}
				return
			}
			if !errors.Is(err, mysql.ErrDestructivePush) {
				t.Fatalf("err = %v, want ErrDestructivePush", err)
			}
		})
	}
}

// An empty table has nothing to lose, which is what keeps the
// development loop — push a table, reshape it a minute later — free of
// consent values nobody would read.
func TestPushDropsFromAnEmptyTableWithoutConsent(t *testing.T) {
	drv := pushDriver(sharedDatabase(), map[string]int64{"users": 0, "audit_log": 0},
		map[string]bool{"users": true, "audit_log": true})
	db := mysql.New(drv)

	res, err := mysql.Push(context.Background(), db, usersSchema(false, false))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	want := "ALTER TABLE `users` DROP COLUMN `email`;"
	if len(res.Statements) != 1 || res.Statements[0] != want {
		t.Fatalf("Statements = %v, want [%v]", res.Statements, want)
	}
	if !res.Applied {
		t.Error("Applied = false, want true — an empty table has nothing to lose")
	}
}

// A batched ALTER TABLE carries several actions, and only some of them
// destroy data. The rule has to see the actions, not the statement, or
// it either withholds an ADD COLUMN or lets a DROP COLUMN through
// beside one.
func TestPushSeesTheDestructiveActionInsideABatchedAlter(t *testing.T) {
	live := []liveTable{{
		name: "users",
		pk:   "id",
		columns: []liveColumn{
			{name: "id", colType: "bigint", dataType: "bigint", notNull: true, autoIncrement: true},
			{name: "email", colType: "varchar(190)", dataType: "varchar", notNull: true},
		},
	}}
	users := mysql.NewTable("users")
	mysql.Add(users, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(users, mysql.Varchar("nickname", 40).NotNull())

	drv := pushDriver(live, map[string]int64{"users": 12}, nil)
	res, err := mysql.Push(context.Background(), mysql.New(drv), mysql.NewSchema(users))
	if !errors.Is(err, mysql.ErrDestructivePush) {
		t.Fatalf("err = %v, want ErrDestructivePush", err)
	}
	if len(res.Statements) != 1 || !strings.Contains(res.Statements[0], "ADD COLUMN") {
		t.Fatalf("Statements = %v, want one batched ALTER carrying both actions", res.Statements)
	}
	if len(res.DataLoss) != 1 || res.DataLoss[0].Object != "email" {
		t.Fatalf("DataLoss = %+v, want just the dropped column", res.DataLoss)
	}
	if res.DataLoss[0].SQL != res.Statements[0] {
		t.Errorf("DataLoss SQL = %v, want the whole statement it is part of", res.DataLoss[0].SQL)
	}
}

// The same rule drops/pg applies to a rename left behind in the call,
// applied to a consent left behind in the call.
func TestPushReportsConsentThatAuthorisesNothing(t *testing.T) {
	tests := []struct {
		name      string
		allow     []mysql.Destructive
		wantStale []string // Table.Object of each stale-consent notice
		wantErr   bool
	}{
		{
			name:  "consent that names the change",
			allow: []mysql.Destructive{{Op: mysql.OpDropColumn, Table: "users", Object: "email"}},
		},
		{
			name:      "a table drop written with an object",
			allow:     []mysql.Destructive{{Op: mysql.OpDropTable, Table: "users", Object: "users"}},
			wantStale: []string{"users.users"},
			wantErr:   true,
		},
		{
			name:      "consent for a table that is no longer there",
			allow:     []mysql.Destructive{{Op: mysql.OpDropColumn, Table: "customers", Object: "email"}},
			wantStale: []string{"customers.email"},
			wantErr:   true,
		},
		{
			name: "yesterday's consent beside today's",
			allow: []mysql.Destructive{
				{Op: mysql.OpDropColumn, Table: "users", Object: "email"},
				{Op: mysql.OpDropColumn, Table: "users", Object: "nickname"},
			},
			wantStale: []string{"users.nickname"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			drv := pushDriver(sharedDatabase(), map[string]int64{"users": 12, "audit_log": 0}, nil)
			db := mysql.New(drv)

			res, err := mysql.Push(context.Background(), db, usersSchema(false, false),
				mysql.PushOptions{Allow: tt.allow})
			if got := errors.Is(err, mysql.ErrDestructivePush); got != tt.wantErr {
				t.Fatalf("ErrDestructivePush: got = %v (%v), want %v", got, err, tt.wantErr)
			}
			var stale []string
			for _, n := range res.Notices {
				if n.Rule != "stale-consent" {
					continue
				}
				if n.Message == "" {
					t.Errorf("notice %+v carries no message; the caller has nothing to act on", n)
				}
				stale = append(stale, n.Table+"."+n.Object)
			}
			if !reflect.DeepEqual(stale, tt.wantStale) {
				t.Errorf("stale-consent notices: got = %v, want %v", stale, tt.wantStale)
			}
		})
	}
}
