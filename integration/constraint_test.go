package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/mysql"
	"github.com/bernardoforcillo/drops/pg"
	"github.com/bernardoforcillo/drops/sqlite"
	"github.com/bernardoforcillo/drops/stdlib"

	_ "modernc.org/sqlite"
)

// The server names the constraint it refused on, and drops has to hand
// that name back. Everything the field-error mapping does is built on
// it, and a driver reports it as a struct field rather than through a
// method — which is exactly the sort of thing only a live server
// catches.
func TestPGErrorCarriesTheConstraintTheServerNamed(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()

	users := pg.NewTable(integration.UniqueName(t, "u"))
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	email := pg.Add(users, pg.Text("email").NotNull().Unique())
	age := pg.Add(users, pg.Integer("age"))
	users.AddCheck(users.Name()+"_age_check", `"age" >= 0`)
	dropPG(t, db, users)
	execPG(t, db, pg.CreateTable(users))

	if _, err := db.Insert(users).Row(email.Val("a@example.com"), age.Val(1)).Exec(ctx); err != nil {
		t.Fatalf("seed insert: %v", err)
	}

	_, err := db.Insert(users).Row(email.Val("a@example.com"), age.Val(2)).Exec(ctx)
	var pe *pg.PgError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v (%T), want a *pg.PgError", err, err)
	}
	if !errors.Is(err, pg.ErrUniqueViolation) {
		t.Fatalf("errors.Is(err, ErrUniqueViolation) = false for %v", err)
	}
	if want := users.Name() + "_email_key"; pe.Constraint != want {
		t.Fatalf("Constraint = %q, want %q (from %v)", pe.Constraint, want, err)
	}

	// A not-null violation names a column instead of a constraint,
	// and that column is the only thing that can point at a field.
	_, err = db.Insert(users).Row(age.Val(3)).Exec(ctx)
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v (%T), want a *pg.PgError", err, err)
	}
	if !errors.Is(err, pg.ErrNotNullViolation) {
		t.Fatalf("errors.Is(err, ErrNotNullViolation) = false for %v", err)
	}
	if pe.Column != "email" {
		t.Fatalf("Column = %q, want %q (from %v)", pe.Column, "email", err)
	}

	_, err = db.Insert(users).Row(email.Val("b@example.com"), age.Val(-1)).Exec(ctx)
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v (%T), want a *pg.PgError", err, err)
	}
	if want := users.Name() + "_age_check"; pe.Constraint != want {
		t.Fatalf("Constraint = %q, want %q (from %v)", pe.Constraint, want, err)
	}
}

// pgAccount is the struct the field errors below are reported
// against.
type pgAccount struct {
	ID      int64  `drop:"id"`
	Email   string `drop:"email"`
	Nick    string `drop:"nick"`
	TeamID  int64  `drop:"team_id"`
	Balance int32  `drop:"balance"`
}

type pgTeam struct {
	ID   int64  `drop:"id"`
	Name string `drop:"name"`
}

// Every violation the mapping claims to translate, caused on a live
// server, asserted by the field name that comes back. Constructing a
// PgError by hand would prove the switch statement and nothing at all
// about the names PostgreSQL generates.
func TestPGConstraintViolationNamesTheField(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()

	teams := pg.NewTable(integration.UniqueName(t, "team"))
	teamID := pg.Add(teams, pg.BigSerial("id").PrimaryKey())
	pg.Add(teams, pg.Text("name").NotNull())

	accounts := pg.NewTable(integration.UniqueName(t, "acct"))
	pg.Add(accounts, pg.BigSerial("id").PrimaryKey())
	pg.Add(accounts, pg.Text("email").NotNull().Unique())
	nick := pg.Add(accounts, pg.Text("nick"))
	pg.Add(accounts, pg.BigInt("team_id").References(teamID))
	pg.Add(accounts, pg.Integer("balance").NotNull().Default("0"))
	accounts.AddCheck(accounts.Name()+"_balance_check", `"balance" >= 0`)
	// A unique index created by name: PostgreSQL calls the constraint
	// whatever the index is called, so nothing about the column is
	// recoverable from it and the mapping has to be declared.
	nickIdx := pg.NewIndex(integration.UniqueName(t, "nick_idx"), accounts, nick).Unique()

	// Cleanups run last-registered-first, so the child table has to be
	// registered second or the parent's DROP is refused for still
	// being referenced — and the leftover poisons the next run.
	dropPG(t, db, teams)
	dropPG(t, db, accounts)
	execPG(t, db, pg.CreateTable(teams))
	execPG(t, db, pg.CreateTable(accounts))
	execPG(t, db, pg.CreateIndex(nickIdx))

	// The parent side of a foreign key: PostgreSQL blames the child
	// table's constraint, which the parent's entity cannot derive
	// anything from — so it is declared, exactly as Ecto's
	// no_assoc_constraint has to be.
	teamEntity := pg.NewEntity[pgTeam](teams).
		MapConstraint(accounts.Name()+"_team_id_fkey", "ID")
	accountEntity := pg.NewEntity[pgAccount](accounts).
		MapConstraint(nickIdx.Name(), "Nick")

	home := pgTeam{Name: "home"}
	if err := teamEntity.Create(db, ctx, &home); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	first := pgAccount{Email: "a@example.com", Nick: "ada", TeamID: home.ID, Balance: 1}
	if err := accountEntity.Create(db, ctx, &first); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	cases := []struct {
		name       string
		row        pgAccount
		wantField  string
		wantColumn string
		wantRule   drops.Rule
		wantIs     error
	}{
		{
			name:       "unique column",
			row:        pgAccount{Email: "a@example.com", Nick: "grace", TeamID: home.ID, Balance: 1},
			wantField:  "Email",
			wantColumn: "email",
			wantRule:   drops.RuleUnique,
			wantIs:     pg.ErrUniqueViolation,
		},
		{
			name:       "named unique index",
			row:        pgAccount{Email: "b@example.com", Nick: "ada", TeamID: home.ID, Balance: 1},
			wantField:  "Nick",
			wantColumn: "nick",
			wantRule:   drops.RuleUnique,
			wantIs:     pg.ErrUniqueViolation,
		},
		{
			name:       "foreign key",
			row:        pgAccount{Email: "c@example.com", Nick: "grace", TeamID: 987654321, Balance: 1},
			wantField:  "TeamID",
			wantColumn: "team_id",
			wantRule:   drops.RuleForeignKey,
			wantIs:     pg.ErrForeignKeyViolation,
		},
		{
			name:       "check",
			row:        pgAccount{Email: "d@example.com", Nick: "hedy", TeamID: home.ID, Balance: -5},
			wantField:  "Balance",
			wantColumn: "balance",
			wantRule:   drops.RuleCheck,
			wantIs:     pg.ErrCheckViolation,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := tc.row
			err := accountEntity.Create(db, ctx, &row)
			fe, ok := drops.AsFieldError(err)
			if !ok {
				t.Fatalf("err = %v (%T), want a *drops.FieldError", err, err)
			}
			if fe.Field != tc.wantField {
				t.Errorf("Field = %q, want %q (constraint %q)", fe.Field, tc.wantField, fe.Constraint)
			}
			if fe.Column != tc.wantColumn {
				t.Errorf("Column = %q, want %q", fe.Column, tc.wantColumn)
			}
			if fe.Rule != tc.wantRule {
				t.Errorf("Rule = %q, want %q", fe.Rule, tc.wantRule)
			}
			// The driver error is still underneath: a codebase that
			// already branches on the sentinel keeps working.
			if !errors.Is(err, tc.wantIs) {
				t.Errorf("errors.Is(err, %v) = false", tc.wantIs)
			}
			var pe *pg.PgError
			if !errors.As(err, &pe) {
				t.Errorf("errors.As to *pg.PgError = false")
			}
		})
	}

	// A not-null violation names no constraint at all, and resolves
	// through the column PostgreSQL blames instead.
	t.Run("not null", func(t *testing.T) {
		if _, err := db.Exec(ctx, `INSERT INTO `+drops.StdQuoteIdent(accounts.Name())+` (email, balance) VALUES (NULL, 1)`); err != nil {
			fe, ok := drops.AsFieldError(accountEntity.FieldError(err))
			if !ok {
				t.Fatalf("err = %v (%T), want a *drops.FieldError", err, err)
			}
			if fe.Field != "Email" || fe.Rule != drops.RuleNotNull {
				t.Fatalf("Field = %q Rule = %q, want Email/not_null", fe.Field, fe.Rule)
			}
		} else {
			t.Fatal("INSERT of a NULL email succeeded")
		}
	})

	// The primary key is a unique constraint like any other, and
	// PostgreSQL names it after the table rather than the column.
	t.Run("primary key", func(t *testing.T) {
		dup := pgAccount{ID: first.ID, Email: "e@example.com", Nick: "mary", TeamID: home.ID, Balance: 1}
		err := accountEntity.Create(db, ctx, &dup)
		fe, ok := drops.AsFieldError(err)
		if !ok {
			t.Fatalf("err = %v (%T), want a *drops.FieldError", err, err)
		}
		if fe.Field != "ID" || fe.Rule != drops.RuleUnique {
			t.Fatalf("Field = %q Rule = %q, want ID/unique", fe.Field, fe.Rule)
		}
	})

	// DELETE is a write path too, and refusing to delete a row another
	// row still references is the second thing every form layer needs
	// to say out loud.
	t.Run("delete refused by a child row", func(t *testing.T) {
		_, err := teamEntity.Delete(db, ctx, home.ID)
		fe, ok := drops.AsFieldError(err)
		if !ok {
			t.Fatalf("err = %v (%T), want a *drops.FieldError", err, err)
		}
		if fe.Field != "ID" || fe.Rule != drops.RuleForeignKey {
			t.Fatalf("Field = %q Rule = %q, want ID/foreign_key", fe.Field, fe.Rule)
		}
	})

	// A failure that is not a constraint violation is left exactly as
	// it was: a FieldError that cannot name a field is worse than the
	// error it replaced.
	t.Run("unrelated failure passes through", func(t *testing.T) {
		_, err := db.Exec(ctx, `SELECT * FROM a_table_that_does_not_exist_here`)
		if _, ok := drops.AsFieldError(accountEntity.FieldError(err)); ok {
			t.Fatalf("err = %v became a FieldError", err)
		}
		if !errors.Is(err, pg.ErrUndefinedTable) {
			t.Fatalf("errors.Is(err, ErrUndefinedTable) = false for %v", err)
		}
	})

}

// myAccount is the MySQL counterpart of pgAccount.
type myAccount struct {
	ID      int64  `drop:"id"`
	Email   string `drop:"email"`
	Nick    string `drop:"nick"`
	TeamID  int64  `drop:"team_id"`
	Balance int32  `drop:"balance"`
}

type myTeam struct {
	ID   int64  `drop:"id"`
	Name string `drop:"name"`
}

// The same translation against MySQL, which reports the failure
// differently at every step: an index name rather than a constraint
// name for a duplicate, the foreign key buried in the message text, and
// a not-null violation that names only the column.
func TestMySQLConstraintViolationNamesTheField(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()

	teams := mysql.NewTable(integration.UniqueName(t, "team"))
	teamID := mysql.Add(teams, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(teams, mysql.Varchar("name", 100).NotNull())

	accounts := mysql.NewTable(integration.UniqueName(t, "acct"))
	mysql.Add(accounts, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(accounts, mysql.Varchar("email", 191).NotNull().Unique())
	nick := mysql.Add(accounts, mysql.Varchar("nick", 191))
	mysql.Add(accounts, mysql.BigInt("team_id").References(teamID))
	mysql.Add(accounts, mysql.Integer("balance").NotNull().Default("0"))
	nickIdx := mysql.NewIndex(integration.UniqueName(t, "nick_idx"), accounts, nick).Unique()
	// Declared on the table as well as executed, so the name is
	// derivable rather than needing a MapConstraint call.
	accounts.AddIndex(nickIdx)

	// Registered parent-first, so the LIFO cleanup drops the child
	// before the parent it still references.
	dropMySQL(t, db, teams)
	dropMySQL(t, db, accounts)
	execMySQL(t, db, mysql.CreateTable(teams))
	execMySQL(t, db, mysql.CreateTable(accounts))
	execMySQL(t, db, nickIdx)
	// CreateTable does not render CHECK constraints — drops adds them
	// by ALTER — so this one is added the way a migration would.
	checkName := accounts.Name() + "_balance_check"
	if _, err := db.Exec(ctx, "ALTER TABLE `"+accounts.Name()+"` ADD CONSTRAINT `"+checkName+"` CHECK (balance >= 0)"); err != nil {
		t.Fatalf("add check: %v", err)
	}

	teamEntity := mysql.NewEntity[myTeam](teams)
	accountEntity := mysql.NewEntity[myAccount](accounts).
		MapConstraint(checkName, "Balance")

	home := myTeam{Name: "home"}
	if err := teamEntity.Create(db, ctx, &home); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	first := myAccount{Email: "a@example.com", Nick: "ada", TeamID: home.ID, Balance: 1}
	if err := accountEntity.Create(db, ctx, &first); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	cases := []struct {
		name       string
		row        myAccount
		wantField  string
		wantColumn string
		wantRule   drops.Rule
		wantIs     error
	}{
		{
			name:       "unique column",
			row:        myAccount{Email: "a@example.com", Nick: "grace", TeamID: home.ID, Balance: 1},
			wantField:  "Email",
			wantColumn: "email",
			wantRule:   drops.RuleUnique,
			wantIs:     mysql.ErrUniqueViolation,
		},
		{
			name:       "named unique index",
			row:        myAccount{Email: "b@example.com", Nick: "ada", TeamID: home.ID, Balance: 1},
			wantField:  "Nick",
			wantColumn: "nick",
			wantRule:   drops.RuleUnique,
			wantIs:     mysql.ErrUniqueViolation,
		},
		{
			name:       "foreign key",
			row:        myAccount{Email: "c@example.com", Nick: "grace", TeamID: 987654321, Balance: 1},
			wantField:  "TeamID",
			wantColumn: "team_id",
			wantRule:   drops.RuleForeignKey,
			wantIs:     mysql.ErrForeignKeyViolation,
		},
		{
			name:       "check",
			row:        myAccount{Email: "d@example.com", Nick: "hedy", TeamID: home.ID, Balance: -5},
			wantField:  "Balance",
			wantColumn: "balance",
			wantRule:   drops.RuleCheck,
			wantIs:     mysql.ErrCheckViolation,
		},
		{
			name:       "primary key",
			row:        myAccount{ID: 1, Email: "e@example.com", Nick: "mary", TeamID: home.ID, Balance: 1},
			wantField:  "ID",
			wantColumn: "id",
			wantRule:   drops.RuleUnique,
			wantIs:     mysql.ErrUniqueViolation,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := tc.row
			if tc.name == "primary key" {
				row.ID = first.ID
			}
			err := accountEntity.Create(db, ctx, &row)
			fe, ok := drops.AsFieldError(err)
			if !ok {
				t.Fatalf("err = %v (%T), want a *drops.FieldError", err, err)
			}
			if fe.Field != tc.wantField {
				t.Errorf("Field = %q, want %q (constraint %q)", fe.Field, tc.wantField, fe.Constraint)
			}
			if fe.Column != tc.wantColumn {
				t.Errorf("Column = %q, want %q", fe.Column, tc.wantColumn)
			}
			if fe.Rule != tc.wantRule {
				t.Errorf("Rule = %q, want %q", fe.Rule, tc.wantRule)
			}
			if !errors.Is(err, tc.wantIs) {
				t.Errorf("errors.Is(err, %v) = false", tc.wantIs)
			}
			var se *mysql.ServerError
			if !errors.As(err, &se) {
				t.Errorf("errors.As to *mysql.ServerError = false")
			}
		})
	}

	t.Run("not null", func(t *testing.T) {
		_, err := db.Exec(ctx, "INSERT INTO `"+accounts.Name()+"` (email, balance) VALUES (NULL, 1)")
		fe, ok := drops.AsFieldError(accountEntity.FieldError(err))
		if !ok {
			t.Fatalf("err = %v (%T), want a *drops.FieldError", err, err)
		}
		if fe.Field != "Email" || fe.Rule != drops.RuleNotNull {
			t.Fatalf("Field = %q Rule = %q, want Email/not_null", fe.Field, fe.Rule)
		}
	})
}

// SQLite reports a constraint failure as an extended result code and a
// message naming the columns — no SQLSTATE, no error number, nothing
// the protocol carries. The classifier has to hold against the real
// engine, which is the only place those codes and words come from.
func TestSQLiteClassifiesConstraintFailures(t *testing.T) {
	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	db := sqlite.New(stdlib.New(sqlDB))
	ctx := context.Background()

	// Foreign keys are off by default and per connection.
	if _, err := db.Exec(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("pragma: %v", err)
	}
	for _, ddl := range []string{
		`CREATE TABLE teams (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`,
		`CREATE TABLE accounts (
			id INTEGER PRIMARY KEY,
			email TEXT NOT NULL UNIQUE,
			team_id INTEGER REFERENCES teams(id),
			balance INTEGER NOT NULL CONSTRAINT balance_nonneg CHECK (balance >= 0)
		)`,
		`INSERT INTO teams (id, name) VALUES (1, 'home')`,
		`INSERT INTO accounts (id, email, team_id, balance) VALUES (1, 'a@example.com', 1, 5)`,
	} {
		if _, err := db.Exec(ctx, ddl); err != nil {
			t.Fatalf("setup %q: %v", ddl, err)
		}
	}

	cases := []struct {
		name        string
		stmt        string
		want        error
		wantCode    int
		wantDetail  string
		wantColumns []string
	}{
		{
			name:        "unique",
			stmt:        `INSERT INTO accounts (id, email, team_id, balance) VALUES (2, 'a@example.com', 1, 1)`,
			want:        sqlite.ErrUniqueViolation,
			wantCode:    2067,
			wantDetail:  "accounts.email",
			wantColumns: []string{"email"},
		},
		{
			name:        "primary key",
			stmt:        `INSERT INTO accounts (id, email, team_id, balance) VALUES (1, 'b@example.com', 1, 1)`,
			want:        sqlite.ErrUniqueViolation,
			wantCode:    1555,
			wantDetail:  "accounts.id",
			wantColumns: []string{"id"},
		},
		{
			name:        "not null",
			stmt:        `INSERT INTO accounts (id, email, team_id, balance) VALUES (3, NULL, 1, 1)`,
			want:        sqlite.ErrNotNullViolation,
			wantCode:    1299,
			wantDetail:  "accounts.email",
			wantColumns: []string{"email"},
		},
		{
			name:       "check",
			stmt:       `INSERT INTO accounts (id, email, team_id, balance) VALUES (4, 'c@example.com', 1, -1)`,
			want:       sqlite.ErrCheckViolation,
			wantCode:   275,
			wantDetail: "balance_nonneg",
		},
		{
			name:     "foreign key",
			stmt:     `INSERT INTO accounts (id, email, team_id, balance) VALUES (5, 'd@example.com', 42, 1)`,
			want:     sqlite.ErrForeignKeyViolation,
			wantCode: 787,
		},
		{
			name:     "undefined table",
			stmt:     `INSERT INTO nowhere (id) VALUES (1)`,
			want:     sqlite.ErrUndefinedTable,
			wantCode: 1,
		},
		{
			name:     "undefined column",
			stmt:     `SELECT nope FROM accounts`,
			want:     sqlite.ErrUndefinedColumn,
			wantCode: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.Exec(ctx, tc.stmt)
			if err == nil {
				t.Fatalf("statement succeeded: %s", tc.stmt)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("errors.Is(err, %v) = false for %v", tc.want, err)
			}
			var se *sqlite.SQLiteError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v (%T), want a *sqlite.SQLiteError", err, err)
			}
			if se.Code != tc.wantCode {
				t.Errorf("Code = %d, want %d", se.Code, tc.wantCode)
			}
			if se.Detail != tc.wantDetail {
				t.Errorf("Detail = %q, want %q", se.Detail, tc.wantDetail)
			}
			if !reflect.DeepEqual(se.Columns, tc.wantColumns) {
				t.Errorf("Columns = %#v, want %#v", se.Columns, tc.wantColumns)
			}
			if len(tc.wantColumns) > 0 && se.Table != "accounts" {
				t.Errorf("Table = %q, want %q", se.Table, "accounts")
			}
		})
	}

	// A statement that fails for a reason SQLite did not classify —
	// here a cancelled context — must come back as it was.
	t.Run("unrelated failure passes through", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		_, err := db.Exec(cancelled, `SELECT 1`)
		if err == nil {
			t.Skip("driver ran the statement despite the cancelled context")
		}
		var se *sqlite.SQLiteError
		if errors.As(err, &se) {
			t.Fatalf("err = %v was wrapped as a constraint failure", err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("errors.Is(err, context.Canceled) = false for %v", err)
		}
	})
}
