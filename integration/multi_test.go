package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/mysql"
	"github.com/bernardoforcillo/drops/pg"
	"github.com/bernardoforcillo/drops/sqlite"
	"github.com/bernardoforcillo/drops/stdlib"
)

// drops.Multi promises that a script of named steps is one
// transaction: every step commits together, and a step that fails
// takes the earlier ones down with it.
//
// The first half of that is the root package's own logic and is
// asserted in multi_test.go next to it. The second half is not. That a
// ROLLBACK actually removes rows an earlier step inserted is the
// server's behaviour, and it is exactly the claim a fake driver
// counting Commit calls cannot make. So it is asserted here, against
// each engine, with a real constraint violation as the failure — the
// three-in-the-morning shape the type exists for.
//
// Multi takes a drops.Driver and nothing else, so these legs differ
// only in their DDL and their placeholder syntax. That is the point:
// it is one implementation for every dialect.

// multiRollbackLeg is one engine's answer to the same two questions.
func multiRollbackLeg(t *testing.T, drv drops.Driver, table, ph1, ph2 string) {
	t.Helper()
	ctx := context.Background()
	insert := "INSERT INTO " + table + " (id, label) VALUES (" + ph1 + ", " + ph2 + ")"

	count := func() int64 {
		t.Helper()
		rows, err := drv.Query(ctx, "SELECT count(*) FROM "+table)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		defer func() { _ = rows.Close() }()
		var n int64
		if !rows.Next() {
			t.Fatalf("count returned no row: %v", rows.Err())
		}
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan count: %v", err)
		}
		return n
	}

	t.Run("commits every step together", func(t *testing.T) {
		m := drops.NewMulti()
		drops.Step(m, "team", func(ctx context.Context, tx drops.Tx, _ drops.Results) (int64, error) {
			if _, err := tx.Exec(ctx, insert, 1, "team"); err != nil {
				return 0, err
			}
			return 1, nil
		})
		drops.Step(m, "audit", func(ctx context.Context, tx drops.Tx, prior drops.Results) (int64, error) {
			teamID, err := drops.StepResult[int64](prior, "team")
			if err != nil {
				return 0, err
			}
			if _, err := tx.Exec(ctx, insert, teamID+1, "audit"); err != nil {
				return 0, err
			}
			return teamID + 1, nil
		})

		res, err := m.Exec(ctx, drv)
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if got := strings.Join(res.Names(), ","); got != "team,audit" {
			t.Errorf("Results.Names() = %q", got)
		}
		if n := count(); n != 2 {
			t.Errorf("%d rows committed, want 2", n)
		}
	})

	t.Run("a failing step rolls back the earlier ones", func(t *testing.T) {
		m := drops.NewMulti().
			Run("first", func(ctx context.Context, tx drops.Tx, _ drops.Results) (any, error) {
				_, err := tx.Exec(ctx, insert, 100, "first")
				return int64(100), err
			}).
			// Re-using id 1, which the previous subtest committed:
			// the server rejects it, and that rejection is the
			// error the Multi has to report and roll back on.
			Run("collides", func(ctx context.Context, tx drops.Tx, _ drops.Results) (any, error) {
				_, err := tx.Exec(ctx, insert, 1, "collides")
				return nil, err
			}).
			Run("never", func(ctx context.Context, tx drops.Tx, _ drops.Results) (any, error) {
				_, err := tx.Exec(ctx, insert, 101, "never")
				return nil, err
			})

		before := count()
		_, err := m.Exec(ctx, drv)
		if err == nil {
			t.Fatal("the duplicate key should have failed the Multi")
		}

		var me *drops.MultiError
		if !errors.As(err, &me) {
			t.Fatalf("error is %T (%v), want *drops.MultiError", err, err)
		}
		if me.FailedStep != "collides" || me.FailedStepIdx != 1 {
			t.Errorf("failed step = %q at %d, want collides at 1", me.FailedStep, me.FailedStepIdx)
		}
		if got := strings.Join(me.Completed.Names(), ","); got != "first" {
			t.Errorf("Completed = %q, want the one step that had succeeded", got)
		}
		if id, err := drops.StepResult[int64](me.Completed, "first"); err != nil || id != 100 {
			t.Errorf("the completed step's value is gone: %v (%v)", id, err)
		}

		// The row "first" inserted is what the rollback has to
		// take with it, and the row "never" would have inserted
		// must never have existed.
		if n := count(); n != before {
			t.Errorf("%d rows after the rollback, want %d — the transaction did not unwind", n, before)
		}
	})
}

// openRaw is what these tests need and the dialect openers do not
// give: the *sql.DB behind the dialect wrapper, so the same connection
// can build the table through the dialect and then run the Multi
// through a bare drops.Driver.
func openRaw(t *testing.T, driverName, dsn string) *sql.DB {
	t.Helper()
	sqlDB, err := sql.Open(driverName, dsn)
	if err != nil {
		t.Fatalf("open %s: %v", driverName, err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := sqlDB.PingContext(context.Background()); err != nil {
		t.Fatalf("ping %s: %v", dsn, err)
	}
	return sqlDB
}

func TestPGMultiIsOneTransaction(t *testing.T) {
	sqlDB := openRaw(t, "pgx", integration.DSN(t, integration.EnvPostgres))
	drv := stdlib.New(sqlDB)
	db := pg.New(drv)

	tbl := pg.NewTable(integration.UniqueName(t, "multi"))
	dropPG(t, db, tbl)
	pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	pg.Add(tbl, pg.Text("label").NotNull())
	execPG(t, db, pg.CreateTable(tbl))

	multiRollbackLeg(t, drv, `"`+tbl.Name()+`"`, "$1", "$2")
}

func TestMySQLMultiIsOneTransaction(t *testing.T) {
	sqlDB := openRaw(t, "mysql", integration.DSN(t, integration.EnvMySQL))
	drv := stdlib.New(sqlDB)
	db := mysql.New(drv)

	tbl := mysql.NewTable(integration.UniqueName(t, "multi"))
	dropMySQL(t, db, tbl)
	mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	mysql.Add(tbl, mysql.Text("label").NotNull())
	execMySQL(t, db, mysql.CreateTable(tbl))

	multiRollbackLeg(t, drv, "`"+tbl.Name()+"`", "?", "?")
}

func TestSQLiteMultiIsOneTransaction(t *testing.T) {
	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	// One connection: an in-memory database is per-connection, so a
	// pool would hand the Multi a different, empty database.
	sqlDB.SetMaxOpenConns(1)
	drv := stdlib.New(sqlDB)

	tbl := sqlite.NewTable("multi_rollback")
	sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(tbl, sqlite.Text("label").NotNull())
	exec(t, sqlite.New(drv), sqlite.CreateTable(tbl))

	multiRollbackLeg(t, drv, `"`+tbl.Name()+`"`, "?", "?")
}
