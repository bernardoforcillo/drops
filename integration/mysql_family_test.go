package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"

	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/mysql"
)

// The places drops/mysql says the two families part company, asked of
// both of them.
//
// Every claim pinned here was written from documentation and from
// rendered SQL, because for most of this project's life neither server
// was reachable. Each one shapes something drops does or refuses to
// do — which spelling a helper renders, which helper exists at all,
// how long a lock name may be — so a claim that turned out to be wrong
// would be a helper emitting SQL one family cannot parse, which is the
// class of bug this whole directory exists to catch.
//
// A case says what EACH family does, not what one of them does, so a
// divergence that stops being one fails here rather than quietly
// widening what drops thinks it has to work around.
//
// Measured against MySQL 8.0.46 and MariaDB 10.11.14, in their default
// configurations.
type familyCase struct {
	name string
	sql  string
	args []any
	// Each side is "" when the server accepts the statement, and
	// otherwise a fragment that must appear in the error — an error
	// number where the server gives one distinctive enough to match on.
	mysql   string
	mariadb string
	why     string
}

func TestMySQLFamilyDivergences(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	_, _, mariadb := mysqlServerVersion(t, db)

	// One JSON column to aim the accessor cases at, so they exercise
	// the same shape drops renders rather than a literal the parser
	// treats differently.
	tbl := mysql.NewTable(integration.UniqueName(t, "fam"))
	doc := mysql.Add(tbl, mysql.JSON("doc"))
	dropMySQL(t, db, tbl)
	execMySQL(t, db, mysql.CreateTable(tbl))
	if _, err := db.Insert(tbl).Row(doc.Val([]byte(`{"a": 1, "b": {"c": "x"}}`))).Exec(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	name := mysql.Dialect.QuoteIdent(tbl.Name())

	cases := []familyCase{{
		name:    "the -> accessor",
		sql:     "SELECT `doc` -> '$.a' FROM " + name,
		mariadb: "1064",
		why: "json.go renders JSON_EXTRACT for JSONGet rather than ->, " +
			"and this is the reason: MariaDB has no such operator, on a " +
			"JSON column or anywhere else.",
	}, {
		name:    "CAST(x AS JSON)",
		sql:     "SELECT CAST('{\"a\":1}' AS JSON)",
		mariadb: "1064",
		why: "json.go leaves pg's ToJSON out because there is no " +
			"spelling that means it on both: MariaDB's JSON is an alias " +
			"for LONGTEXT and there is no type to cast to.",
	}, {
		name:  "JSON_QUERY",
		sql:   "SELECT JSON_QUERY('{\"a\":{\"b\":1}}', '$.a')",
		mysql: "1305",
		why: "the one helper in json.go that works on a single family. " +
			"MySQL took JSON_VALUE from the standard clause and stopped, " +
			"so mysql.JSONQuery says MariaDB only.",
	}, {
		name:    "JSON_VALUE with a bound path",
		sql:     "SELECT JSON_VALUE(`doc`, ?) FROM " + name,
		args:    []any{"$.b.c"},
		mysql:   "1064",
		mariadb: "",
		why: "MySQL takes JSON_VALUE's path in its grammar rather than " +
			"as an argument. mysql.JSONValue writes the path in as a " +
			"literal for this reason; before it did, it rendered a " +
			"statement MySQL could not parse and MariaDB could.",
	}, {
		name:    "the VALUES row alias in an upsert",
		sql:     "INSERT INTO " + name + " (`doc`) VALUES ('{}') AS new ON DUPLICATE KEY UPDATE `doc` = new.`doc`",
		mariadb: "1064",
		why: "eventstore.go and the upsert builders use the older " +
			"VALUES(col) spelling. MySQL 8.0.19's row alias is the " +
			"replacement and MariaDB has never had it.",
	}, {
		name:  "a user-level lock name past 64 characters",
		sql:   "SELECT GET_LOCK(REPEAT('x', 100), 0)",
		mysql: "4163",
		why: "outboxLockName folds a name back under 64 characters with " +
			"a hash. 64 is the portable ceiling and not MySQL pedantry: " +
			"MariaDB takes this one, and rejects at 193.",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.mysql
			if mariadb {
				want = tc.mariadb
			}
			err, _ := mysqlStreamErr(t, func() (drops.Rows, error) { return db.Query(ctx, tc.sql, tc.args...) })
			switch {
			case want == "" && err != nil:
				t.Errorf("this family is supposed to accept %s: %v\n%s", tc.name, err, tc.why)
			case want != "" && err == nil:
				t.Errorf("this family is supposed to reject %s with %s, and took it\n%s", tc.name, want, tc.why)
			case want != "" && !strings.Contains(err.Error(), want):
				t.Errorf("rejected %s with %v, want an error carrying %s\n%s", tc.name, err, want, tc.why)
			}
		})
	}

	// The other side of the lock-name cap, which is what makes 64 the
	// portable ceiling rather than one family's whim.
	if mariadb {
		err, _ := mysqlStreamErr(t, func() (drops.Rows, error) {
			return db.Query(ctx, "SELECT GET_LOCK(REPEAT('y', 193), 0)")
		})
		if err == nil {
			t.Error("MariaDB is supposed to reject a lock name past 192 characters")
		}
	}
}

// What both families do with a NON-ASCII case pair in an identifier,
// which drops/mysql's identKey has always declined to guess at.
//
// identKey folds ASCII and stops, and the reason recorded beside it
// used to be that no MySQL was reachable to ask. Both families are
// reachable now and they agree, so this is not a divergence — it is
// the answer to a question the package had left open.
//
// Over the utf8mb4 connection this suite and drops both use, MySQL
// 8.0.46 and MariaDB 10.11.14 alike resolve `tenantÉ` to the column
// declared `tenanté`. That it is a CASE fold and not the accent
// insensitivity of utf8mb4_general_ci is the second assertion: the
// unaccented `tenante` is a different column on both, error 1054.
//
// Two things this does NOT show, because measuring them wrong is
// easy. It is not a statement about latin1 connections: under one,
// the exact lowercase spelling fails too, so the identifier bytes are
// being misread rather than folded, and nothing about case can be
// concluded from it. And it is not a reason to widen identKey. The
// invariant the tenant policy block states — identKey never reads two
// names as one column unless the server does — is still satisfied by
// an ASCII-only fold, which errs NARROW: the guard answers no for a
// handle the renderer answers yes for. Widening it is a change to
// make deliberately against this measurement, not a bug fix.
func TestMySQLFoldsANonASCIICasePairInAnIdentifier(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()

	tbl := mysql.NewTable(integration.UniqueName(t, "na"))
	mysql.Add(tbl, mysql.BigInt("id"))
	lower := mysql.Add(tbl, mysql.Integer("tenanté"))
	dropMySQL(t, db, tbl)
	execMySQL(t, db, mysql.CreateTable(tbl))
	if _, err := db.Insert(tbl).Row(lower.Val(7)).Exec(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	name := mysql.Dialect.QuoteIdent(tbl.Name())

	// The uppercase spelling of the same name: one column, or two.
	if got := mysqlScalar(t, db, "SELECT `tenantÉ` FROM "+name); got != "7" {
		t.Errorf("`tenantÉ` read %q, want the 7 written through `tenanté` — "+
			"the two spellings are supposed to be one column", got)
	}

	// And the accent is not folded away with the case, which is what
	// makes this a case fold rather than utf8mb4_general_ci deciding
	// that é and e are the same letter.
	err, _ := mysqlStreamErr(t, func() (drops.Rows, error) {
		return db.Query(ctx, "SELECT `tenante` FROM "+name)
	})
	if err == nil {
		t.Fatal("`tenante` resolved to `tenanté`; identifier matching is supposed to fold case " +
			"and not accents, and identKey's rule is written for a fold that leaves accents alone")
	}
	if !strings.Contains(err.Error(), "1054") {
		t.Errorf("`tenante` was rejected with %v, want error 1054", err)
	}
}
