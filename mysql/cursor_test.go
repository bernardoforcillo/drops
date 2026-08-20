package mysql_test

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/mysql"
)

// postsTable is the schema the cursor tests page over: a created_at
// sort column, a unique id to break ties, and a nullable column for
// the NULL-placement cases.
func postsTable() (*mysql.Table, *mysql.Col[int64], *mysql.Col[time.Time], *mysql.Col[string], *mysql.Col[string]) {
	t := mysql.NewTable("posts")
	id := mysql.Add(t, mysql.BigSerial("id").PrimaryKey())
	created := mysql.Add(t, mysql.Timestamp("created_at", false).NotNull())
	title := mysql.Add(t, mysql.Varchar("title", 255).NotNull())
	nick := mysql.Add(t, mysql.Varchar("nick", 64))
	return t, id, created, title, nick
}

func TestCursorEncodeDecodeRoundTrip(t *testing.T) {
	tbl := mysql.NewTable("t")
	spec := mysql.NewCursorSpec(
		mysql.OrderKey{Col: mysql.Add(tbl, mysql.BigInt("a"))},
		mysql.OrderKey{Col: mysql.Add(tbl, mysql.Varchar("b", 64))},
		mysql.OrderKey{Col: mysql.Add(tbl, mysql.Boolean("c"))},
		mysql.OrderKey{Col: mysql.Add(tbl, mysql.DoublePrecision("d"))},
		mysql.OrderKey{Col: mysql.Add(tbl, mysql.Timestamp("e", false))},
		mysql.OrderKey{Col: mysql.Add(tbl, mysql.Blob("f"))},
		mysql.OrderKey{Col: mysql.Add(tbl, mysql.BigInt("g").PrimaryKey())},
	)
	now := time.Now().UTC().Truncate(time.Microsecond)
	values := []any{int64(7), "hello", true, 1.5, now, []byte("payload"), int64(1)}

	cur, err := mysql.EncodeCursor(spec, values...)
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	got, err := cur.Decode()
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(got) != len(values) {
		t.Fatalf("len = %d, want %d", len(got), len(values))
	}
	for i, want := range values {
		switch w := want.(type) {
		case time.Time:
			if !got[i].(time.Time).Equal(w) {
				t.Errorf("[%d] time: got %v want %v", i, got[i], w)
			}
		case []byte:
			gb, ok := got[i].([]byte)
			if !ok || string(gb) != string(w) {
				t.Errorf("[%d] bytes: got %v want %v", i, got[i], w)
			}
		default:
			if got[i] != want {
				t.Errorf("[%d] got %v (%T) want %v (%T)", i, got[i], got[i], want, want)
			}
		}
	}
}

func TestCursorEncodeRejectsLenMismatch(t *testing.T) {
	_, id, _, _, _ := postsTable()
	spec := mysql.NewCursorSpec(mysql.OrderKey{Col: id})
	if _, err := mysql.EncodeCursor(spec, int64(1), int64(2)); err == nil {
		t.Error("expected a length mismatch error")
	}
}

// A cursor is a token handed to a client, so it comes back as bytes
// the client controls. Anything that is not a cursor this package
// issued must be refused rather than half-decoded.
func TestCursorRejectsForeignAndCorruptTokens(t *testing.T) {
	_, id, _, _, _ := postsTable()
	spec := mysql.NewCursorSpec(mysql.OrderKey{Col: id})
	good, err := mysql.EncodeCursor(spec, int64(1))
	if err != nil {
		t.Fatal(err)
	}

	// The shape drops/pg and drops/sqlite emit: the same typed JSON,
	// without the backend stamp. It decodes cleanly as JSON, which is
	// exactly why it has to be rejected on the stamp.
	foreign := mysql.Cursor(base64Raw(`{"v":[{"t":"i","v":1}]}`))

	for _, tc := range []struct {
		name string
		cur  mysql.Cursor
	}{
		{"empty", ""},
		{"not base64", "!!!!"},
		{"not JSON", mysql.Cursor(base64Raw("nonsense"))},
		{"another backend's cursor", foreign},
		{"unknown value type", mysql.Cursor(base64Raw(`{"d":"mysql","v":[{"t":"q","v":1}]}`))},
	} {
		if _, err := tc.cur.Decode(); err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
	}
	if _, err := good.Decode(); err != nil {
		t.Errorf("a cursor this package issued must decode: %v", err)
	}
}

// The keyset guard is written out as a disjunction on purpose: MariaDB
// does not turn a row constructor into a range scan. See the package
// doc on cursor.go for the measurement.
func TestAfterCursorRendersTheExpandedGuard(t *testing.T) {
	tbl, id, created, _, _ := postsTable()
	spec := mysql.NewCursorSpec(
		mysql.OrderKey{Col: created},
		mysql.OrderKey{Col: id},
	)
	cur, err := mysql.EncodeCursor(spec, time.Unix(0, 0).UTC(), int64(42))
	if err != nil {
		t.Fatal(err)
	}
	db := mysql.New(&fakeDriver{})
	sql, args := sqlOf(db.Select().From(tbl).OrderByCursor(spec).AfterCursor(spec, cur).Limit(20))

	for _, want := range []string{
		"(`posts`.`created_at` > ?)",
		"((`posts`.`created_at` = ?) AND (`posts`.`id` > ?))",
		"ORDER BY `posts`.`created_at` ASC, `posts`.`id` ASC",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing %s\ngot: %s", want, sql)
		}
	}
	if strings.Contains(sql, ") > (") {
		t.Errorf("row-constructor comparison must not be emitted: %s", sql)
	}
	// created, created, id, limit.
	if len(args) != 4 {
		t.Errorf("args = %v, want 4", args)
	}
}

func TestBeforeCursorFlipsEveryComparison(t *testing.T) {
	tbl, id, created, _, _ := postsTable()
	spec := mysql.NewCursorSpec(
		mysql.OrderKey{Col: created, Desc: true},
		mysql.OrderKey{Col: id, Desc: true},
	)
	cur, err := mysql.EncodeCursor(spec, time.Unix(0, 0).UTC(), int64(42))
	if err != nil {
		t.Fatal(err)
	}
	db := mysql.New(&fakeDriver{})
	forward, _ := sqlOf(db.Select().From(tbl).AfterCursor(spec, cur))
	backward, _ := sqlOf(db.Select().From(tbl).BeforeCursor(spec, cur))

	// Descending forward walks down the order, so it compares "<";
	// backward over the same spec compares ">".
	if !strings.Contains(forward, "(`posts`.`created_at` < ?)") {
		t.Errorf("forward: %s", forward)
	}
	if !strings.Contains(backward, "(`posts`.`created_at` > ?)") {
		t.Errorf("backward: %s", backward)
	}
}

// Neither server accepts NULLS FIRST / NULLS LAST, and both sort NULL
// as the smallest value. The guard has to encode that placement itself.
func TestKeysetGuardHandlesNulls(t *testing.T) {
	tbl, id, _, _, nick := postsTable()

	asc := mysql.NewCursorSpec(
		mysql.OrderKey{Col: nick},
		mysql.OrderKey{Col: id},
	)
	desc := mysql.NewCursorSpec(
		mysql.OrderKey{Col: nick, Desc: true},
		mysql.OrderKey{Col: id, Desc: true},
	)
	db := mysql.New(&fakeDriver{})

	// Ascending past a NULL: every non-NULL row is still to come, and
	// the remaining NULL rows are picked up by the id tiebreaker.
	atNull, err := mysql.EncodeCursor(asc, nil, int64(7))
	if err != nil {
		t.Fatal(err)
	}
	sql, _ := sqlOf(db.Select().From(tbl).AfterCursor(asc, atNull))
	for _, want := range []string{
		"(`posts`.`nick` IS NOT NULL)",
		"((`posts`.`nick` IS NULL) AND (`posts`.`id` > ?))",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("ascending past NULL missing %s\ngot: %s", want, sql)
		}
	}

	// Descending past a NULL: NULLs sort last, so nothing follows on
	// that key and only the tiebreaker contributes.
	descAtNull, err := mysql.EncodeCursor(desc, nil, int64(7))
	if err != nil {
		t.Fatal(err)
	}
	sql, _ = sqlOf(db.Select().From(tbl).AfterCursor(desc, descAtNull))
	if strings.Contains(sql, "`posts`.`nick` <") {
		t.Errorf("nothing sorts past a trailing NULL: %s", sql)
	}
	if !strings.Contains(sql, "((`posts`.`nick` IS NULL) AND (`posts`.`id` < ?))") {
		t.Errorf("descending past NULL: %s", sql)
	}

	// Descending past a value on a nullable column must reach the
	// NULLs that follow it.
	descAtValue, err := mysql.EncodeCursor(desc, "m", int64(7))
	if err != nil {
		t.Fatal(err)
	}
	sql, _ = sqlOf(db.Select().From(tbl).AfterCursor(desc, descAtValue))
	if !strings.Contains(sql, "((`posts`.`nick` < ?) OR (`posts`.`nick` IS NULL))") {
		t.Errorf("descending on a nullable column must include the NULLs: %s", sql)
	}
}

// The same walk over a NOT NULL column must not pay for a disjunct
// that can never match.
func TestKeysetGuardOmitsNullBranchOnNotNullColumn(t *testing.T) {
	tbl, id, created, _, _ := postsTable()
	spec := mysql.NewCursorSpec(
		mysql.OrderKey{Col: created, Desc: true},
		mysql.OrderKey{Col: id, Desc: true},
	)
	cur, err := mysql.EncodeCursor(spec, time.Unix(0, 0).UTC(), int64(1))
	if err != nil {
		t.Fatal(err)
	}
	sql, _ := sqlOf(mysql.New(&fakeDriver{}).Select().From(tbl).AfterCursor(spec, cur))
	if strings.Contains(sql, "`created_at` IS NULL") {
		t.Errorf("NOT NULL column needs no IS NULL branch: %s", sql)
	}
}

// A sort key with no unique tail is refused, because MySQL's default
// collation makes ties common: 'alice', 'Alice' and 'ALICE' are one
// group, and a strict comparison steps over all three.
func TestCursorSpecRefusesANonUniqueTail(t *testing.T) {
	tbl, id, _, title, nick := postsTable()
	db := mysql.New(&fakeDriver{})

	sql, _ := sqlOf(db.Select().From(tbl).OrderByCursor(mysql.NewCursorSpec(mysql.OrderKey{Col: title})))
	if !strings.Contains(sql, "FALSE") || !strings.Contains(sql, "posts.title") {
		t.Errorf("a non-unique tail must be refused, naming the column: %s", sql)
	}
	if strings.Contains(sql, "ORDER BY") {
		t.Errorf("a refused spec must not order the query either: %s", sql)
	}

	// A nullable UNIQUE column is not a tiebreaker: MySQL allows any
	// number of NULLs in a unique index.
	nullableUnique := mysql.NewTable("n")
	mysql.Add(nullableUnique, mysql.BigSerial("id").PrimaryKey())
	nu := mysql.Add(nullableUnique, mysql.Varchar("slug", 64).Unique())
	sql, _ = sqlOf(db.Select().From(nullableUnique).OrderByCursor(mysql.NewCursorSpec(mysql.OrderKey{Col: nu})))
	if !strings.Contains(sql, "FALSE") {
		t.Errorf("a nullable UNIQUE tail must be refused: %s", sql)
	}

	// Appending the primary key fixes it.
	ok := mysql.NewCursorSpec(mysql.OrderKey{Col: title}, mysql.OrderKey{Col: id})
	sql, _ = sqlOf(db.Select().From(tbl).OrderByCursor(ok))
	if strings.Contains(sql, "FALSE") {
		t.Errorf("a spec ending on the primary key must be accepted: %s", sql)
	}

	// And so does saying you know better.
	sql, _ = sqlOf(db.Select().From(tbl).
		OrderByCursor(mysql.NewCursorSpec(mysql.OrderKey{Col: nick}).AllowNonUniqueKey()))
	if strings.Contains(sql, "FALSE") {
		t.Errorf("AllowNonUniqueKey must page anyway: %s", sql)
	}
}

// A composite primary key is a tiebreaker only when every one of its
// columns is in the spec.
func TestCursorSpecCompositePrimaryKey(t *testing.T) {
	tbl := mysql.NewTable("memberships")
	org := mysql.Add(tbl, mysql.BigInt("org_id").PrimaryKey())
	user := mysql.Add(tbl, mysql.BigInt("user_id").PrimaryKey())
	db := mysql.New(&fakeDriver{})

	sql, _ := sqlOf(db.Select().From(tbl).OrderByCursor(mysql.NewCursorSpec(mysql.OrderKey{Col: org})))
	if !strings.Contains(sql, "FALSE") {
		t.Errorf("half a composite key is not unique: %s", sql)
	}
	sql, _ = sqlOf(db.Select().From(tbl).OrderByCursor(
		mysql.NewCursorSpec(mysql.OrderKey{Col: org}, mysql.OrderKey{Col: user})))
	if strings.Contains(sql, "FALSE") {
		t.Errorf("both key columns present: %s", sql)
	}
}

// A single-column UNIQUE index declared on the table counts too.
func TestCursorSpecUniqueIndexTail(t *testing.T) {
	tbl := mysql.NewTable("accounts")
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	email := mysql.Add(tbl, mysql.Varchar("email", 255).NotNull())
	tbl.AddIndex(mysql.NewIndex("uq_accounts_email", tbl, email).Unique())

	sql, _ := sqlOf(mysql.New(&fakeDriver{}).Select().From(tbl).
		OrderByCursor(mysql.NewCursorSpec(mysql.OrderKey{Col: email})))
	if strings.Contains(sql, "FALSE") {
		t.Errorf("a UNIQUE index is a tiebreaker: %s", sql)
	}
}

// A cursor taken under one ordering must not be spent under another:
// the page it produces looks plausible and comes from the wrong end of
// the table.
func TestCursorIsBoundToItsOrdering(t *testing.T) {
	tbl, id, created, _, _ := postsTable()
	newest := mysql.NewCursorSpec(
		mysql.OrderKey{Col: created, Desc: true},
		mysql.OrderKey{Col: id, Desc: true},
	)
	oldest := mysql.NewCursorSpec(
		mysql.OrderKey{Col: created},
		mysql.OrderKey{Col: id},
	)
	cur, err := mysql.EncodeCursor(newest, time.Unix(0, 0).UTC(), int64(1))
	if err != nil {
		t.Fatal(err)
	}
	sql, _ := sqlOf(mysql.New(&fakeDriver{}).Select().From(tbl).AfterCursor(oldest, cur))
	if !strings.Contains(sql, "FALSE") || !strings.Contains(sql, "different ORDER BY") {
		t.Errorf("a cursor from another ordering must be refused: %s", sql)
	}
}

// The refusal tag is a comment holding text the client supplied, so it
// must not be able to close the comment — or, on MySQL, to open a
// version-gated one the server executes.
func TestCursorErrorTagCannotEscapeItsComment(t *testing.T) {
	_, id, _, _, _ := postsTable()
	spec := mysql.NewCursorSpec(mysql.OrderKey{Col: id})
	evil := mysql.Cursor(base64Raw(`{"d":"mysql","v":[{"t":"*/ OR 1=1 -- /*!","v":1}]}`))

	sql, _ := sqlOf(mysql.New(&fakeDriver{}).Select().From(id.Table()).AfterCursor(spec, evil))
	if strings.Contains(sql, "*/ OR") || strings.Contains(sql, "/*!") {
		t.Errorf("comment escape survived: %s", sql)
	}
	if !strings.Contains(sql, "FALSE") {
		t.Errorf("guard must still be false: %s", sql)
	}
}

// base64Raw is the URL-safe unpadded encoding cursors travel in.
func base64Raw(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}
