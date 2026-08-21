package integration_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/mysql"
	"github.com/bernardoforcillo/drops/pg"
	"github.com/bernardoforcillo/drops/sqlite"
)

// Writing NULL through the builder, against every engine there is.
//
// This belongs here rather than in a unit test because the claim is
// not about the SQL drops renders — that is asserted next to each
// dialect — but about what the server and its driver do with a nil
// parameter bound to a text column. Whether pgx, go-sql-driver/mysql
// and modernc.org/sqlite all accept one, and whether the row that
// lands is actually NULL, is a fact about them.
//
// The SQLite leg is the one that could not be written at all before
// SetNull existed: Col[T] there has no Expr, the package has no
// untyped Bind, and ColumnValue's methods are unexported, so nothing
// a caller could write put a NULL into an INSERT.

// nullProbe is what each leg asserts: for a given binding, is the
// stored value NULL, and if not, what is it?
type nullProbe struct {
	label     string
	wantNull  bool
	wantValue string
}

var nullProbes = []nullProbe{
	{label: "SetNull", wantNull: true},
	{label: "ValPtr(nil)", wantNull: true},
	{label: "ValPtr(&v)", wantValue: "written"},
	{label: "ValNull(invalid)", wantNull: true},
	{label: "ValNull(valid)", wantValue: "written"},
}

func TestPGBoundNullReachesTheServer(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()

	tbl := pg.NewTable(integration.UniqueName(t, "boundnull"))
	dropPG(t, db, tbl)
	id := pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	bio := pg.Add(tbl, pg.Text("bio").Nullable())
	execPG(t, db, pg.CreateTable(tbl))

	v := "written"
	bindings := []pg.ColumnValue{
		bio.SetNull(),
		bio.ValPtr(nil),
		bio.ValPtr(&v),
		bio.ValNull(sql.Null[string]{}),
		bio.ValNull(sql.Null[string]{V: "written", Valid: true}),
	}

	for i, p := range nullProbes {
		t.Run("insert/"+p.label, func(t *testing.T) {
			key := int64(i + 1)
			if _, err := db.Insert(tbl).Row(id.Val(key), bindings[i]).Exec(ctx); err != nil {
				t.Fatalf("insert: %v", err)
			}
			got := pgReadBio(t, db, tbl, key)
			assertProbe(t, p, got)
		})
	}

	// The same bindings through UPDATE ... SET, since that is a
	// different assembly path and the design's promise is that both
	// carry a bound NULL.
	for i, p := range nullProbes {
		t.Run("update/"+p.label, func(t *testing.T) {
			key := int64(100 + i)
			if _, err := db.Insert(tbl).Row(id.Val(key), bio.Val("seed")).Exec(ctx); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if _, err := db.Update(tbl).Set(bindings[i]).Where(id.Eq(key)).Exec(ctx); err != nil {
				t.Fatalf("update: %v", err)
			}
			got := pgReadBio(t, db, tbl, key)
			assertProbe(t, p, got)
		})
	}
}

func TestMySQLBoundNullReachesTheServer(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()

	tbl := mysql.NewTable(integration.UniqueName(t, "boundnull"))
	dropMySQL(t, db, tbl)
	id := mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	bio := mysql.Add(tbl, mysql.Varchar("bio", 191).Nullable())
	execMySQL(t, db, mysql.CreateTable(tbl))

	v := "written"
	bindings := []mysql.ColumnValue{
		bio.SetNull(),
		bio.ValPtr(nil),
		bio.ValPtr(&v),
		bio.ValNull(sql.Null[string]{}),
		bio.ValNull(sql.Null[string]{V: "written", Valid: true}),
	}

	for i, p := range nullProbes {
		t.Run("insert/"+p.label, func(t *testing.T) {
			key := int64(i + 1)
			if _, err := db.Insert(tbl).Row(id.Val(key), bindings[i]).Exec(ctx); err != nil {
				t.Fatalf("insert: %v", err)
			}
			assertProbe(t, p, mysqlReadBio(t, db, tbl, key))
		})
	}
	for i, p := range nullProbes {
		t.Run("update/"+p.label, func(t *testing.T) {
			key := int64(100 + i)
			if _, err := db.Insert(tbl).Row(id.Val(key), bio.Val("seed")).Exec(ctx); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if _, err := db.Update(tbl).Set(bindings[i]).Where(id.Eq(key)).Exec(ctx); err != nil {
				t.Fatalf("update: %v", err)
			}
			assertProbe(t, p, mysqlReadBio(t, db, tbl, key))
		})
	}
}

func TestSQLiteBoundNullReachesTheEngine(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	tbl := sqlite.NewTable("boundnull")
	id := sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
	bio := sqlite.Add(tbl, sqlite.Text("bio").Nullable())
	exec(t, db, sqlite.CreateTable(tbl))

	v := "written"
	bindings := []sqlite.ColumnValue{
		bio.SetNull(),
		bio.ValPtr(nil),
		bio.ValPtr(&v),
		bio.ValNull(sql.Null[string]{}),
		bio.ValNull(sql.Null[string]{V: "written", Valid: true}),
	}

	for i, p := range nullProbes {
		t.Run("insert/"+p.label, func(t *testing.T) {
			key := int64(i + 1)
			if _, err := db.Insert(tbl).Values(id.Val(key), bindings[i]).Exec(ctx); err != nil {
				t.Fatalf("insert: %v", err)
			}
			assertProbe(t, p, sqliteReadBio(t, db, tbl, key))
		})
	}
	for i, p := range nullProbes {
		t.Run("update/"+p.label, func(t *testing.T) {
			key := int64(100 + i)
			if _, err := db.Insert(tbl).Values(id.Val(key), bio.Val("seed")).Exec(ctx); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if _, err := db.Update(tbl).Set(bindings[i]).Where(id.Eq(key)).Exec(ctx); err != nil {
				t.Fatalf("update: %v", err)
			}
			assertProbe(t, p, sqliteReadBio(t, db, tbl, key))
		})
	}
}

func assertProbe(t *testing.T, p nullProbe, got sql.NullString) {
	t.Helper()
	if p.wantNull {
		if got.Valid {
			t.Errorf("%s stored %q, want NULL", p.label, got.String)
		}
		return
	}
	if !got.Valid || got.String != p.wantValue {
		t.Errorf("%s stored %v, want %q", p.label, got, p.wantValue)
	}
}

// The read-back goes through a raw SELECT into an sql.NullString
// rather than through an entity, so the assertion is about what the
// server holds and not about drops' own scan path.

func pgReadBio(t *testing.T, db *pg.DB, tbl *pg.Table, key int64) sql.NullString {
	t.Helper()
	rows, err := db.Query(context.Background(),
		`SELECT bio FROM `+drops.StdQuoteIdent(tbl.Name())+` WHERE id = $1`, key)
	if err != nil {
		t.Fatal(err)
	}
	return scanOneNullString(t, rows)
}

func mysqlReadBio(t *testing.T, db *mysql.DB, tbl *mysql.Table, key int64) sql.NullString {
	t.Helper()
	rows, err := db.Query(context.Background(),
		"SELECT bio FROM `"+tbl.Name()+"` WHERE id = ?", key)
	if err != nil {
		t.Fatal(err)
	}
	return scanOneNullString(t, rows)
}

func sqliteReadBio(t *testing.T, db *sqlite.DB, tbl *sqlite.Table, key int64) sql.NullString {
	t.Helper()
	rows, err := db.Query(context.Background(),
		`SELECT bio FROM `+drops.StdQuoteIdent(tbl.Name())+` WHERE id = ?`, key)
	if err != nil {
		t.Fatal(err)
	}
	return scanOneNullString(t, rows)
}

func scanOneNullString(t *testing.T, rows drops.Rows) sql.NullString {
	t.Helper()
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no row came back")
	}
	var v sql.NullString
	if err := rows.Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// ----------------------------------------------------------------------
// The read path.

type nullReadRow struct {
	ID  int64   `drop:"id"`
	Bio *string `drop:"bio"`
}

type nullReadWrapped struct {
	ID  int64            `drop:"id"`
	Bio sql.Null[string] `drop:"bio"`
}

// A field that can receive NULL does, through drops' own scanner,
// against a live server. This is the other half of the design: the
// pointer and the named sql.Null[T] are the two shapes NewEntity
// accepts for a nullable column, so both have to actually work.
func TestPGNullScansIntoAReceptiveField(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()

	tbl := pg.NewTable(integration.UniqueName(t, "nullread"))
	dropPG(t, db, tbl)
	id := pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	bio := pg.Add(tbl, pg.Text("bio").Nullable())
	execPG(t, db, pg.CreateTable(tbl))
	if _, err := db.Insert(tbl).Row(id.Val(1), bio.SetNull()).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Insert(tbl).Row(id.Val(2), bio.Val("here")).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	ptrEnt := pg.NewEntity[nullReadRow](tbl)
	gotNull, err := ptrEnt.Get(db, ctx, int64(1))
	if err != nil {
		t.Fatalf("Get(NULL row): %v", err)
	}
	if gotNull.Bio != nil {
		t.Errorf("Bio = %v, want nil", gotNull.Bio)
	}
	gotVal, err := ptrEnt.Get(db, ctx, int64(2))
	if err != nil {
		t.Fatalf("Get(value row): %v", err)
	}
	if gotVal.Bio == nil || *gotVal.Bio != "here" {
		t.Errorf("Bio = %v, want \"here\"", gotVal.Bio)
	}

	// sql.Null[T] must be a named field, never embedded: embedding
	// promotes its Scan onto the struct, which then claims to be a
	// single-column destination.
	wrapEnt := pg.NewEntity[nullReadWrapped](tbl)
	wrapped, err := wrapEnt.Get(db, ctx, int64(1))
	if err != nil {
		t.Fatalf("Get into sql.Null[string]: %v", err)
	}
	if wrapped.Bio.Valid {
		t.Errorf("Bio = %+v, want invalid", wrapped.Bio)
	}
}

// The failure this whole check exists to replace, measured against a
// real server so the comparison is not a guess: a NULL scanned into a
// plain string is a driver error naming a column index, from inside a
// query, at whatever hour the first NULL row shows up.
//
// The entity that would have produced it cannot be declared, and the
// panic that stops it names the column, the field and both fixes.
func TestPGNullIntoAPlainFieldIsRefusedAtDeclaration(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()

	tbl := pg.NewTable(integration.UniqueName(t, "nullrefuse"))
	dropPG(t, db, tbl)
	id := pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	bio := pg.Add(tbl, pg.Text("bio").Nullable())
	execPG(t, db, pg.CreateTable(tbl))
	if _, err := db.Insert(tbl).Row(id.Val(1), bio.SetNull()).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	// What the server and driver do without the check: a raw scan of
	// that very row into a string.
	rows, err := db.Query(ctx, `SELECT bio FROM `+drops.StdQuoteIdent(tbl.Name())+` WHERE id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		rows.Close()
		t.Fatal("no row came back")
	}
	var s string
	scanErr := rows.Scan(&s)
	rows.Close()
	if scanErr == nil {
		t.Fatal("expected the driver to refuse a NULL into a string")
	}
	if !strings.Contains(scanErr.Error(), "converting NULL") {
		t.Logf("driver reported: %v", scanErr)
	}

	// And what drops does instead, before any query runs.
	msg := func() (msg string) {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("expected NewEntity to panic")
			}
			msg, _ = r.(string)
		}()
		type plain struct {
			ID  int64  `drop:"id"`
			Bio string `drop:"bio"`
		}
		pg.NewEntity[plain](tbl)
		return ""
	}()
	for _, want := range []string{`"bio"`, "field Bio", "string"} {
		if !strings.Contains(msg, want) {
			t.Errorf("panic should name %q, got: %s", want, msg)
		}
	}
	if strings.Contains(msg, "converting NULL") {
		t.Errorf("the point is not to repeat the driver's message: %s", msg)
	}
}

// The same measurement on the other two engines, because "the check
// replaces a real failure" is a claim about each of them separately.
// The failure is produced on purpose by waiving the check, so the row
// that reaches the scanner is the row a schema without the check would
// have reached it with.
//
// The error is database/sql's convertAssign, not any driver's, which
// is why all three read alike — but that is a measurement, not an
// assumption, and this is where it is measured.
func TestNullIntoAPlainFieldFailsIdenticallyOnEveryEngine(t *testing.T) {
	type plain struct {
		ID  int64  `drop:"id"`
		Bio string `drop:"bio"`
	}
	type ptr struct {
		ID  int64   `drop:"id"`
		Bio *string `drop:"bio"`
	}
	const want = "converting NULL to string is unsupported"

	t.Run("mysql", func(t *testing.T) {
		db := openMySQL(t)
		ctx := context.Background()
		tbl := mysql.NewTable(integration.UniqueName(t, "nullrefuse"))
		dropMySQL(t, db, tbl)
		id := mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
		bio := mysql.Add(tbl, mysql.Varchar("bio", 191).Nullable())
		execMySQL(t, db, mysql.CreateTable(tbl))
		if _, err := db.Insert(tbl).Row(id.Val(1), bio.SetNull()).Exec(ctx); err != nil {
			t.Fatal(err)
		}
		_, err := mysql.NewEntity[plain](tbl, mysql.AllowNullableColumns("bio")).Get(db, ctx, int64(1))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("waived scan: %v, want %q", err, want)
		}
		got, err := mysql.NewEntity[ptr](tbl).Get(db, ctx, int64(1))
		if err != nil || got.Bio != nil {
			t.Errorf("pointer read: %+v, %v", got, err)
		}
	})

	t.Run("sqlite", func(t *testing.T) {
		db := openSQLite(t)
		ctx := context.Background()
		tbl := sqlite.NewTable("nullrefuse")
		id := sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
		bio := sqlite.Add(tbl, sqlite.Text("bio").Nullable())
		exec(t, db, sqlite.CreateTable(tbl))
		if _, err := db.Insert(tbl).Values(id.Val(1), bio.SetNull()).Exec(ctx); err != nil {
			t.Fatal(err)
		}
		_, err := sqlite.NewEntity[plain](tbl, sqlite.AllowNullableColumns("bio")).Get(db, ctx, int64(1))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("waived scan: %v, want %q", err, want)
		}
		got, err := sqlite.NewEntity[ptr](tbl).Get(db, ctx, int64(1))
		if err != nil || got.Bio != nil {
			t.Errorf("pointer read: %+v, %v", got, err)
		}
	})
}
