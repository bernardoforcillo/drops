package pg_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/pg"
)

// A live database holding two tables: the one the Go schema declares
// and one belonging to somebody else. That second table is the whole
// point — the shared Postgres (Supabase, Neon, an RDS instance serving
// four services, PostGIS) where a push used to emit
// DROP TABLE ... CASCADE for everything it had not been told about.
type liveColumn struct {
	name     string
	udt      string
	notNull  bool
	sequence bool // a nextval() default, which reads back as bigserial
}

type liveTable struct {
	name    string
	columns []liveColumn
}

func sharedDatabase() []liveTable {
	return []liveTable{
		{name: "users", columns: []liveColumn{
			{name: "id", udt: "int8", notNull: true, sequence: true},
			{name: "email", udt: "text", notNull: true},
			{name: "amount", udt: "text", notNull: true},
		}},
		{name: "audit_log", columns: []liveColumn{
			{name: "id", udt: "int8", notNull: true, sequence: true},
			{name: "payload", udt: "text"},
		}},
	}
}

// pushDriver answers the introspection queries for tables, the row
// estimate query from estimates, and the emptiness probe from empty.
// A table missing from estimates is one PostgreSQL has never analysed,
// which is what reltuples = -1 means and what sends Push to the probe.
//
// It answers by statement rather than by position, because the order
// of Push's catalogue reads is Push's business: it grew a row-estimate
// query and an emptiness probe in this phase alone, and a queue of
// result sets would have had to be rewritten for each. That is what
// [dropstest.Driver.Answer] is for, and it is why this file no longer
// carries a driver of its own.
func pushDriver(tables []liveTable, estimates map[string]int64, empty map[string]bool) *dropstest.Driver {
	return dropstest.New().Answer(func(q string, _ []any) ([]string, [][]any, bool) {
		switch {
		case strings.Contains(q, "SELECT EXISTS"):
			for name, isEmpty := range empty {
				if strings.Contains(q, `"`+name+`"`) {
					return []string{"exists"}, [][]any{{!isEmpty}}, true
				}
			}
			return []string{"exists"}, [][]any{{true}}, true
		case strings.Contains(q, "reltuples"):
			data := [][]any{}
			for _, t := range tables {
				est, ok := estimates[t.name]
				if !ok {
					est = -1
				}
				data = append(data, []any{t.name, est})
			}
			return []string{"relname", "reltuples"}, data, true
		case strings.Contains(q, "information_schema.tables"):
			data := [][]any{}
			for _, t := range tables {
				data = append(data, []any{"public", t.name})
			}
			return []string{"table_schema", "table_name"}, data, true
		case strings.Contains(q, "information_schema.columns"):
			data := [][]any{}
			for _, t := range tables {
				for _, c := range t.columns {
					nullable := "YES"
					if c.notNull {
						nullable = "NO"
					}
					var def, prec, scale any
					if c.sequence {
						def = "nextval('" + t.name + "_id_seq'::regclass)"
						prec, scale = int64(64), int64(0)
					}
					data = append(data, []any{
						"public", t.name, c.name, c.udt,
						nil, prec, scale, nullable, def,
					})
				}
			}
			return []string{
				"table_schema", "table_name", "column_name",
				"udt_name", "character_maximum_length",
				"numeric_precision", "numeric_scale",
				"is_nullable", "column_default",
			}, data, true
		case strings.Contains(q, "PRIMARY KEY"):
			data := [][]any{}
			for _, t := range tables {
				data = append(data, []any{"public", t.name, t.name + "_pkey", "id"})
			}
			return []string{
				"table_schema", "table_name", "constraint_name", "column_name",
			}, data, true
		}
		return nil, nil, false
	})
}

// usersSchema is the Go schema under test: the users table alone, so
// audit_log is undeclared, with email optional and amount optionally
// retyped so one subtest can ask for each kind of destructive change.
func usersSchema(withEmail, retypeAmount bool) *pg.Schema {
	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	if withEmail {
		pg.Add(users, pg.Text("email").NotNull())
	}
	if retypeAmount {
		pg.Add(users, pg.Integer("amount").NotNull())
	} else {
		pg.Add(users, pg.Text("amount").NotNull())
	}
	return pg.NewSchema(users)
}

// appliedDDL returns the schema statements the driver was asked to run,
// so a test can prove a withheld statement never reached the server.
func appliedDDL(drv *dropstest.Driver) []string {
	var out []string
	for _, q := range drv.SQL() {
		t := strings.TrimSpace(q)
		if strings.HasPrefix(t, "DROP TABLE") || strings.HasPrefix(t, "ALTER TABLE") ||
			strings.HasPrefix(t, "CREATE TABLE") {
			out = append(out, t)
		}
	}
	return out
}

func TestPushLeavesUndeclaredTablesAlone(t *testing.T) {
	drv := pushDriver(sharedDatabase(), map[string]int64{"users": 0, "audit_log": 0}, nil)
	db := pg.New(drv)

	res, err := pg.Push(context.Background(), db, usersSchema(true, false))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(res.Statements) != 0 {
		t.Errorf("Statements = %v, want none — audit_log is not drops's to drop", res.Statements)
	}
	if got := appliedDDL(drv); len(got) != 0 {
		t.Errorf("executed = %v, want nothing executed", got)
	}
	var notice pg.SchemaNotice
	for _, n := range res.Notices {
		if n.Rule == "unmanaged-table" {
			notice = n
		}
	}
	if notice.Table != "audit_log" {
		t.Fatalf("notices = %v, want an unmanaged-table notice for audit_log", res.Notices)
	}
	if want := `DROP TABLE "audit_log" CASCADE;`; notice.SQL != want {
		t.Errorf("notice SQL = %v, want %v", notice.SQL, want)
	}
}

func TestPushDropsUndeclaredTableWhenAskedAndItIsEmpty(t *testing.T) {
	drv := pushDriver(sharedDatabase(), map[string]int64{"users": 0, "audit_log": 0}, nil)
	db := pg.New(drv)

	res, err := pg.Push(context.Background(), db, usersSchema(true, false),
		pg.PushOptions{DropUnmanagedTables: true})
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	want := `DROP TABLE "audit_log" CASCADE;`
	if len(res.Statements) != 1 || res.Statements[0] != want {
		t.Fatalf("Statements = %v, want [%v]", res.Statements, want)
	}
	if !res.Applied {
		t.Error("Applied = false, want true")
	}
	for _, n := range res.Notices {
		if n.Rule == "unmanaged-table" {
			t.Errorf("notice %v: a table Push actually dropped is not a withheld one", n)
		}
	}
}

func TestPushWithholdsDestructiveChangesToTablesWithRows(t *testing.T) {
	cases := []struct {
		name      string
		schema    *pg.Schema
		opts      pg.PushOptions
		estimates map[string]int64
		empty     map[string]bool
		wantOp    pg.DestructiveOp
		wantObj   string
		wantRows  int64
	}{
		{
			name:      "drop table with rows",
			schema:    usersSchema(true, false),
			opts:      pg.PushOptions{DropUnmanagedTables: true},
			estimates: map[string]int64{"users": 0, "audit_log": 4200},
			wantOp:    pg.OpDropTable,
			wantRows:  4200,
		},
		{
			// reltuples is -1 on a table nothing has analysed. The
			// probe settles it, and here it finds rows.
			name:      "drop table never analysed but holding rows",
			schema:    usersSchema(true, false),
			opts:      pg.PushOptions{DropUnmanagedTables: true},
			estimates: map[string]int64{"users": 0},
			wantOp:    pg.OpDropTable,
			wantRows:  -1,
		},
		{
			name:      "drop column with rows",
			schema:    usersSchema(false, false),
			estimates: map[string]int64{"users": 12, "audit_log": 0},
			wantOp:    pg.OpDropColumn,
			wantObj:   "email",
			wantRows:  12,
		},
		{
			name:      "retype column with rows",
			schema:    usersSchema(true, true),
			estimates: map[string]int64{"users": 12, "audit_log": 0},
			wantOp:    pg.OpRetypeColumn,
			wantObj:   "amount",
			wantRows:  12,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			drv := pushDriver(sharedDatabase(), tc.estimates, tc.empty)
			db := pg.New(drv)

			res, err := pg.Push(context.Background(), db, tc.schema, tc.opts)
			if !errors.Is(err, pg.ErrDestructivePush) {
				t.Fatalf("err = %v, want ErrDestructivePush", err)
			}
			if res == nil {
				t.Fatal("PushResult = nil, want the withheld plan alongside the error")
			}
			if res.Applied {
				t.Error("Applied = true, want false")
			}
			if len(res.DataLoss) != 1 {
				t.Fatalf("DataLoss = %+v, want exactly one entry", res.DataLoss)
			}
			got := res.DataLoss[0]
			if got.Op != tc.wantOp {
				t.Errorf("Op = %v, want %v", got.Op, tc.wantOp)
			}
			if got.Object != tc.wantObj {
				t.Errorf("Object = %v, want %v", got.Object, tc.wantObj)
			}
			if got.Rows != tc.wantRows {
				t.Errorf("Rows = %v, want %v", got.Rows, tc.wantRows)
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
	cases := []struct {
		name  string
		allow []pg.Destructive
		want  bool
	}{
		{
			name:  "the exact column",
			allow: []pg.Destructive{{Op: pg.OpDropColumn, Table: "users", Object: "email"}},
			want:  true,
		},
		{
			name:  "yesterday's consent for another column",
			allow: []pg.Destructive{{Op: pg.OpDropColumn, Table: "users", Object: "amount"}},
			want:  false,
		},
		{
			name:  "the right object, the wrong operation",
			allow: []pg.Destructive{{Op: pg.OpDropTable, Table: "users"}},
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			drv := pushDriver(sharedDatabase(), map[string]int64{"users": 12, "audit_log": 0}, nil)
			db := pg.New(drv)

			res, err := pg.Push(context.Background(), db, usersSchema(false, false),
				pg.PushOptions{Allow: tc.allow})
			if tc.want {
				if err != nil {
					t.Fatalf("Push: %v", err)
				}
				if !res.Applied {
					t.Error("Applied = false, want true")
				}
				return
			}
			if !errors.Is(err, pg.ErrDestructivePush) {
				t.Fatalf("err = %v, want ErrDestructivePush", err)
			}
		})
	}
}

func TestPushDropsFromANeverAnalysedTableThatIsEmpty(t *testing.T) {
	// The development loop: a table pushed a minute ago has no
	// statistics yet, so the estimate is -1 and only the probe can say
	// that there is nothing in it.
	drv := pushDriver(sharedDatabase(), map[string]int64{}, map[string]bool{"users": true, "audit_log": true})
	db := pg.New(drv)

	res, err := pg.Push(context.Background(), db, usersSchema(false, false))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if !res.Applied || len(res.DataLoss) != 0 {
		t.Errorf("Applied = %v, DataLoss = %+v, want the drop applied: the table is empty",
			res.Applied, res.DataLoss)
	}
}

func TestPushDropsFromAnEmptyTableWithoutConsent(t *testing.T) {
	drv := pushDriver(sharedDatabase(), map[string]int64{"users": 0, "audit_log": 0}, nil)
	db := pg.New(drv)

	res, err := pg.Push(context.Background(), db, usersSchema(false, false))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	want := `ALTER TABLE "users" DROP COLUMN "email";`
	if len(res.Statements) != 1 || res.Statements[0] != want {
		t.Fatalf("Statements = %v, want [%v]", res.Statements, want)
	}
	if !res.Applied {
		t.Error("Applied = false, want true — an empty table has nothing to lose")
	}
}

func TestPushDryRunStillRefusesUnconsentedDataLoss(t *testing.T) {
	drv := pushDriver(sharedDatabase(), map[string]int64{"users": 12, "audit_log": 0}, nil)
	db := pg.New(drv)

	res, err := pg.Push(context.Background(), db, usersSchema(false, false),
		pg.PushOptions{DryRun: true})
	if !errors.Is(err, pg.ErrDestructivePush) {
		t.Fatalf("err = %v, want ErrDestructivePush", err)
	}
	if len(res.DataLoss) != 1 {
		t.Errorf("DataLoss = %+v, want the preview to name what it would destroy", res.DataLoss)
	}
}

// A consent that has quietly stopped applying is the more dangerous of
// the two stale declarations drops can be handed: applyRenames refuses
// one outright with ErrRenameNotApplicable, while an Allow entry that
// matches nothing used to be discarded without a word — leaving a call
// site that reads exactly like one whose DROP is still authorised.
func TestPushReportsConsentThatAuthorisesNothing(t *testing.T) {
	// The live users table has 12 rows and the Go schema no longer
	// declares its email column, so the diff carries exactly one
	// destructive change: OpDropColumn on users.email.
	tests := []struct {
		name      string
		allow     []pg.Destructive
		wantStale []string // Table.Object of each stale-consent notice
		wantErr   bool
	}{
		{
			name:  "consent that names the change",
			allow: []pg.Destructive{{Op: pg.OpDropColumn, Table: "users", Object: "email"}},
		},
		{
			// Object is empty on a table drop by construction, so this
			// value can never match anything Push derives.
			name:      "a table drop written with an object",
			allow:     []pg.Destructive{{Op: pg.OpDropTable, Table: "users", Object: "users"}},
			wantStale: []string{"users.users"},
			wantErr:   true,
		},
		{
			name:      "consent for a table that is no longer there",
			allow:     []pg.Destructive{{Op: pg.OpDropColumn, Table: "customers", Object: "email"}},
			wantStale: []string{"customers.email"},
			wantErr:   true,
		},
		{
			name: "yesterday's consent beside today's",
			allow: []pg.Destructive{
				{Op: pg.OpDropColumn, Table: "users", Object: "email"},
				{Op: pg.OpDropColumn, Table: "users", Object: "nickname"},
			},
			wantStale: []string{"users.nickname"},
		},
		{
			name:      "an operation that is not one",
			allow:     []pg.Destructive{{Op: "drop-database", Table: "users"}},
			wantStale: []string{"users."},
			wantErr:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			drv := pushDriver(sharedDatabase(), map[string]int64{"users": 12, "audit_log": 0}, nil)
			db := pg.New(drv)

			res, err := pg.Push(context.Background(), db, usersSchema(false, false),
				pg.PushOptions{Allow: tt.allow})
			if got := errors.Is(err, pg.ErrDestructivePush); got != tt.wantErr {
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

// A push with nothing to do is the likeliest place for a consent to
// have gone stale — the change it named is the one that has already
// been applied — so the notice has to survive the empty-diff exit.
func TestPushReportsStaleConsentWhenThereIsNothingToDo(t *testing.T) {
	drv := pushDriver(sharedDatabase(), map[string]int64{"users": 12, "audit_log": 0}, nil)
	db := pg.New(drv)

	res, err := pg.Push(context.Background(), db, usersSchema(true, false),
		pg.PushOptions{Allow: []pg.Destructive{{Op: pg.OpDropColumn, Table: "users", Object: "email"}}})
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(res.Statements) != 0 {
		t.Fatalf("Statements = %v, want none", res.Statements)
	}
	var stale []pg.SchemaNotice
	for _, n := range res.Notices {
		if n.Rule == "stale-consent" {
			stale = append(stale, n)
		}
	}
	if len(stale) != 1 || stale[0].Object != "email" {
		t.Errorf("notices = %v, want one stale-consent notice for users.email", res.Notices)
	}
}
